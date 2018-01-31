package main

import (
	"context"
	"fmt"
	"go/build"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cheggaaa/pb"
	"github.com/fsnotify/fsnotify"
	"github.com/jawher/mow.cli"
	"github.com/xlab/closer"
	"github.com/xlab/pace"
)

var app = cli.App("GodyBuilder", "Auto-compile daemon and parallel builder for Go executables targeting Docker environments.")
var goPaths []string

func init() {
	log.SetFlags(0)
	if paths := os.Getenv("GOPATH"); len(paths) == 0 {
		log.Fatalln("GOPATH env variable is not set")
	} else {
		goPaths = strings.Split(paths, ":")
	}
}

func main() {
	app.Command("build", "Build all listed Go packages using shared state in a Docker container.", buildCmd)
	app.Command("watch", "Start watching for changes in source code of packages and their deps.", watchCmd)
	app.Command("cleanup", "Removes the builder container completely.", cleanupCmd)
	if err := app.Run(os.Args); err != nil {
		log.Fatalln(err)
	}
}

func buildCmd(c *cli.Cmd) {
	debug := c.BoolOpt("v verbose", false, "Verbosive logging.")
	packages := c.StringsOpt("p pkg", nil, "Adds packages into list to build.")
	godyImage := c.StringOpt("i image", "bringhub/gody_build", "The builder container image to use.")
	godyName := c.StringOpt("n name", "gody_builder_1", "Override the builder container name.")
	outDir := c.StringOpt("o out", "bin/", "Output directory for executable artifacts.")
	workers := c.IntOpt("j jobs", 2, "Number of parallel build jobs.")
	fancy := c.BoolOpt("f fancy", true, "Enables dynamic reporting.")
	progress := c.BoolOpt("progress", false, "Enables progress bar.")

	c.Action = func() {
		defer closer.Close()

		if *debug {
			if len(*packages) == 0 {
				log.Println("gody: no packages specified, just preparing the builder container")
			} else {
				log.Println("gody: building", len(*packages), "packages")
			}
		}

		var spinStop func()
		var spinDone <-chan struct{}
		if *fancy && len(*packages) > 0 {
			spinStop, spinDone = spinWith("gody: analysing packages")
		}
		for _, p := range *packages {
			if pkg := findPackage(p, *debug); pkg == nil {
				closer.Fatalln("gody: could not find package in GOPATH:", p)
			} else if !pkg.IsCommand() {
				closer.Fatalf("gody: package %s is not a command", p)
			}
		}
		if *fancy && len(*packages) > 0 {
			spinStop()
			<-spinDone
		}
		if err := os.MkdirAll(*outDir, 0755); err != nil {
			closer.Fatalln("gody: failed to prepare output dir:", err)
		}

		outAbs, _ := filepath.Abs(*outDir)
		mounts := make(map[string]string)
		gopathsEnv := make([]string, 0, 1)
		for i, p := range goPaths {
			abs, _ := filepath.Abs(p)
			mounts[filepath.Join(abs, "src")] = fmt.Sprintf("/gopath_%d/src", i+1)
			mounts[outAbs] = fmt.Sprintf("/gopath_%d/bin", i+1)
			gopathsEnv = append(gopathsEnv, fmt.Sprintf("/gopath_%d", i+1))
		}
		env := map[string]string{
			"GOPATH": strings.Join(gopathsEnv, ":"),
		}
		if *debug {
			log.Println("gody: mounts", mounts)
			log.Println("gody: env", env)
		}
		if *fancy {
			spinStop, spinDone = spinWith("gody: starting build container")
			closer.Bind(func() {
				spinStop()
				<-spinDone
			})
		}
		d, err := NewContainer(*godyName, *godyImage, mounts, env)
		if err != nil {
			closer.Fatalln(err)
		}
		if err := d.Start(); err != nil {
			closer.Fatalln("gody: failed to start container:", err)
		}
		closer.Bind(func() {
			if err := d.Stop(0); err != nil {
				log.Println("gody: failed to stop container:", err)
			}
		})
		if len(*packages) == 0 {
			return
		}
		spinStop()
		<-spinDone

		var onBuiltFunc func(pkg string)
		var p pace.Pace
		if *fancy && *progress {
			bar := pb.New(len(*packages))
			bar.ShowPercent = true
			bar.ShowSpeed = false
			bar.ShowTimeLeft = true
			bar.ShowFinalTime = true
			bar.RefreshRate = time.Second
			onBuiltFunc = func(pkg string) {
				bar.Add(1)
			}
			bar.Start()
			closer.Bind(func() {
				bar.Finish()
			})
		} else {
			p = pace.New("pkgs", time.Minute, pace.DefaultReporter())
			closer.Bind(func() {
				p.Report(pace.DefaultReporter())
			})
			onBuiltFunc = func(pkg string) {
				p.Step(1)
			}
		}

		b := NewBuilder(d, *debug)
		ctx, cancelFn := context.WithCancel(context.Background())
		closer.Bind(cancelFn)
		if *debug {
			log.Println("gody: using", *workers, "parallel jobs")
		}
		spinStop, spinDone = spinWith("gody")
		switch err = b.Build(ctx, *workers, onBuiltFunc, *packages...); err {
		case error(nil):
			return
		case context.Canceled, context.DeadlineExceeded:
			if *debug {
				log.Println("gody: build cancelled")
			}
			return
		default:
			closer.Fatalln(err)
		}
	}
}

