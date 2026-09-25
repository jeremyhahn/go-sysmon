package fleet

import (
	"context"
	"crypto/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Ancestry (#190): how a request between aggregators knows where it has been.
//
// A loop in a fleet tree cannot be seen from any one configuration -- A lists
// B, B lists C, C lists A, each file valid on its own -- and it cannot be seen
// from names either, because a peer's name is whatever its parent chose to call
// it. So every aggregator process takes a random instance ID when it starts,
// never configured and never reused, and every request one aggregator makes of
// another carries the IDs of the aggregators it has passed through. An
// aggregator that finds its own ID in that chain is being asked by itself, and
// refuses; one whose chain is longer than MaxTreeDepth refuses too. Relayed
// requests -- a drill-in, settings, a history range -- carry the chain of the
// request they relay, so no relay can bounce around a loop.
//
// A poll is not a relay: nothing asked for it, and its chain is this
// aggregator alone. A loop among polls is found from what the polls return
// instead -- every subtree names the aggregators at and below it (see
// FleetSubtree.Aggregators and pollSubtree) -- which does not depend on the
// order the aggregators happened to poll each other in.

// Headers one aggregator sends another.
const (
	// HeaderAncestry is the comma-separated instance IDs of the aggregators
	// a request has passed through, the sender's last.
	HeaderAncestry = "X-Sysmon-Fleet-Ancestry"
	// HeaderBudget is the milliseconds the sender will still wait for the
	// answer. A receiver that relays onward spends less than that.
	HeaderBudget = "X-Sysmon-Fleet-Budget-Ms"
	// HeaderRefusal names why a request was refused with 508: "loop" or
	// "depth".
	HeaderRefusal = "X-Sysmon-Fleet-Refusal"
)

// Refusal reasons, as HeaderRefusal carries them.
const (
	RefusalLoop  = "loop"
	RefusalDepth = "depth"
)

// instanceIDLen is the length of crypto/rand.Text: 128 random bits in
// base32.
const instanceIDLen = 26

// hopMargin is the least a relaying aggregator keeps back for itself from the
// budget it was given, to write its own answer in; a tenth is kept when that is
// more.
const hopMargin = 50 * time.Millisecond

// ancestryKey marks a context as carrying the chain of an incoming request.
type ancestryKey struct{}

// WithAncestry returns ctx carrying the chain an incoming request arrived
// with. Requests this aggregator then makes on that request's behalf carry it
// on, with this aggregator's ID added.
func WithAncestry(ctx context.Context, chain []string) context.Context {
	return context.WithValue(ctx, ancestryKey{}, chain)
}

// InvalidAncestryError reports a malformed HeaderAncestry.
type InvalidAncestryError struct{ Value string }

func (e *InvalidAncestryError) Error() string {
	return "fleet: malformed " + HeaderAncestry + " header"
}

// FleetLoopError reports a request that has already passed through this
// aggregator: the fleet's peer lists form a loop.
type FleetLoopError struct{}

func (e *FleetLoopError) Error() string {
	return "fleet: this request has already passed through this aggregator; the fleet's peer lists form a loop"
}

// TreeTooDeepError reports a request that has passed through more aggregators
// than MaxTreeDepth allows.
type TreeTooDeepError struct{ Depth int }

func (e *TreeTooDeepError) Error() string {
	return "fleet: this request has passed through " + strconv.Itoa(e.Depth) +
		" aggregators, more than the " + strconv.Itoa(MaxTreeDepth) + " a fleet tree may have"
}

// FleetRefusedError reports a peer that refused a request with 508: the request
// looped back to it, or the tree is too deep there.
type FleetRefusedError struct {
	Peer   string
	Reason string
}

func (e *FleetRefusedError) Error() string {
	if e.Reason == RefusalDepth {
		return "fleet node " + e.Peer + " refused: the fleet tree is deeper than " + strconv.Itoa(MaxTreeDepth) + " levels there"
	}
	return "fleet node " + e.Peer + " refused: the fleet's peer lists loop back through it"
}

// ParseAncestry reads HeaderAncestry. Empty is no chain. Each ID must look like
// one this code generates, and there may be no more than one past the depth
// limit -- a longer chain is refused anyway, and is not worth parsing.
func ParseAncestry(header string) ([]string, error) {
	if header == "" {
		return nil, nil
	}
	ids := strings.Split(header, ",")
	if len(ids) > MaxTreeDepth+1 {
		return nil, &TreeTooDeepError{Depth: len(ids)}
	}
	for _, id := range ids {
		if !validInstanceID(id) {
			return nil, &InvalidAncestryError{Value: header}
		}
	}
	return ids, nil
}

// validInstanceID reports whether id is shaped like crypto/rand.Text.
func validInstanceID(id string) bool {
	if len(id) != instanceIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'A' || c > 'Z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

// InstanceID is this aggregator process's identity in the fleet tree: random,
// taken at start, never configured. Its summary carries it, so a parent can
// tell a peer that is itself under another name.
func (p *Poller) InstanceID() string { return p.instanceID }

// CheckAncestry refuses a request that has already passed through this
// aggregator, or through more aggregators than a tree may have.
func (p *Poller) CheckAncestry(chain []string) error {
	for _, id := range chain {
		if id == p.instanceID {
			return &FleetLoopError{}
		}
	}
	if len(chain) > MaxTreeDepth {
		return &TreeTooDeepError{Depth: len(chain)}
	}
	return nil
}

// outgoingChain is the chain a request made on ctx's behalf carries: the
// incoming request's, if it is relaying one, then this aggregator's own ID.
func (p *Poller) outgoingChain(ctx context.Context) []string {
	chain, _ := ctx.Value(ancestryKey{}).([]string)
	out := make([]string, 0, len(chain)+1)
	return append(append(out, chain...), p.instanceID)
}

// stampRequest puts the chain and the budget on an outgoing request. The
// budget is what is left of ctx's deadline less this hop's margin, so a child
// that relays onward answers -- with what it has -- before this node stops
// waiting. With less left than the margin, the child is given what is left:
// a leaf relays nothing, and still answers in time.
func (p *Poller) stampRequest(ctx context.Context, req *http.Request) {
	req.Header.Set(HeaderAncestry, strings.Join(p.outgoingChain(ctx), ","))
	deadline, ok := ctx.Deadline()
	if kept, set := ctx.Value(budgetKey{}).(time.Time); set && (!ok || kept.Before(deadline)) {
		deadline, ok = kept, true
	}
	if ok {
		left := time.Until(deadline)
		if spare := left - max(hopMargin, left/10); spare > 0 {
			left = spare
		}
		req.Header.Set(HeaderBudget, strconv.FormatInt(max(left.Milliseconds(), 1), 10))
	}
}

// ParseBudget reads HeaderBudget: the time the sender will still wait, or 0
// when it did not say. A value that is not a positive integer is ignored
// rather than refused: the budget only ever shortens this node's own.
func ParseBudget(header string) time.Duration {
	ms, err := strconv.ParseInt(header, 10, 64)
	if err != nil || ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// newInstanceID is 128 random bits as base32 text.
func newInstanceID() string { return rand.Text() }
