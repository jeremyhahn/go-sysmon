package fleet

// Name agreement values, reported per node as FleetNode.NameMatch.
//
// Like the build agreement values, these describe configuration, not health:
// a peer held under the wrong name is running perfectly well, and it is the
// aggregator's view of it that is wrong.
const (
	NameSame      = "same"
	NameDifferent = "different"
	NameUnknown   = "unknown"
)

// compareName reports whether alias, the name the aggregator holds for a node
// and queries its history by, is the name the node reports writing under.
//
// A node that did not answer its latest poll, or answered without stating a
// name, yields NameUnknown rather than agreement: its silence has not been
// shown to match, and an older peer is exactly the one whose alias nobody has
// checked. Names are compared exactly, because a Prometheus label value is.
func compareName(alias, reported string, reachable bool) string {
	if !reachable || reported == "" {
		return NameUnknown
	}
	if alias == reported {
		return NameSame
	}
	return NameDifferent
}
