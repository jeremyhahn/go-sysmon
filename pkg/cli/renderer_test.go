package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// gb converts gigabytes to bytes as uint64.
func gb(n float64) uint64 { return uint64(n * float64(1024*1024*1024)) }

// tb converts terabytes to bytes as uint64.
func tb(n float64) uint64 { return uint64(n * float64(1024*1024*1024*1024)) }

// testSnapshot builds a complete synthetic snapshot for rendering tests.
func testSnapshot() *types.Snapshot {
	return &types.Snapshot{
		Timestamp: time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC),
		Host: types.HostInfo{
			Hostname:        "godlike",
			OS:              "linux",
			Platform:        "Ubuntu",
			PlatformVersion: "24.04",
			KernelVersion:   "6.8.0-101-generic",
			KernelArch:      "x86_64",
			Uptime:          30*86400 + 5*3600 + 42*60,
			BootTime:        uint64(time.Date(2026, 2, 18, 19, 0, 0, 0, time.UTC).Unix()),
		},
		CPUSummary: types.CPUSummary{
			Sockets:        1,
			CoresPerSocket: 24,
			ThreadsPerCore: 1,
			TotalCores:     24,
			TotalThreads:   24,
			MaxMHz:         5100,
			MinMHz:         800,
		},
		CPUs: []types.CPUInfo{
			{
				Index:        0,
				ModelName:    "Intel(R) Core(TM) Ultra 9 285K",
				VendorID:     "GenuineIntel",
				CacheSize:    36864,
				Microcode:    "0x11b",
				UsagePercent: 32.1,
			},
			{
				Index:        1,
				ModelName:    "Intel(R) Core(TM) Ultra 9 285K",
				VendorID:     "GenuineIntel",
				CacheSize:    36864,
				Microcode:    "0x11b",
				UsagePercent: 18.5,
			},
		},
		Memory: types.MemoryInfo{
			TotalBytes:     gb(96),
			UsedBytes:      gb(45.2),
			AvailableBytes: gb(50.8),
			FreeBytes:      gb(12.3),
			UsedPercent:    47.1,
			BuffersBytes:   gb(1.2),
			CachedBytes:    gb(35.6),
			SharedBytes:    gb(0.5),
			SlabBytes:      gb(1.8),
			SwapTotalBytes: 0,
			SwapUsedBytes:  0,
			SwapPercent:    0,
			DIMMs: []types.DIMMInfo{
				{
					Location:           "DIMMA1",
					BankLocator:        "BANK 0",
					Manufacturer:       "G Skill Intl",
					PartNumber:         "F5-8400J4052G24G",
					SizeBytes:          gb(24),
					SpeedMTs:           4800,
					ConfiguredSpeedMTs: 4800,
					Type:               "DDR5",
					Rank:               1,
					ConfiguredVoltage:  1.1,
				},
			},
		},
		Disks: []types.DiskInfo{
			{
				Name:                  "nvme0n1",
				Model:                 "CT2000T705SSD3",
				FirmwareVersion:       "PACR5111",
				NVMeVersion:           "2.0",
				DriveType:             "SSD/NVMe",
				SizeBytes:             tb(2),
				TotalBytes:            tb(2),
				UsedBytes:             tb(2 * 0.423),
				UsedPercent:           42.3,
				SMARTEnabled:          true,
				SMARTHealthy:          true,
				Temperature:           59,
				PowerOnHours:          8065,
				PowerCycles:           154,
				WearLevelPercent:      3,
				AvailableSparePercent: 100,
				SpareThresholdPercent: 5,
				LifeRemainingPercent:  97,
				EstimatedHoursLeft:    uint64((29*365 + 270) * 24),
				DataUnitsWritten:      85000,
				DataUnitsRead:         7250,
				MediaErrors:           0,
				ErrorLogEntries:       0,
				UnsafeShutdowns:       80,
				Partitions: []types.PartitionInfo{
					{Device: "nvme0n1p1", Mountpoint: "/boot/efi", Fstype: "vfat"},
					{Device: "nvme0n1p2", Mountpoint: "/", Fstype: "ext4"},
				},
			},
		},
		Networks: []types.NetworkInfo{
			{
				Name:         "enp5s0",
				HardwareAddr: "aa:bb:cc:dd:ee:ff",
				Addresses:    []string{"192.168.1.100/24", "fe80::1/64"},
				MTU:          1500,
				Speed:        1000,
				Duplex:       "full",
				Driver:       "igc",
				IsUp:         true,
				IsLoopback:   false,
				IsVirtual:    false,
				BytesSent:    1200000000,
				BytesRecv:    5600000000,
				PacketsSent:  1234567,
				PacketsRecv:  4567890,
			},
			{
				Name:       "lo",
				MTU:        65536,
				IsUp:       true,
				IsLoopback: true,
			},
		},
		GPUs: []types.GPUInfo{
			{
				Index:          0,
				Name:           "NVIDIA RTX PRO 6000",
				DriverVersion:  "570.86.16",
				GPUUtilPercent: 42,
				MemoryTotalMiB: 24576,
				MemoryUsedMiB:  8192,
				MemoryFreeMiB:  16384,
				MemoryPercent:  33.3,
				TemperatureGPU: 55,
				PowerDrawW:     120.5,
				PowerLimitW:    350,
				ECCEnabled:     true,
				ECCSingleBit:   0,
				ECCDoubleBit:   0,
			},
			{
				Index:          1,
				Name:           "Intel Graphics",
				DriverVersion:  "i915",
				TemperatureGPU: 40,
			},
		},
		LoadAvg: types.LoadAverage{
			Load1:  0.85,
			Load5:  0.72,
			Load15: 0.65,
		},
		Processes: types.ProcessSummary{
			Total:    450,
			Running:  2,
			Sleeping: 445,
			Zombie:   0,
		},
		Sensors: types.SensorData{
			CoreTemps: []types.CoreTemp{
				{PackageID: 0, CoreID: 0, Label: "Core 0", TempCelsius: 45.0, HighCelsius: 80.0, CritCelsius: 100.0},
				{PackageID: 0, CoreID: 1, Label: "Core 1", TempCelsius: 47.0, HighCelsius: 80.0, CritCelsius: 100.0},
			},
			CoreVoltages: []types.CoreVoltage{
				{Channel: 0, Label: "Vcore", VoltageV: 1.200, HwmonName: "nct6776"},
			},
			PackagePower: []types.PackagePower{
				{PackageName: "package-0", PowerW: 45.2, MaxPowerW: 125.0},
			},
			ThermalThrottle: []types.ThrottleInfo{
				{CPU: 0, CoreThrottleCount: 0, PackageThrottleCount: 0},
			},
			ThermalZones: []types.ThermalZone{
				{Name: "thermal_zone0", Type: "x86_pkg_temp", TempCelsius: 45.0, Policy: "step_wise"},
			},
			Fans: []types.FanInfo{
				{Label: "CPU Fan", RPM: 1200, MinRPM: 0, MaxRPM: 3000, HwmonName: "nct6776"},
			},
			PSI: types.PSIData{
				CPU:    types.PSIResource{SomeAvg10: 0.10, SomeAvg60: 0.05, SomeAvg300: 0.02},
				Memory: types.PSIResource{SomeAvg10: 0.20, SomeAvg60: 0.10, SomeAvg300: 0.05, FullAvg10: 0.01, FullAvg60: 0.00, FullAvg300: 0.00},
				IO:     types.PSIResource{SomeAvg10: 1.50, SomeAvg60: 0.80, SomeAvg300: 0.30, FullAvg10: 0.50, FullAvg60: 0.20, FullAvg300: 0.10},
			},
		},
	}
}

