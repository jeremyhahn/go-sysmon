package fleet

import "github.com/jeremyhahn/go-sysmon/pkg/types"

// compareName reports whether alias, the name the aggregator holds for a node
// and queries its history by, is the name the node reports writing under.
//
// A node that did not answer its latest poll, or answered without stating a
// name, yields types.NameUnknown rather than agreement: its silence has not been
// shown to match, and an older peer is exactly the one whose alias nobody has
// checked. Names are compared exactly, because a Prometheus label value is.
func compareName(alias, reported string, reachable bool) types.NameMatch {
	if !reachable || reported == "" {
		return types.NameUnknown
	}
	if alias == reported {
		return types.NameSame
	}
	return types.NameDifferent
}
