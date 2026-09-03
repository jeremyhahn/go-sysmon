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

// The HDIO_DRIVE_CMD argument block, where byte 3 is a size contract.
//
// This is the block that crash-looped ci-node-1 (#20). It carried the SMART
// signature's 0x4F in byte 3, which HDIO_DRIVE_CMD reads as the SECTOR COUNT,
// so the kernel copied 512 x 79 = 40,448 bytes back into a 516-byte stack
// buffer and the next allocation died inside runtime.mallocgc.
//
// A wrong value here cannot be caught anywhere downstream: by the time the
// ioctl returns, the memory is already overwritten. So the block is built by a
// pure function and pinned, the same treatment buildSMART16CDB gets.
func TestBuildHDIOArgs_SectorCountMatchesTheBufferWeProvide(t *testing.T) {
	args := buildHDIOArgs(smartCmdReadData)

	if len(args) != hdioArgsLen {
		t.Fatalf("argument block is %d bytes, want %d", len(args), hdioArgsLen)
	}
	if args[hdioArgCommand] != ataCmdSMART {
		t.Errorf("command = 0x%02X, want 0x%02X", args[hdioArgCommand], ataCmdSMART)
	}
	if args[hdioArgFeature] != smartCmdReadData {
		t.Errorf("feature = 0x%02X, want 0x%02X", args[hdioArgFeature], smartCmdReadData)
	}

	// The assertion this test exists for. One sector, because one sector is
	// what the caller's buffer holds.
	if args[hdioArgSectorCount] != 1 {
		t.Errorf("SECTOR COUNT = %d, so the kernel would write %d bytes into a %d-byte buffer",
			args[hdioArgSectorCount], int(args[hdioArgSectorCount])*smartDataSize, hdioArgsLen+smartDataSize)
	}

	// LBA low is asserted explicitly, not merely checked for the signature:
	// the kernel maps it to the ATA PASS-THROUGH LBA-low and supplies the
	// SMART signature itself, so ANY non-zero value here is wrong -- and a
	// "no signature bytes" check alone would pass one.
	if args[hdioArgLBALow] != 0 {
		t.Errorf("LBA low = 0x%02X, want 0; the kernel supplies the SMART signature itself",
			args[hdioArgLBALow])
	}

	// The SMART signature must appear NOWHERE in this block. It belongs in the
	// ATA PASS-THROUGH(16) CDB, which the kernel builds itself; every byte of
	// this block means something else, and 0x4F landing in any of them is a
	// register model being applied to an interface that does not use one.
	for i, b := range args {
		if b == smartCylLow || b == smartCylHigh {
			t.Errorf("args[%d] = 0x%02X, a SMART signature byte; this block carries no LBA", i, b)
		}
	}
}

// TestBuildHDIOArgs_OnlyTheFeatureByteVariesBySubCommand keeps the size
// contract fixed across both SMART sub-commands. The threshold read reuses the
// same buffer, so a sector count that varied with the sub-command would overrun
// on one path and not the other -- the hardest version of this bug to find.
func TestBuildHDIOArgs_OnlyTheFeatureByteVariesBySubCommand(t *testing.T) {
	data := buildHDIOArgs(smartCmdReadData)
	thresholds := buildHDIOArgs(smartCmdReadThresholds)

	if thresholds[hdioArgFeature] != smartCmdReadThresholds {
		t.Errorf("threshold feature = 0x%02X, want 0x%02X",
			thresholds[hdioArgFeature], smartCmdReadThresholds)
	}
	for i := range data {
		if i == hdioArgFeature {
			continue
		}
		if data[i] != thresholds[i] {
			t.Errorf("args[%d] differs between sub-commands (0x%02X vs 0x%02X); only the feature byte should",
				i, data[i], thresholds[i])
		}
	}
}

// TestHDIOResponse_StartsAfterTheArgumentBlock pins where the answer begins.
//
// The argument block and the response share one buffer, so the offset the
// response is read from has to be the same constant the sector count was sized
// against. Reading one byte late shifts every SMART attribute by a byte and
// decodes into plausible nonsense rather than failing -- the same class of
// mistake as #20, one step later.
func TestHDIOResponse_StartsAfterTheArgumentBlock(t *testing.T) {
	var buf [hdioArgsLen + smartDataSize]byte
	// A header that is not zero, so reading it as data would be visible.
	for i := 0; i < hdioArgsLen; i++ {
		buf[i] = 0xEE
	}
	for i := range buf[hdioArgsLen:] {
		buf[hdioArgsLen+i] = byte(i % 251)
	}

	got := hdioResponse(buf[:])

	if len(got) != smartDataSize {
		t.Fatalf("response is %d bytes, want %d", len(got), smartDataSize)
	}
	for i := range got {
		if want := byte(i % 251); got[i] != want {
			t.Fatalf("response[%d] = 0x%02X, want 0x%02X: the data is offset from where it was written",
				i, got[i], want)
		}
	}

	// A buffer too small to hold a whole sector yields a ZEROED one, not a
	// partial copy. The comment used to say "zeroed sector" while the code
	// would have copied whatever was there and padded the rest -- which would
	// have been rejected downstream as a corrupt reading from the drive
	// rather than as the caller's mistake it is.
	for _, short := range [][]byte{buf[:2], buf[:hdioArgsLen], buf[:hdioArgsLen+5], nil} {
		got := hdioResponse(short)
		if len(got) != smartDataSize {
			t.Fatalf("a %d-byte buffer produced %d bytes, want %d", len(short), len(got), smartDataSize)
		}
		for i, b := range got {
			if b != 0 {
				t.Fatalf("a %d-byte buffer produced data at [%d] = 0x%02X; want a zeroed sector",
					len(short), i, b)
			}
		}
	}
}
