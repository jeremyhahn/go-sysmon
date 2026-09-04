package collector

import (
	"encoding/binary"
	"os"
	"testing"

	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/alert"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// The fixture is a real controller's answer, not a synthetic one.
//
// testdata/megaraid-config-dellemc01.bin is 1120 bytes captured from
// ci-node-1's PERC H740P Mini on 2026-09-02. Its configuration is known
// independently -- from `smartctl --scan-open`, from the machine's own build
// notes, and from the drives' INQUIRY responses read through the controller --
// so the decoder can be checked against an answer nobody derived from the
// decoder itself:
//
//	target 0, "OS",     RAID1,  2 × SATA SSD,          device IDs 0 and 1
//	target 1, "raid10", RAID10, 4 × 10K SAS HITACHI,   device IDs 6, 7, 8, 9
//
// A synthetic fixture would have encoded my reading of the layout and then
// confirmed it, which is the failure mode this whole file exists to avoid.
func realConfig(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/megaraid-config-dellemc01.bin")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func TestParseControllerConfig_ReproducesTheKnownLayout(t *testing.T) {
	cfg, err := parseControllerConfig(realConfig(t))
	if err != nil {
		t.Fatalf("parseControllerConfig() error = %v", err)
	}

	if len(cfg.Arrays) != 2 || len(cfg.Volumes) != 2 {
		t.Fatalf("got %d arrays and %d volumes, want 2 and 2", len(cfg.Arrays), len(cfg.Volumes))
	}

	byTarget := map[uint8]megaraidVolume{}
	for _, v := range cfg.Volumes {
		byTarget[v.TargetID] = v
	}

	tests := []struct {
		target    uint8
		name      string
		level     string
		state     string
		degraded  bool
		numDrives uint8
		spans     int
		members   []uint16
	}{
		{
			target: 0, name: "OS", level: "raid1", state: "active",
			numDrives: 2, spans: 1, members: []uint16{0, 1},
		},
		{
			// Two spans, and BOTH reference the same four-drive array: this
			// controller lays two spans over one array rather than building
			// the volume from two. Walking spans instead of distinct arrays
			// returns all four drives twice, which is what the first version
			// of this decoder did and what a hand-written fixture would not
			// have caught.
			target: 1, name: "raid10", level: "raid10", state: "active",
			numDrives: 4, spans: 2, members: []uint16{6, 7, 9, 8},
		},
	}

	for _, tc := range tests {
		vol, ok := byTarget[tc.target]
		if !ok {
			t.Errorf("no volume with target ID %d", tc.target)
			continue
		}
		if vol.Name != tc.name {
			t.Errorf("target %d name = %q, want %q", tc.target, vol.Name, tc.name)
		}
		if got := vol.RAIDLevelName(); got != tc.level {
			t.Errorf("target %d level = %q, want %q", tc.target, got, tc.level)
		}
		if got := vol.ArrayState(); got != tc.state {
			t.Errorf("target %d state = %q, want %q", tc.target, got, tc.state)
		}
		if vol.Degraded() != tc.degraded {
			t.Errorf("target %d degraded = %v, want %v", tc.target, vol.Degraded(), tc.degraded)
		}
		if vol.NumDrives != tc.numDrives {
			t.Errorf("target %d numDrives = %d, want %d", tc.target, vol.NumDrives, tc.numDrives)
		}
		if len(vol.ArrayRefs) != tc.spans {
			t.Errorf("target %d has %d spans, want %d", tc.target, len(vol.ArrayRefs), tc.spans)
		}

		var got []uint16
		for _, m := range cfg.MembersOf(vol) {
			got = append(got, m.DeviceID)
		}
		if len(got) != len(tc.members) {
			t.Errorf("target %d members = %v, want %v", tc.target, got, tc.members)
			continue
		}
		for i := range got {
			if got[i] != tc.members[i] {
				t.Errorf("target %d members = %v, want %v", tc.target, got, tc.members)
				break
			}
		}
	}
}

// TestParseControllerConfig_MembersCarryTheirEnclosureSlot pins the physical
// location, which is the only human-usable identity a drive with no device node
// has: the bay to pull when one fails.
func TestParseControllerConfig_MembersCarryTheirEnclosureSlot(t *testing.T) {
	cfg, err := parseControllerConfig(realConfig(t))
	if err != nil {
		t.Fatalf("parseControllerConfig() error = %v", err)
	}

	// On this machine every drive is in enclosure 64 and its slot equals its
	// device ID -- which is a property of this chassis, not a rule, so the
	// assertion is that the fields are populated and consistent rather than
	// that slot always equals device ID.
	seen := 0
	for _, arr := range cfg.Arrays {
		for _, m := range arr.Members {
			seen++
			if m.Enclosure != 64 {
				t.Errorf("device %d enclosure = %d, want 64", m.DeviceID, m.Enclosure)
			}
			if uint16(m.Slot) != m.DeviceID {
				t.Errorf("device %d slot = %d; on this chassis they coincide", m.DeviceID, m.Slot)
			}
		}
	}
	if seen != 6 {
		t.Errorf("saw %d members across all arrays, want 6", seen)
	}
}

// TestParseControllerConfig_RefusesInconsistentReplies covers the firmware
// telling us about more than it sent. Every count and entry size in the reply
// is firmware-supplied, and using one to index without checking it is how a
// decoder reads past the end of a buffer.
func TestParseControllerConfig_RefusesInconsistentReplies(t *testing.T) {
	tests := []struct {
		name  string
		build func([]byte) []byte
	}{
		{
			name:  "truncated below the header",
			build: func(b []byte) []byte { return b[:8] },
		},
		{
			// The panic. A 64 KiB buffer whose first four bytes declare a
			// length of 1 was truncated to reply[:1], and the next read walked
			// off it and took the collector down -- #20's lesson arriving by
			// another route, in the code written to deal with #20.
			name: "declares fewer bytes than its own header",
			build: func(b []byte) []byte {
				binary.LittleEndian.PutUint32(b[confSize:], 1)
				return b
			},
		},
		{
			name: "declares zero bytes",
			build: func(b []byte) []byte {
				binary.LittleEndian.PutUint32(b[confSize:], 0)
				return b
			},
		},
		{
			name: "declares more bytes than were returned",
			build: func(b []byte) []byte {
				binary.LittleEndian.PutUint32(b[confSize:], uint32(len(b)+1))
				return b
			},
		},
		{
			name: "more arrays than the reply can hold",
			build: func(b []byte) []byte {
				binary.LittleEndian.PutUint16(b[confArrayCount:], 4096)
				return b
			},
		},
		{
			name: "more volumes than the reply can hold",
			build: func(b []byte) []byte {
				binary.LittleEndian.PutUint16(b[confLDCount:], 4096)
				return b
			},
		},
		{
			name: "array entries too small for their own fields",
			build: func(b []byte) []byte {
				binary.LittleEndian.PutUint16(b[confArraySize:], 4)
				return b
			},
		},
		{
			name: "volume entries too small for their own fields",
			build: func(b []byte) []byte {
				binary.LittleEndian.PutUint16(b[confLDSize:], 8)
				return b
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := append([]byte(nil), realConfig(t)...)
			if _, err := parseControllerConfig(tc.build(raw)); err == nil {
				t.Error("parseControllerConfig() accepted a reply that does not describe itself")
			}
		})
	}
}

// TestParseControllerConfig_ClampsAnOverstatedMemberCount is the same defence
// one level down: an array entry is bounded, but the count inside it is not.
func TestParseControllerConfig_ClampsAnOverstatedMemberCount(t *testing.T) {
	raw := append([]byte(nil), realConfig(t)...)
	arraySize := int(binary.LittleEndian.Uint16(raw[confArraySize:]))
	binary.LittleEndian.PutUint16(raw[confHeaderLen+arrayNumDrives:], 60000)

	cfg, err := parseControllerConfig(raw)
	if err != nil {
		t.Fatalf("parseControllerConfig() error = %v", err)
	}
	room := (arraySize - arrayMembers) / arrayMemberLen
	if got := len(cfg.Arrays[0].Members); got > room || got > arrayMaxMembers {
		t.Errorf("array reported %d members from an entry with room for %d", got, room)
	}
}

// TestMembersOf_DeclinesAMissingArray covers the join failing. A volume
// credited with somebody else's drives is worse than one reported with none.
func TestMembersOf_DeclinesAMissingArray(t *testing.T) {
	cfg := &megaraidConfig{
		Arrays: []megaraidArray{{Members: []megaraidMember{{DeviceID: 3}}}},
	}
	vol := megaraidVolume{ArrayRefs: []uint16{0, 99}}

	got := cfg.MembersOf(vol)
	if len(got) != 1 || got[0].DeviceID != 3 {
		t.Errorf("MembersOf() = %v; the span that resolves must be kept and the one that does not must be dropped", got)
	}
}

// TestVolumeState_UnknownIsNotHealthy is the fail-open guard. A state the
// firmware reports and we do not recognise must not read as a working array.
func TestVolumeState_UnknownIsNotHealthy(t *testing.T) {
	tests := []struct {
		state    uint8
		want     string
		degraded bool
	}{
		{mrLDOptimal, "active", false},
		{mrLDDegraded, "active", true},
		{mrLDPartiallyDegraded, "active", true},
		{mrLDOffline, "broken", false},
		{200, "", false},
	}
	for _, tc := range tests {
		vol := megaraidVolume{State: tc.state}
		if got := vol.ArrayState(); got != tc.want {
			t.Errorf("state %d -> %q, want %q", tc.state, got, tc.want)
		}
		if got := vol.Degraded(); got != tc.degraded {
			t.Errorf("state %d degraded = %v, want %v", tc.state, got, tc.degraded)
		}
	}
}

// TestRAIDLevelName_ReportsTheNumberItCannotName keeps an unrecognised level
// from being labelled as a level it is not.
func TestRAIDLevelName_ReportsTheNumberItCannotName(t *testing.T) {
	if got := (megaraidVolume{RAIDLevel: 0x11}).RAIDLevelName(); got != "raid10" {
		t.Errorf("0x11 -> %q, want raid10", got)
	}
	if got := (megaraidVolume{RAIDLevel: 0x7E}).RAIDLevelName(); got != "raid-level-0x7e" {
		t.Errorf("unknown level -> %q, want the number", got)
	}
}

// TestMembersOf_CountsAnArrayOnceHoweverManySpansUseIt is the regression from
// the line above, stated on its own so it cannot be lost if the fixture is
// replaced: two spans over one array describe one set of drives.
func TestMembersOf_CountsAnArrayOnceHoweverManySpansUseIt(t *testing.T) {
	cfg := &megaraidConfig{
		Arrays: []megaraidArray{{Members: []megaraidMember{{DeviceID: 6}, {DeviceID: 7}}}},
	}
	vol := megaraidVolume{SpanDepth: 2, ArrayRefs: []uint16{0, 0}}

	if got := cfg.MembersOf(vol); len(got) != 2 {
		t.Errorf("MembersOf() returned %d members for two spans over one array, want 2", len(got))
	}

	// And two spans over DIFFERENT arrays really do contribute both.
	cfg.Arrays = append(cfg.Arrays, megaraidArray{Members: []megaraidMember{{DeviceID: 8}}})
	if got := cfg.MembersOf(megaraidVolume{SpanDepth: 2, ArrayRefs: []uint16{0, 1}}); len(got) != 3 {
		t.Errorf("MembersOf() returned %d members for two spans over two arrays, want 3", len(got))
	}
}

// TestVolumeRAIDInfo_DescribesAHardwareArrayInTheSharedVocabulary checks the
// shape the rest of the system consumes. pkg/alert, pkg/metrics and the fleet
// roll-up all read RAIDInfo, and a hardware volume that filled it differently
// would need each of them to learn a second notion of what a RAID array is.
func TestVolumeRAIDInfo_DescribesAHardwareArrayInTheSharedVocabulary(t *testing.T) {
	cfg, err := parseControllerConfig(realConfig(t))
	if err != nil {
		t.Fatalf("parseControllerConfig() error = %v", err)
	}

	var raid10 megaraidVolume
	for _, v := range cfg.Volumes {
		if v.TargetID == 1 {
			raid10 = v
		}
	}

	// "Active" is the CONTROLLER's verdict, taken from the configuration --
	// not from the members we managed to decode, and not from whether a drive
	// answered a health command. All four of this volume's drives are ONLINE
	// in the config, so all four are active however the health reads go.
	info := volumeRAIDInfo(raid10, cfg.MembersOf(raid10))

	if info.Level != "raid10" || info.State != "active" || info.Degraded {
		t.Errorf("info = %+v, want an active, undegraded raid10", info)
	}
	if info.TotalDevices != 4 {
		t.Errorf("total devices = %d, want 4", info.TotalDevices)
	}
	// Four online, whatever they said about their own health.
	if info.ActiveDevices != 4 {
		t.Errorf("active devices = %d, want 4", info.ActiveDevices)
	}
	// And it must not depend on how many members we decoded. A drive we could
	// not identify, or the backplane's enclosure processor skipped because it
	// is not a drive, previously reduced this count and made our own decode
	// gap look like a degraded array.
	if short := volumeRAIDInfo(raid10, cfg.MembersOf(raid10)[:1]); short.ActiveDevices == info.ActiveDevices {
		t.Error("the in-sync count is meant to come from the configuration; this fixture cannot tell")
	}
	// RAIDMember names a block device, and these are not block devices; they
	// are on DiskInfo.Members instead. Filling this would give one field two
	// meanings.
	if len(info.Members) != 0 {
		t.Errorf("RAIDInfo.Members = %v, want empty for a hardware volume", info.Members)
	}
	// NOTHING HERE ASKS THE CONTROLLER ABOUT REBUILD PROGRESS, so nothing here
	// may report any. "idle" and 100 are what a healthy md array reports --
	// what "nothing wrong" looks like in this struct -- so a volume in the
	// middle of a rebuild would have been published as a COMPLETED one, on the
	// one occasion this product exists for.
	if info.SyncAction != "" {
		t.Errorf("SyncAction = %q, want \"\": the controller was never asked", info.SyncAction)
	}
	if info.SyncProgressPercent != 0 {
		t.Errorf("SyncProgressPercent = %v, want 0 with no action: a percentage nobody measured",
			info.SyncProgressPercent)
	}
	// alert and the fleet roll-up must agree this array is working.
	if alert.RAIDNotOperational(info.State) {
		t.Error("an optimal hardware volume was reported as not serving I/O")
	}
}

// TestVolumeRAIDInfo_AnOfflineVolumeIsNotOperational is the fail-open guard at
// the seam: the state string has to land inside the set pkg/alert already
// treats as "not serving I/O", or the roll-up would call an offline array ok.
func TestVolumeRAIDInfo_AnOfflineVolumeIsNotOperational(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state uint8
	}{
		{"offline", mrLDOffline},
		{"a state we do not recognise", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := volumeRAIDInfo(megaraidVolume{State: tc.state, NumDrives: 2}, nil)
			if !alert.RAIDNotOperational(info.State) {
				t.Errorf("state %q was treated as a working array", info.State)
			}
		})
	}

	// And a degraded array IS operational -- it still serves I/O with reduced
	// redundancy -- so it must be reported as degraded rather than as stopped.
	degraded := volumeRAIDInfo(megaraidVolume{State: mrLDDegraded, NumDrives: 2}, nil)
	if alert.RAIDNotOperational(degraded.State) {
		t.Error("a degraded array was reported as not serving I/O")
	}
	if !degraded.Degraded {
		t.Error("a degraded array was not reported as degraded")
	}
}

