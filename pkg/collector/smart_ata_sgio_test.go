//go:build linux

package collector

import "testing"

// TestBuildSMART16CDB verifies the ATA PASS-THROUGH(16) command block for a
// SMART READ DATA request matches the wire format libata/SAT expects: the right
// opcode, PIO Data-In protocol, a one-sector data-in transfer, the SMART
// signature in the LBA mid/high bytes, and the SMART command in byte 14.
func TestBuildSMART16CDB(t *testing.T) {
	cdb := buildSMART16CDB(smartCmdReadData)

	want := map[int]byte{
		0:  0x85,             // ATA PASS-THROUGH(16)
		1:  0x08,             // PROTOCOL=4 (PIO Data-In) << 1
		2:  0x0E,             // T_LENGTH=2, BYT_BLOK=1, T_DIR=1
		4:  smartCmdReadData, // features = SMART READ DATA (0xD0)
		6:  0x01,             // sector_count = 1
		10: smartCylLow,      // 0x4F SMART signature
		12: smartCylHigh,     // 0xC2 SMART signature
		14: ataCmdSMART,      // 0xB0 SMART command
	}
	for off, exp := range want {
		if cdb[off] != exp {
			t.Errorf("cdb[%d] = 0x%02X, want 0x%02X", off, cdb[off], exp)
		}
	}
}

// TestBuildSMART16CDB_Thresholds confirms only the features byte changes between
// the READ DATA and READ THRESHOLDS sub-commands; the transfer shape and SMART
// signature are identical, which is what lets the caller reuse one reader for
// both.
func TestBuildSMART16CDB_Thresholds(t *testing.T) {
	data := buildSMART16CDB(smartCmdReadData)
	thr := buildSMART16CDB(smartCmdReadThresholds)

	if thr[4] != smartCmdReadThresholds {
		t.Errorf("threshold cdb features = 0x%02X, want 0x%02X", thr[4], smartCmdReadThresholds)
	}
	for i := range data {
		if i == 4 {
			continue
		}
		if data[i] != thr[i] {
			t.Errorf("cdb[%d] differs (data=0x%02X thr=0x%02X); only the features byte should change", i, data[i], thr[i])
		}
	}
}
