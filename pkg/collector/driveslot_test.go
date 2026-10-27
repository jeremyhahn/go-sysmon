package collector

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/alert"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// newTestDriveSlots builds a collector that logs into buf and opens dev in
// place of the IPMI device. A nil dev leaves the real opener, which finds no
// device at the configured path.
func newTestDriveSlots(t *testing.T, cfg DriveSlotConfig, dev func() (ipmiTransport, error)) (*DriveSlotCollector, *bytes.Buffer) {
	t.Helper()
	if cfg.IPMIDevice == "" {
		cfg.IPMIDevice = filepath.Join(t.TempDir(), "ipmi0")
	}
	buf := &bytes.Buffer{}
	c, err := NewDriveSlotCollector(slog.New(slog.NewTextHandler(buf, nil)), cfg)
	if err != nil {
		t.Fatalf("NewDriveSlotCollector() error = %v", err)
	}
	if dev != nil {
		c.openIPMI = func(string) (ipmiTransport, error) { return dev() }
	}
	return c, buf
}

// collectNow runs one poll to completion.
func collectNow(t *testing.T, c *DriveSlotCollector) types.DriveSlots {
	t.Helper()
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if err := c.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	return c.Info()
}

func TestDriveSlotCollector_NoBMCIsUnavailableNotAnError(t *testing.T) {
	c, logs := newTestDriveSlots(t, DriveSlotConfig{}, nil)
	if got := c.Info(); got.Available || got.Reason == "" {
		t.Errorf("Info() before the first poll = %+v, want unavailable with a reason", got)
	}

	got := collectNow(t, c)
	if got.Available || got.Source != types.DriveSlotSourceNone || got.Reason != driveSlotUnconfigured+c.ipmiDevice {
		t.Errorf("Info() = %+v, want unavailable because nothing is configured", got)
	}
	if logs.Len() != 0 {
		t.Errorf("an unconfigured host logged %q; it is not a failure", logs)
	}
}

func TestDriveSlotCollector_ReadsRedfishFirst(t *testing.T) {
	f := newFakeBMC(t, "redfish-dellemc02.json")
	opened := false
	c, _ := newTestDriveSlots(t, f.config(t), func() (ipmiTransport, error) {
		opened = true
		return loadIPMIFixture(t, "ipmi-dellemc02.txt"), nil
	})
	got := collectNow(t, c)
	if !got.Available || got.Source != types.DriveSlotSourceRedfish || got.Reason != "" || len(got.Slots) != 4 {
		t.Fatalf("Info() = %+v, want four bays from Redfish", got)
	}
	for i := 1; i < len(got.Slots); i++ {
		if got.Slots[i-1].ID > got.Slots[i].ID {
			t.Errorf("slots are not in ID order: %q before %q", got.Slots[i-1].ID, got.Slots[i].ID)
		}
	}
	if opened {
		t.Error("the IPMI device was opened although Redfish answered")
	}
}

// The case #289 was filed for: ci-node-2's bay 3 lit its amber LED and
// nothing in go-sysmon said so. Read through the collector and evaluated as
// the service does, the recorded BMC must raise the alert for that bay, and a
// warning for the bay whose drive predicts its own failure.
func TestDriveSlotCollector_SlotFaultFixtureRaisesTheAlert(t *testing.T) {
	f := newFakeBMC(t, "redfish-dellemc02.json")
	c, _ := newTestDriveSlots(t, f.config(t), nil)
	snap := &types.Snapshot{DriveSlots: collectNow(t, c)}

	got := map[string]types.Severity{}
	for _, a := range alert.New().Evaluate(config.Defaults().Alerts, snap, time.Now()) {
		if a.Rule == alert.RuleDriveSlotFault {
			got[a.Resource] = a.Severity
		}
	}
	want := map[string]types.Severity{
		dellemcDrives + "Disk.Bay.2:Enclosure.Internal.0-1:RAID.Integrated.1-1": types.SeverityWarning,
		dellemcDrives + "Disk.Bay.3:Enclosure.Internal.0-1:RAID.Integrated.1-1": types.SeverityCritical,
	}
	if len(got) != len(want) {
		t.Fatalf("drive_slot_fault fired for %v, want %v", got, want)
	}
	for id, severity := range want {
		if got[id] != severity {
			t.Errorf("drive_slot_fault for %s = %q, want %q", id, got[id], severity)
		}
	}
}

