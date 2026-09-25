package collector

import (
	"bufio"
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// raplSnapshot stores a single RAPL energy reading for delta calculation.
type raplSnapshot struct {
	EnergyUJ uint64
	Time     time.Time
}

// SensorCollector reads system sensor data from sysfs and procfs.
type SensorCollector struct {
	sensors atomic.Pointer[types.SensorData]
	logger  *slog.Logger

	// Configurable base paths for testability.
	hwmonBase    string
	thermalBase  string
	powercapBase string
	cpuBase      string
	pressureBase string

	// RAPL energy tracking for power delta calculation.
	prevEnergy atomic.Pointer[map[string]raplSnapshot]
}

// NewSensorCollector creates a SensorCollector with default sysfs paths.
func NewSensorCollector(logger *slog.Logger) *SensorCollector {
	if logger == nil {
		logger = slog.Default()
	}
	empty := types.SensorData{}
	c := &SensorCollector{
		logger:       logger,
		hwmonBase:    "/sys/class/hwmon",
		thermalBase:  "/sys/class/thermal",
		powercapBase: "/sys/class/powercap",
		cpuBase:      "/sys/devices/system/cpu",
		pressureBase: "/proc/pressure",
	}
	c.sensors.Store(&empty)
	return c
}

// Collect reads all sensor data from sysfs and procfs.
func (c *SensorCollector) Collect() error {
	data := types.SensorData{}
	data.CoreTemps = c.collectCoreTemps()
	data.CoreVoltages = c.collectCoreVoltages()
	data.PackagePower = c.collectPackagePower()
	data.ThermalThrottle = c.collectThrottleInfo()
	data.ThermalZones = c.collectThermalZones()
	data.Fans = c.collectFans()
	data.PSI = c.collectPSI()
	c.sensors.Store(&data)
	return nil
}

// collectPackagePower scans the powercap sysfs tree for intel-rapl package
// zones and returns instantaneous power readings computed from energy deltas.
func (c *SensorCollector) collectPackagePower() []types.PackagePower {
	entries, err := os.ReadDir(c.powercapBase)
	if err != nil {
		c.logger.Debug("sensor: cannot read powercap base", "path", c.powercapBase, "error", err)
		return nil
	}

	now := time.Now()

	// Load the previous snapshot map (may be nil on first call).
	prevMap := c.prevEnergy.Load()
	nextMap := make(map[string]raplSnapshot, len(entries))

	var result []types.PackagePower

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "intel-rapl:") {
			continue
		}

		zonePath := filepath.Join(c.powercapBase, entry.Name())
		name := readSysfsString(filepath.Join(zonePath, "name"))

		// Only process top-level package zones; skip sub-zones like core/uncore/dram.
		if !strings.HasPrefix(name, "package-") {
			continue
		}

		energyUJ := readSysfsUint64(filepath.Join(zonePath, "energy_uj"))
		maxPowerUW := readSysfsUint64(filepath.Join(zonePath, "constraint_0_max_power_uw"))

		nextMap[name] = raplSnapshot{EnergyUJ: energyUJ, Time: now}

		var powerW float64
		if prevMap != nil {
			if prev, ok := (*prevMap)[name]; ok {
				deltaTime := now.Sub(prev.Time).Seconds()
				if deltaTime > 0 {
					var deltaEnergy uint64
					if energyUJ >= prev.EnergyUJ {
						deltaEnergy = energyUJ - prev.EnergyUJ
					} else {
						// Counter wrapped; treat current value as the full delta.
						deltaEnergy = energyUJ
					}
					powerW = float64(deltaEnergy) / 1_000_000.0 / deltaTime
				}
			}
		}

		result = append(result, types.PackagePower{
			PackageName:  name,
			PowerW:       powerW,
			MaxPowerW:    float64(maxPowerUW) / 1_000_000.0,
			EnergyJoules: float64(energyUJ) / 1_000_000.0,
		})
	}

	c.prevEnergy.Store(&nextMap)
	return result
}

// Info returns the most recently collected sensor data.
func (c *SensorCollector) Info() types.SensorData {
	return *c.sensors.Load()
}

