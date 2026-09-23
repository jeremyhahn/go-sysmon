package types_test

import (
	"encoding/json"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// The dashboard hides the Containers and Virtualization tabs on exactly one
// string, "absent", and shows them on anything else -- including a peer too
// old to send the field. These are the wire names it matches on.
func TestVirtInfo_StackDetectionWireNames(t *testing.T) {
	info := types.VirtInfo{
		ContainerRuntime: types.StackDetection{
			State:    types.PresencePresent,
			Evidence: []types.StackEvidence{{Stack: "docker", Kind: types.EvidenceSocket, Path: "/run/docker.sock"}},
		},
		Hypervisor: types.StackDetection{State: types.PresenceAbsent, Evidence: []types.StackEvidence{}},
	}
	types.ReplaceNilSlices(&info)

	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		ContainerRuntime map[string]any `json:"container_runtime"`
		Hypervisor       map[string]any `json:"hypervisor"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := wire.ContainerRuntime["state"]; got != "present" {
		t.Errorf("container_runtime.state = %v, want present", got)
	}
	if got := wire.Hypervisor["state"]; got != "absent" {
		t.Errorf("hypervisor.state = %v, want absent", got)
	}
	evidence, ok := wire.ContainerRuntime["evidence"].([]any)
	if !ok || len(evidence) != 1 {
		t.Fatalf("container_runtime.evidence = %v, want one entry", wire.ContainerRuntime["evidence"])
	}
	entry, ok := evidence[0].(map[string]any)
	if !ok || entry["stack"] != "docker" || entry["kind"] != "socket" || entry["path"] != "/run/docker.sock" {
		t.Errorf("evidence entry = %v, want stack/kind/path of the docker socket", evidence[0])
	}
	if _, present := wire.Hypervisor["reason"]; !present {
		t.Error("hypervisor.reason is missing; the panel reads it to explain an unknown state")
	}
}

func TestPresence_UnknownIsItsOwnValue(t *testing.T) {
	// "unknown" must never collide with "absent": a node that could not look
	// would then hide a tab it has no grounds to hide.
	if types.PresenceUnknown == types.PresenceAbsent || types.PresenceUnknown == "" {
		t.Errorf("PresenceUnknown = %q, want a distinct, explicit value", types.PresenceUnknown)
	}
	if string(types.PresenceUnknown) != "unknown" {
		t.Errorf("PresenceUnknown = %q, want \"unknown\"", types.PresenceUnknown)
	}
}
