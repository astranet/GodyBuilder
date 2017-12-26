package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	spin "github.com/tj/go-spin"
)

func outputMux(ctx context.Context, out *safeBuffer) (stdOut, stdErr *safeBuffer) {
	stdOut = newSafeBuffer()
	stdErr = newSafeBuffer()
	logBuffer := func(buf io.Reader) {
		t := time.NewTimer(time.Millisecond)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s := bufio.NewScanner(buf)
				for s.Scan() {
					// optionally replace with reactive
					// line-by-line logging or similar
					fmt.Fprintln(out, s.Text())
				}
				t.Reset(defaultOutputPolling)
			}
		}
	}
	go logBuffer(stdOut)
	go logBuffer(stdErr)
	return
}

type safeBuffer struct {
	buf *bytes.Buffer
	mux *sync.Mutex
}

func newSafeBuffer() *safeBuffer {
	return &safeBuffer{
		buf: new(bytes.Buffer),
		mux: new(sync.Mutex),
	}
}

func (b *safeBuffer) Read(p []byte) (n int, err error) {
	b.mux.Lock()
	n, err = b.buf.Read(p)
	b.mux.Unlock()
	return
}

func (b *safeBuffer) Write(p []byte) (n int, err error) {
	b.mux.Lock()
	n, err = b.buf.Write(p)
	b.mux.Unlock()
	return
}

func (b *safeBuffer) String() (s string) {
	b.mux.Lock()
	s = b.buf.String()
	b.mux.Unlock()
	return
}

func (b *safeBuffer) Bytes() (s []byte) {
	b.mux.Lock()
	s = b.buf.Bytes()
	b.mux.Unlock()
	return
}

func spinWith(label string) (func(), <-chan struct{}) {
	s := spin.New()
	ctx, cancelFn := context.WithCancel(context.Background())
	doneC := make(chan struct{})

	go func() {
		for {
			select {
			case <-ctx.Done():
				fmt.Printf("\r  \033[36m%s\033[m done.\n", label)
				close(doneC)
				return
			default:
				fmt.Printf("\r  \033[36m%s\033[m %s ", label, s.Next())
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()

	return cancelFn, doneC
}