func TestRenderOverview(t *testing.T) {
	t.Parallel()

	t.Run("renders expected sections", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		if err := RenderOverview(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()

		checks := []string{
			"godlike",
			"Ubuntu",
			"24.04",
			"6.8.0-101-generic",
			"x86_64",
			"30d 5h 42m",
			"0.85",
			"Intel(R) Core(TM) Ultra 9 285K",
			"24 Cores",
			"47.1%",
			"DDR5",
			"4800 MT/s",
			"G Skill Intl",
			"nvme0n1",
			"enp5s0",
			"2 GPUs",
			"NVIDIA RTX PRO 6000",
			"Intel Graphics",
			"ECC: on",
			"1000 Mbps",
			"450 total",
			"2 running",
			"445 sleeping",
		}
		for _, want := range checks {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q\nFull output:\n%s", want, out)
			}
		}
	})

	t.Run("renders system-level sensor summaries", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		if err := RenderOverview(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()

		checks := []string{
			"Thermal:",
			"thermal_zone0",
			"45.0°C",
			"x86_pkg_temp",
			"Fans:",
			"CPU Fan",
			"1200 RPM",
			"PSI:",
			"CPU: 0.10%",
			"Memory: 0.20%",
			"IO: 1.50%",
		}
		for _, want := range checks {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q\nFull output:\n%s", want, out)
			}
		}
	})

	t.Run("thermal and fans absent when empty", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		snap.Sensors.ThermalZones = nil
		snap.Sensors.Fans = nil
		if err := RenderOverview(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()
		if strings.Contains(out, "Thermal:") {
			t.Errorf("expected Thermal: absent when no thermal zones\nOutput:\n%s", out)
		}
		if strings.Contains(out, "Fans:") {
			t.Errorf("expected Fans: absent when no fans\nOutput:\n%s", out)
		}
		// PSI is always shown.
		if !strings.Contains(out, "PSI:") {
			t.Errorf("expected PSI: always present\nOutput:\n%s", out)
		}
	})

	t.Run("nil snapshot returns error", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		err := RenderOverview(&buf, nil)
		if err == nil {
			t.Fatal("expected error for nil snapshot")
		}
		var ce *types.CollectorError
		if !errors.As(err, &ce) {
			t.Errorf("expected CollectorError, got %T: %v", err, err)
		}
	})
}

