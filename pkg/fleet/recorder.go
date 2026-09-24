package fleet

import (
	"context"
	"sync"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// maxHistoryFetches bounds how many history payloads are fetched at once
// across the whole fleet. Each is about a fifth of a busy host's snapshot, and
// workers that fall into step -- they all start together after a reload --
// would otherwise decode the fleet's worth at the same instant.
const maxHistoryFetches = 4

// recorder runs one history worker per peer, apart from the summary poll.
//
// The summary poll is what the fleet view is made of, and it runs at the
// fleet interval. History wants a reading at the exporter's sample interval,
// and the history payload is heavier and can be slow. Fetching it inside the
// poll cycle tied the two together: a peer whose history endpoint stalled held
// the whole cycle, and every card, until its timeout. Here each peer's history
// is its own goroutine on its own timer, so nothing it does can reach the
// poll (#76).
//
// Fetching at the sample interval cannot write a reading twice: the exporter
// writes a peer's reading only if the peer has collected since the last one it
// wrote (#173).
//
// A worker exists for a peer while history is being recorded and the peer is
// configured, and it is cancelled -- its in-flight request with it -- when
// either stops being true, when the peer's URL changes, and when Run returns.
//
// The workers map is owned by the goroutine running Poller.Run: sync and stop
// are called only from there, so it needs no lock.
type recorder struct {
	p       *Poller
	sem     chan struct{}
	workers map[string]*historyWorker
	wg      sync.WaitGroup
	// cadence is the cadence the running workers were started under. A
	// reload that changes it restarts them, so none is left waiting out a
	// timer armed for the old interval.
	cadence time.Duration
	// newTimer starts the timer that paces a worker. Always newClockTimer
	// outside tests, which fire one by hand to state exactly when a worker
	// turns rather than wait out a real interval.
	newTimer func(time.Duration) timer
}

// timer paces a history worker. Production uses a *time.Timer.
type timer interface {
	// Chan delivers the expiry.
	Chan() <-chan time.Time
	// Reset rearms the timer, as time.Timer.Reset does.
	Reset(d time.Duration) bool
	// Stop disarms the timer, as time.Timer.Stop does.
	Stop() bool
}

// clockTimer is the production timer: a *time.Timer.
type clockTimer struct {
	*time.Timer
}

// Chan returns the timer's channel.
func (c clockTimer) Chan() <-chan time.Time {
	return c.C
}

// newClockTimer starts a clockTimer that expires after d.
func newClockTimer(d time.Duration) timer {
	return clockTimer{time.NewTimer(d)}
}

// historyWorker is one running worker: the URL it was started for and how to
// stop it.
type historyWorker struct {
	url    string
	cancel context.CancelFunc
}

func newRecorder(p *Poller) *recorder {
	return &recorder{
		p:        p,
		sem:      make(chan struct{}, maxHistoryFetches),
		workers:  make(map[string]*historyWorker),
		newTimer: newClockTimer,
	}
}

// sync makes the running workers match cfg and the current cadence. It starts
// and cancels goroutines and returns; it never waits for one, so a worker
// stuck in a fetch cannot hold up the poll that called it.
func (r *recorder) sync(cfg config.Fleet) {
	cadence := r.p.historyCadence()
	want := make(map[string]config.Peer, len(cfg.Peers))
	if cadence > 0 {
		for _, peer := range cfg.Peers {
			want[peer.Name] = peer
		}
	}
	retimed := cadence != r.cadence
	r.cadence = cadence
	for name, w := range r.workers {
		if peer, ok := want[name]; !ok || peer.URL != w.url || retimed {
			w.cancel()
			delete(r.workers, name)
		}
	}
	for name, peer := range want {
		if _, running := r.workers[name]; running {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		r.workers[name] = &historyWorker{url: peer.URL, cancel: cancel}
		r.wg.Add(1)
		go r.run(ctx, peer)
	}
}

// stop cancels every worker and waits for all of them to return.
func (r *recorder) stop() {
	for name, w := range r.workers {
		w.cancel()
		delete(r.workers, name)
	}
	r.wg.Wait()
}

// run is one peer's worker: a reading now, then one per cadence, until
// cancelled.
func (r *recorder) run(ctx context.Context, peer config.Peer) {
	defer r.wg.Done()
	timer := r.newTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.Chan():
		}
		// Timed from the start of the turn, not its end, so the time spent
		// queued and fetching is not added to every interval: a 15 s sample
		// interval with a 3 s fetch still samples every 15 s.
		started := r.p.now()
		next := r.tick(ctx, peer)
		timer.Reset(max(next-r.p.now().Sub(started), 0))
	}
}

// tick is one turn of a worker: a reading if history is still being recorded,
// and how long to wait before the next turn.
//
// A cadence of zero means recording has just been switched off. Nothing is
// fetched; the next sync cancels the worker, and until then it checks again at
// the floor rather than spinning.
//
// A peer no poll has read yet -- a worker started with the configuration,
// before the first summary -- is asked again at the floor rather than a whole
// cadence later, so its first reading follows its first summary.
func (r *recorder) tick(ctx context.Context, peer config.Peer) time.Duration {
	cadence := r.p.historyCadence()
	if cadence <= 0 || r.p.peerSource(peer) == "" {
		return MinPollInterval
	}
	r.record(ctx, peer)
	return max(cadence, MinPollInterval)
}

// record fetches one history reading from peer and hands it to the observer.
//
// Only a peer the latest poll read by its summary is asked. A peer too old to
// serve one is polled for its whole snapshot, which the poll records itself;
// asking it again here would fetch the same megabytes twice. A peer not yet
// polled is left until it has been.
//
// A failure costs this reading and nothing else. A gap in the history is the
// honest record of a reading that did not arrive.
func (r *recorder) record(ctx context.Context, peer config.Peer) {
	if !r.current(peer) || r.p.peerSource(peer) != types.SummarySourceSummary {
		return
	}
	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-r.sem }()

	cfg := r.p.cfgFn()
	var snap types.Snapshot
	err := r.p.get(ctx, peer, cfg, endpointHistory, maxSnapshotBytes, &snap)
	if endpointNotServed(err) {
		// A peer that serves a summary but not the history payload is not a
		// build this project has shipped; a proxy forwarding one path and not
		// the other is -- answering 404, or 200 with a page of its own. The
		// whole snapshot is a superset of the payload, so the history stays
		// complete either way.
		err = r.p.get(ctx, peer, cfg, endpointSnapshot, maxSnapshotBytes, &snap)
	}
	if err != nil {
		if ctx.Err() == nil {
			r.p.logger.Debug("fleet: no history reading from peer", "peer", peer.Name, "error", err)
		}
		return
	}
	// Checked again after the fetch: a reload may have repointed the name
	// while it was in flight, and this reading is then another machine's.
	if !r.current(peer) {
		return
	}
	r.p.observe(peer.Name, &snap)
}

// current reports whether peer is still configured at the URL this worker was
// started for. The worker can only be cancelled at the next sync; this is what
// stops it recording under a name that now belongs to another address.
func (r *recorder) current(peer config.Peer) bool {
	for _, p := range r.p.cfgFn().Peers {
		if p.Name == peer.Name {
			return p.URL == peer.URL
		}
	}
	return false
}
