package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

type Builder interface {
	Build(ctx context.Context, workers int, onBuilt func(pkg string), pkgs ...string) error
	BuildOnRequest(ctx context.Context, workers int, onBuilt func(pkg string), pkgC <-chan string) error
}

type Runner interface {
	Exec(ctx context.Context, wd, cmd string, args ...string) (string, error)
}

type builder struct {
	r     Runner
	debug bool

	activeMux *sync.Mutex
	active    map[string]func()
}

func NewBuilder(r Runner, debug bool) Builder {
	return &builder{
		r:     r,
		debug: debug,

		active:    make(map[string]func()),
		activeMux: new(sync.Mutex),
	}
}

func (b *builder) Build(ctx context.Context, workers int, onBuilt func(pkg string), pkgs ...string) error {
	if len(pkgs) == 0 {
		return nil
	}
	if workers == 0 {
		workers = 1
	}
	pkgC := make(chan string, len(pkgs))
	go func() {
		for _, p := range pkgs {
			pkgC <- p
		}
		close(pkgC)
	}()
	return b.BuildOnRequest(ctx, workers, onBuilt, pkgC)
}

func (b *builder) BuildOnRequest(ctx context.Context, workers int, onBuilt func(pkg string), pkgC <-chan string) error {
	pkgDedupChan := wrapDedup(pkgC)
	if workers == 0 {
		workers = 1
	}
	var errs []error
	errsMux := new(sync.RWMutex)
	wg := new(sync.WaitGroup)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(N int) {
			defer wg.Done()

			for {
				select {
				case <-ctx.Done():
					return
				case pkg, ok := <-pkgDedupChan:
					if !ok {
						return
					}
					b.activeMux.Lock()
					if cancelFn, ok := b.active[pkg]; ok {
						cancelFn()
						delete(b.active, pkg)
						if b.debug {
							log.Printf("[worker %d] cancels active build of %s", N, pkg)
						}
					}
					if b.debug {
						log.Printf("[worker %d] builds %s", N, pkg)
					}
					pkgCtx, pkgCancel := context.WithCancel(ctx)
					b.active[pkg] = pkgCancel
					b.activeMux.Unlock()
					ts := time.Now()
					switch err := b.buildPkg(pkgCtx, pkg); err {
					case context.Canceled, context.DeadlineExceeded:
						if b.debug {
							log.Println("worker", N, "cancelled")
						}
						b.activeMux.Lock()
						if _, ok := b.active[pkg]; ok {
							delete(b.active, pkg)
						}
						b.activeMux.Unlock()
						continue
					case error(nil):
						if b.debug {
							log.Println("worker", N, "done in", time.Since(ts))
						}
						if onBuilt != nil {
							onBuilt(pkg)
						}
						b.activeMux.Lock()
						if cancelFn, ok := b.active[pkg]; ok {
							cancelFn()
							delete(b.active, pkg)
						}
						b.activeMux.Unlock()
						continue
					default:
						if b.debug {
							log.Println("worker", N, "failed with", err)
						}
						b.activeMux.Lock()
						if cancelFn, ok := b.active[pkg]; ok {
							cancelFn()
							delete(b.active, pkg)
						}
						b.activeMux.Unlock()
						errsMux.Lock()
						errs = append(errs, err)
						errsMux.Unlock()
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
	go func() { // drain
		for range pkgDedupChan {
		}
	}()
	errsMux.RLock()
	defer errsMux.RUnlock()
	if len(errs) > 1 {
		return fmt.Errorf("%v and %d more errors", errs[0], len(errs)-1)
	} else if len(errs) == 1 {
		return errs[0]
	}
	return nil
}

func (b *builder) buildPkg(ctx context.Context, pkg string) error {
	_, err := b.r.Exec(ctx, "/go/src", "go", "install", pkg)
	return err
}

// wrapDedup wraps target channel, limiting it to be on-demand,
// maintaining an unique set of values while waiting reads.
func wrapDedup(pkgC <-chan string) <-chan string {
	limitedOut := make(chan string)
	go func() {
		set := make(map[string]struct{})
		t := time.NewTimer(time.Second)
		t.Stop()
	CHECK:
		for {
			select {
			case next, ok := <-pkgC:
				if !ok {
					for k := range set {
						limitedOut <- k
					}
					close(limitedOut)
					return
				}
				set[next] = struct{}{}
			default:
				t.Reset(200 * time.Millisecond)
				for k := range set {
					select {
					case limitedOut <- k:
						delete(set, k)
						t.Reset(200 * time.Millisecond)
					case <-t.C:
						t.Stop()
						goto CHECK
					}
				}
				<-t.C
			}
		}
	}()
	return limitedOut
}