// TestDriveSlotCollector_ScanningDisabledSensorRaisesNoAlert verifies a drive
// slot sensor whose scanning is disabled is reported unknown. Its state bits
// may be stale, so the fault they claim must not raise drive_slot_fault.
func TestDriveSlotCollector_ScanningDisabledSensorRaisesNoAlert(t *testing.T) {
	dev := loadIPMIFixture(t, "ipmi-dellemc02.txt")
	// Bay 3: event messages on, scanning off (flags 0x80), present and faulted.
	dev.readings[0x40] = []byte{0x00, 0x00, 0x80, 0x03, 0x00}
	c, _ := newTestDriveSlots(t, DriveSlotConfig{}, func() (ipmiTransport, error) { return dev, nil })
	slots := c.poll(context.Background())

	bay := slotsByID(slots.Slots)["Bay 3"]
	if bay.State != "sensor scanning disabled" || bay.Health != types.DriveSlotHealthUnknown || bay.Fault || bay.Present {
		t.Errorf("Bay 3 = %+v, want an unknown slot with scanning disabled", bay)
	}
	snap := &types.Snapshot{DriveSlots: slots}
	for _, a := range alert.New().Evaluate(config.Defaults().Alerts, snap, time.Now()) {
		if a.Rule == alert.RuleDriveSlotFault {
			t.Errorf("drive_slot_fault fired for %s from a sensor that is not scanning", a.Resource)
		}
	}
}

func TestDriveSlotCollector_FallsBackToIPMIWhenRedfishFails(t *testing.T) {
	f := newFakeBMC(t, "redfish-dellemc02.json")
	cfg := f.config(t)
	cfg.PasswordFile = writePasswordFile(t, "stale-password", 0o600)
	dev := loadIPMIFixture(t, "ipmi-dellemc02.txt")
	c, logs := newTestDriveSlots(t, cfg, func() (ipmiTransport, error) { return dev, nil })

	got := collectNow(t, c)
	if !got.Available || got.Source != types.DriveSlotSourceIPMI || len(got.Slots) != 4 {
		t.Fatalf("Info() = %+v, want four bays from IPMI", got)
	}
	if !strings.Contains(got.Reason, "HTTP 401") || !strings.HasSuffix(got.Reason, "read through IPMI instead") {
		t.Errorf("Reason = %q, want the Redfish failure and the fallback", got.Reason)
	}
	if got.Slots[0].ID != "Bay 3" || !got.Slots[0].Fault {
		t.Errorf("first slot = %+v, want the faulted Bay 3", got.Slots[0])
	}
	if !dev.closed {
		t.Error("the IPMI device was left open after the poll")
	}
	for _, secret := range []string{"stale-password", fakeBMCUser} {
		if strings.Contains(logs.String(), secret) || strings.Contains(got.Reason, secret) {
			t.Errorf("the log or reason carries %q", secret)
		}
	}
	if !strings.Contains(logs.String(), "drive-slot") {
		t.Errorf("the fallback was not logged: %q", logs)
	}
}

