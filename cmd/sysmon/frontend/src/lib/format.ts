// Human-readable formatting utilities.

const BYTE_UNITS = ["B", "KB", "MB", "GB", "TB", "PB"] as const;

/**
 * formatBytes converts a raw byte count to the most readable unit string.
 * e.g. 1536 -> "1.5 KB", 0.8 -> "0.8 B", -2048 -> "-2.0 KB"
 *
 * The unit index is clamped to the table: below one byte, log1024 is negative
 * and would index before "B" (#175). A negative value is formatted by its
 * magnitude and keeps its sign, and a non-finite one renders as a dash rather
 * than as "NaN".
 */
export function formatBytes(bytes: number, decimals = 1): string {
  if (!Number.isFinite(bytes)) return "-";
  if (bytes === 0) return "0 B";
  const magnitude = Math.abs(bytes);
  const k = 1024;
  const i = Math.min(
    Math.max(Math.floor(Math.log(magnitude) / Math.log(k)), 0),
    BYTE_UNITS.length - 1
  );
  const value = bytes / Math.pow(k, i);
  return `${value.toFixed(decimals)} ${BYTE_UNITS[i]}`;
}

/**
 * formatBytesRate converts a bytes-per-second rate to a human-readable string.
 * e.g. 2048 -> "2.0 KB/s"
 */
export function formatBytesRate(bytesPerSec: number, decimals = 1): string {
  return `${formatBytes(bytesPerSec, decimals)}/s`;
}

const BIT_RATE_UNITS = ["bps", "Kbps", "Mbps", "Gbps", "Tbps"] as const;

/**
 * formatBitRate converts a bits-per-second rate to the most readable decimal
 * (SI) unit, as network links are rated: 1 Kbps is 1000 bps, not 1024.
 * e.g. 1500 -> "1.5 Kbps", 1e9 -> "1.0 Gbps", 2.5e15 -> "2500.0 Tbps"
 *
 * Like formatBytes, a value below one bit stays in bps, a negative value keeps
 * its sign, and a non-finite one renders as a dash.
 */
export function formatBitRate(bitsPerSec: number, decimals = 1): string {
  if (!Number.isFinite(bitsPerSec)) return "-";
  if (bitsPerSec === 0) return "0 bps";
  const magnitude = Math.abs(bitsPerSec);
  const i = Math.min(
    Math.max(Math.floor(Math.log10(magnitude) / 3), 0),
    BIT_RATE_UNITS.length - 1
  );
  const value = bitsPerSec / Math.pow(1000, i);
  return `${value.toFixed(decimals)} ${BIT_RATE_UNITS[i]}`;
}

/**
 * formatByteRateAsBits shows a bytes-per-second rate, as the collector reports
 * interface traffic, in bits per second.
 * e.g. 125000 -> "1.0 Mbps"
 */
export function formatByteRateAsBits(bytesPerSec: number, decimals = 1): string {
  return formatBitRate(bytesPerSec * 8, decimals);
}

/**
 * formatDuration converts a duration in seconds to a human-readable string.
 * e.g. 90061 -> "1d 1h 1m"
 */
export function formatDuration(seconds: number): string {
  if (seconds <= 0) return "0s";

  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);

  const parts: string[] = [];
  if (days > 0) parts.push(`${days}d`);
  if (hours > 0) parts.push(`${hours}h`);
  if (minutes > 0) parts.push(`${minutes}m`);
  if (parts.length === 0) parts.push(`${seconds}s`);

  return parts.join(" ");
}

/**
 * formatPercent formats a floating-point percentage with one decimal place.
 */
export function formatPercent(value: number): string {
  return `${value.toFixed(1)}%`;
}

/**
 * formatMHz formats a CPU frequency.
 */
export function formatMHz(mhz: number): string {
  if (mhz >= 1000) {
    return `${(mhz / 1000).toFixed(2)} GHz`;
  }
  return `${mhz.toFixed(0)} MHz`;
}

/**
 * usageColor returns a DaisyUI semantic color class based on a usage percentage.
 * green < 50%, yellow 50-80%, red > 80%
 */
export function usageColor(percent: number): string {
  if (percent >= 80) return "text-error";
  if (percent >= 50) return "text-warning";
  return "text-success";
}

/**
 * usageBgColor returns a progress bar color class for a given usage percentage.
 */
export function usageProgressColor(percent: number): string {
  if (percent >= 80) return "progress-error";
  if (percent >= 50) return "progress-warning";
  return "progress-success";
}

/**
 * usageEchartsColor returns an ECharts color string for a usage percentage.
 *
 * The colours come from the chart palette rather than being fixed here,
 * because a gauge letters its percentage in this colour: it is text, and the
 * values that read on a black chart are the least readable thing on a white
 * one. See ChartPalette.usage.
 */
export function usageEchartsColor(
  percent: number,
  usage: { ok: string; warn: string; crit: string }
): string {
  if (percent >= 80) return usage.crit;
  if (percent >= 50) return usage.warn;
  return usage.ok;
}

/**
 * formatHoursHuman converts a duration in hours to a human-readable string.
 * e.g. 26280 (3 years) -> "3 years, 0 days"
 * e.g. 400 -> "16 days, 16 hours"
 */
