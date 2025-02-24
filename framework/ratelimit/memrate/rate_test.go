// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.7
// +build go1.7

package memrate

import (
	"context"
	. "github.com/smartystreets/goconvey/convey"
	"github.com/youminxue/odin/framework/ratelimit"
	"log"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimit(t *testing.T) {
	if Limit(10) == Inf {
		t.Errorf("Limit(10) == Inf should be false")
	}
}

func closeEnough(a, b Limit) bool {
	return (math.Abs(float64(a)/float64(b)) - 1.0) < 1e-9
}

func TestEvery(t *testing.T) {
	cases := []struct {
		interval time.Duration
		lim      Limit
	}{
		{0, Inf},
		{-1, Inf},
		{1 * time.Nanosecond, Limit(1e9)},
		{1 * time.Microsecond, Limit(1e6)},
		{1 * time.Millisecond, Limit(1e3)},
		{10 * time.Millisecond, Limit(100)},
		{100 * time.Millisecond, Limit(10)},
		{1 * time.Second, Limit(1)},
		{2 * time.Second, Limit(0.5)},
		{time.Duration(2.5 * float64(time.Second)), Limit(0.4)},
		{4 * time.Second, Limit(0.25)},
		{10 * time.Second, Limit(0.1)},
		{time.Duration(math.MaxInt64), Limit(1e9 / float64(math.MaxInt64))},
	}
	for _, tc := range cases {
		lim := Every(tc.interval)
		if !closeEnough(lim, tc.lim) {
			t.Errorf("Every(%v) = %v want %v", tc.interval, lim, tc.lim)
		}
	}
}

const (
	d = 100 * time.Millisecond
)

var (
	t0 = time.Now()
	t1 = t0.Add(time.Duration(1) * d)
	t2 = t0.Add(time.Duration(2) * d)
	t3 = t0.Add(time.Duration(3) * d)
	t4 = t0.Add(time.Duration(4) * d)
	t5 = t0.Add(time.Duration(5) * d)
	t9 = t0.Add(time.Duration(9) * d)
)

type allow struct {
	t  time.Time
	n  int
	ok bool
}

func run(t *testing.T, lim *Limiter, allows []allow) {
	t.Helper()
	for i, allow := range allows {
		ok := lim.AllowN(allow.t, allow.n)
		if ok != allow.ok {
			t.Errorf("step %d: lim.AllowN(%v, %v) = %v want %v",
				i, allow.t, allow.n, ok, allow.ok)
		}
	}
}

func TestLimiterBurst1(t *testing.T) {
	run(t, NewLimiter(10, 1), []allow{
		{t0, 1, true},
		{t0, 1, false},
		{t0, 1, false},
		{t1, 1, true},
		{t1, 1, false},
		{t1, 1, false},
		{t2, 2, false}, // burst size is 1, so n=2 always fails
		{t2, 1, true},
		{t2, 1, false},
	})
}

func TestLimiterBurst3(t *testing.T) {
	run(t, NewLimiter(10, 3), []allow{
		{t0, 2, true},
		{t0, 2, false},
		{t0, 1, true},
		{t0, 1, false},
		{t1, 4, false},
		{t2, 1, true},
		{t3, 1, true},
		{t4, 1, true},
		{t4, 1, true},
		{t4, 1, false},
		{t4, 1, false},
		{t9, 3, true},
		{t9, 0, true},
	})
}

func TestLimiterJumpBackwards(t *testing.T) {
	run(t, NewLimiter(10, 3), []allow{
		{t1, 1, true}, // start at t1
		{t0, 1, true}, // jump back to t0, two tokens remain
		{t0, 1, true},
		{t0, 1, false},
		{t0, 1, false},
		{t1, 1, true}, // got a token
		{t1, 1, false},
		{t1, 1, false},
		{t2, 1, true}, // got another token
		{t2, 1, false},
		{t2, 1, false},
	})
}

// Ensure that tokensFromDuration doesn't produce
// rounding errors by truncating nanoseconds.
// See golang.org/issues/34861.
func TestLimiter_noTruncationErrors(t *testing.T) {
	if !NewLimiter(0.7692307692307693, 1).Allow() {
		t.Fatal("expected true")
	}
}

func TestSimultaneousRequests(t *testing.T) {
	const (
		limit       = 1
		burst       = 5
		numRequests = 15
	)
	var (
		wg    sync.WaitGroup
		numOK = uint32(0)
	)

	// Very slow replenishing bucket.
	lim := NewLimiter(limit, burst)

	// Tries to take a token, atomically updates the counter and decreases the wait
	// group counter.
	f := func() {
		defer wg.Done()
		if ok := lim.Allow(); ok {
			atomic.AddUint32(&numOK, 1)
		}
	}

	wg.Add(numRequests)
	for i := 0; i < numRequests; i++ {
		go f()
	}
	wg.Wait()
	if numOK != burst {
		t.Errorf("numOK = %d, want %d", numOK, burst)
	}
}

func TestLongRunningQPS(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	if runtime.GOOS == "openbsd" {
		t.Skip("low resolution time.Sleep invalidates test (golang.org/issue/14183)")
		return
	}

	// The test runs for a few seconds executing many requests and then checks
	// that overall number of requests is reasonable.
	const (
		limit = 100
		burst = 100
	)
	var numOK = int32(0)

	lim := NewLimiter(limit, burst)

	var wg sync.WaitGroup
	f := func() {
		if ok := lim.Allow(); ok {
			atomic.AddInt32(&numOK, 1)
		}
		wg.Done()
	}

	start := time.Now()
	end := start.Add(5 * time.Second)
	for time.Now().Before(end) {
		wg.Add(1)
		go f()

		// This will still offer ~500 requests per second, but won't consume
		// outrageous amount of CPU.
		time.Sleep(2 * time.Millisecond)
	}
	wg.Wait()
	elapsed := time.Since(start)
	ideal := burst + (limit * float64(elapsed) / float64(time.Second))

	// We should never get more requests than allowed.
	if want := int32(ideal + 1); numOK > want {
		t.Errorf("numOK = %d, want %d (ideal %f)", numOK, want, ideal)
	}
	// We should get very close to the number of requests allowed.
	if want := int32(0.999 * ideal); numOK < want {
		t.Errorf("numOK = %d, want %d (ideal %f)", numOK, want, ideal)
	}
}

type request struct {
	t   time.Time
	n   int
	act time.Time
	ok  bool
}

// dFromDuration converts a duration to a multiple of the global constant d
func dFromDuration(dur time.Duration) int {
	// Adding a millisecond to be swallowed by the integer division
	// because we don't care about small inaccuracies
	return int((dur + time.Millisecond) / d)
}

// dSince returns multiples of d since t0
func dSince(t time.Time) int {
	return dFromDuration(t.Sub(t0))
}

func runReserve(t *testing.T, lim *Limiter, req request) *Reservation {
	t.Helper()
	return runReserveMax(t, lim, req, InfDuration)
}

func runReserveMax(t *testing.T, lim *Limiter, req request, maxReserve time.Duration) *Reservation {
	t.Helper()
	r := lim.reserveN(req.t, req.n, maxReserve)
	if r.ok && (dSince(r.timeToAct) != dSince(req.act)) || r.ok != req.ok {
		t.Errorf("lim.reserveN(t%d, %v, %v) = (t%d, %v) want (t%d, %v)",
			dSince(req.t), req.n, maxReserve, dSince(r.timeToAct), r.ok, dSince(req.act), req.ok)
	}
	return &r
}

func TestSimpleReserve(t *testing.T) {
	lim := NewLimiter(10, 2)

	runReserve(t, lim, request{t0, 2, t0, true})
	runReserve(t, lim, request{t0, 2, t2, true})
	runReserve(t, lim, request{t3, 2, t4, true})
}

func TestMix(t *testing.T) {
	lim := NewLimiter(10, 2)

	runReserve(t, lim, request{t0, 3, t1, false}) // should return false because n > Burst
	runReserve(t, lim, request{t0, 2, t0, true})
	run(t, lim, []allow{{t1, 2, false}}) // not enough tokens - don't allow
	runReserve(t, lim, request{t1, 2, t2, true})
	run(t, lim, []allow{{t1, 1, false}}) // negative tokens - don't allow
	run(t, lim, []allow{{t3, 1, true}})
}

func TestCancelInvalid(t *testing.T) {
	lim := NewLimiter(10, 2)

	runReserve(t, lim, request{t0, 2, t0, true})
	r := runReserve(t, lim, request{t0, 3, t3, false})
	r.CancelAt(t0)                               // should have no effect
	runReserve(t, lim, request{t0, 2, t2, true}) // did not get extra tokens
}

func TestCancelLast(t *testing.T) {
	lim := NewLimiter(10, 2)

	runReserve(t, lim, request{t0, 2, t0, true})
	r := runReserve(t, lim, request{t0, 2, t2, true})
	r.CancelAt(t1) // got 2 tokens back
	runReserve(t, lim, request{t1, 2, t2, true})
}

func TestCancelTooLate(t *testing.T) {
	lim := NewLimiter(10, 2)

	runReserve(t, lim, request{t0, 2, t0, true})
	r := runReserve(t, lim, request{t0, 2, t2, true})
	r.CancelAt(t3) // too late to cancel - should have no effect
	runReserve(t, lim, request{t3, 2, t4, true})
}

func TestCancel0Tokens(t *testing.T) {
	lim := NewLimiter(10, 2)

	runReserve(t, lim, request{t0, 2, t0, true})
	r := runReserve(t, lim, request{t0, 1, t1, true})
	runReserve(t, lim, request{t0, 1, t2, true})
	r.CancelAt(t0) // got 0 tokens back
	runReserve(t, lim, request{t0, 1, t3, true})
}

func TestCancel1Token(t *testing.T) {
	lim := NewLimiter(10, 2)

	runReserve(t, lim, request{t0, 2, t0, true})
	r := runReserve(t, lim, request{t0, 2, t2, true})
	runReserve(t, lim, request{t0, 1, t3, true})
	r.CancelAt(t2) // got 1 token back
	runReserve(t, lim, request{t2, 2, t4, true})
}

func TestCancelMulti(t *testing.T) {
	lim := NewLimiter(10, 4)

	runReserve(t, lim, request{t0, 4, t0, true})
	rA := runReserve(t, lim, request{t0, 3, t3, true})
	runReserve(t, lim, request{t0, 1, t4, true})
	rC := runReserve(t, lim, request{t0, 1, t5, true})
	rC.CancelAt(t1) // get 1 token back
	rA.CancelAt(t1) // get 2 tokens back, as if C was never reserved
	runReserve(t, lim, request{t1, 3, t5, true})
}

func TestReserveJumpBack(t *testing.T) {
	lim := NewLimiter(10, 2)

	runReserve(t, lim, request{t1, 2, t1, true}) // start at t1
	runReserve(t, lim, request{t0, 1, t1, true}) // should violate Limit,Burst
	runReserve(t, lim, request{t2, 2, t3, true})
}

func TestReserveJumpBackCancel(t *testing.T) {
	lim := NewLimiter(10, 2)

	runReserve(t, lim, request{t1, 2, t1, true}) // start at t1
	r := runReserve(t, lim, request{t1, 2, t3, true})
	runReserve(t, lim, request{t1, 1, t4, true})
	r.CancelAt(t0)                               // cancel at t0, get 1 token back
	runReserve(t, lim, request{t1, 2, t4, true}) // should violate Limit,Burst
}

func TestReserveSetLimit(t *testing.T) {
	lim := NewLimiter(5, 2)

	runReserve(t, lim, request{t0, 2, t0, true})
	runReserve(t, lim, request{t0, 2, t4, true})
	lim.SetLimitAt(t2, 10)
	runReserve(t, lim, request{t2, 1, t4, true}) // violates Limit and Burst
}

func TestReserveSetBurst(t *testing.T) {
	lim := NewLimiter(5, 2)

	runReserve(t, lim, request{t0, 2, t0, true})
	runReserve(t, lim, request{t0, 2, t4, true})
	lim.SetBurstAt(t3, 4)
	runReserve(t, lim, request{t0, 4, t9, true}) // violates Limit and Burst
}

func TestReserveSetLimitCancel(t *testing.T) {
	lim := NewLimiter(5, 2)

	runReserve(t, lim, request{t0, 2, t0, true})
	r := runReserve(t, lim, request{t0, 2, t4, true})
	lim.SetLimitAt(t2, 10)
	r.CancelAt(t2) // 2 tokens back
	runReserve(t, lim, request{t2, 2, t3, true})
}

func TestReserveMax(t *testing.T) {
	lim := NewLimiter(10, 2)
	maxT := d

	runReserveMax(t, lim, request{t0, 2, t0, true}, maxT)
	runReserveMax(t, lim, request{t0, 1, t1, true}, maxT)  // reserve for close future
	runReserveMax(t, lim, request{t0, 1, t2, false}, maxT) // time to act too far in the future
}

type wait struct {
	name   string
	ctx    context.Context
	n      int
	delay  int // in multiples of d
	nilErr bool
}

func runWait(t *testing.T, lim *Limiter, w wait) {
	t.Helper()
	start := time.Now()
	err := lim.WaitN(w.ctx, w.n)
	delay := time.Since(start)
	if (w.nilErr && err != nil) || (!w.nilErr && err == nil) || w.delay != dFromDuration(delay) {
		errString := "<nil>"
		if !w.nilErr {
			errString = "<non-nil error>"
		}
		t.Errorf("lim.WaitN(%v, lim, %v) = %v with delay %v ; want %v with delay %v",
			w.name, w.n, err, delay, errString, d*time.Duration(w.delay))
	}
}

func TestWaitSimple(t *testing.T) {
	lim := NewLimiter(10, 3)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runWait(t, lim, wait{"already-cancelled", ctx, 1, 0, false})

	runWait(t, lim, wait{"exceed-burst-error", context.Background(), 4, 0, false})

	runWait(t, lim, wait{"act-now", context.Background(), 2, 0, true})
	runWait(t, lim, wait{"act-later", context.Background(), 3, 2, true})
}

func TestWaitCancel(t *testing.T) {
	lim := NewLimiter(10, 3)

	ctx, cancel := context.WithCancel(context.Background())
	runWait(t, lim, wait{"act-now", ctx, 2, 0, true}) // after this lim.tokens = 1
	go func() {
		time.Sleep(d)
		cancel()
	}()
	runWait(t, lim, wait{"will-cancel", ctx, 3, 1, false})
	// should get 3 tokens back, and have lim.tokens = 2
	t.Logf("tokens:%v last:%v lastEvent:%v", lim.tokens, lim.last, lim.lastEvent)
	runWait(t, lim, wait{"act-now-after-cancel", context.Background(), 2, 0, true})
}

func TestWaitTimeout(t *testing.T) {
	lim := NewLimiter(10, 3)

	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	runWait(t, lim, wait{"act-now", ctx, 2, 0, true})
	runWait(t, lim, wait{"w-timeout-err", ctx, 3, 0, false})
}

func TestWaitInf(t *testing.T) {
	lim := NewLimiter(Inf, 0)

	runWait(t, lim, wait{"exceed-burst-no-error", context.Background(), 3, 0, true})
}

func BenchmarkAllowN(b *testing.B) {
	lim := NewLimiter(Every(1*time.Second), 1)
	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			lim.AllowN(now, 1)
		}
	})
}