func TestDriveSlotCollector_UnavailableReasons(t *testing.T) {
	permission := &IPMIDeviceError{Path: "/dev/ipmi0", Err: fs.ErrPermission}
	missing := &IPMIDeviceError{Path: "/dev/ipmi0", Err: fs.ErrNotExist}
	cases := map[string]struct {
		redfish bool
		devErr  error
		want    []string
	}{
		"redfish fails and there is no device": {true, missing, []string{"redfish: GET /redfish/v1/Systems: HTTP 500"}},
		"both sources fail":                    {true, permission, []string{"redfish: GET", "; ipmi: no access to /dev/ipmi0"}},
		"only IPMI, and it fails":              {false, permission, []string{"ipmi: no access to /dev/ipmi0", "docs/collector/drive-slots.md"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var cfg DriveSlotConfig
			if tc.redfish {
				f := newFakeBMC(t, "")
				f.handlers["/redfish/v1/Systems"] = func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
				}
				cfg = f.config(t)
			}
			cfg.IPMIDevice = "/dev/ipmi0"
			c, _ := newTestDriveSlots(t, cfg, func() (ipmiTransport, error) { return nil, tc.devErr })
			got := c.poll(context.Background())
			if got.Available {
				t.Fatalf("poll() = %+v, want unavailable", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got.Reason, w) {
					t.Errorf("Reason = %q, want it to contain %q", got.Reason, w)
				}
			}
		})
	}
}

func TestDriveSlotCollector_IPMIReadFailures(t *testing.T) {
	closeFails := loadIPMIFixture(t, "ipmi-dellemc02.txt")
	closeFails.closeErr = errors.New("bad file descriptor")
	c, _ := newTestDriveSlots(t, DriveSlotConfig{}, func() (ipmiTransport, error) { return closeFails, nil })
	got := c.poll(context.Background())
	if got.Available || !strings.Contains(got.Reason, "bad file descriptor") {
		t.Errorf("poll() with a failing Close = %+v, want the close failure", got)
	}

	trunc := loadIPMIFixture(t, "ipmi-truncated.txt")
	c, _ = newTestDriveSlots(t, DriveSlotConfig{}, func() (ipmiTransport, error) { return trunc, nil })
	got = c.poll(context.Background())
	if got.Available || !strings.HasPrefix(got.Reason, "ipmi: ") || !strings.Contains(got.Reason, "truncated") || !trunc.closed {
		t.Errorf("poll() of a truncated repository = %+v, want an IPMI failure and the device closed", got)
	}
}

func TestDriveSlotCollector_CollectIsThrottled(t *testing.T) {
	c, _ := newTestDriveSlots(t, DriveSlotConfig{}, nil)
	collectNow(t, c)
	first := c.query.Load()
	if err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	if c.query.Load() != first {
		t.Error("a second Collect within the interval started another poll")
	}
}

// TestDriveSlotTier_PollIsNeverStarved walks the snapshot ticks at every
// interval the UI offers. At each tick the tier lets through, it applies
// Collect's own throttle, then checks that no poll comes later than one
// driveSlotInterval plus one tick after the last one. In a slower tier, at the
// 60-second interval, the collector was asked only every 30 minutes.
func TestDriveSlotTier_PollIsNeverStarved(t *testing.T) {
	const span = 2 * time.Hour
	for _, interval := range []time.Duration{
		500 * time.Millisecond, time.Second, 5 * time.Second, 10 * time.Second,
		15 * time.Second, 30 * time.Second, time.Minute,
	} {
		lastPoll := time.Duration(-1)
		for tick := uint64(0); time.Duration(tick)*interval <= span; tick++ {
			if !shouldCollect(tick, driveSlotTier) {
				continue
			}
			now := time.Duration(tick) * interval
			if lastPoll >= 0 && now-lastPoll < driveSlotInterval {
				continue
			}
			if lastPoll >= 0 && now-lastPoll > driveSlotInterval+interval {
				t.Fatalf("at a %v interval the poll after %v came at %v, more than %v plus one tick later",
					interval, lastPoll, now, driveSlotInterval)
			}
			lastPoll = now
		}
		if lastPoll < span-driveSlotInterval-interval {
			t.Fatalf("at a %v interval the last poll was at %v of %v", interval, lastPoll, span)
		}
	}
}

