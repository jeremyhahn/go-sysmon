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

// TestBuildIdentify16CDB verifies IDENTIFY DEVICE goes out as a one-sector PIO
// Data-In command carrying no SMART signature. The absent signature is the part
// worth pinning: SMART commands require 0x4F/0xC2 in LBA mid/high, IDENTIFY
// requires them clear, and copying the SMART builder's bytes here would produce
// a command drives reject.
func TestBuildIdentify16CDB(t *testing.T) {
	cdb := buildIdentify16CDB()

	want := map[int]byte{
		0:  0x85,           // ATA PASS-THROUGH(16)
		1:  0x08,           // PROTOCOL=4 (PIO Data-In) << 1
		2:  0x0E,           // T_LENGTH=2, BYT_BLOK=1, T_DIR=1
		6:  0x01,           // sector_count = 1
		14: ataCmdIdentify, // 0xEC
	}
	for off, exp := range want {
		if cdb[off] != exp {
			t.Errorf("cdb[%d] = 0x%02X, want 0x%02X", off, cdb[off], exp)
		}
	}
	for _, off := range []int{9, 10, 11, 12} {
		if cdb[off] != 0x00 {
			t.Errorf("cdb[%d] = 0x%02X, want 0x00: IDENTIFY DEVICE carries no LBA signature", off, cdb[off])
		}
	}
}

// identifyResponse builds a 512-byte IDENTIFY DEVICE payload with words 82 and
// 83 set, so the decoder can be exercised without a drive.
func identifyResponse(word82, word83 uint16) []byte {
	id := make([]byte, smartDataSize)
	id[2*identifyWordCommandSets1] = byte(word82)
	id[2*identifyWordCommandSets1+1] = byte(word82 >> 8)
	id[2*identifyWordCommandSets2] = byte(word83)
	id[2*identifyWordCommandSets2+1] = byte(word83 >> 8)
	return id
}

// TestIdentifySMARTSupported_ReadsTheSupportBit covers the two answers a valid
// IDENTIFY response can give.
func TestIdentifySMARTSupported_ReadsTheSupportBit(t *testing.T) {
	const valid = 0x4000 // word 83: bit 14 set, bit 15 clear

	tests := []struct {
		name   string
		word82 uint16
		want   bool
	}{
		{"drive implements SMART", valid | 0x0001, true},
		{"drive implements no SMART", valid &^ 0x0001, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := identifySMARTSupported(identifyResponse(tc.word82, valid))
			if err != nil {
				t.Fatalf("identifySMARTSupported() error = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("identifySMARTSupported() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIdentifySMARTSupported_RefusesToGuess is the case that matters most. A
// response whose command-set words are marked invalid does not say the drive
// has no SMART -- it says the drive did not answer the question. Returning
// false there would turn an absence into an answer, which is exactly the
// substitution this change exists to remove.
func TestIdentifySMARTSupported_RefusesToGuess(t *testing.T) {
	tests := []struct {
		name string
		id   []byte
	}{
		{"validity bits clear", identifyResponse(0x0001, 0x0000)},
		{"validity bits say 0xF000", identifyResponse(0x0001, 0xF000)},
		{"all-zero response", make([]byte, smartDataSize)},
		{"truncated response", make([]byte, 8)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := identifySMARTSupported(tc.id)
			if err == nil {
				t.Fatalf("identifySMARTSupported() = (%v, nil), want an error: an unreadable answer is not a negative one", got)
			}
			if got {
				t.Error("identifySMARTSupported() reported support alongside an error")
			}
		})
	}
}