// TestParseArray_ReadsTheControllerVerdictOnEachDrive pins the member state
// against the real capture. Every drive on ci-node-1 reports
// MR_PD_STATE_ONLINE (0x18) while both its arrays are optimal, which is the
// only value this decoder has been able to measure -- so ONLY that value counts
// as online, and anything else reports a drive as not in service. That errs
// toward saying an array has fewer working drives than it has, which is the
// direction that raises attention rather than suppressing it.
func TestParseArray_ReadsTheControllerVerdictOnEachDrive(t *testing.T) {
	cfg, err := parseControllerConfig(realConfig(t))
	if err != nil {
		t.Fatalf("parseControllerConfig() error = %v", err)
	}
	for _, arr := range cfg.Arrays {
		for _, m := range arr.Members {
			if m.State != mrPDStateOnline {
				t.Errorf("device %d state = 0x%04x, want 0x%04x", m.DeviceID, m.State, mrPDStateOnline)
			}
			if !m.Online() {
				t.Errorf("device %d was not reported as online", m.DeviceID)
			}
		}
	}

	// A state we have not seen is not online. It is the fail-safe direction:
	// an unrecognised value must not be counted as a working drive.
	for _, state := range []uint16{0x00, 0x10, 0x11, 0x14, 0x99} {
		if (megaraidMember{State: state}).Online() {
			t.Errorf("state 0x%04x was counted as online", state)
		}
	}
}

