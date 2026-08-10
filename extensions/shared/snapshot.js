// Snapshot parsing and helper accessors for go-sysmon extensions.
// Works with the JSON structure defined in pkg/types/types.go.
//
// This module is plain CommonJS with no GObject Introspection imports, so it
// loads under Cinnamon's require() and under Node for unit tests.

/**
 * parseSnapshot safely parses a JSON snapshot string.
 * @param {string} jsonString - raw JSON from the event stream
 * @returns {object|null} parsed snapshot or null on failure
 */
function parseSnapshot(jsonString) {
    try {
        const parsed = JSON.parse(jsonString);
        // A JSON document can legally be a bare scalar; anything that is not an
        // object is not a snapshot.
        if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
            return null;
        }
        return parsed;
    } catch (e) {
        return null;
    }
}

/**
 * avgCpuUsage computes the average usage_percent across all CPUs.
 * @param {Array} cpus - array of CPUInfo objects
 * @returns {number} average usage percentage (0-100)
 */
function avgCpuUsage(cpus) {
    if (!cpus || cpus.length === 0) {
        return 0;
    }
    let total = 0;
    for (let i = 0; i < cpus.length; i++) {
        total += cpus[i].usage_percent || 0;
    }
    return total / cpus.length;
}

/**
 * maxCpuTemp returns the highest temp_celsius from sensor core_temps.
 * @param {Array} coreTemps - array of CoreTemp objects from sensors.core_temps
 * @returns {number} maximum temperature in Celsius, or 0 if none
 */
function maxCpuTemp(coreTemps) {
    if (!coreTemps || coreTemps.length === 0) {
        return 0;
    }
    let max = 0;
    for (let i = 0; i < coreTemps.length; i++) {
        const temp = coreTemps[i].temp_celsius || 0;
        if (temp > max) {
            max = temp;
        }
    }
    return max;
}

/**
 * totalNetRate computes the total network throughput (sent + received)
 * across all interfaces.
 * @param {Array} networks - array of NetworkInfo objects
 * @returns {number} total bytes per second (sent + received)
 */
function totalNetRate(networks) {
    if (!networks || networks.length === 0) {
        return 0;
    }
    let total = 0;
    for (let i = 0; i < networks.length; i++) {
        total += (networks[i].bytes_sent_rate || 0) + (networks[i].bytes_recv_rate || 0);
    }
    return total;
}

/**
 * netRates sums transmit and receive rates separately, skipping loopback so
 * the panel reflects traffic that actually leaves the machine.
 * @param {Array} networks - array of NetworkInfo objects
 * @returns {{sent: number, recv: number}} rates in bytes per second
 */
function netRates(networks) {
    const rates = { sent: 0, recv: 0 };
    if (!networks) {
        return rates;
    }
    for (let i = 0; i < networks.length; i++) {
        if (networks[i].is_loopback) {
            continue;
        }
        rates.sent += networks[i].bytes_sent_rate || 0;
        rates.recv += networks[i].bytes_recv_rate || 0;
    }
    return rates;
}

/**
 * diskUsage aggregates filesystem usage across every disk that reports a
 * capacity.
 * @param {Array} disks - array of DiskInfo objects
 * @returns {{total: number, used: number, percent: number}} aggregate usage
 */
function diskUsage(disks) {
    const usage = { total: 0, used: 0, percent: 0 };
    if (!disks) {
        return usage;
    }
    for (let i = 0; i < disks.length; i++) {
        usage.total += disks[i].total_bytes || 0;
        usage.used += disks[i].used_bytes || 0;
    }
    if (usage.total > 0) {
        usage.percent = (usage.used / usage.total) * 100;
    }
    return usage;
}

/**
 * diskIORates sums read and write throughput across every disk.
 * @param {Array} disks - array of DiskInfo objects
 * @returns {{read: number, write: number}} rates in bytes per second
 */
function diskIORates(disks) {
    const rates = { read: 0, write: 0 };
    if (!disks) {
        return rates;
    }
    for (let i = 0; i < disks.length; i++) {
        rates.read += disks[i].read_bytes_rate || 0;
        rates.write += disks[i].write_bytes_rate || 0;
    }
    return rates;
}

/**
 * topProcesses returns the highest CPU consumers, sorted descending. The input
 * array is not modified.
 * @param {Array} processes - array of ProcessDetail objects
 * @param {number} limit - maximum number of entries to return
 * @returns {Array} the top entries by cpu_percent
 */
function topProcesses(processes, limit) {
    if (!processes || processes.length === 0) {
        return [];
    }
    const sorted = processes.slice().sort(
        (a, b) => (b.cpu_percent || 0) - (a.cpu_percent || 0)
    );
    return sorted.slice(0, Math.max(0, limit));
}

/**
 * usageLevel classifies a 0-1 usage fraction so callers can colour it
 * consistently: "normal" below 50%, "warn" to 80%, "critical" above.
 * @param {number} fraction - usage as a fraction between 0 and 1
 * @returns {string} one of "normal", "warn", "critical"
 */
function usageLevel(fraction) {
    if (!fraction || isNaN(fraction) || fraction < 0.5) {
        return "normal";
    }
    if (fraction < 0.8) {
        return "warn";
    }
    return "critical";
}

module.exports = {
    avgCpuUsage,
    diskIORates,
    diskUsage,
    maxCpuTemp,
    netRates,
    parseSnapshot,
    topProcesses,
    totalNetRate,
    usageLevel,
};