// collectCoreTemps scans hwmon devices and returns per-core temperature
// readings for coretemp (Intel) and k10temp (AMD) drivers.
// Results are sorted by PackageID then CoreID.
func (c *SensorCollector) collectCoreTemps() []types.CoreTemp {
	entries, err := os.ReadDir(c.hwmonBase)
	if err != nil {
		c.logger.Debug("sensor: cannot read hwmon base", "path", c.hwmonBase, "error", err)
		return nil
	}

	var temps []types.CoreTemp
	var amd []string

	for _, entry := range entries {
		hwmonPath := filepath.Join(c.hwmonBase, entry.Name())
		name := readSysfsString(filepath.Join(hwmonPath, "name"))

		switch name {
		case "coretemp":
			temps = append(temps, c.collectIntelCoreTemps(hwmonPath)...)
		case "k10temp":
			amd = append(amd, hwmonPath)
		default:
			c.logger.Debug("sensor: skipping hwmon device", "name", name, "path", hwmonPath)
		}
	}
	for packageID, hwmonPath := range socketOrder(amd) {
		temps = append(temps, c.collectAMDCoreTemps(hwmonPath, packageID)...)
	}

	sort.Slice(temps, func(i, j int) bool {
		if temps[i].PackageID != temps[j].PackageID {
			return temps[i].PackageID < temps[j].PackageID
		}
		return temps[i].CoreID < temps[j].CoreID
	})

	return temps
}

// collectIntelCoreTemps reads per-core temperatures from a coretemp hwmon
// device. It performs a first pass to find the package ID from a "Package id X"
// label, then a second pass to collect individual core readings.
func (c *SensorCollector) collectIntelCoreTemps(hwmonPath string) []types.CoreTemp {

	indices := hwmonSensorIndices(hwmonPath, "temp")

	// First pass: find the package ID from a "Package id X" label.
	packageID := 0
	for _, n := range indices {
		label := readSysfsString(filepath.Join(hwmonPath, tempFile(n, "label")))
		if strings.HasPrefix(label, "Package id ") {
			idStr := strings.TrimPrefix(label, "Package id ")
			if id, err := strconv.Atoi(strings.TrimSpace(idStr)); err == nil {
				packageID = id
			}
			break
		}
	}

	// Second pass: collect per-core readings.
	var temps []types.CoreTemp
	for _, n := range indices {
		inputPath := filepath.Join(hwmonPath, tempFile(n, "input"))
		rawInput := readSysfsUint64(inputPath)

		label := readSysfsString(filepath.Join(hwmonPath, tempFile(n, "label")))

		// Skip package-level entries.
		if strings.HasPrefix(label, "Package id ") {
			continue
		}

		if !strings.HasPrefix(label, "Core ") {
			c.logger.Debug("sensor: coretemp: skipping non-core label", "label", label)
			continue
		}

		coreIDStr := strings.TrimPrefix(label, "Core ")
		coreID, err := strconv.Atoi(strings.TrimSpace(coreIDStr))
		if err != nil {
			c.logger.Debug("sensor: coretemp: cannot parse core id", "label", label)
			continue
		}

		rawHigh := readSysfsUint64(filepath.Join(hwmonPath, tempFile(n, "max")))
		rawCrit := readSysfsUint64(filepath.Join(hwmonPath, tempFile(n, "crit")))

		temps = append(temps, types.CoreTemp{
			PackageID:   packageID,
			CoreID:      coreID,
			Label:       label,
			TempCelsius: float64(rawInput) / 1000.0,
			HighCelsius: float64(rawHigh) / 1000.0,
			CritCelsius: float64(rawCrit) / 1000.0,
		})
	}

	return temps
}