// TestVolumeRAIDInfo_AnOfflineDriveThatAnswersIsStillOffline is the other
// direction of the same confusion. A drive the controller has taken out of
// service can go on answering SMART perfectly well, and counting it as active
// would report a degraded array as whole.
func TestVolumeRAIDInfo_AnOfflineDriveThatAnswersIsStillOffline(t *testing.T) {
	info := volumeRAIDInfo(
		megaraidVolume{State: mrLDDegraded, NumDrives: 2, RAIDLevel: 0x01},
		[]megaraidMember{
			{DeviceID: 0, State: mrPDStateOnline},
			{DeviceID: 1, State: 0x10}, // OFFLINE, and it still answers SMART
		},
	)

	if info.ActiveDevices != 1 {
		t.Errorf("active devices = %d, want 1: the offline drive answered, and is still offline", info.ActiveDevices)
	}
	if info.TotalDevices != 2 {
		t.Errorf("total devices = %d, want 2", info.TotalDevices)
	}
	if !info.Degraded {
		t.Error("a degraded volume was not reported as degraded")
	}
}

// TestVolumeRAIDInfo_UnknownSyncStateIsSilentNotSatisfied is the half that
// matters more than the field values: an alert rule whose input is hardcoded to
// the all-clear is a rule that reads as coverage and provides none.
//
// raid_resync must be ABSENT for a hardware volume -- because nothing asked --
// rather than satisfied by a fabricated "idle".
func TestVolumeRAIDInfo_UnknownSyncStateIsSilentNotSatisfied(t *testing.T) {
	info := volumeRAIDInfo(
		megaraidVolume{State: mrLDOptimal, NumDrives: 2, RAIDLevel: 0x01}, nil)

	snap := &types.Snapshot{Disks: []types.DiskInfo{{Name: "sda", RAID: info}}}
	for _, a := range alert.New().Evaluate(config.Defaults().Alerts, snap, time.Now()) {
		if a.Rule == alert.RuleRAIDResync {
			t.Errorf("raid_resync fired on a volume whose sync state was never read: %q", a.Message)
		}
	}

	// And an md array, which DOES report its sync state, still alerts. The fix
	// must not have bought silence by making the rule unreachable.
	syncing := &types.RAIDInfo{
		Level: "raid1", State: "active", SyncAction: "recover", SyncProgressPercent: 42,
	}
	mdSnap := &types.Snapshot{Disks: []types.DiskInfo{{Name: "md0", RAID: syncing}}}
	fired := false
	for _, a := range alert.New().Evaluate(config.Defaults().Alerts, mdSnap, time.Now()) {
		if a.Rule == alert.RuleRAIDResync {
			fired = true
		}
	}
	if !fired {
		t.Error("raid_resync did not fire for an md array that is recovering")
	}
}

