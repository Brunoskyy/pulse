// Package pool runs jobs on a fixed number of goroutines.
package pool

import (
	"context"
	"sync"
)

// Pool is a bounded worker pool. Submit blocks while every worker is busy and
// the queue is full, which is the back-pressure the scheduler wants: a slow
// network delays new probes instead of piling up goroutines.
type Pool struct {
	jobs chan func()
	wg   sync.WaitGroup
	once sync.Once
}

// New starts size workers with a queue of the same length.
func New(size int) *Pool {
	if size < 1 {
		size = 1
	}
	p := &Pool{jobs: make(chan func(), size)}
	p.wg.Add(size)
	for range size {
		go func() {
			defer p.wg.Done()
			for job := range p.jobs {
				job()
			}
		}()
	}
	return p
}

// Submit queues a job. It returns false, without running the job, when ctx is
// done before a slot frees up.
func (p *Pool) Submit(ctx context.Context, job func()) bool {
	select {
	case p.jobs <- job:
		return true
	case <-ctx.Done():
		return false
	}
}

// Close stops accepting work and waits for queued and running jobs to finish.
// Submit must not be called after Close.
func (p *Pool) Close() {
	p.once.Do(func() { close(p.jobs) })
	p.wg.Wait()
}
