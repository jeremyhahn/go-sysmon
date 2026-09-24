package fleet

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

func TestInstanceID_IsRandomAndWellFormed(t *testing.T) {
	a, _ := treePoller(t)
	b, _ := treePoller(t)
	if !validInstanceID(a.InstanceID()) || !validInstanceID(b.InstanceID()) || a.InstanceID() == b.InstanceID() {
		t.Errorf("instance IDs %q and %q; want two distinct well-formed IDs", a.InstanceID(), b.InstanceID())
	}
}

func TestParseAncestry(t *testing.T) {
	p, _ := treePoller(t)
	id := p.InstanceID()
	if chain, err := ParseAncestry(""); err != nil || chain != nil {
		t.Errorf("empty = %v, %v; want no chain", chain, err)
	}
	if chain, err := ParseAncestry(id + "," + newInstanceID()); err != nil || len(chain) != 2 {
		t.Errorf("two IDs = %v, %v", chain, err)
	}
	var invalid *ErrInvalidAncestry
	for _, bad := range []string{"x", id + ",", "abc" + id[3:], id + " "} {
		if _, err := ParseAncestry(bad); !errors.As(err, &invalid) || !strings.Contains(err.Error(), HeaderAncestry) {
			t.Errorf("ParseAncestry(%q) = %v, want ErrInvalidAncestry", bad, err)
		}
	}
	long := strings.TrimSuffix(strings.Repeat(id+",", MaxTreeDepth+2), ",")
	var deep *ErrTreeTooDeep
	if _, err := ParseAncestry(long); !errors.As(err, &deep) {
		t.Errorf("a chain past the depth limit = %v, want ErrTreeTooDeep", err)
	}
}

// TestCheckAncestry: a request that has passed through this aggregator is a
// loop; one that has passed through more aggregators than a tree may have is
// too deep. Both are refused, and neither is a name conflict.
func TestCheckAncestry(t *testing.T) {
	p, _ := treePoller(t)
	var loop *ErrFleetLoop
	if err := p.CheckAncestry([]string{newInstanceID(), p.InstanceID()}); !errors.As(err, &loop) || err.Error() == "" {
		t.Errorf("own ID in the chain = %v, want ErrFleetLoop", err)
	}
	chain := make([]string, MaxTreeDepth+1)
	for i := range chain {
		chain[i] = newInstanceID()
	}
	var deep *ErrTreeTooDeep
	if err := p.CheckAncestry(chain); !errors.As(err, &deep) || !strings.Contains(err.Error(), strconv.Itoa(MaxTreeDepth)) {
		t.Errorf("a chain of %d = %v, want ErrTreeTooDeep", len(chain), err)
	}
	if err := p.CheckAncestry(chain[:MaxTreeDepth]); err != nil {
		t.Errorf("a chain of %d = %v, want admitted", MaxTreeDepth, err)
	}
}

// TestOutgoingChain: a relayed request carries the incoming chain then this
// aggregator's ID; a poll, which relays nothing, carries this aggregator's ID
// alone.
func TestOutgoingChain(t *testing.T) {
	p, _ := treePoller(t)
	incoming := newInstanceID()
	if got := p.outgoingChain(context.Background()); len(got) != 1 || got[0] != p.InstanceID() {
		t.Errorf("a poll = %v; want this node alone", got)
	}
	relayed := WithAncestry(context.Background(), []string{incoming})
	if got := p.outgoingChain(relayed); len(got) != 2 || got[0] != incoming || got[1] != p.InstanceID() {
		t.Errorf("a relay = %v; want the incoming chain, then this node", got)
	}
}

// TestRequestsCarryChainAndBudget: every request to a child carries the chain
// and, from a deadline, the time left less this hop's margin.
func TestRequestsCarryChainAndBudget(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy))
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)

	h := *mid.lastHeaders.Load()
	if h.Get(HeaderAncestry) != p.InstanceID() {
		t.Errorf("poll ancestry = %q, want this node's ID", h.Get(HeaderAncestry))
	}
	if ms := ParseBudget(h.Get(HeaderBudget)); ms <= 0 || ms > cfg.Timeout() {
		t.Errorf("poll budget = %v; want within the fleet timeout %v", ms, cfg.Timeout())
	}

	ctx, cancel := context.WithTimeout(WithAncestry(context.Background(), []string{"AAAAAAAAAAAAAAAAAAAAAAAAAA"}), time.Second)
	defer cancel()
	if _, err := p.PeerSnapshot(ctx, "a", time.Second); err != nil {
		t.Fatalf("drill-in: %v", err)
	}
	h = *mid.lastHeaders.Load()
	if got := h.Get(HeaderAncestry); got != "AAAAAAAAAAAAAAAAAAAAAAAAAA,"+p.InstanceID() {
		t.Errorf("relayed ancestry = %q; want the incoming chain then this node", got)
	}
	if ms := ParseBudget(h.Get(HeaderBudget)); ms <= 0 || ms >= time.Second-hopMargin+time.Millisecond {
		t.Errorf("relayed budget = %v; want less than a second by at least the hop margin", ms)
	}
}

func TestParseBudget(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 0, "x": 0, "-5": 0, "0": 0, "250": 250 * time.Millisecond} {
		if got := ParseBudget(in); got != want {
			t.Errorf("ParseBudget(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestErrFleetRefused(t *testing.T) {
	if !strings.Contains((&ErrFleetRefused{Peer: "m", Reason: RefusalDepth}).Error(), "deeper") {
		t.Error("a depth refusal does not say so")
	}
	if !strings.Contains((&ErrFleetRefused{Peer: "m", Reason: RefusalLoop}).Error(), "loop") {
		t.Error("a loop refusal does not say so")
	}
}
