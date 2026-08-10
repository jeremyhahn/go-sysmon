// Snapshot parsing and helper accessors for go-sysmon extensions.
// Works with the JSON structure defined in pkg/types/types.go.

/**
 * parseSnapshot safely parses a JSON snapshot string.
 * @param {string} jsonString - raw JSON from the WebSocket
 * @returns {object|null} parsed snapshot or null on failure
 */
function parseSnapshot(jsonString) {
    try {
        return JSON.parse(jsonString);
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
        let temp = coreTemps[i].temp_celsius || 0;
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
