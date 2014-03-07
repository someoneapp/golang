// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Futex is only available on DragonFly BSD, FreeBSD and Linux.
// The race detector emits calls to split stack functions so it breaks
// the test.

// +build dragonfly freebsd linux
// +build !race

package runtime_test

import (
	"runtime"
	"testing"
	"time"
)

type futexsleepTest struct {
	mtx uint32
	ns  int64
	msg string
	ch  chan futexsleepTest
}

var futexsleepTests = []futexsleepTest{
	beforeY2038: {mtx: 0, ns: 86400 * 1e9, msg: "before the year 2038", ch: make(chan futexsleepTest, 1)},
	afterY2038:  {mtx: 0, ns: (1<<31 + 100) * 1e9, msg: "after the year 2038", ch: make(chan futexsleepTest, 1)},
}

const (
	beforeY2038 = iota
	afterY2038
)

func TestFutexsleep(t *testing.T) {
	start := time.Now()
	for _, tt := range futexsleepTests {
		go func(tt futexsleepTest) {
			runtime.Entersyscall()
			runtime.Futexsleep(&tt.mtx, tt.mtx, tt.ns)
			runtime.Exitsyscall()
			tt.ch <- tt
		}(tt)
	}
loop:
	for {
		select {
		case tt := <-futexsleepTests[beforeY2038].ch:
			t.Errorf("futexsleep test %q finished early after %s", tt.msg, time.Since(start))
			break loop
		case tt := <-futexsleepTests[afterY2038].ch:
			// Looks like FreeBSD 10 kernel has changed
			// the semantics of timedwait on userspace
			// mutex to make broken stuff look broken.
			switch {
			case runtime.GOOS == "freebsd" && runtime.GOARCH == "386":
				t.Log("freebsd/386 may not work correctly after the year 2038, see golang.org/issue/7194")
			default:
				t.Errorf("futexsleep test %q finished early after %s", tt.msg, time.Since(start))
				break loop
			}
		case <-time.After(time.Second):
			break loop
		}
	}
	for _, tt := range futexsleepTests {
		runtime.Futexwakeup(&tt.mtx, 1)
	}
}
