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

// keyOf is a listed node's name and host name: all paging needs of it. The
// fleet view pages two kinds of list -- the whole tree's rows, and on a fleet
// with no child aggregators, its nodes directly -- by one set of rules.
type keyOf[T any] func(*T) (name, hostname string)

// rowKey and nodeKey are the keys of the two lists.
func rowKey(r *row) (string, string)              { return r.name, r.hostname }
func nodeKey(n *types.FleetNode) (string, string) { return n.Name, n.Hostname }

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

// pageIndexes applies q to a list of n nodes in display order, each known
// by key: the positions of the matching nodes from the page's offset, at most
// q.Limit of them, how many matched in all, and the offset the page starts at
// -- q.Offset, or the start of the anchor's page.
//
// It works on positions rather than a slice so the list need not exist: a
// flat fleet's view pages its cached peers with this node inserted among
// them, without building the combined list per request.
//
// An offset past the last match is an empty page, not an error: a node leaving
// the fleet can shrink the list under a page a client is already on, and the
// client learns where the end now is from the count.
func pageIndexes(n int, key func(int) (name, hostname string), q Query) ([]int, int, int) {
	q = q.bounded()
	needle := foldASCII(q.Search)
	// The fold is the one the node order uses -- A-Z only -- so search and
	// order share one account of what "the same name" is, and neither depends
	// on a Unicode case table. Node names are ASCII by rule (pkg/nodename),
	// and so are hostnames in practice; anything else is matched exactly.
	matches := func(i int) bool {
		if needle == "" {
			return true
		}
		name, hostname := key(i)
		return containsFolded(name, needle) || containsFolded(hostname, needle)
	}
	if q.Anchor != "" && q.Limit > 0 {
		rank := 0
		for i := 0; i < n; i++ {
			if !matches(i) {
				continue
			}
			if name, _ := key(i); name == q.Anchor {
				q.Offset = rank - rank%q.Limit
				break
			}
			rank++
		}
	}
	if needle == "" {
		// Every node matches, so the page is a run of the list itself.
		start := min(q.Offset, n)
		end := n
		if q.Limit > 0 {
			// Compared rather than added: limit is the client's, and
			// start+limit can overflow to a negative bound.
			end = start + min(q.Limit, end-start)
		}
		idx := make([]int, end-start)
		for k := range idx {
			idx[k] = start + k
		}
		return idx, n, q.Offset
	}
	idx := []int{}
	matched := 0
	for i := 0; i < n; i++ {
		if !matches(i) {
			continue
		}
		if matched >= q.Offset && (q.Limit == 0 || len(idx) < q.Limit) {
			idx = append(idx, i)
		}
		matched++
	}
	return idx, matched, q.Offset
}

// page applies q to items, which must be in display order; see pageIndexes.
func page[T any](items []T, key keyOf[T], q Query) ([]T, int, int) {
	idx, matched, offset := pageIndexes(len(items), func(i int) (string, string) { return key(&items[i]) }, q)
	out := make([]T, len(idx))
	for j, i := range idx {
		out[j] = items[i]
	}
	return out, matched, offset
}