// TestVolumeRAIDInfo_MDOnlyCountersAreUnsetAndSaidSo pins the contract for the
// last two md-only fields, using the real controller's configuration rather
// than a fixture that would encode the belief being tested (#60).
//
// The four fields the RAIDInfo sentinel covers -- SyncProgressPercent,
// MismatchCount, FailedDevices, SpareDevices -- must all be at their zero
// values on a hardware volume, and SyncAction must be empty to say why. A
// consumer applying the one documented rule then gets the right answer for all
// four; if any of them were populated without SyncAction being set, that rule
// would silently stop working for the others.
//
// This is not asserting that zero is the truth. It is asserting that zero is
// accompanied by the flag which says it is not a measurement.
func TestVolumeRAIDInfo_MDOnlyCountersAreUnsetAndSaidSo(t *testing.T) {
	cfg, err := parseControllerConfig(realConfig(t))
	if err != nil {
		t.Fatalf("parse the real controller config: %v", err)
	}
	if len(cfg.Volumes) == 0 {
		t.Fatal("the fixture has no volumes, so this proves nothing")
	}

	for _, vol := range cfg.Volumes {
		info := volumeRAIDInfo(vol, cfg.MembersOf(vol))
		if info == nil {
			t.Fatalf("volume %q produced no RAIDInfo", vol.Name)
		}

		// The flag first: without it the zeroes below are just zeroes.
		if info.SyncAction != "" {
			t.Errorf("volume %q: SyncAction = %q, want empty -- the controller is never asked",
				vol.Name, info.SyncAction)
		}

		unread := []struct {
			field string
			value float64
		}{
			{"SyncProgressPercent", info.SyncProgressPercent},
			{"MismatchCount", float64(info.MismatchCount)},
			{"FailedDevices", float64(info.FailedDevices)},
			{"SpareDevices", float64(info.SpareDevices)},
		}
		for _, u := range unread {
			if u.value != 0 {
				t.Errorf("volume %q: %s = %v, want 0. If the controller can now report this, "+
					"the RAIDInfo sentinel has to change with it -- one flag covers all four, "+
					"so populating one of them alone breaks the rule for the other three",
					vol.Name, u.field, u.value)
			}
		}

		// And the fields that ARE read stay read: the gap is four fields wide,
		// not the whole struct.
		if info.TotalDevices == 0 {
			t.Errorf("volume %q: TotalDevices = 0, so the volume lost the counts it does report",
				vol.Name)
		}
	}

	// The real controller has no failed drive, which makes every count above
	// zero whether or not anything derives them -- the fixture cannot reach the
	// case it claims to guard. So the guard is asserted on a DEGRADED volume,
	// where a derivation would produce a non-zero number and be visible.
	//
	// "Total minus online" is the obvious derivation and it is wrong: a drive
	// rebuilding is not a failed drive, a hot spare is not a member, and the
	// MFI state constants beyond ONLINE have never been observed on real
	// hardware here. One offline member out of two would report "1 failed",
	// which is a claim nobody made.
	degraded := volumeRAIDInfo(
		megaraidVolume{State: mrLDDegraded, NumDrives: 2, RAIDLevel: 0x01},
		[]megaraidMember{
			{DeviceID: 0, State: mrPDStateOnline},
			{DeviceID: 1, State: 0x10}, // OFFLINE
		},
	)
	if degraded.ActiveDevices != 1 {
		t.Fatalf("active devices = %d, want 1: the fixture is not degraded, so it proves nothing",
			degraded.ActiveDevices)
	}
	if degraded.FailedDevices != 0 {
		t.Errorf("FailedDevices = %d on a degraded volume, want 0: a count derived from "+
			"member states is not a count the controller reported", degraded.FailedDevices)
	}
	if degraded.SpareDevices != 0 {
		t.Errorf("SpareDevices = %d, want 0 for the same reason", degraded.SpareDevices)
	}
}
