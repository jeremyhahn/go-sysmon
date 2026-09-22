package collector

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/pkg/types/typestest"
)

// TestAssemble_HostWithNoneOfAnythingSendsNoNullLists is #151 at its source.
//
// A collector that has never run holds exactly what a host with none of
// anything would give it: no CPUs read, no GPUs, no NPUs, no disks, no
// interfaces, no sensors, no containers, no VMs. Every list in the snapshot
// comes from one of those caches, so this is the snapshot most likely to carry
// a nil list -- and the one no development machine produces, because every
// development machine has most of those things.
func TestAssemble_HostWithNoneOfAnythingSendsNoNullLists(t *testing.T) {
	sc := NewSystemCollector(slog.New(slog.NewTextHandler(io.Discard, nil)))

	var snap types.Snapshot
	sc.assemble(&snap)

	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	nulls, err := typestest.NullSlices(raw, snap)
	if err != nil {
		t.Fatalf("NullSlices: %v", err)
	}
	if len(nulls) != 0 {
		t.Errorf("an empty host's snapshot encodes these lists as null: %v", nulls)
	}

	// The guard is only worth anything if the empty host really is empty:
	// a list with contents cannot be null whatever the normaliser does.
	if len(snap.CPUs)+len(snap.GPUs)+len(snap.NPUs)+len(snap.Disks)+len(snap.Networks) != 0 {
		t.Fatalf("a collector that has never run reported devices; the fixture is not an empty host")
	}
}
