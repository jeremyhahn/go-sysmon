// Formatting utilities for go-sysmon extensions.
// Mirrors the logic in pkg/cli/format.go.

const KB = 1024;
const MB = KB * 1024;
const GB = MB * 1024;
const TB = GB * 1024;

/**
 * formatBytes converts a byte count to a human-readable string with 1 decimal.
 * @param {number} bytes - byte count
 * @returns {string} formatted string (e.g. "1.5 GB")
 */
function formatBytes(bytes) {
    if (bytes === 0 || bytes === undefined || bytes === null) {
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
 * @returns {string} formatted string (e.g. "65\u00b0C" or "-")
 */
function formatTemp(celsius) {
    if (!celsius || celsius === 0) {
        return "-";
    }
    return celsius.toFixed(0) + "\u00b0C";
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
