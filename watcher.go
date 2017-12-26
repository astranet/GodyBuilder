package main

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

type Watcher interface {
	Add(id, path string) error
	Remove(path string) error
	SetOnChange(func(ids []string))
	Close()
}

func NewFSWatcher(w *fsnotify.Watcher, debug bool) Watcher {
	f := &fsWatcher{
		debug: debug,
		w:     w,
		mm:    new(sync.RWMutex),
		exitC: make(chan struct{}),
		doneC: make(chan struct{}),
	}
	go f.mon()
	return f
}

type fsWatcher struct {
	w     *fsnotify.Watcher
	mm    *sync.RWMutex
	m     map[string]map[string]struct{}
	debug bool
	exitC chan struct{}
	doneC chan struct{}

	onChange func(ids []string)
}

func (f *fsWatcher) SetOnChange(fn func(ids []string)) {
	f.onChange = fn
}

func (f *fsWatcher) Add(id, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	} else if !info.IsDir() {
		switch filepath.Ext(path) {
		case ".go", ".c", ".h":
		default:
			return nil
		}
		path = filepath.Dir(path)
	}
	f.mm.Lock()
	if f.m == nil {
		f.m = make(map[string]map[string]struct{})
	}
	if f.m[path] == nil {
		f.m[path] = make(map[string]struct{})
		if err := f.w.Add(path); err != nil {
			f.mm.Unlock()
			return err
		}
	}
	f.m[path][id] = struct{}{}
	f.mm.Unlock()
	return nil
}

func (f *fsWatcher) mon() {
	bufMux := new(sync.Mutex)
	buf := make(map[string]time.Time)
	notify := func(path string) {
		bufMux.Lock()
		buf[path] = time.Now()
		bufMux.Unlock()
	}
	go func() {
		for {
			select {
			case <-f.exitC:
				return
			default:
				bufMux.Lock()
				queue := make([]string, 0, len(buf))
				for path, ts := range buf {
					if time.Since(ts) > time.Second {
						queue = append(queue, path)
						delete(buf, path)
					}
				}
				bufMux.Unlock()
				go func() {
					for _, path := range queue {
						f.mm.RLock()
						ids, ok := f.m[path]
						f.mm.RUnlock()
						if ok && f.onChange != nil {
							f.onChange(idList(ids))
						} else if ids, ok := f.findParentIDs(path); ok {
							f.onChange(idList(ids))
						}
					}
				}()
				time.Sleep(200 * time.Millisecond)
			}
		}
	}()
	for {
		select {
		case <-f.exitC:
			close(f.doneC)
			return
		case ev, ok := <-f.w.Events:
			if !ok {
				close(f.doneC)
				return
			}
			if f.debug {
				log.Println(ev.String())
			}
			switch {
			case opIs(ev.Op, fsnotify.Remove):
				notify(ev.Name)
			case opIs(ev.Op, fsnotify.Write):
				notify(ev.Name)
			case opIs(ev.Op, fsnotify.Create):
				if ids, ok := f.findParentIDs(ev.Name); ok {
					for id := range ids {
						if err := f.Add(id, ev.Name); err != nil {
							log.Println("watcher:", err)
						}
					}
				} else {
					log.Println("watcher: no parent for", ev.Name)
				}
				notify(ev.Name)
			}
		case err := <-f.w.Errors:
			if v, ok := err.(*os.SyscallError); ok {
				if v.Err == syscall.EINTR {
					continue
				}
			}
			log.Println("watcher:", err)
		}
	}
}

func (f *fsWatcher) Remove(path string) error {
	f.mm.Lock()
	delete(f.m, path)
	f.mm.Unlock()
	return f.w.Remove(path)
}

func (f *fsWatcher) Close() {
	close(f.exitC)
	<-f.doneC
}

func (f *fsWatcher) findParentIDs(path string) (map[string]struct{}, bool) {
	f.mm.RLock()
	defer f.mm.RUnlock()
	parent := filepath.Dir(path)
	for path, ids := range f.m {
		if path == parent {
			return ids, true
		}
	}
	return nil, false
}

func opIs(op, mask fsnotify.Op) bool {
	return op&mask == mask
}

func idList(set map[string]struct{}) []string {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
