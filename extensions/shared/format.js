// Formatting utilities for go-sysmon extensions.
// Mirrors the logic in pkg/cli/format.go.
//
// This module is plain CommonJS with no GObject Introspection imports, so it
// loads under Cinnamon's require() and under Node for unit tests.

const KB = 1024;
const MB = KB * 1024;
const GB = MB * 1024;
const TB = GB * 1024;

// DASH is shown wherever a value is absent, so an empty reading is visibly
// different from a genuine zero.
const DASH = "-";

/**
 * formatBytes converts a byte count to a human-readable string with 1 decimal.
 * @param {number} bytes - byte count
 * @returns {string} formatted string (e.g. "1.5 GB")
 */
function formatBytes(bytes) {
    if (bytes === 0 || bytes === undefined || bytes === null || isNaN(bytes)) {
        return "0 B";
    }
    if (bytes >= TB) {
        return (bytes / TB).toFixed(1) + " TB";
    }
    if (bytes >= GB) {
        return (bytes / GB).toFixed(1) + " GB";
    }
    if (bytes >= MB) {
        return (bytes / MB).toFixed(1) + " MB";
    }
    if (bytes >= KB) {
        return (bytes / KB).toFixed(1) + " KB";
    }
    return bytes.toFixed(0) + " B";
}

/**
 * formatRate converts a bytes-per-second rate to a per-second string.
 * @param {number} bytesPerSec - rate in bytes per second
 * @returns {string} formatted string (e.g. "1.5 MB/s")
 */
function formatRate(bytesPerSec) {
    return formatBytes(bytesPerSec) + "/s";
}

/**
 * formatBits converts a bytes-per-second rate to network units, which are
 * conventionally quoted in bits.
 * @param {number} bytesPerSec - rate in bytes per second
 * @returns {string} formatted string (e.g. "1.2 Gbps")
 */
function formatBits(bytesPerSec) {
    if (!bytesPerSec || isNaN(bytesPerSec)) {
        return "0 bps";
    }
    const bits = bytesPerSec * 8;
    if (bits >= 1e12) {
        return (bits / 1e12).toFixed(2) + " Tbps";
    }
    if (bits >= 1e9) {
        return (bits / 1e9).toFixed(2) + " Gbps";
    }
    if (bits >= 1e6) {
        return (bits / 1e6).toFixed(2) + " Mbps";
    }
    if (bits >= 1e3) {
        return (bits / 1e3).toFixed(1) + " Kbps";
    }
    return bits.toFixed(0) + " bps";
}

/**
 * formatCompactRate converts a bytes-per-second rate to a very short string
 * suitable for a panel gauge, where horizontal space is scarce.
 * @param {number} bytesPerSec - rate in bytes per second
 * @returns {string} formatted string (e.g. "1.5M")
 */
function formatCompactRate(bytesPerSec) {
    if (!bytesPerSec || isNaN(bytesPerSec) || bytesPerSec < 0) {
        return "0B";
    }
    if (bytesPerSec >= GB) {
        return (bytesPerSec / GB).toFixed(1) + "G";
    }
    if (bytesPerSec >= MB) {
        return (bytesPerSec / MB).toFixed(1) + "M";
    }
    if (bytesPerSec >= KB) {
        return (bytesPerSec / KB).toFixed(0) + "K";
    }
    return bytesPerSec.toFixed(0) + "B";
}

/**
 * formatPercent formats a float as a percentage string.
 * @param {number} value - percentage value (0-100)
 * @returns {string} formatted string (e.g. "42.3%")
 */
function formatPercent(value) {
    if (value === undefined || value === null || isNaN(value)) {
        return "0.0%";
    }
    return value.toFixed(1) + "%";
}

/**
 * formatTemp formats a temperature in Celsius.
 * Returns "-" for zero or missing values.
 * @param {number} celsius - temperature in degrees Celsius
 * @returns {string} formatted string (e.g. "65°C" or "-")
 */
function formatTemp(celsius) {
    if (!celsius || celsius === 0 || isNaN(celsius)) {
        return DASH;
    }
    return celsius.toFixed(0) + "°C";
}

/**
 * formatMHz formats a frequency in MHz, converting to GHz when >= 1000.
 * @param {number} mhz - frequency in MHz
 * @returns {string} formatted string (e.g. "3.5 GHz" or "800 MHz")
 */
function formatMHz(mhz) {
    if (mhz === undefined || mhz === null || isNaN(mhz)) {
        return "0 MHz";
    }
    if (mhz >= 1000) {
        return (mhz / 1000).toFixed(1) + " GHz";
    }
    return mhz.toFixed(0) + " MHz";
}

/**
 * formatDuration converts a span in seconds to a compact "1d 2h 3m" string.
 * @param {number} seconds - duration in seconds
 * @returns {string} formatted string
 */
function formatDuration(seconds) {
    if (!seconds || seconds < 0 || isNaN(seconds)) {
        return DASH;
    }

    const days = Math.floor(seconds / 86400);
    const hours = Math.floor((seconds % 86400) / 3600);
    const minutes = Math.floor((seconds % 3600) / 60);

    const parts = [];
    if (days > 0) {
        parts.push(days + "d");
    }
    if (hours > 0) {
        parts.push(hours + "h");
    }
    // Always show minutes so a sub-hour uptime is not rendered as an empty
    // string.
    if (minutes > 0 || parts.length === 0) {
        parts.push(minutes + "m");
    }
    return parts.join(" ");
}

/**
 * formatValue renders any scalar, substituting a dash for absent values so a
 * missing reading never shows as "undefined".
 * @param {*} value - the value to render
 * @param {string} [suffix] - optional unit appended to non-empty values
 * @returns {string} formatted string
 */
function formatValue(value, suffix) {
    if (value === undefined || value === null || value === "") {
        return DASH;
    }
    if (typeof value === "number" && isNaN(value)) {
        return DASH;
    }
    return String(value) + (suffix ? " " + suffix : "");
}

/**
 * formatList joins a string array for display, substituting a dash when empty.
 * @param {Array<string>} values - values to join
 * @param {string} [separator] - separator, defaulting to ", "
 * @returns {string} formatted string
 */
function formatList(values, separator) {
    if (!values || values.length === 0) {
        return DASH;
    }
    return values.join(separator === undefined ? ", " : separator);
}

module.exports = {
    DASH,
    formatBits,
    formatBytes,
    formatCompactRate,
    formatDuration,
    formatList,
    formatMHz,
    formatPercent,
    formatRate,
    formatTemp,
    formatValue,
};
