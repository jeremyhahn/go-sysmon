package fleet

import (
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Paging the fleet view (#15).
//
// GET /api/fleet used to return every node on every poll, and the dashboard
// searched and paged the list itself. That is fine at ten nodes and is the
// ceiling at ten thousand: the payload grows with the fleet (about 9 MB a poll
// at ten thousand busy nodes) when the screen shows twenty-five cards. The
// view is now filtered and paged here, and only the page is sent.
//
// What is NOT paged is the roll-up. It is always the whole fleet, computed over
// every node whatever page was asked for, and it is bounded instead by naming
// at most types.RollupNames nodes in any list.

// Query parameters of GET /api/fleet.
const (
	QueryParamSearch = "q"
	QueryParamOffset = "offset"
	QueryParamLimit  = "limit"
	QueryParamAnchor = "anchor"
)

// Query selects a page of the fleet view. The zero value is the whole fleet.
type Query struct {
	// Search keeps the nodes whose name or hostname contains it, ignoring
	// the case of A-Z. Empty keeps every node.
	Search string
	// Offset skips that many matching nodes, in name order.
	Offset int
	// Limit is the most nodes the page holds. Zero means no limit.
	Limit int
	// Anchor, when it names a matching node and Limit is set, replaces
	// Offset with the start of the page that node is on. It is how a client
	// opens the page holding one node it knows only by name: a search for the
	// name can match many others first -- "db" is in every "db-01" and in any
	// hostname containing it -- and only the server knows where it falls.
	// An anchor that matches nothing leaves Offset as given.
	Anchor string
}

// bounded returns q with negative bounds read as zero: no offset, no limit.
// ParseQuery never produces them, but Poller.View is public, and a negative
// bound from an embedding caller must not reach a slice expression.
func (q Query) bounded() Query {
	q.Offset = max(q.Offset, 0)
	q.Limit = max(q.Limit, 0)
	return q
}

// ErrInvalidQuery reports a fleet query parameter that cannot be honoured.
type ErrInvalidQuery struct {
	Param  string
	Value  string
	Reason string
}

func (e *ErrInvalidQuery) Error() string {
	return "fleet: invalid " + e.Param + " " + strconv.Quote(e.Value) + ": " + e.Reason
}

// ErrMalformedQuery reports a query string that does not parse at all.
type ErrMalformedQuery struct {
	Raw   string
	Cause error
}

func (e *ErrMalformedQuery) Error() string {
	return "fleet: malformed query " + strconv.Quote(e.Raw) + ": " + e.Cause.Error()
}

func (e *ErrMalformedQuery) Unwrap() error { return e.Cause }

// ParseQuery reads a fleet query from a request's raw query string. An absent
// parameter takes its zero value; one that is present must be well formed, and
// is rejected rather than guessed at -- a page that quietly ignored a malformed
// limit would hand a client the whole fleet it was trying not to ask for.
//
// It takes the RAW string rather than url.URL.Query for the same reason: that
// drops a pair it cannot parse -- "limit=25;offset=0" -- without saying so,
// which turned a bounded request into the whole fleet with a 200.
func ParseQuery(raw string) (Query, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return Query{}, &ErrMalformedQuery{Raw: raw, Cause: err}
	}
	var q Query
	for _, param := range []string{QueryParamSearch, QueryParamOffset, QueryParamLimit, QueryParamAnchor} {
		if got := values[param]; len(got) > 1 {
			return Query{}, &ErrInvalidQuery{Param: param, Value: strings.Join(got, ","), Reason: "given more than once"}
		}
	}

	search := values.Get(QueryParamSearch)
	if !utf8.ValidString(search) {
		return Query{}, &ErrInvalidQuery{Param: QueryParamSearch, Value: search, Reason: "not valid UTF-8"}
	}
	q.Search = strings.TrimSpace(search)

	q.Anchor = values.Get(QueryParamAnchor)
	if !utf8.ValidString(q.Anchor) {
		return Query{}, &ErrInvalidQuery{Param: QueryParamAnchor, Value: q.Anchor, Reason: "not valid UTF-8"}
	}

	if q.Offset, err = parseBound(values, QueryParamOffset, 0); err != nil {
		return Query{}, err
	}
	if q.Limit, err = parseBound(values, QueryParamLimit, 1); err != nil {
		return Query{}, err
	}
	return q, nil
}

// parseBound reads an optional integer parameter that must be at least least.
// Absent is zero.
func parseBound(values url.Values, param string, least int) (int, error) {
	if !values.Has(param) {
		return 0, nil
	}
	raw := values.Get(param)
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, &ErrInvalidQuery{Param: param, Value: raw, Reason: "not an integer"}
	}
	if n < least {
		return 0, &ErrInvalidQuery{Param: param, Value: raw, Reason: "must be at least " + strconv.Itoa(least)}
	}
	return n, nil
}