export function formatHoursHuman(hours: number): string {
  if (hours <= 0) return "0 hours";

  const HOURS_PER_YEAR = 8760;
  const HOURS_PER_DAY = 24;

  if (hours >= HOURS_PER_YEAR) {
    const years = Math.floor(hours / HOURS_PER_YEAR);
    const remainingDays = Math.floor((hours % HOURS_PER_YEAR) / HOURS_PER_DAY);
    if (remainingDays === 0) return `${years} year${years !== 1 ? "s" : ""}`;
    return `${years} year${years !== 1 ? "s" : ""}, ${remainingDays} day${remainingDays !== 1 ? "s" : ""}`;
  }

  if (hours >= HOURS_PER_DAY) {
    const days = Math.floor(hours / HOURS_PER_DAY);
    const remainingHours = Math.floor(hours % HOURS_PER_DAY);
    if (remainingHours === 0) return `${days} day${days !== 1 ? "s" : ""}`;
    return `${days} day${days !== 1 ? "s" : ""}, ${remainingHours} hour${remainingHours !== 1 ? "s" : ""}`;
  }

  const h = Math.floor(hours);
  return `${h} hour${h !== 1 ? "s" : ""}`;
}

/**
 * formatDataUnits converts NVMe data units to a human-readable byte string.
 * Each NVMe data unit = 512,000 bytes (1000 sectors of 512 bytes).
 * e.g. 85070 -> "43.5 TB"
 */
export function formatDataUnits(units: number): string {
  return formatBytes(units * 512000);
}

/**
 * formatWatts formats a power value in watts to a human-readable string.
 * e.g. 440.5 -> "440.5 W"
 */
export function formatWatts(w: number): string {
  return `${w.toFixed(1)} W`;
}

/**
 * formatTemp formats a temperature in Celsius to a human-readable string.
 * Returns "-" when the value is 0 (not reported).
 * e.g. 52.0 -> "52.0°C"
 */
export function formatTemp(celsius: number): string {
  if (celsius === 0) return "-";
  return `${celsius.toFixed(1)}°C`;
}

/**
 * tempColor returns a DaisyUI semantic color class based on proximity to the
 * critical threshold. green < 70%, yellow 70-85%, red > 85% of crit.
 */
export function tempColor(celsius: number, critCelsius: number): string {
  if (critCelsius <= 0) {
    // No threshold data: use fixed bands.
    if (celsius >= 90) return "text-error";
    if (celsius >= 70) return "text-warning";
    return "text-success";
  }
  const ratio = celsius / critCelsius;
  if (ratio >= 0.85) return "text-error";
  if (ratio >= 0.70) return "text-warning";
  return "text-success";
}

/**
 * tempProgressColor returns a DaisyUI progress bar color class based on
 * proximity to the critical threshold.
 */
export function tempProgressColor(celsius: number, critCelsius: number): string {
  if (critCelsius <= 0) {
    if (celsius >= 90) return "progress-error";
    if (celsius >= 70) return "progress-warning";
    return "progress-success";
  }
  const ratio = celsius / critCelsius;
  if (ratio >= 0.85) return "progress-error";
  if (ratio >= 0.70) return "progress-warning";
  return "progress-success";
}

/**
 * formatCores renders a "percent of one core" value as a core count.
 * 1703 becomes "17.0 cores", which is what the number actually means.
 */
export function formatCores(percentOfOneCore: number): string {
  const cores = percentOfOneCore / 100;
  if (cores < 10) return `${cores.toFixed(1)} cores`;
  return `${Math.round(cores)} cores`;
}

/**
 * percentOfCapacity converts a "percent of one core" value into a share of the
 * capacity available to that workload, so it lands on the 0-100 scale people
 * expect. capacityCores is the vCPU count for a guest, the quota for a limited
 * container, or the host thread count when nothing caps it.
 */
export function percentOfCapacity(
  percentOfOneCore: number,
  capacityCores: number
): number {
  if (capacityCores <= 0) return 0;
  return percentOfOneCore / capacityCores;
}

/**
 * formatAge renders an age in seconds compactly — "4s", "3m", "2h 5m".
 * Sub-second ages round to "0s" rather than showing a fraction.
 */
export function formatAge(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "0s";
  const whole = Math.floor(seconds);
  if (whole < 60) return `${whole}s`;
  return formatDuration(whole);
}

/**
 * formatRelativeTime renders how long ago an RFC3339 timestamp was, e.g.
 * "3m ago". An unparseable or absent timestamp yields "—" so a malformed
 * field never breaks the row that contains it.
 */
export function formatRelativeTime(rfc3339: string): string {
  if (!rfc3339) return "—";
  const then = Date.parse(rfc3339);
  if (Number.isNaN(then)) return "—";
  const seconds = (Date.now() - then) / 1000;
  // A clock skew between peer and browser can put "since" in the future;
  // report that as "just now" rather than a negative age.
  if (seconds < 1) return "just now";
  return `${formatAge(seconds)} ago`;
}

/**
 * formatUnixTime renders a Unix timestamp in seconds as a local date-time.
 * Returns "—" for the zero value, which is how the backend spells "unknown".
 */
export function formatUnixTime(unixSeconds: number): string {
  if (!unixSeconds) return "—";
  return new Date(unixSeconds * 1000).toLocaleString();
}
