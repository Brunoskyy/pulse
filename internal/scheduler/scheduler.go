// Package scheduler decides when each check runs.
package scheduler

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Brunoskyy/pulse/internal/clock"
	"github.com/Brunoskyy/pulse/internal/config"
)

// Submitter is the part of the pool the scheduler needs.
type Submitter interface {
	Submit(ctx context.Context, job func()) bool
}

// Scheduler runs every check on its interval. Three rules:
//
//   - Start times are spread: each check waits a random fraction of its
//     interval before its first run, so forty checks on a 30s interval do
//     not all fire in the same millisecond after a restart.
//   - Each interval gets up to ±10% jitter, so checks that happen to line up
//     drift apart instead of staying aligned forever.
//   - A check never overlaps itself. If the previous probe is still running
//     when the next one is due (a slow target near its timeout), that tick
//     is skipped and counted.
type Scheduler struct {
	Clock   clock.Clock
	Pool    Submitter
	Run     func(ctx context.Context, c config.Check)
	Rand    func() float64 // in [0,1); injectable for tests
	Skipped atomic.Int64

	wg sync.WaitGroup
}

// Start launches one goroutine per check. They stop when ctx is cancelled;
// Wait blocks until they have.
func (s *Scheduler) Start(ctx context.Context, checks []config.Check) {
	if s.Rand == nil {
		s.Rand = rand.Float64
	}
	for _, c := range checks {
		s.wg.Add(1)
		go s.loop(ctx, c)
	}
}

// Wait returns once every check loop has exited.
func (s *Scheduler) Wait() { s.wg.Wait() }

func (s *Scheduler) loop(ctx context.Context, c config.Check) {
	defer s.wg.Done()
	var running atomic.Bool
	delay := time.Duration(s.Rand() * float64(c.Interval))
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.Clock.After(delay):
		}
		if running.CompareAndSwap(false, true) {
			ok := s.Pool.Submit(ctx, func() {
				defer running.Store(false)
				s.Run(ctx, c)
			})
			if !ok {
				running.Store(false)
				return
			}
		} else {
			s.Skipped.Add(1)
		}
		delay = Jitter(c.Interval, s.Rand())
	}
}

// Jitter returns interval scaled by a factor in [0.9, 1.1), given r in [0,1).
func Jitter(interval time.Duration, r float64) time.Duration {
	return time.Duration(float64(interval) * (0.9 + 0.2*r))
}