func TestRenderHost(t *testing.T) {
	t.Parallel()

	t.Run("renders all host fields", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		if err := RenderHost(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()

		checks := []string{
			"Host Information",
			"godlike",
			"linux",
			"Ubuntu",
			"24.04",
			"6.8.0-101-generic",
			"x86_64",
			"30d 5h 42m",
			"2026-02-18 19:00:00 UTC",
		}
		for _, want := range checks {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q\nFull output:\n%s", want, out)
			}
		}
	})

	t.Run("nil snapshot returns error", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		err := RenderHost(&buf, nil)
		if err == nil {
			t.Fatal("expected error for nil snapshot")
		}
		var ce *types.CollectorError
		if !errors.As(err, &ce) {
			t.Errorf("expected CollectorError, got %T: %v", err, err)
		}
	})
}

func TestRenderCPU(t *testing.T) {
	t.Parallel()

	t.Run("renders cpu details and per-core bars", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		if err := RenderCPU(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()

		checks := []string{
			"CPU Information",
			"Intel(R) Core(TM) Ultra 9 285K",
			"GenuineIntel",
			"800 - 5100 MHz",
			"36864 KB",
			"0x11b",
			"Per-Core Usage:",
			"Core  0:",
			"32.1%",
			"18.5%",
			"Average:",
		}
		for _, want := range checks {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q\nFull output:\n%s", want, out)
			}
		}
	})

	t.Run("renders cpu sensor sections", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		if err := RenderCPU(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()

		checks := []string{
			"CPU Temperatures:",
			"Core 0",
			"45.0°C",
			"80.0°C",
			"100.0°C",
			"Core 1",
			"47.0°C",
			"CPU Voltages:",
			"Vcore",
			"1.200 V",
			"nct6776",
			"Package Power (RAPL):",
			"package-0",
			"45.2 W",
			"125.0 W",
		}
		for _, want := range checks {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q\nFull output:\n%s", want, out)
			}
		}
	})

	t.Run("throttle section hidden when all counts are zero", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		// testSnapshot has ThrottleInfo with all zero counts.
		if err := RenderCPU(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()
		if strings.Contains(out, "Thermal Throttle Events") {
			t.Errorf("expected throttle section to be absent when all counts are zero\nOutput:\n%s", out)
		}
	})

	t.Run("throttle section shown when count is non-zero", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		snap.Sensors.ThermalThrottle = []types.ThrottleInfo{
			{CPU: 0, CoreThrottleCount: 5, PackageThrottleCount: 2},
		}
		if err := RenderCPU(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "Thermal Throttle Events") {
			t.Errorf("expected throttle section when counts are non-zero\nOutput:\n%s", out)
		}
	})

	t.Run("bar chart contains fill characters", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		if err := RenderCPU(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "█") || !strings.Contains(out, "░") {
			t.Error("expected ASCII bar chart characters in output")
		}
	})

	t.Run("nil snapshot returns error", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if err := RenderCPU(&buf, nil); err == nil {
			t.Fatal("expected error for nil snapshot")
		}
	})
}

