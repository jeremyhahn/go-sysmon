package collector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/outbound"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

const (
	// DefaultIPMIDevice is the kernel IPMI device the in-band fallback reads.
	DefaultIPMIDevice = "/dev/ipmi0"

	// DefaultBMCTimeout bounds one Redfish request when the configuration
	// does not.
	DefaultBMCTimeout = 10 * time.Second

	// driveSlotInterval is how often the BMC is asked. A bay's state changes
	// on the scale of minutes, a Redfish walk costs a request per drive, and
	// iDRAC answers each in hundreds of milliseconds, so the snapshot cadence
	// would be the wrong one.
	driveSlotInterval = time.Minute

	// driveSlotPollTimeout bounds a whole poll, every request of the walk.
	driveSlotPollTimeout = 2 * time.Minute

	// driveSlotUnconfigured is the Reason on a host with neither source. It
	// is a fact about the host, not a failure, and is never logged.
	driveSlotUnconfigured = "no BMC configured (bmc.redfish_url) and no IPMI device at "
)

// DriveSlotConfig is how the drive-slot collector reaches the BMC. The zero
// value reads IPMI only, from DefaultIPMIDevice.
type DriveSlotConfig struct {
	// RedfishURL is the BMC's base address, https://host[:port]. Empty turns
	// Redfish off.
	RedfishURL string
	// Username is the Redfish account, which needs only read access.
	Username string
	// PasswordFile holds the account's password. The password itself is
	// never held in configuration: the file is read for each poll, and the
	// copy is cleared when the poll's requests are built.
	PasswordFile string
	// TLS verifies the BMC's certificate: CAFile pins the authority for a
	// self-signed BMC, and InsecureSkipVerify is the explicit opt-out.
	TLS outbound.TLS
	// Timeout bounds one Redfish request; zero is DefaultBMCTimeout.
	Timeout time.Duration
	// IPMIDevice is the kernel IPMI device; empty is DefaultIPMIDevice.
	IPMIDevice string
}

// driveSlotQuery is one background poll. done closes when it concludes.
type driveSlotQuery struct {
	done chan struct{}
}

// DriveSlotCollector reads per-bay drive health from the node's BMC: Redfish
// when one is configured, and the in-band IPMI drive-slot sensors otherwise
// or when Redfish fails (#289).
//
// Only reads reach the BMC. The Redfish client issues GETs alone, and the
// IPMI client refuses every command but the three it reads with.
type DriveSlotCollector struct {
	logger     *slog.Logger
	redfish    *redfishClient
	ipmiDevice string
	// openIPMI opens the IPMI device. It is a seam so tests can serve
	// recorded SDR and reading bytes in place of a real BMC.
	openIPMI func(path string) (ipmiTransport, error)

	info      atomic.Pointer[types.DriveSlots]
	inFlight  atomic.Bool
	lastStart atomic.Int64
	query     atomic.Pointer[driveSlotQuery]
}

// NewDriveSlotCollector builds the collector. It returns an error only for a
// configuration that can never work: an unparsable Redfish URL or TLS
// material that does not load.
func NewDriveSlotCollector(logger *slog.Logger, cfg DriveSlotConfig) (*DriveSlotCollector, error) {
	device := cfg.IPMIDevice
	if device == "" {
		device = DefaultIPMIDevice
	}
	c := newDriveSlotCollector(logger, device)
	if cfg.RedfishURL != "" {
		rc, err := newRedfishClient(cfg)
		if err != nil {
			return nil, err
		}
		c.redfish = rc
	}
	return c, nil
}

// newDriveSlotCollector builds a collector that reads IPMI from device alone.
func newDriveSlotCollector(logger *slog.Logger, device string) *DriveSlotCollector {
	if logger == nil {
		logger = slog.Default()
	}
	c := &DriveSlotCollector{
		logger:     logger,
		ipmiDevice: device,
		openIPMI:   openIPMIDevice,
	}
	c.info.Store(&types.DriveSlots{Reason: "the first reading has not been taken yet"})
	return c
}

// Collect starts a background poll when one is due and none is running, and
// returns at once: a slow or unreachable BMC never holds up a snapshot.
func (c *DriveSlotCollector) Collect() error {
	now := time.Now()
	last := c.lastStart.Load()
	if last != 0 && now.Sub(time.UnixMilli(last)) < driveSlotInterval {
		return nil
	}
	if !c.inFlight.CompareAndSwap(false, true) {
		return nil
	}
	c.lastStart.Store(now.UnixMilli())

	q := &driveSlotQuery{done: make(chan struct{})}
	c.query.Store(q)
	go func() {
		defer close(q.done)
		defer c.inFlight.Store(false)

		ctx, cancel := context.WithTimeout(context.Background(), driveSlotPollTimeout)
		defer cancel()
		c.store(c.poll(ctx))
	}()
	return nil
}

