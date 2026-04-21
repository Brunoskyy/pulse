package pool

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolBoundsConcurrency(t *testing.T) {
	before := runtime.NumGoroutine()
	p := New(4)
	var running, peak, done atomic.Int64
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		p.Submit(context.Background(), func() {
			defer wg.Done()
			n := running.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			running.Add(-1)
			done.Add(1)
		})
	}
	wg.Wait()
	p.Close()
	if done.Load() != 40 {
		t.Fatalf("ran %d jobs, want 40", done.Load())
	}
	if peak.Load() > 4 {
		t.Fatalf("peak concurrency %d exceeds pool size 4", peak.Load())
	}
	waitGoroutines(t, before)
}

func TestSubmitGivesUpWhenContextEnds(t *testing.T) {
	p := New(1)
	release := make(chan struct{})
	p.Submit(context.Background(), func() { <-release }) // occupies the worker
	p.Submit(context.Background(), func() {})            // fills the queue
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	ran := false
	if p.Submit(ctx, func() { ran = true }) {
		t.Fatal("Submit should fail once the context is done and the pool is full")
	}
	close(release)
	p.Close()
	if ran {
		t.Fatal("a rejected job must not run")
	}
}

func TestCloseWaitsForRunningJobs(t *testing.T) {
	p := New(2)
	var finished atomic.Bool
	p.Submit(context.Background(), func() { time.Sleep(20 * time.Millisecond); finished.Store(true) })
	p.Close()
	if !finished.Load() {
		t.Fatal("Close returned before the running job finished")
	}
}

func BenchmarkPool(b *testing.B) {
	p := New(16)
	defer p.Close()
	var wg sync.WaitGroup
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		wg.Add(1)
		p.Submit(context.Background(), func() { wg.Done() })
	}
	wg.Wait()
}

func waitGoroutines(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > want {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines leaked: %d running, started with %d", runtime.NumGoroutine(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
