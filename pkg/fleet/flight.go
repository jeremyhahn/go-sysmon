package fleet

import (
	"context"
	"sync"
	"time"
)

// Shared work between requests (#190).
//
// Everything this aggregator asks a child for on a request's behalf -- a
// page's summaries, a relayed drill-in, settings or history range -- goes
// through a flightGroup, so that no request, and in particular no anonymous
// one, can make more than one request's worth of work at each hop:
//
//   - Identical work in flight is done once; every asker waits for it.
//   - The work runs detached from whichever request started it
//     (context.WithoutCancel), so that request leaving -- or arriving with a
//     short budget -- does not fail the others waiting. Its deadline is the
//     latest any of its askers will wait, never past the configured fleet
//     timeout; each asker gives up on its own context.
//   - A result is reused for a short while, so a burst of identical requests
//     costs one.
//   - At most a bounded number of pieces of work run at once, per group.
type flightGroup struct {
	calls sync.Map // key -> *call
	done  sync.Map // key -> *reused
	sem   chan struct{}
	reuse time.Duration
}

func newFlightGroup(concurrency int, reuse time.Duration) *flightGroup {
	return &flightGroup{sem: make(chan struct{}, concurrency), reuse: reuse}
}

// call is one piece of work in flight.
type call struct {
	done chan struct{}
	val  any
	err  error

	mu       sync.Mutex
	deadline time.Time
	ceiling  time.Time
	timer    *time.Timer
	cancel   context.CancelCauseFunc
}

// reused is a finished result, kept until expires.
type reused struct {
	val     any
	err     error
	expires time.Time
}

// extend moves the call's deadline to at, when that is later, never past its
// ceiling.
func (c *call) extend(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at.After(c.ceiling) {
		at = c.ceiling
	}
	if at.After(c.deadline) {
		c.deadline = at
		if c.timer != nil {
			c.timer.Reset(time.Until(at))
		}
	}
}

// expire ends the call's work when its deadline has passed; a deadline moved
// later in the meantime re-arms the timer instead.
func (c *call) expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !time.Now().Before(c.deadline) {
		c.cancel(context.DeadlineExceeded)
	}
}

// flightContext is the context detached work runs under. Its deadline is the
// call's, which askers may extend, and running past it is a deadline error --
// as with context.WithDeadline, whose deadline cannot move.
type flightContext struct {
	context.Context
	c *call
}

func (f flightContext) Deadline() (time.Time, bool) {
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	return f.c.deadline, true
}

func (f flightContext) Err() error {
	if f.Context.Err() == nil {
		return nil
	}
	return context.Cause(f.Context)
}

// budgetKey carries, on detached work, the deadline the work is asked to keep
// -- its asker's -- which the request it makes tells the child (stampRequest),
// rather than the ceiling it may be extended to.
type budgetKey struct{}

// do returns the result of fn for key: reused if fresh, shared if in flight,
// run otherwise. limit caps how long the work may run whoever asks; an asker
// with its own deadline extends the work to it, up to limit. nil val and a
// context error mean the asker's own context ended first.
func (g *flightGroup) do(ctx context.Context, key string, limit time.Duration, fn func(context.Context) (any, error)) (any, error) {
	if v, ok := g.done.Load(key); ok {
		r := v.(*reused)
		if time.Now().Before(r.expires) {
			return r.val, r.err
		}
		g.done.Delete(key)
	}

	ceiling := time.Now().Add(limit)
	want := ceiling
	if d, ok := ctx.Deadline(); ok && d.Before(ceiling) {
		want = d
	}

	c := &call{done: make(chan struct{}), deadline: want, ceiling: ceiling}
	if existing, loaded := g.calls.LoadOrStore(key, c); loaded {
		c = existing.(*call)
		c.extend(want)
		select {
		case <-c.done:
			return c.val, c.err
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}

	// Detached from the asker: its going away, or its short budget, must not
	// fail the others waiting. Its chain of ancestry still travels with the
	// work, and so does the deadline the asker wants kept.
	detached, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	work := context.WithValue(flightContext{Context: detached, c: c}, budgetKey{}, want)
	c.mu.Lock()
	c.cancel = cancel
	c.timer = time.AfterFunc(time.Until(c.deadline), c.expire)
	c.mu.Unlock()
	go func() {
		defer func() {
			c.mu.Lock()
			c.timer.Stop()
			c.mu.Unlock()
			cancel(context.Canceled)
			if g.reuse > 0 {
				g.done.Store(key, &reused{val: c.val, err: c.err, expires: time.Now().Add(g.reuse)})
			}
			g.calls.Delete(key)
			close(c.done)
		}()
		select {
		case g.sem <- struct{}{}:
			defer func() { <-g.sem }()
		case <-work.Done():
			c.err = context.Cause(work)
			return
		}
		c.val, c.err = fn(work)
	}()

	select {
	case <-c.done:
		return c.val, c.err
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// forget drops finished results older than their reuse.
func (g *flightGroup) forget() {
	now := time.Now()
	g.done.Range(func(k, v any) bool {
		if !now.Before(v.(*reused).expires) {
			g.done.Delete(k)
		}
		return true
	})
}