func TestRenderMemory(t *testing.T) {
	t.Parallel()

	t.Run("renders memory details and dimm table", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		if err := RenderMemory(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()

		checks := []string{
			"Memory Information",
			"RAM:",
			"Available:",
			"Free:",
			"Buffers:",
			"Swap:",
			"Memory Modules (DIMMs):",
			"DIMMA1",
			"BANK 0",
			"DDR5",
			"4800 MT/s",
			"G Skill Intl",
			"F5-8400J4052G24G",
			"1.1V",
			"Total Capacity:",
		}
		for _, want := range checks {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q\nFull output:\n%s", want, out)
			}
		}
	})

	t.Run("nil snapshot returns error", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if err := RenderMemory(&buf, nil); err == nil {
			t.Fatal("expected error for nil snapshot")
		}
	})
}

func TestRenderStorage(t *testing.T) {
	t.Parallel()

	t.Run("renders disk details and partitions", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		if err := RenderStorage(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()

		checks := []string{
			"Storage Information",
			"nvme0n1",
			"CT2000T705SSD3",
			"PACR5111",
			"SSD/NVMe",
			"42.3%",
			"Healthy",
			"Wear Level:",
			"Spare:",
			"Est. Life:",
			"59°C",
			"8065 hours",
			"154",
			"Media Errors:",
			"80",
			"Partitions:",
			"nvme0n1p1",
			"/boot/efi",
			"vfat",
			"nvme0n1p2",
			"/",
			"ext4",
		}
		for _, want := range checks {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q\nFull output:\n%s", want, out)
			}
		}
	})

	t.Run("unhealthy disk shows failed status", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		snap.Disks[0].SMARTHealthy = false
		if err := RenderStorage(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "FAILED") {
			t.Errorf("expected FAILED in output for unhealthy disk\nOutput:\n%s", out)
		}
	})

	t.Run("nil snapshot returns error", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if err := RenderStorage(&buf, nil); err == nil {
			t.Fatal("expected error for nil snapshot")
		}
	})
}

func TestRenderNetwork(t *testing.T) {
	t.Parallel()

	t.Run("renders interface details", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		if err := RenderNetwork(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()

		checks := []string{
			"Network Interfaces",
			"enp5s0",
			"[UP]",
			// The interface kind now comes from sysfs and is more specific
			// than the old physical/virtual split.
			"ethernet",
			"aa:bb:cc:dd:ee:ff",
			"192.168.1.100/24",
			"fe80::1/64",
			"1000 Mbps",
			"full duplex",
			"igc",
			"1500",
			"1,234,567",
			"4,567,890",
			"lo",
			"loopback",
		}
		for _, want := range checks {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q\nFull output:\n%s", want, out)
			}
		}
	})

	t.Run("down interface shows DOWN label", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		snap.Networks[0].IsUp = false
		if err := RenderNetwork(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "[DOWN]") {
			t.Errorf("expected [DOWN] label for downed interface\nOutput:\n%s", out)
		}
	})

	t.Run("nil snapshot returns error", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if err := RenderNetwork(&buf, nil); err == nil {
			t.Fatal("expected error for nil snapshot")
		}
	})
}

func TestAverageCPUUsage(t *testing.T) {
	t.Parallel()

	t.Run("correct average", func(t *testing.T) {
		t.Parallel()
		cpus := []types.CPUInfo{
			{UsagePercent: 10},
			{UsagePercent: 30},
		}
		got := averageCPUUsage(cpus)
		if got != 20.0 {
			t.Errorf("averageCPUUsage = %v, want 20.0", got)
		}
	})

	t.Run("empty slice returns zero", func(t *testing.T) {
		t.Parallel()
		got := averageCPUUsage(nil)
		if got != 0 {
			t.Errorf("averageCPUUsage(nil) = %v, want 0", got)
		}
	})
}

func TestAllDisksHealthy(t *testing.T) {
	t.Parallel()

	t.Run("all healthy returns true", func(t *testing.T) {
		t.Parallel()
		disks := []types.DiskInfo{
			{SMARTEnabled: true, SMARTHealthy: true},
			{SMARTEnabled: true, SMARTHealthy: true},
		}
		if !allDisksHealthy(disks) {
			t.Error("expected all disks healthy")
		}
	})

	t.Run("one failed returns false", func(t *testing.T) {
		t.Parallel()
		disks := []types.DiskInfo{
			{SMARTEnabled: true, SMARTHealthy: true},
			{SMARTEnabled: true, SMARTHealthy: false},
		}
		if allDisksHealthy(disks) {
			t.Error("expected not all disks healthy")
		}
	})

	t.Run("smart disabled disk does not affect result", func(t *testing.T) {
		t.Parallel()
		disks := []types.DiskInfo{
			{SMARTEnabled: false, SMARTHealthy: false},
		}
		if !allDisksHealthy(disks) {
			t.Error("SMART-disabled disks should not affect health result")
		}
	})
}