// Wait blocks until the most recently started poll concludes or ctx ends.
// The one-shot CLI uses it so its single snapshot carries a reading.
func (c *DriveSlotCollector) Wait(ctx context.Context) error {
	q := c.query.Load()
	if q == nil {
		return nil
	}
	select {
	case <-q.done:
		return nil
	case <-ctx.Done():
		return &types.CollectorError{Collector: "drive-slot", Cause: ctx.Err()}
	}
}

// Info returns the latest reading.
func (c *DriveSlotCollector) Info() types.DriveSlots {
	return *c.info.Load()
}

// store publishes a reading, and logs a failure once when it first appears
// rather than every minute it persists.
func (c *DriveSlotCollector) store(next types.DriveSlots) {
	prev := c.info.Swap(&next)
	if next.Reason == "" || next.Reason == prev.Reason || strings.HasPrefix(next.Reason, driveSlotUnconfigured) {
		return
	}
	c.logger.Warn("drive-slot: BMC reading incomplete", "source", string(next.Source), "reason", next.Reason)
}

// poll reads every bay from the best source that answers.
func (c *DriveSlotCollector) poll(ctx context.Context) types.DriveSlots {
	var redfishErr error
	if c.redfish != nil {
		slots, err := c.redfish.driveSlots(ctx)
		if err == nil {
			return types.DriveSlots{Available: true, Source: types.DriveSlotSourceRedfish, Slots: sortSlots(slots)}
		}
		redfishErr = err
	}

	slots, ipmiErr := c.readIPMI(ctx)
	if ipmiErr == nil {
		out := types.DriveSlots{Available: true, Source: types.DriveSlotSourceIPMI, Slots: sortSlots(slots)}
		if redfishErr != nil {
			out.Reason = "redfish: " + redfishErr.Error() + "; read through IPMI instead"
		}
		return out
	}

	missing := errors.Is(ipmiErr, fs.ErrNotExist)
	switch {
	case redfishErr == nil && missing:
		return types.DriveSlots{Reason: driveSlotUnconfigured + c.ipmiDevice}
	case missing:
		return types.DriveSlots{Reason: "redfish: " + redfishErr.Error()}
	case redfishErr != nil:
		return types.DriveSlots{Reason: "redfish: " + redfishErr.Error() + "; ipmi: " + ipmiErr.Error()}
	default:
		return types.DriveSlots{Reason: "ipmi: " + ipmiErr.Error()}
	}
}

// readIPMI opens the device for one poll and reads the drive-slot sensors.
func (c *DriveSlotCollector) readIPMI(ctx context.Context) (slots []types.DriveSlot, err error) {
	t, err := c.openIPMI(c.ipmiDevice)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := t.Close(); closeErr != nil && err == nil {
			err = &IPMIDeviceError{Path: c.ipmiDevice, Err: closeErr}
		}
	}()
	return (&ipmiClient{t: t}).driveSlots(ctx)
}

// sortSlots orders bays by ID, so the wire, the table and the metrics list
// them the same way on every poll.
func sortSlots(slots []types.DriveSlot) []types.DriveSlot {
	sort.Slice(slots, func(i, j int) bool { return slots[i].ID < slots[j].ID })
	return slots
}

// NoDriveSlotsError reports a source that answered with no drive bays.
type NoDriveSlotsError struct {
	Source types.DriveSlotSource
}

func (e *NoDriveSlotsError) Error() string {
	if e.Source == types.DriveSlotSourceIPMI {
		return "the BMC's sensor repository has no drive-slot sensors (sensor type 0x0D)"
	}
	return "the BMC's Redfish service lists no drives"
}

// IPMIDeviceError reports the IPMI device failing to open or close.
type IPMIDeviceError struct {
	Path string
	Err  error
}

func (e *IPMIDeviceError) Error() string {
	if errors.Is(e.Err, fs.ErrPermission) {
		return fmt.Sprintf("no access to %s: the service needs read access to the device (see docs/collector/drive-slots.md)", e.Path)
	}
	return fmt.Sprintf("device %s: %v", e.Path, e.Err)
}

func (e *IPMIDeviceError) Unwrap() error { return e.Err }