// socketOrder returns k10temp hwmon devices in socket order.
//
// k10temp reports no package id: unlike coretemp, it has no "Package id N"
// label, and every socket's CCDs are numbered from Tccd1. It binds one device
// per socket, to that socket's data-fabric function (0000:00:18.3 for the
// first, 0000:00:19.3 for the second), and /sys/class/hwmon/hwmonN links to
// the device's directory under it. So the devices' resolved paths, sorted,
// are in socket order, and the position in that order is the package. Without
// it both sockets' CCD 1 was one sensor, and their two readings one series.
func socketOrder(hwmonPaths []string) []string {
	type device struct{ hwmon, resolved string }
	devices := make([]device, 0, len(hwmonPaths))
	for _, p := range hwmonPaths {
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			// The device went away between listing and resolving it: it has
			// no socket, and nothing left to read.
			continue
		}
		devices = append(devices, device{hwmon: p, resolved: resolved})
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].resolved < devices[j].resolved })
	ordered := make([]string, len(devices))
	for i, d := range devices {
		ordered[i] = d.hwmon
	}
	return ordered
}

// collectAMDCoreTemps reads CCD temperatures from a k10temp hwmon device, the
// packageID'th socket. Tctl is the package-level die control temperature and
// is skipped. Tccd{N} labels represent per-CCD temperatures and are returned
// as CoreTemp entries with CoreID equal to N.
func (c *SensorCollector) collectAMDCoreTemps(hwmonPath string, packageID int) []types.CoreTemp {
	var temps []types.CoreTemp

	for _, n := range hwmonSensorIndices(hwmonPath, "temp") {
		inputPath := filepath.Join(hwmonPath, tempFile(n, "input"))
		rawInput := readSysfsUint64(inputPath)

		label := readSysfsString(filepath.Join(hwmonPath, tempFile(n, "label")))

		// Tctl is the package die control temp — skip it.
		if label == "Tctl" || label == "Tdie" {
			continue
		}

		if !strings.HasPrefix(label, "Tccd") {
			c.logger.Debug("sensor: k10temp: skipping non-CCD label", "label", label)
			continue
		}

		ccdIDStr := strings.TrimPrefix(label, "Tccd")
		ccdID, err := strconv.Atoi(strings.TrimSpace(ccdIDStr))
		if err != nil {
			c.logger.Debug("sensor: k10temp: cannot parse CCD id", "label", label)
			continue
		}

		rawHigh := readSysfsUint64(filepath.Join(hwmonPath, tempFile(n, "max")))
		rawCrit := readSysfsUint64(filepath.Join(hwmonPath, tempFile(n, "crit")))

		temps = append(temps, types.CoreTemp{
			PackageID:   packageID,
			CoreID:      ccdID,
			Label:       label,
			TempCelsius: float64(rawInput) / 1000.0,
			HighCelsius: float64(rawHigh) / 1000.0,
			CritCelsius: float64(rawCrit) / 1000.0,
		})
	}

	return temps
}

// tempFile returns the sysfs filename for the given sensor index and attribute,
// e.g. tempFile(3, "input") → "temp3_input".
func tempFile(n int, attr string) string {
	return "temp" + strconv.Itoa(n) + "_" + attr
}

// hwmonSensorIndices returns the sensor indices present under hwmonPath for the
// given file prefix ("temp", "fan", "in"), sorted ascending.
//
// Indices must be discovered rather than assumed contiguous: the coretemp
// driver numbers its sensors from the physical core ID, so a 24-core CPU whose
// cores are numbered 0, 4, 8, 9 … yields temp2, temp6, temp10, temp11 … with
// gaps. Walking n = 1, 2, 3 … and stopping at the first missing file finds only
// the first core.
func hwmonSensorIndices(hwmonPath, prefix string) []int {
	matches, err := filepath.Glob(filepath.Join(hwmonPath, prefix+"*_input"))
	if err != nil {
		return nil
	}

	indices := make([]int, 0, len(matches))
	for _, m := range matches {
		base := filepath.Base(m)
		digits := strings.TrimSuffix(strings.TrimPrefix(base, prefix), "_input")
		n, convErr := strconv.Atoi(digits)
		if convErr != nil {
			continue
		}
		indices = append(indices, n)
	}
	sort.Ints(indices)
	return indices
}

