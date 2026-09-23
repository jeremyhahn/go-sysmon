package fleet

import "github.com/jeremyhahn/go-sysmon/pkg/types"

// HistorySnapshot returns the snapshot a node serves at GET
// /api/snapshot/history: s without its process list and without each CPU's
// flag list. It is what an aggregator that records the fleet's history fetches
// from each peer, alongside the summary, when history is being recorded.
//
// Those two are removed because they are most of a snapshot's bytes and
// history records neither. A busy host's process list runs to a thousand
// entries, and a many-core host repeats the same hundred-odd CPU flags once
// per thread; metrics.Write, which turns a snapshot into the series the
// history keeps, reads no field of either. The process COUNTS stay: they are a
// few integers.
//
// What is removed is fixed here and nowhere else, and it is checked against
// what metrics.Write actually reads rather than against a list of fields: see
// TestHistorySnapshot_KeepsEverythingHistoryRecords. A field metrics.Write
// starts reading tomorrow is either untouched by this function or fails that
// test -- it cannot be stripped silently.
//
// s is not modified; it may be the monitor's published snapshot, which other
// goroutines are reading. The result shares every field it does not replace.
func HistorySnapshot(s *types.Snapshot) *types.Snapshot {
	if s == nil {
		return nil
	}
	out := *s
	out.Processes.Processes = []types.ProcessDetail{}
	out.CPUs = make([]types.CPUInfo, len(s.CPUs))
	for i := range s.CPUs {
		out.CPUs[i] = s.CPUs[i]
		out.CPUs[i].Flags = []string{}
	}
	return &out
}