func watchCmd(c *cli.Cmd) {
	debug := c.BoolOpt("v verbose", false, "Verbosive logging.")
	packages := c.StringsOpt("p pkg", nil, "Adds packages into list to build.")
	godyImage := c.StringOpt("i image", "bringhub/gody_build", "The builder container image to use.")
	godyName := c.StringOpt("n name", "gody_builder_1", "Override the builder container name.")
	outDir := c.StringOpt("o out", "bin/", "Output directory for executable artifacts.")
	workers := c.IntOpt("j jobs", 2, "Number of parallel build jobs.")
	fancy := c.BoolOpt("f fancy", true, "Enables dynamic reporting.")
	exclude := c.StringsOpt("E exclude", nil, "Exclude specfic path prefixes (e.g. vendor).")
	include := c.StringsOpt("I include", nil, "Include package prefixes.")
	notify := c.StringOpt("N notify", "docker", "Specify a notification backend. By default restarts a docker container.")
	notifyHold := c.StringOpt("H hold", "1s", "Specify holding time before issuing a notification after collecting events.")

	c.Action = func() {
		defer closer.Close()
		if len(*packages) == 0 {
			closer.Fatalln("gody: no packages specified")
		} else if len(*include) == 0 {
			closer.Fatalln("gody: no watch prefixes specified (see -I option)")
		}
		if *debug {
			log.Println("gody: submitted", len(*packages), "packages")
		}
		backend, pattern := decodeNotify(*notify)
		if *debug {
			log.Printf("gody: using %s notification backend with pattern %s", backend, pattern)
		}
		var r Restarter
		if backend == NotifyDocker {
			if rr, err := NewRestarter(); err != nil {
				closer.Fatalln("gody: failed to init Docker restarter", err)
			} else {
				r = rr
			}
		}

		var spinStop func()
		var spinDone <-chan struct{}
		if *fancy && len(*packages) > 0 {
			spinStop, spinDone = spinWith("gody: analysing packages")
		}
		deps := make(map[string]map[string]struct{})
		for _, p := range *packages {
			if pkg := findPackage(p, *debug); pkg == nil {
				closer.Fatalln("gody: could not find package in GOPATH:", p)
			} else if !pkg.IsCommand() {
				closer.Fatalf("gody: package %s is not a command", p)
			} else {
				if d, err := findDeps(pkg, *include, *exclude, *debug); err != nil {
					closer.Fatalln(err)
				} else {
					for path := range d {
						if deps[path] == nil {
							deps[path] = make(map[string]struct{})
						}
						deps[path][pkg.ImportPath] = struct{}{}
					}
				}
			}
			if tmp, _, err := compilePattern(pattern, p); err != nil {
				closer.Fatalf("gody: failed to compile Rx pattern: %s error: %v", tmp, err)
			}
		}
		if *fancy && len(*packages) > 0 {
			spinStop()
			<-spinDone
		}
		if err := os.MkdirAll(*outDir, 0755); err != nil {
			closer.Fatalln("gody: failed to prepare output dir:", err)
		}

		outAbs, _ := filepath.Abs(*outDir)
		mounts := make(map[string]string)
		gopathsEnv := make([]string, 0, 1)
		for i, p := range goPaths {
			abs, _ := filepath.Abs(p)
			mounts[filepath.Join(abs, "src")] = fmt.Sprintf("/gopath_%d/src", i+1)
			mounts[outAbs] = fmt.Sprintf("/gopath_%d/bin", i+1)
			gopathsEnv = append(gopathsEnv, fmt.Sprintf("/gopath_%d", i+1))
		}
		env := map[string]string{
			"GOPATH": strings.Join(gopathsEnv, ":"),
		}
		if *debug {
			log.Println("gody: mounts", mounts)
			log.Println("gody: env", env)
		}
		if *fancy {
			spinStop, spinDone = spinWith("gody: starting build container")
			closer.Bind(func() {
				spinStop()
				<-spinDone
			})
		}
		d, err := NewContainer(*godyName, *godyImage, mounts, env)
		if err != nil {
			closer.Fatalln(err)
		}
		if err := d.Start(); err != nil {
			closer.Fatalln("gody: failed to start container:", err)
		}
		closer.Bind(func() {
			if err := d.Stop(0); err != nil {
				log.Println("gody: failed to stop container:", err)
			}
		})
		if len(*packages) == 0 {
			return
		}
		spinStop()
		<-spinDone

		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			closer.Fatalln(err)
		}
		closer.Bind(func() {
			watcher.Close()
		})
		pkgC := make(chan string, 1024)
		closer.Bind(func() {
			close(pkgC)
		})
		w := NewFSWatcher(watcher, duration(*notifyHold, time.Second), false)
		w.SetOnChange(func(ids []string) {
			log.Printf("gody: rebuilding %d packages", len(ids))
			for _, id := range ids {
				pkgC <- id
			}
		})
		closer.Bind(w.Close)
		for path, ids := range deps {
			for id := range ids {
				if err := w.Add(id, path); err != nil {
					log.Println("gody watch:", id, err)
				}
			}
		}
		onBuiltFunc := func(pkg string) {
			pattern, patternRx, err := compilePattern(pattern, pkg)
			switch backend {
			case NotifyPrint:
				log.Println("gody: updated", pattern)
			case NotifyDocker:
				if err != nil {
					log.Printf("gody: failed to compile pattern %s error: %v", pattern, err)
					return
				}
				if *debug {
					log.Println("gody: restarting docker containers matching", pattern)
				}
				go func() {
					if n, err := r.RestartRx(patternRx, time.Minute); err != nil {
						log.Println("gody: failed to restart", err)
					} else if *debug {
						log.Printf("gody: restarted %d containers", n)
					}
				}()
			}
		}
		b := NewBuilder(d, *debug)
		ctx, cancelFn := context.WithCancel(context.Background())
		closer.Bind(cancelFn)
		if *debug {
			log.Println("gody: using", *workers, "parallel jobs")
		}
		switch err = b.BuildOnRequest(ctx, *workers, onBuiltFunc, pkgC); err {
		case error(nil):
			return
		case context.Canceled, context.DeadlineExceeded:
			if *debug {
				log.Println("gody: build cancelled")
			}
			return
		default:
			closer.Fatalln(err)
		}
	}
}

