# Sensors

`SensorCollector` reads hardware sensors out of sysfs and hwmon. It runs on the
medium tier, every 5th tick.

Sensor data is the least uniform thing this project touches. What a machine
exposes depends on its chipset, its kernel modules and its firmware, and there
is no way to know in advance which of these paths will exist. Everything below
is best-effort: a missing source produces an empty list, not an error.

## Core temperatures

From `/sys/class/hwmon/*/temp*_input`. Each reading carries the package and
core it belongs to, the current temperature, and the high and critical
thresholds when the driver publishes them.

The input numbers are sparse. `coretemp` on a 24-thread CPU might use `temp1`,
`temp2`, `temp6`, `temp10` and so on, so the collector globs rather than
counting upward — an earlier version stopped at the first gap and reported one
core's temperature for the whole package.

## Core voltages

From hwmon voltage inputs. The collector looks for Vcore and VID labels and
merges the first match into the CPU entries.

## Package power (RAPL)

From Intel's Running Average Power Limit interface: current draw in watts, the
maximum limit, and accumulated energy in joules.

## Thermal throttle events

From `/sys/devices/system/cpu/cpu*/thermal_throttle/`, per core and per
package. These are counters, not rates. A non-zero value means the CPU has
throttled at some point since boot, which is the thing you want to know when a
benchmark came in slow.

## Thermal zones

From `/sys/class/thermal/thermal_zone*/`: zone name, type, current temperature
and cooling policy. Zones cover things `coretemp` does not — chipset, NVMe
controller, ambient sensors on a laptop.

## Fan speeds

From hwmon fan inputs: current RPM, the min/max range, and the name of the
monitor chip reporting it.

## Pressure stall information

From `/proc/pressure/{cpu,memory,io}`. PSI answers a question plain utilisation
cannot: how much time did tasks spend *waiting* for a resource. A machine at
60% CPU with high CPU pressure is contended; the same 60% with no pressure is
comfortable.

Each file gives 10s, 60s and 300s averages for "some" and "full" stalls, plus a
total stall time in microseconds.

## GPU sensors

GPU thermals and power come from `GPUCollector`, not this one, because they
arrive through a different interface per vendor:

- NVIDIA through NVML, the vendor C library
- Intel and AMD through sysfs under `/sys/class/drm/card*/`

## DIMM temperatures

Per-module temperatures need the `spd5118` (DDR5) or `jc42` (DDR4) driver bound
to the SPD hub on the SMBus. On most systems it is not bound by default.

sysmon probes for these once per process, never per cycle. An earlier version
ran `modprobe` and wrote to `/sys/bus/i2c/.../new_device` on every collection —
once a second, forever, on a machine where it was never going to work. If the
probe fails, that is logged once and the field stays empty.
