package types_test

import (
	"encoding/json"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// The diagnostic fields a populated container or VM entry must carry.
//
// These used to be asserted only by a browser test that skipped itself on a
// host running nothing (#35) -- so on a CI runner with no containers, the
// guarantee was checked by nobody. The browser test still runs against the
// live server and still checks every entry it finds; this is the half that
// does not depend on the machine having anything to find.
//
// They are asserted by JSON NAME rather than by struct field, because the name
// is the contract. Renaming a field is a Go refactor; renaming its tag breaks
// every dashboard and script reading the payload, and only this notices.
func TestSnapshotPayload_EntriesCarryTheirDiagnosticFields(t *testing.T) {
	tests := []struct {
		name   string
		value  any
		fields []string
	}{
		{
			name: "container",
			// Zero values throughout: the question is whether the KEY is
			// emitted, and a field with omitempty would vanish here, which is
			// exactly the regression worth catching.
			value: types.ContainerInfo{},
			fields: []string{
				"throttled_percent", "nr_throttled", "oom_kills", "memory_peak_bytes",
				"cpu_pressure", "memory_pressure", "io_pressure", "uptime_seconds",
			},
		},
		{
			name:  "virtual machine",
			value: types.VMInfo{},
			fields: []string{
				"vcpu_threads", "thread_count", "uptime_seconds", "net_rx_rate", "net_tx_rate",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			for _, field := range tt.fields {
				if _, ok := decoded[field]; !ok {
					t.Errorf("%q is absent from the %s payload; a consumer reading it gets "+
						"nothing, and nothing reads as a zero", field, tt.name)
				}
			}
		})
	}
}