func cleanupCmd(c *cli.Cmd) {
	debug := c.BoolOpt("v verbose", false, "Log every invoked command.")
	godyName := c.StringOpt("n name", "gody_builder_1", "Override the builder container name.")
	c.Action = func() {
		defer closer.Close()
		if *debug {
			log.Println("gody: will terminate and remove", *godyName)
		}
		d, err := NewContainer(*godyName, "", nil, nil)
		if err != nil {
			closer.Fatalln(err)
		}
		d.Stop(0)
		if err := d.Remove(); err != nil {
			closer.Fatalln(err)
		}
	}
}

func findPackage(name string, debug bool) *build.Package {
	pkg, err := build.Import(name, "", 0)
	if err != nil {
		if debug {
			log.Println("findPackage:", err)
		}
		return nil
	}
	return pkg
}

func findDeps(p *build.Package, includes, excludes []string, debug bool) (map[string]struct{}, error) {
	excludeRxs, err := compileRxs(excludes)
	if err != nil {
		return nil, err
	}
	deps := make(map[string]struct{}, len(p.Imports))
	addDeps := func(dep *build.Package) {
		if !containsPrefix(dep.ImportPath, includes) {
			return
		}
		for _, f := range dep.GoFiles {
			deps[filepath.Join(dep.Dir, f)] = struct{}{}
		}
		for _, f := range dep.HFiles {
			deps[filepath.Join(dep.Dir, f)] = struct{}{}
		}
		for _, f := range dep.CFiles {
			deps[filepath.Join(dep.Dir, f)] = struct{}{}
		}
	}
	seenDeps := make(map[string]struct{}, len(p.Imports))
	addImports := func(p *build.Package) []*build.Package {
		addDeps(p)
		seenDeps[p.ImportPath] = struct{}{}
		list := make([]*build.Package, 0, len(p.Imports))
		for _, pkg := range p.Imports {
			if _, ok := seenDeps[pkg]; ok {
				continue
			}
			if dep, err := build.Import(pkg, "", 0); err == nil {
				addDeps(dep)
				seenDeps[dep.ImportPath] = struct{}{}
				list = append(list, dep)
			}
		}
		return list
	}
	list := addImports(p)
	for len(list) > 0 {
		newList := make([]*build.Package, 0, len(list))
		for _, p := range list {
			newList = append(newList, addImports(p)...)
		}
		list = newList
	}
	for path := range deps {
		if isMatching(path, excludeRxs) {
			if debug {
				log.Println("gody watch: skipping", path)
			}
			delete(deps, path)
		}
	}
	return deps, nil
}