// voltageFile returns the sysfs filename for the given voltage channel and
// attribute, e.g. voltageFile(0, "input") → "in0_input".
func voltageFile(n int, attr string) string {
	return "in" + strconv.Itoa(n) + "_" + attr
}

// collectCoreVoltages scans all hwmon devices and returns voltage readings
// from in{N}_input files. Results are sorted by HwmonName then Channel.
func (c *SensorCollector) collectCoreVoltages() []types.CoreVoltage {
	entries, err := os.ReadDir(c.hwmonBase)
	if err != nil {
		c.logger.Debug("sensor: cannot read hwmon base for voltages", "path", c.hwmonBase, "error", err)
		return nil
	}

	const maxVoltageSensors = 128

	var voltages []types.CoreVoltage

	for _, entry := range entries {
		hwmonPath := filepath.Join(c.hwmonBase, entry.Name())
		hwmonName := readSysfsString(filepath.Join(hwmonPath, "name"))

		for n := 0; n < maxVoltageSensors; n++ {
			inputPath := filepath.Join(hwmonPath, voltageFile(n, "input"))
			rawInput := readSysfsString(inputPath)
			if rawInput == "" {
				// No sensor at this index; channels are not necessarily contiguous,
				// but we stop after the first gap to avoid scanning the full range.
				if n > 0 {
					break
				}
				continue
			}

			millivolts := readSysfsUint64(inputPath)

			label := readSysfsString(filepath.Join(hwmonPath, voltageFile(n, "label")))
			if label == "" {
				label = "in" + strconv.Itoa(n)
			}

			voltages = append(voltages, types.CoreVoltage{
				Channel:   n,
				Label:     label,
				VoltageV:  float64(millivolts) / 1000.0,
				HwmonName: hwmonName,
			})
		}
	}

	sort.Slice(voltages, func(i, j int) bool {
		if voltages[i].HwmonName != voltages[j].HwmonName {
			return voltages[i].HwmonName < voltages[j].HwmonName
		}
		return voltages[i].Channel < voltages[j].Channel
	})

	return voltages
}

// collectThrottleInfo scans cpu{N}/thermal_throttle directories under cpuBase
// and returns per-CPU thermal throttle event counts, sorted by CPU number.
func (c *SensorCollector) collectThrottleInfo() []types.ThrottleInfo {
	entries, err := os.ReadDir(c.cpuBase)
	if err != nil {
		c.logger.Debug("sensor: cannot read cpu base", "path", c.cpuBase, "error", err)
		return nil
	}

	var result []types.ThrottleInfo

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "cpu") {
			continue
		}
		// Require cpu{N} where N is a non-negative integer.
		numStr := strings.TrimPrefix(name, "cpu")
		cpuNum, parseErr := strconv.Atoi(numStr)
		if parseErr != nil {
			continue
		}

		throttleDir := filepath.Join(c.cpuBase, name, "thermal_throttle")
		if _, statErr := os.Stat(throttleDir); statErr != nil {
			// thermal_throttle directory absent for this CPU; skip silently.
			continue
		}

		core := readSysfsUint64(filepath.Join(throttleDir, "core_throttle_count"))
		pkg := readSysfsUint64(filepath.Join(throttleDir, "package_throttle_count"))

		result = append(result, types.ThrottleInfo{
			CPU:                  cpuNum,
			CoreThrottleCount:    core,
			PackageThrottleCount: pkg,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].CPU < result[j].CPU
	})

	return result
}

