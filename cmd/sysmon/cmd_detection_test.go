package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// The containers, vms and images commands report the same runtime and
// hypervisor detection as the API and dashboard (#187): the wire type in
// --json, one line in text.

// detections are the three states, each with what the text line must say.
var detections = []struct {
	name     string
	d        types.StackDetection
	wantText string
}{
	{
		name: "present",
		d: types.StackDetection{State: types.PresencePresent, Evidence: []types.StackEvidence{
			{Stack: "containerd", Kind: types.EvidenceSocket, Path: "/run/containerd/containerd.sock"},
			{Stack: "docker", Kind: types.EvidenceBinary, Path: "/usr/bin/docker"},
		}},
		wantText: "present (containerd, docker)",
	},
	{
		name:     "absent",
		d:        types.StackDetection{State: types.PresenceAbsent, Evidence: []types.StackEvidence{}},
		wantText: "absent",
	},
	{
		name:     "unknown",
		d:        types.StackDetection{State: types.PresenceUnknown, Evidence: []types.StackEvidence{}, Reason: "/proc is mounted hidepid=2"},
		wantText: "unknown — /proc is mounted hidepid=2",
	},
}

// wireDetection decodes one detection field from a command's JSON output.
func wireDetection(t *testing.T, raw []byte, key string) types.StackDetection {
	t.Helper()
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\nraw:\n%s", err, raw)
	}
	field, ok := parsed[key]
	if !ok {
		t.Fatalf("output has no %q field; raw:\n%s", key, raw)
	}
	var d types.StackDetection
	if err := json.Unmarshal(field, &d); err != nil {
		t.Fatalf("%s is not a StackDetection: %v", key, err)
	}
	return d
}

// assertSameDetection compares a decoded detection with the one served.
func assertSameDetection(t *testing.T, got, want types.StackDetection) {
	t.Helper()
	if got.State != want.State || got.Reason != want.Reason || len(got.Evidence) != len(want.Evidence) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want.Evidence {
		if got.Evidence[i] != want.Evidence[i] {
			t.Errorf("evidence[%d] = %+v, want %+v", i, got.Evidence[i], want.Evidence[i])
		}
	}
}

func TestRunContainersCmd_ReportsRuntimeDetection(t *testing.T) {
	for _, tt := range detections {
		t.Run(tt.name, func(t *testing.T) {
			resetGlobals(t)
			snap := virtFixture()
			snap.Virt.ContainerRuntime = tt.d
			withFixtureSnapshot(t, snap, nil)

			var text bytes.Buffer
			if err := runContainersCmd(newTestCmd(&text), nil); err != nil {
				t.Fatalf("runContainersCmd() error = %v", err)
			}
			if !strings.Contains(text.String(), "Runtime:    "+tt.wantText) {
				t.Errorf("text missing %q; got:\n%s", "Runtime:    "+tt.wantText, text.String())
			}

			jsonOutput = true
			var raw bytes.Buffer
			if err := runContainersCmd(newTestCmd(&raw), nil); err != nil {
				t.Fatalf("runContainersCmd() --json error = %v", err)
			}
			assertSameDetection(t, wireDetection(t, raw.Bytes(), "container_runtime"), tt.d)
		})
	}
}

func TestRunVMsCmd_ReportsHypervisorDetection(t *testing.T) {
	for _, tt := range detections {
		t.Run(tt.name, func(t *testing.T) {
			resetGlobals(t)
			snap := virtFixture()
			snap.Virt.Hypervisor = tt.d
			withFixtureSnapshot(t, snap, nil)

			var text bytes.Buffer
			if err := runVMsCmd(newTestCmd(&text), nil); err != nil {
				t.Fatalf("runVMsCmd() error = %v", err)
			}
			if !strings.Contains(text.String(), "Hypervisor: "+tt.wantText) {
				t.Errorf("text missing %q; got:\n%s", "Hypervisor: "+tt.wantText, text.String())
			}

			jsonOutput = true
			var raw bytes.Buffer
			if err := runVMsCmd(newTestCmd(&raw), nil); err != nil {
				t.Fatalf("runVMsCmd() --json error = %v", err)
			}
			assertSameDetection(t, wireDetection(t, raw.Bytes(), "hypervisor"), tt.d)
		})
	}
}

func TestRunImagesCmd_ReportsRuntimeDetection(t *testing.T) {
	for _, tt := range detections {
		t.Run(tt.name, func(t *testing.T) {
			resetGlobals(t)
			snap := imagesFixture()
			snap.Virt.ContainerRuntime = tt.d
			withRuntimeSnapshot(t, snap, nil)

			var text bytes.Buffer
			if err := runImagesCmd(newTestCmd(&text), nil); err != nil {
				t.Fatalf("runImagesCmd() error = %v", err)
			}
			if !strings.Contains(text.String(), "Runtime:    "+tt.wantText) {
				t.Errorf("text missing %q; got:\n%s", "Runtime:    "+tt.wantText, text.String())
			}

			jsonOutput = true
			var raw bytes.Buffer
			if err := runImagesCmd(newTestCmd(&raw), nil); err != nil {
				t.Fatalf("runImagesCmd() --json error = %v", err)
			}
			assertSameDetection(t, wireDetection(t, raw.Bytes(), "container_runtime"), tt.d)
			// RuntimeInfo's own fields stay where they were.
			var parsed map[string]any
			if err := json.Unmarshal(raw.Bytes(), &parsed); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if _, ok := parsed["root_dir"]; !ok {
				t.Errorf("images --json lost root_dir from the top level; got %v", keysOf(parsed))
			}
		})
	}
}