func isMatching(path string, rxs []*regexp.Regexp) bool {
	for _, rx := range rxs {
		if rx.MatchString(path) {
			return true
		}
	}
	return false
}

func compileRxs(rxs []string) ([]*regexp.Regexp, error) {
	var compiled []*regexp.Regexp
	for _, rx := range rxs {
		r, err := regexp.Compile(rx)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Regexp: %s error: %v", rx, err)
		}
		compiled = append(compiled, r)
	}
	return compiled, nil
}

func containsPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if p == "..." {
			return true
		}
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

type NotifyBackend string

const (
	NotifyDocker NotifyBackend = "docker"
	NotifyPrint  NotifyBackend = "print"
)

func decodeNotify(str string) (backend NotifyBackend, pattern string) {
	parts := strings.Split(str, ":")
	backend = NotifyBackend(parts[0])
	switch backend {
	case NotifyDocker:
		if len(parts) == 1 {
			pattern = "docker_{cmd-dashed}_\\d+"
			return
		}
		pattern = strings.Join(parts[1:], ":")
		return
	case NotifyPrint:
		if len(parts) == 1 {
			pattern = "{pkg}"
			return
		}
		pattern = strings.Join(parts[1:], ":")
		return
	default:
		log.Println("Warning: unknown notification backend:", backend)
		return
	}
}

func compilePattern(pattern string, pkgName string) (string, *regexp.Regexp, error) {
	parts := strings.Split(pkgName, "/")
	cmdName := parts[len(parts)-1]
	pattern = strings.Replace(pattern, "{pkg}", pkgName, -1)
	pattern = strings.Replace(pattern, "{cmd-dashed}", strings.Replace(cmdName, "_", "(_|-)", -1), -1)
	pattern = strings.Replace(pattern, "{cmd}", cmdName, -1)
	rx, err := regexp.Compile(pattern)
	if err != nil {
		return pattern, nil, err
	}
	return pattern, rx, nil
}

func duration(s string, defaults time.Duration) time.Duration {
	dur, err := time.ParseDuration(s)
	if err != nil {
		dur = defaults
	}
	return dur
}
