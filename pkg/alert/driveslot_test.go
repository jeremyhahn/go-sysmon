package alert_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/alert"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

func slotSnapshot(slots ...types.DriveSlot) *types.Snapshot {
	return &types.Snapshot{DriveSlots: types.DriveSlots{
		Available: true,
		Source:    types.DriveSlotSourceRedfish,
		Slots:     slots,
	}}
}

func slotAlerts(snap *types.Snapshot) map[string]types.Alert {
	got := map[string]types.Alert{}
	for _, a := range alert.New().Evaluate(defaults(), snap, time.Now()) {
		if a.Rule == alert.RuleDriveSlotFault {
			got[a.Resource] = a
		}
	}
	return got
}

func TestDriveSlotFault_SeverityFollowsTheSlot(t *testing.T) {
	cases := map[string]struct {
		slot types.DriveSlot
		want types.Severity
	}{
		"fault flag": {
			types.DriveSlot{Present: true, Fault: true, Health: types.DriveSlotHealthCritical, State: "Fault"},
			types.SeverityCritical,
		},
		"critical health without the flag": {
			types.DriveSlot{Present: true, Health: types.DriveSlotHealthCritical},
			types.SeverityCritical,
		},
		"predicted failure": {
			types.DriveSlot{Present: true, FailurePredicted: true, Health: types.DriveSlotHealthWarning},
			types.SeverityWarning,
		},
		"degraded bay": {
			types.DriveSlot{Present: true, Health: types.DriveSlotHealthWarning},
			types.SeverityWarning,
		},
		"fault on a bay reported empty": {
			types.DriveSlot{Fault: true},
			types.SeverityCritical,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tc.slot.ID, tc.slot.Slot = "Disk.Bay.3", "Bay 3"
			got, ok := slotAlerts(slotSnapshot(tc.slot))["Disk.Bay.3"]
			if !ok {
				t.Fatalf("no %s alert for %+v", alert.RuleDriveSlotFault, tc.slot)
			}
			if got.Severity != tc.want {
				t.Errorf("Severity = %q, want %q", got.Severity, tc.want)
			}
			if got.ID != alert.RuleDriveSlotFault+":Disk.Bay.3" {
				t.Errorf("ID = %q, want the rule and the slot ID", got.ID)
			}
			if !strings.Contains(got.Message, "Bay 3") {
				t.Errorf("Message = %q, want it to name the bay", got.Message)
			}
		})
	}
}

func TestDriveSlotFault_MessageCarriesTheState(t *testing.T) {
	got := slotAlerts(slotSnapshot(types.DriveSlot{
		ID: "Drive 0", Slot: "Drive 0", Present: true, Fault: true, State: "Drive Present, Drive Fault",
	}))["Drive 0"]
	if got.Message != "drive slot Drive 0 reports a fault (Drive Present, Drive Fault)" {
		t.Errorf("Message = %q", got.Message)
	}
}

func TestDriveSlotFault_QuietSlotsRaiseNothing(t *testing.T) {
	got := slotAlerts(slotSnapshot(
		types.DriveSlot{ID: "0", Slot: "Bay 0", Present: true, Health: types.DriveSlotHealthOK},
		types.DriveSlot{ID: "1", Slot: "Bay 1", Present: true, Health: types.DriveSlotHealthOK, Rebuilding: true},
		types.DriveSlot{ID: "2", Slot: "Bay 2"},
		types.DriveSlot{ID: "3", Slot: "Bay 3", Present: true},
	))
	if len(got) != 0 {
		t.Errorf("drive_slot_fault fired for %v, want nothing: healthy, rebuilding, empty and unread bays are not faults", got)
	}
}

func TestDriveSlotFault_UnavailableCollectorRaisesNothing(t *testing.T) {
	snap := &types.Snapshot{DriveSlots: types.DriveSlots{Reason: "no BMC configured"}}
	if got := slotAlerts(snap); len(got) != 0 {
		t.Errorf("drive_slot_fault fired for %v on a host with no BMC", got)
	}
}