// collectThermalZones scans thermal_zone{N} directories under thermalBase and
// returns the type, temperature (in Celsius) and policy for each zone, sorted
// by zone name.
func (c *SensorCollector) collectThermalZones() []types.ThermalZone {
	entries, err := os.ReadDir(c.thermalBase)
	if err != nil {
		c.logger.Debug("sensor: cannot read thermal base", "path", c.thermalBase, "error", err)
		return nil
	}

	var result []types.ThermalZone

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "thermal_zone") {
			continue
		}

		zonePath := filepath.Join(c.thermalBase, name)
		zoneType := readSysfsString(filepath.Join(zonePath, "type"))
		tempRaw := readSysfsUint64(filepath.Join(zonePath, "temp"))
		policy := readSysfsString(filepath.Join(zonePath, "policy"))

		result = append(result, types.ThermalZone{
			Name:        name,
			Type:        zoneType,
			TempCelsius: float64(tempRaw) / 1000.0,
			Policy:      policy,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result
}

// fanFile returns the sysfs filename for the given fan index and attribute,
// e.g. fanFile(1, "input") -> "fan1_input".
func fanFile(n int, attr string) string {
	return "fan" + strconv.Itoa(n) + "_" + attr
}

// collectFans scans hwmon devices and returns fan speed readings from
// fan{N}_input files, sorted by HwmonName then label.
func (c *SensorCollector) collectFans() []types.FanInfo {
	entries, err := os.ReadDir(c.hwmonBase)
	if err != nil {
		c.logger.Debug("sensor: cannot read hwmon base for fans", "path", c.hwmonBase, "error", err)
		return nil
	}

	var fans []types.FanInfo

	for _, entry := range entries {
		hwmonPath := filepath.Join(c.hwmonBase, entry.Name())
		hwmonName := readSysfsString(filepath.Join(hwmonPath, "name"))

		for _, n := range hwmonSensorIndices(hwmonPath, "fan") {
			inputPath := filepath.Join(hwmonPath, fanFile(n, "input"))

			// Some drivers (acpi_fan) expose a fanN_input file that errors with
			// ENODEV on read. Reporting those as 0 RPM invents a fan that is not
			// there, so skip any input that cannot actually be read.
			if readSysfsString(inputPath) == "" {
				c.logger.Debug("sensor: fan input unreadable", "path", inputPath)
				continue
			}

			rpm := readSysfsUint64(inputPath)

			label := readSysfsString(filepath.Join(hwmonPath, fanFile(n, "label")))
			if label == "" {
				label = "fan" + strconv.Itoa(n)
			}

			minRPM := readSysfsUint64(filepath.Join(hwmonPath, fanFile(n, "min")))
			maxRPM := readSysfsUint64(filepath.Join(hwmonPath, fanFile(n, "max")))

			fans = append(fans, types.FanInfo{
				Label:     label,
				RPM:       rpm,
				MinRPM:    minRPM,
				MaxRPM:    maxRPM,
				HwmonName: hwmonName,
			})
		}
	}

	sort.Slice(fans, func(i, j int) bool {
		if fans[i].HwmonName != fans[j].HwmonName {
			return fans[i].HwmonName < fans[j].HwmonName
		}
		return fans[i].Label < fans[j].Label
	})

	return fans
}

// psiHalf is one line of a PSI file: the "some" or the "full" reading.
type psiHalf struct {
	avg10, avg60, avg300 float64
	total                uint64
}

// psiKeys are the fields every PSI line carries. A line missing any of them, or
// holding one that does not parse, is not a reading: the kernel has written the
// same four fields since PSI was introduced, so anything else is a truncated or
// corrupted read, and taking the fields that did parse would present the rest
// as zero pressure.
var psiKeys = []string{"avg10", "avg60", "avg300", "total"}

// parsePSILine parses a single PSI line of the form:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=0
//
// path names the file the line came from; the error names its resource, the
// file's base name, as every PSIParseError does. Every field in psiKeys
// must be present and parse; otherwise the line yields a *types.PSIParseError
// and no reading at all, because zero is also a perfectly ordinary reading and
// the two must not be confused.
func parsePSILine(path, line string) (psiHalf, error) {
	values := make(map[string]string, len(psiKeys))
	fields := strings.Fields(line)
	if len(fields) > 0 {
		for _, field := range fields[1:] {
			if key, val, ok := strings.Cut(field, "="); ok {
				values[key] = val
			}
		}
	}
	for _, key := range psiKeys {
		if _, ok := values[key]; !ok {
			return psiHalf{}, &types.PSIParseError{Resource: filepath.Base(path), Cause: &types.ProcStatusFieldError{Path: path, Field: key}}
		}
	}

	var half psiHalf
	var err error
	if half.avg10, err = strconv.ParseFloat(values["avg10"], 64); err != nil {
		return psiHalf{}, &types.PSIParseError{Resource: filepath.Base(path), Cause: err}
	}
	if half.avg60, err = strconv.ParseFloat(values["avg60"], 64); err != nil {
		return psiHalf{}, &types.PSIParseError{Resource: filepath.Base(path), Cause: err}
	}
	if half.avg300, err = strconv.ParseFloat(values["avg300"], 64); err != nil {
		return psiHalf{}, &types.PSIParseError{Resource: filepath.Base(path), Cause: err}
	}
	if half.total, err = strconv.ParseUint(values["total"], 10, 64); err != nil {
		return psiHalf{}, &types.PSIParseError{Resource: filepath.Base(path), Cause: err}
	}
	return half, nil
}

// parsePSIFile parses a PSI resource file into both halves of a PSIResource.
// path names the file, for the error.
//
// Available is true only when the file yielded a complete reading, and on any
// error the whole resource is left at its zero value, unavailable. A "some" line
// is required of every resource. A "full" line is required too, except of cpu:
// the kernel added cpu's full line in 5.13 and older kernels omit it, which is
// normal and leaves the full fields at zero. A full line that is present but
// malformed fails the resource closed, since its zeros would read as a machine
// on which nothing ever stalls.
func parsePSIFile(path string, data []byte, fullRequired bool) (types.PSIResource, error) {
	var some, full *psiHalf
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		var dst **psiHalf
		switch {
		case strings.HasPrefix(line, "some"):
			dst = &some
		case strings.HasPrefix(line, "full"):
			dst = &full
		default:
			continue
		}
		half, err := parsePSILine(path, line)
		if err != nil {
			return types.PSIResource{}, err
		}
		*dst = &half
	}
	if err := scanner.Err(); err != nil {
		return types.PSIResource{}, &types.PSIParseError{Resource: filepath.Base(path), Cause: err}
	}
	if some == nil {
		return types.PSIResource{}, &types.PSIParseError{Resource: filepath.Base(path), Cause: &types.ProcStatusFieldError{Path: path, Field: "some"}}
	}
	if full == nil && fullRequired {
		return types.PSIResource{}, &types.PSIParseError{Resource: filepath.Base(path), Cause: &types.ProcStatusFieldError{Path: path, Field: "full"}}
	}

	res := types.PSIResource{
		Available:  true,
		SomeAvg10:  some.avg10,
		SomeAvg60:  some.avg60,
		SomeAvg300: some.avg300,
		SomeTotal:  some.total,
	}
	if full != nil {
		res.FullAvg10, res.FullAvg60, res.FullAvg300, res.FullTotal = full.avg10, full.avg60, full.avg300, full.total
	}
	return res, nil
}

