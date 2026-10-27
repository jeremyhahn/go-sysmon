package metrics

import (
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

func TestWrite_DriveSlotsAreLabelledBySlot(t *testing.T) {
	out := render(&types.Snapshot{DriveSlots: types.DriveSlots{
		Available: true,
		Source:    types.DriveSlotSourceRedfish,
		Slots: []types.DriveSlot{
			{ID: "Disk.Bay.2", Present: true, Health: types.DriveSlotHealthWarning, FailurePredicted: true,
				MediaLifeLeftPercent: 3, MediaLifeReported: true},
			{ID: "Disk.Bay.3", Present: true, Health: types.DriveSlotHealthCritical, Fault: true, Rebuilding: true},
		},
	}})
	for _, want := range []string{
		"sysmon_drive_slots_available 1\n",
		`sysmon_drive_slot_present{slot="Disk.Bay.2"} 1`,
		`sysmon_drive_slot_failure_predicted{slot="Disk.Bay.2"} 1`,
		`sysmon_drive_slot_fault{slot="Disk.Bay.2"} 0`,
		`sysmon_drive_slot_health{slot="Disk.Bay.2"} 1`,
		`sysmon_drive_slot_media_life_left_percent{slot="Disk.Bay.2"} 3`,
		`sysmon_drive_slot_fault{slot="Disk.Bay.3"} 1`,
		`sysmon_drive_slot_rebuilding{slot="Disk.Bay.3"} 1`,
		`sysmon_drive_slot_health{slot="Disk.Bay.3"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, `sysmon_drive_slot_media_life_left_percent{slot="Disk.Bay.3"}`) {
		t.Error("a media life the BMC did not report was exported")
	}
}

func TestWrite_UnknownSlotHealthIsOmitted(t *testing.T) {
	out := render(&types.Snapshot{DriveSlots: types.DriveSlots{
		Available: true,
		Slots:     []types.DriveSlot{{ID: "Drive 2"}},
	}})
	if strings.Contains(out, "sysmon_drive_slot_health") {
		t.Errorf("an unreported health was exported as a reading:\n%s", out)
	}
	if !strings.Contains(out, `sysmon_drive_slot_present{slot="Drive 2"} 0`) {
		t.Errorf("an empty bay is not exported as absent:\n%s", out)
	}
}

func TestWrite_UnavailableDriveSlotsSayZero(t *testing.T) {
	out := render(&types.Snapshot{DriveSlots: types.DriveSlots{Reason: "no BMC configured"}})
	if !strings.Contains(out, "sysmon_drive_slots_available 0\n") {
		t.Errorf("an unavailable collector is not exported as 0:\n%s", out)
	}
	if strings.Contains(out, "no BMC configured") {
		t.Errorf("the unavailable reason leaked into the exposition:\n%s", out)
	}
}