func TestDiskTypeString(t *testing.T) {
	t.Parallel()

	t.Run("uses drive type field when set", func(t *testing.T) {
		t.Parallel()
		d := types.DiskInfo{DriveType: "NVMe"}
		got := diskTypeString(d)
		if got != "NVMe" {
			t.Errorf("diskTypeString = %q, want %q", got, "NVMe")
		}
	})

	t.Run("rotational disk returns HDD", func(t *testing.T) {
		t.Parallel()
		d := types.DiskInfo{Rotational: true}
		got := diskTypeString(d)
		if got != "HDD" {
			t.Errorf("diskTypeString = %q, want %q", got, "HDD")
		}
	})

	t.Run("non-rotational without drive type returns SSD/NVMe", func(t *testing.T) {
		t.Parallel()
		d := types.DiskInfo{Rotational: false}
		got := diskTypeString(d)
		if got != "SSD/NVMe" {
			t.Errorf("diskTypeString = %q, want %q", got, "SSD/NVMe")
		}
	})
}

func TestRenderError(t *testing.T) {
	t.Parallel()

	t.Run("error message includes function and cause", func(t *testing.T) {
		t.Parallel()
		cause := errors.New("something went wrong")
		re := &RenderError{Function: "RenderCPU", Cause: cause}
		got := re.Error()
		if !strings.Contains(got, "RenderCPU") {
			t.Errorf("error message missing function name: %q", got)
		}
		if !strings.Contains(got, "something went wrong") {
			t.Errorf("error message missing cause: %q", got)
		}
	})

	t.Run("unwrap returns original cause", func(t *testing.T) {
		t.Parallel()
		cause := errors.New("root cause")
		re := &RenderError{Function: "RenderMemory", Cause: cause}
		if re.Unwrap() != cause {
			t.Error("Unwrap() did not return original cause")
		}
	})
}

// testGPUSnapshot returns a snapshot containing a single synthetic GPU.
func testGPUSnapshot() *types.Snapshot {
	snap := testSnapshot()
	snap.GPUs = []types.GPUInfo{
		{
			Index:             0,
			Name:              "NVIDIA RTX PRO 6000 Blackwell Workstation Edition",
			UUID:              "GPU-a5fbbbac-951f-fdf3-e925-3956785e1688",
			Serial:            "[Not Supported]",
			DriverVersion:     "580.126.09",
			VBIOSVersion:      "98.02.81.00.07",
			ComputeMode:       "Default",
			PerfState:         "P0",
			PCIBusID:          "00000000:02:00.0",
			PCIeGenCurrent:    5,
			PCIeGenMax:        5,
			PCIeWidthCurrent:  8,
			PCIeWidthMax:      16,
			MemoryTotalMiB:    97887,
			MemoryUsedMiB:     21006,
			MemoryFreeMiB:     76881,
			MemoryPercent:     21.5,
			GPUUtilPercent:    98.0,
			MemoryUtilPercent: 56.0,
			EncoderPercent:    0.0,
			DecoderPercent:    0.0,
			TemperatureGPU:    59.0,
			FanSpeedPercent:   34.0,
			PowerDrawW:        440.5,
			PowerLimitW:       600.0,
			PowerDefaultW:     600.0,
			PowerMaxW:         600.0,
			ClockGraphicsMHz:  2805,
			ClockMemoryMHz:    14001,
			ClockMaxGfxMHz:    3090,
			ClockMaxMemMHz:    14001,
			ClockVideoMHz:     2355,
			PCIeRxMBps:        13.0,
			PCIeTxMBps:        2.0,
			ECCEnabled:        false,
		},
	}
	return snap
}