func TestDriveSlotCollector_WaitHonoursTheContext(t *testing.T) {
	c, _ := newTestDriveSlots(t, DriveSlotConfig{}, nil)
	if err := c.Wait(context.Background()); err != nil {
		t.Errorf("Wait() before any poll = %v, want nil", err)
	}

	release := make(chan struct{})
	c.openIPMI = func(string) (ipmiTransport, error) {
		<-release
		return nil, &IPMIDeviceError{Path: "x", Err: fs.ErrNotExist}
	}
	if err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	if c.Collect() != nil || !c.inFlight.Load() {
		t.Error("a poll in flight was not reported in flight")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.Wait(ctx)
	var ce *types.CollectorError
	if !errors.As(err, &ce) || ce.Collector != "drive-slot" {
		t.Errorf("Wait(cancelled) error = %v, want a drive-slot CollectorError", err)
	}
	close(release)
	if err := c.Wait(context.Background()); err != nil {
		t.Errorf("Wait() after release error = %v", err)
	}
}

func TestDriveSlotCollector_LogsAFailureOnce(t *testing.T) {
	c, logs := newTestDriveSlots(t, DriveSlotConfig{}, nil)
	c.store(types.DriveSlots{Reason: "ipmi: no access to /dev/ipmi0"})
	c.store(types.DriveSlots{Reason: "ipmi: no access to /dev/ipmi0"})
	if n := strings.Count(logs.String(), "no access"); n != 1 {
		t.Errorf("a persisting failure was logged %d times, want once", n)
	}
	c.store(types.DriveSlots{Available: true, Source: types.DriveSlotSourceIPMI})
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("a healthy reading was logged as a warning")
	}
}

func TestNewDriveSlotCollector_RefusesABadURL(t *testing.T) {
	_, err := NewDriveSlotCollector(nil, DriveSlotConfig{RedfishURL: "http://bmc.example", Username: "u", PasswordFile: "p"})
	var cfgErr *RedfishConfigError
	if !errors.As(err, &cfgErr) {
		t.Errorf("NewDriveSlotCollector(http) error = %v, want RedfishConfigError", err)
	}
	c, err := NewDriveSlotCollector(nil, DriveSlotConfig{})
	if err != nil || c.ipmiDevice != DefaultIPMIDevice || c.redfish != nil {
		t.Errorf("NewDriveSlotCollector(zero) = %+v, %v, want IPMI only at %s", c, err, DefaultIPMIDevice)
	}
}

func TestSystemCollector_ConfigureDriveSlots(t *testing.T) {
	sc := NewSystemCollector(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	before := sc.driveSlot.Load()
	if before == nil || before.redfish != nil || before.ipmiDevice != DefaultIPMIDevice {
		t.Fatalf("default drive-slot collector = %+v, want IPMI only", before)
	}
	if err := sc.ConfigureDriveSlots(DriveSlotConfig{RedfishURL: "https://root:calvin@bmc"}); err == nil {
		t.Error("ConfigureDriveSlots accepted a URL carrying credentials")
	}
	if sc.driveSlot.Load() != before {
		t.Error("a refused configuration replaced the collector")
	}

	f := newFakeBMC(t, "redfish-dellemc02.json")
	if err := sc.ConfigureDriveSlots(f.config(t)); err != nil {
		t.Fatalf("ConfigureDriveSlots() error = %v", err)
	}
	if err := sc.driveSlot.Load().Collect(); err != nil {
		t.Fatal(err)
	}
	if err := sc.WaitDriveSlots(context.Background()); err != nil {
		t.Fatalf("WaitDriveSlots() error = %v", err)
	}
	if got := sc.driveSlot.Load().Info(); got.Source != types.DriveSlotSourceRedfish || len(got.Slots) != 4 {
		t.Errorf("Info() after configuring = %+v, want four bays from Redfish", got)
	}
}

func TestDriveSlotErrors_SayWhatFailed(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"redfish empty": {&NoDriveSlotsError{Source: types.DriveSlotSourceRedfish}, "Redfish service lists no drives"},
		"ipmi empty":    {&NoDriveSlotsError{Source: types.DriveSlotSourceIPMI}, "no drive-slot sensors"},
		"device":        {&IPMIDeviceError{Path: "/dev/ipmi0", Err: fs.ErrClosed}, "device /dev/ipmi0: file already closed"},
	}
	for name, tc := range cases {
		if !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("%s: %q does not contain %q", name, tc.err, tc.want)
		}
	}
}