// matches reports whether a node answers a search: needle, already folded,
// appears in its name or its hostname.
//
// The fold is the one the node order uses -- A-Z only -- so search and order
// share one account of what "the same name" is, and neither depends on a
// Unicode case table. Node names are ASCII by rule (pkg/nodename), and so are
// hostnames in practice; anything else is matched exactly.
func matches(n *types.FleetNode, needle string) bool {
	return needle == "" || containsFolded(n.Name, needle) || containsFolded(n.Hostname, needle)
}

// containsFolded reports whether needle, already folded, appears in s once s
// is folded. It folds as it compares rather than folding s first, because it
// runs over every node in the fleet on every search request and a folded copy
// of each name is an allocation per node per request.
func containsFolded(s, needle string) bool {
	for start := 0; start+len(needle) <= len(s); start++ {
		i := 0
		for i < len(needle) && foldByte(s[start+i]) == needle[i] {
			i++
		}
		if i == len(needle) {
			return true
		}
	}
	return false
}

// page applies q to nodes, which must be in display order: the matching nodes
// from the page's offset, at most q.Limit of them, how many matched in all,
// and the offset the page starts at -- q.Offset, or the start of the anchor's
// page.
//
// An offset past the last match is an empty page, not an error: a node leaving
// the fleet can shrink the list under a page a client is already on, and the
// client learns where the end now is from the count.
func page(nodes []types.FleetNode, q Query) ([]types.FleetNode, int, int) {
	q = q.bounded()
	needle := foldASCII(q.Search)
	if q.Anchor != "" && q.Limit > 0 {
		if rank, ok := anchorRank(nodes, needle, q.Anchor); ok {
			q.Offset = rank - rank%q.Limit
		}
	}
	shown, matched := slicePage(nodes, needle, q)
	return shown, matched, q.Offset
}

// anchorRank is the position of the node named anchor among the nodes
// matching needle, and whether it matches at all.
func anchorRank(nodes []types.FleetNode, needle, anchor string) (int, bool) {
	rank := 0
	for i := range nodes {
		if !matches(&nodes[i], needle) {
			continue
		}
		if nodes[i].Name == anchor {
			return rank, true
		}
		rank++
	}
	return 0, false
}

// slicePage is page's selection once the offset is settled.
func slicePage(nodes []types.FleetNode, needle string, q Query) ([]types.FleetNode, int) {
	if needle == "" {
		// Every node matches, so the page is a run of the list itself.
		start := min(q.Offset, len(nodes))
		end := len(nodes)
		if q.Limit > 0 {
			// Compared rather than added: limit is the client's, and
			// start+limit can overflow to a negative bound.
			end = start + min(q.Limit, end-start)
		}
		return nodes[start:end], len(nodes)
	}
	out := []types.FleetNode{}
	matched := 0
	for i := range nodes {
		if !matches(&nodes[i], needle) {
			continue
		}
		if matched >= q.Offset && (q.Limit == 0 || len(out) < q.Limit) {
			out = append(out, nodes[i])
		}
		matched++
	}
	return out, matched
}