func BenchmarkWaitNNoDelay(b *testing.B) {
	lim := NewLimiter(Limit(b.N), b.N)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lim.WaitN(ctx, 1)
	}
}

func TestZeroLimit(t *testing.T) {
	r := NewLimiter(0, 1)
	if !r.Allow() {
		t.Errorf("Limit(0, 1) want true when first used")
	}
	if r.Allow() {
		t.Errorf("Limit(0, 1) want false when already used")
	}
}

func TestNewLimiter(t *testing.T) {
	type args struct {
		r    Limit
		b    int
		opts []LimiterOption
	}
	store := NewMemoryStore(nil)
	tests := []struct {
		name string
		args args
	}{
		{
			name: "",
			args: args{
				r: 1,
				b: 3,
				opts: []LimiterOption{
					WithTimer(10*time.Second, func() {
						store.DeleteKey("any")
					}),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewLimiter(tt.args.r, tt.args.b, tt.args.opts...); got == nil {
				t.Error("got should not be nil")
			}
		})
	}
}

func TestTokenLimiter_Allow(t *testing.T) {
	tl := NewLimiter(1, 3)
	if got := tl.Allow(); got != true {
		t.Errorf("Allow() should return true")
	}
	if got := tl.Allow(); got != true {
		t.Errorf("Allow() should return true")
	}
	if got := tl.Allow(); got != true {
		t.Errorf("Allow() should return true")
	}
	if got := tl.Allow(); got != false {
		t.Errorf("Allow() should return false")
	}
}

func TestTokenLimiter_Reserve(t *testing.T) {
	tl := NewLimiter(1, 3)
	if d, ok, _ := tl.ReserveE(); ok != true && d != 0 {
		t.Errorf("Reserve() should return true and d should equal to 0")
	}
	if d, ok, _ := tl.ReserveE(); ok != true && d != 0 {
		t.Errorf("Reserve() should return true and d should equal to 0")
	}
	if d, ok, _ := tl.ReserveE(); ok != true && d != 0 {
		t.Errorf("Reserve() should return true and d should equal to 0")
	}
	if d, ok, _ := tl.ReserveE(); ok != true && d <= 0 {
		t.Errorf("Reserve() should return true and d should greater than 0")
	}
	tl = NewLimiter(1, 0)
	if _, ok, _ := tl.ReserveE(); ok != false {
		t.Errorf("Reserve() should return false")
	}
}

func TestTokenLimiter_Wait(t *testing.T) {
	tl := NewLimiter(1, 3)
	ctx := context.Background()
	if err := tl.Wait(ctx); err != nil {
		t.Errorf("Wait() shouldn't return error")
	}
	if err := tl.Wait(ctx); err != nil {
		t.Errorf("Wait() shouldn't return error")
	}
	if err := tl.Wait(ctx); err != nil {
		t.Errorf("Wait() shouldn't return error")
	}
	ctx, cancel := context.WithTimeout(ctx, 1*time.Millisecond)
	defer cancel()
	if err := tl.Wait(ctx); err.Error() != "rate: Wait(n=1) would exceed context deadline" {
		t.Errorf("Wait() should return error: rate: Wait(n=1) would exceed context deadline, but actual error: %s", err.Error())
	}
}

func TestLimiter_Limit(t *testing.T) {
	Convey("Test limiter", t, func() {
		tl := NewLimiter(1, 3)
		Convey("Limit should equal to 1", func() {
			So(tl.Limit(), ShouldEqual, 1)
		})
		Convey("Burst should equal to 3", func() {
			So(tl.Burst(), ShouldEqual, 3)
		})
	})
}

func TestNewLimiterLimit(t *testing.T) {
	Convey("Test limiter", t, func() {
		tl := NewLimiterLimit(ratelimit.PerSecondBurst(1, 3), WithTimer(10*time.Second, func() {
			log.Println("do nothing")
		}))
		Convey("Limit should equal to 1", func() {
			So(tl.Limit(), ShouldEqual, 1)
		})
		Convey("Burst should equal to 3", func() {
			So(tl.Burst(), ShouldEqual, 3)
		})

		tl.SetLimit(10)
		Convey("Limit should equal to 10", func() {
			So(tl.Limit(), ShouldEqual, 10)
		})

		tl.SetBurst(100)
		Convey("Burst should equal to 100", func() {
			So(tl.Burst(), ShouldEqual, 100)
		})

		tl.resetTimer()

		ok := tl.AllowCtx(context.Background())
		So(ok, ShouldBeTrue)
		ok, err := tl.AllowECtx(context.Background())
		So(err, ShouldBeNil)
		So(ok, ShouldBeTrue)
		dur, ok, err := tl.ReserveECtx(context.Background())
		So(err, ShouldBeNil)
		So(ok, ShouldBeTrue)
		So(dur, ShouldEqual, 0)

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		time.Sleep(600 * time.Millisecond)
		ok = tl.AllowCtx(ctx)
		So(ok, ShouldBeFalse)
		ok, err = tl.AllowECtx(ctx)
		So(err, ShouldResemble, context.DeadlineExceeded)
		So(ok, ShouldBeFalse)
		dur, ok, err = tl.ReserveECtx(ctx)
		So(err, ShouldResemble, context.DeadlineExceeded)
		So(ok, ShouldBeFalse)
		So(dur, ShouldEqual, 0)
	})
}