func TestRenderGPU(t *testing.T) {
	t.Parallel()

	t.Run("renders gpu details", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testGPUSnapshot()
		if err := RenderGPU(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()

		checks := []string{
			"GPU Information",
			"GPU 0:",
			"NVIDIA RTX PRO 6000 Blackwell Workstation Edition",
			"580.126.09",
			"98.02.81.00.07",
			"Default",
			"P0",
			"GPU-a5fbbbac-951f-fdf3-e925-3956785e1688",
			"00000000:02:00.0",
			"Gen5",
			"Utilization:",
			"98.0%",
			"56.0%",
			"21006 MiB / 97887 MiB",
			"59°C",
			"34%",
			"440.5 W",
			"600.0 W",
			"2805",
			"3090 MHz",
			"14001",
			"2355 MHz",
			"TX",
			"RX",
			"ECC:",
			"Disabled",
		}
		for _, want := range checks {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q\nFull output:\n%s", want, out)
			}
		}
	})

	t.Run("renders progress bars", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testGPUSnapshot()
		if err := RenderGPU(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "█") || !strings.Contains(out, "░") {
			t.Error("expected bar chart characters in GPU output")
		}
	})

	t.Run("no GPUs prints message", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot() // no GPUs field populated
		snap.GPUs = nil
		if err := RenderGPU(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "No GPUs detected") {
			t.Errorf("expected no-GPU message, got:\n%s", out)
		}
	})

	t.Run("ecc enabled shows counters", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testGPUSnapshot()
		snap.GPUs[0].ECCEnabled = true
		snap.GPUs[0].ECCSingleBit = 3
		snap.GPUs[0].ECCDoubleBit = 1
		if err := RenderGPU(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "Enabled") {
			t.Errorf("expected ECC Enabled in output:\n%s", out)
		}
		if !strings.Contains(out, "SBE: 3") {
			t.Errorf("expected SBE count in output:\n%s", out)
		}
		if !strings.Contains(out, "DBE: 1") {
			t.Errorf("expected DBE count in output:\n%s", out)
		}
	})

	t.Run("multiple gpus render all", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testGPUSnapshot()
		g2 := snap.GPUs[0]
		g2.Index = 1
		g2.Name = "NVIDIA A100"
		snap.GPUs = append(snap.GPUs, g2)
		if err := RenderGPU(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "GPU 0:") || !strings.Contains(out, "GPU 1:") {
			t.Errorf("expected both GPU 0 and GPU 1 in output:\n%s", out)
		}
		if !strings.Contains(out, "NVIDIA A100") {
			t.Errorf("expected second GPU name in output:\n%s", out)
		}
	})

	t.Run("nil snapshot returns error", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		err := RenderGPU(&buf, nil)
		if err == nil {
			t.Fatal("expected error for nil snapshot")
		}
		var ce *types.CollectorError
		if !errors.As(err, &ce) {
			t.Errorf("expected CollectorError, got %T: %v", err, err)
		}
	})
}

func TestPrimaryInterface(t *testing.T) {
	t.Parallel()

	t.Run("returns first physical up interface", func(t *testing.T) {
		t.Parallel()
		ifaces := []types.NetworkInfo{
			{Name: "lo", IsLoopback: true, IsUp: true},
			{Name: "eth0", IsUp: true, IsVirtual: false, IsLoopback: false},
			{Name: "eth1", IsUp: true, IsVirtual: false, IsLoopback: false},
		}
		got := primaryInterface(ifaces)
		if got == nil || got.Name != "eth0" {
			t.Errorf("primaryInterface = %v, want eth0", got)
		}
	})

	t.Run("returns nil when no physical up interface", func(t *testing.T) {
		t.Parallel()
		ifaces := []types.NetworkInfo{
			{Name: "lo", IsLoopback: true, IsUp: true},
			{Name: "eth0", IsUp: false, IsVirtual: false, IsLoopback: false},
		}
		got := primaryInterface(ifaces)
		if got != nil {
			t.Errorf("expected nil, got %v", got.Name)
		}
	})
}

func TestRenderCPU_WithTemperature(t *testing.T) {
	t.Parallel()

	t.Run("temperature shown next to core when non-zero", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		snap.CPUs[0].TemperatureCelsius = 45.0
		snap.CPUs[1].TemperatureCelsius = 47.5
		if err := RenderCPU(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()

		checks := []string{"45.0°C", "47.5°C"}
		for _, want := range checks {
			if !strings.Contains(out, want) {
				t.Errorf("output missing temperature %q\nFull output:\n%s", want, out)
			}
		}
	})

	t.Run("zero temperature core shows no inline temperature suffix", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		snap := testSnapshot()
		// Ensure both cores have zero temperature (default from testSnapshot).
		snap.CPUs[0].TemperatureCelsius = 0
		snap.CPUs[1].TemperatureCelsius = 0
		// Clear CoreTemps so the temperature table is also absent.
		snap.Sensors.CoreTemps = nil
		if err := RenderCPU(&buf, snap); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out := buf.String()
		if strings.Contains(out, "°C") {
			t.Errorf("expected no temperature in output when all temps are zero and CoreTemps is empty\nFull output:\n%s", out)
		}
	})
}