// collectPSI reads /proc/pressure/{cpu,memory,io} and returns Pressure Stall
// Information.
//
// When the files are absent -- a kernel without CONFIG_PSI, or a container that
// does not expose /proc/pressure -- the readings stay at zero and the Available
// flags are false. The flags are the whole point: zero pressure and no reading
// produce identical numbers, and a consumer that cannot tell them apart treats
// the hosts it cannot see as the idlest in the fleet. The aggregate flag is set
// only when all three resources were read, so a partial reading fails closed.
func (c *SensorCollector) collectPSI() types.PSIData {
	var data types.PSIData

	read := func(resource string, fullRequired bool) types.PSIResource {
		path := c.pressureBase + "/" + resource
		b, err := os.ReadFile(path)
		if err != nil {
			c.logger.Debug("sensor: PSI file unavailable", "path", path, "error", err)
			return types.PSIResource{}
		}
		res, err := parsePSIFile(path, b, fullRequired)
		if err != nil {
			warnOnce(c.logger, "sensor:psi:"+resource, "sensor: PSI file malformed; reporting it as unavailable", "path", path, "error", err)
		}
		return res
	}

	data.CPU = read("cpu", false)
	data.Memory = read("memory", true)
	data.IO = read("io", true)

	data.Available = data.CPU.Available && data.Memory.Available && data.IO.Available

	return data
}
