// Popup section model for go-sysmon desktop extensions.
//
// A panel applet is roughly twenty pixels tall, so it cannot show what the CLI
// renders. This module turns a snapshot into the same information organised as
// collapsible sections, which the applet then walks into menu items. Keeping
// the model here — free of any GObject Introspection import — is what makes
// parity with the CLI and the web UI testable under Node.
//
// Shape:
//   [{ key, title, summary, blocks: [{ title, rows: [{ label, value }] }] }]

const fmt = require("./format.js");
const snap = require("./snapshot.js");

// TOP_PROCESS_COUNT bounds the process block. The full list belongs in the web
// UI, which can paginate; a menu cannot.
const TOP_PROCESS_COUNT = 10;

// CPU_FLAG_WRAP is how many flags are joined onto one line before wrapping, so
// a long flag list stays readable in a narrow menu.
const CPU_FLAG_WRAP = 8;

/** row builds a single label/value pair. */
function row(label, value) {
    return { label: label, value: value };
}

/** block builds a titled group of rows, dropping it when it has none. */
function block(title, rows) {
    return { title: title || "", rows: rows || [] };
}

/**
 * label builds a row label from a name, falling back to a prefixed index.
 *
 * Concatenating a prefix with an absent index produces text like "coreundefined",
 * which is why the index is only used when the collector actually reported one.
 * @param {string} name - the reported name, when there is one
 * @param {string} prefix - prefix for the index fallback
 * @param {number|null} index - the reported index, when there is one
 * @returns {string} a label that never contains "undefined"
 */
function label(name, prefix, index) {
    if (name) {
        return name;
    }
    if (index !== undefined && index !== null && !isNaN(index)) {
        return prefix + index;
    }
    return prefix;
}

/**
 * chunk splits an array into runs of at most size entries.
 * @param {Array} items - the array to split
 * @param {number} size - maximum run length
 * @returns {Array<Array>} the runs
 */
function chunk(items, size) {
    const out = [];
    for (let i = 0; i < items.length; i += size) {
        out.push(items.slice(i, i + size));
    }
    return out;
}

// ---- individual sections ---------------------------------------------------

function hostSection(s) {
    const host = s.host || {};

    return {
        key: "host",
        title: "Host",
        summary: fmt.formatValue(host.hostname) + " · " + fmt.formatValue(host.kernel_version),
        blocks: [
            block("", [
                row("Hostname", fmt.formatValue(host.hostname)),
                row("OS", fmt.formatValue(host.os)),
                row("Platform", fmt.formatValue(host.platform) + " " + fmt.formatValue(host.platform_version)),
                row("Kernel", fmt.formatValue(host.kernel_version) + " (" + fmt.formatValue(host.kernel_arch) + ")"),
                row("Uptime", fmt.formatDuration(host.uptime)),
            ]),
            block("Board", [
                row("Vendor", fmt.formatValue(host.board_vendor)),
                row("Model", fmt.formatValue(host.board_name)),
                row("Version", fmt.formatValue(host.board_version)),
            ]),
            block("BIOS", [
                row("Vendor", fmt.formatValue(host.bios_vendor)),
                row("Version", fmt.formatValue(host.bios_version)),
                row("Date", fmt.formatValue(host.bios_date)),
            ]),
        ],
    };
}

function cpuSection(s) {
    const cpus = s.cpus || [];
    const summary = s.cpu_summary || {};
    const load = s.load_avg || {};
    const first = cpus[0] || {};
    const avg = snap.avgCpuUsage(cpus);

    const blocks = [
        block("", [
            row("Model", fmt.formatValue(first.model_name)),
            row("Vendor", fmt.formatValue(first.vendor_id)),
            row("Family/Model", fmt.formatValue(first.family) + "/" + fmt.formatValue(first.model) +
                " stepping " + fmt.formatValue(first.stepping)),
            row("Microcode", fmt.formatValue(first.microcode)),
            row("Topology", fmt.formatValue(summary.sockets) + " socket(s), " +
                fmt.formatValue(summary.cores_per_socket) + " core(s), " +
                fmt.formatValue(summary.threads_per_core) + " thread(s)/core"),
            row("Cores/Threads", fmt.formatValue(summary.total_cores) + " / " +
                fmt.formatValue(summary.total_threads)),
            row("Frequency", fmt.formatMHz(summary.min_mhz) + " – " + fmt.formatMHz(summary.max_mhz)),
            row("Cache", fmt.formatBytes((first.cache_size || 0) * 1024)),
            row("Usage", fmt.formatPercent(avg)),
            row("Load average", fmt.formatValue((load.load1 || 0).toFixed(2)) + "  " +
                (load.load5 || 0).toFixed(2) + "  " + (load.load15 || 0).toFixed(2)),
        ]),
    ];

    const coreRows = cpus.map((cpu) =>
        row(
            "core" + fmt.formatValue(cpu.index),
            fmt.formatPercent(cpu.usage_percent) + "   " +
                fmt.formatMHz(cpu.mhz) + "   " +
                fmt.formatTemp(cpu.temperature_celsius)
        )
    );
    if (coreRows.length > 0) {
        blocks.push(block("Per-core", coreRows));
    }

    const flags = first.flags || [];
    if (flags.length > 0) {
        blocks.push(block(
            "Flags (" + flags.length + ")",
            chunk(flags, CPU_FLAG_WRAP).map((line) => row("", line.join(" ")))
        ));
    }

    return {
        key: "cpu",
        title: "CPU",
        summary: fmt.formatValue(first.model_name) + " · " + fmt.formatPercent(avg),
        blocks: blocks,
    };
}

function gpuSection(s) {
    const gpus = s.gpus || [];

    if (gpus.length === 0) {
        return {
            key: "gpu",
            title: "GPU",
            summary: "none detected",
            blocks: [block("", [row("", "No GPUs detected")])],
        };
    }

    const blocks = gpus.map((gpu) => block(
        fmt.formatValue(gpu.name),
        [
            row("Driver", fmt.formatValue(gpu.driver_version)),
            row("VBIOS", fmt.formatValue(gpu.vbios_version)),
            row("Utilisation", fmt.formatPercent(gpu.gpu_util_percent)),
            row("Memory", fmt.formatBytes((gpu.memory_used_mib || 0) * 1024 * 1024) + " / " +
                fmt.formatBytes((gpu.memory_total_mib || 0) * 1024 * 1024) +
                " (" + fmt.formatPercent(gpu.memory_percent) + ")"),
            row("Encoder/Decoder", fmt.formatPercent(gpu.encoder_percent) + " / " +
                fmt.formatPercent(gpu.decoder_percent)),
            row("Temperature", fmt.formatTemp(gpu.temperature_gpu)),
            row("Fan", fmt.formatPercent(gpu.fan_speed_percent)),
            row("Power", fmt.formatValue((gpu.power_draw_w || 0).toFixed(1), "W") + " / " +
                fmt.formatValue((gpu.power_limit_w || 0).toFixed(1), "W")),
            row("Perf state", fmt.formatValue(gpu.perf_state)),
            row("Compute mode", fmt.formatValue(gpu.compute_mode)),
        ]
    ));

    return {
        key: "gpu",
        title: "GPU",
        summary: fmt.formatValue(gpus[0].name) + " · " + fmt.formatPercent(gpus[0].gpu_util_percent),
        blocks: blocks,
    };
}

function memorySection(s) {
    const mem = s.memory || {};

    const blocks = [
        block("", [
            row("Total", fmt.formatBytes(mem.total_bytes)),
            row("Used", fmt.formatBytes(mem.used_bytes) + " (" + fmt.formatPercent(mem.used_percent) + ")"),
            row("Available", fmt.formatBytes(mem.available_bytes)),
            row("Free", fmt.formatBytes(mem.free_bytes)),
            row("Buffers", fmt.formatBytes(mem.buffers_bytes)),
            row("Cached", fmt.formatBytes(mem.cached_bytes)),
            row("Shared", fmt.formatBytes(mem.shared_bytes)),
            row("Slab", fmt.formatBytes(mem.slab_bytes)),
        ]),
        block("Swap", [
            row("Total", fmt.formatBytes(mem.swap_total_bytes)),
            row("Used", fmt.formatBytes(mem.swap_used_bytes) + " (" + fmt.formatPercent(mem.swap_percent) + ")"),
            row("Free", fmt.formatBytes(mem.swap_free_bytes)),
        ]),
    ];

    const dimms = mem.dimms || [];
    for (let i = 0; i < dimms.length; i++) {
        const dimm = dimms[i];
        blocks.push(block(
            fmt.formatValue(dimm.location) + " (" + fmt.formatBytes(dimm.size_bytes) + ")",
            [
                row("Manufacturer", fmt.formatValue(dimm.manufacturer)),
                row("Part number", fmt.formatValue(dimm.part_number)),
                row("Type", fmt.formatValue(dimm.type) + " " + fmt.formatValue(dimm.form_factor)),
                row("Speed", fmt.formatValue(dimm.speed_mts, "MT/s") + " (configured " +
                    fmt.formatValue(dimm.configured_speed_mts, "MT/s") + ")"),
                row("Width", fmt.formatValue(dimm.data_width_bits, "bit") + " data, " +
                    fmt.formatValue(dimm.total_width_bits, "bit") + " total"),
                row("Rank", fmt.formatValue(dimm.rank)),
                row("Voltage", fmt.formatValue(dimm.configured_voltage, "V")),
                row("Temperature", fmt.formatTemp(dimm.temperature)),
                row("Bank", fmt.formatValue(dimm.bank_locator)),
            ]
        ));
    }

    return {
        key: "memory",
        title: "Memory",
        summary: fmt.formatBytes(mem.used_bytes) + " / " + fmt.formatBytes(mem.total_bytes) +
            " (" + fmt.formatPercent(mem.used_percent) + ")",
        blocks: blocks,
    };
}

function disksSection(s) {
    const disks = s.disks || [];
    const usage = snap.diskUsage(disks);

    const blocks = disks.map((disk) => {
        const rows = [
            row("Model", fmt.formatValue(disk.model)),
            row("Vendor", fmt.formatValue(disk.vendor)),
            row("Serial", fmt.formatValue(disk.serial)),
            row("Firmware", fmt.formatValue(disk.firmware_version)),
            row("Type", fmt.formatValue(disk.drive_type) +
                (disk.rotational ? " (rotational)" : " (solid state)")),
            row("Transport", fmt.formatValue(disk.transport) + " via " + fmt.formatValue(disk.controller)),
            row("Capacity", fmt.formatBytes(disk.size_bytes)),
            row("Filesystem", fmt.formatBytes(disk.used_bytes) + " / " + fmt.formatBytes(disk.total_bytes) +
                " (" + fmt.formatPercent(disk.used_percent) + ")"),
            row("I/O rate", "read " + fmt.formatRate(disk.read_bytes_rate) +
                ", write " + fmt.formatRate(disk.write_bytes_rate)),
            row("I/O total", "read " + fmt.formatBytes(disk.read_bytes) +
                ", write " + fmt.formatBytes(disk.write_bytes)),
            row("Queue", fmt.formatValue(disk.queue_length) + " in flight, avg " +
                fmt.formatValue((disk.avg_queue_length || 0).toFixed(2))),
            row("Utilisation", fmt.formatPercent(disk.util_percent)),
            row("Peaks", "read " + fmt.formatRate(disk.peak_read_bytes_rate) +
                ", write " + fmt.formatRate(disk.peak_write_bytes_rate) +
                ", queue " + fmt.formatValue(disk.peak_queue_length) +
                ", util " + fmt.formatPercent(disk.peak_util_percent)),
        ];

        if (disk.smart_enabled) {
            rows.push(row("SMART", disk.smart_healthy ? "healthy" : "FAILING"));
            rows.push(row("Temperature", fmt.formatTemp(disk.temperature_celsius)));
            rows.push(row("Power on", fmt.formatValue(disk.power_on_hours, "h") + ", " +
                fmt.formatValue(disk.power_cycles) + " cycles"));
        }

        // NVMe health is only meaningful when the device reported a wear level.
        if (disk.life_remaining_percent) {
            rows.push(row("Life remaining", fmt.formatValue(disk.life_remaining_percent, "%")));
            rows.push(row("Spare", fmt.formatValue(disk.available_spare_percent, "%") +
                " (threshold " + fmt.formatValue(disk.spare_threshold_percent, "%") + ")"));
            rows.push(row("Errors", fmt.formatValue(disk.media_errors) + " media, " +
                fmt.formatValue(disk.error_log_entries) + " log entries"));
            rows.push(row("Unsafe shutdowns", fmt.formatValue(disk.unsafe_shutdowns)));
        }

        const partitions = disk.partitions || [];
        for (let i = 0; i < partitions.length; i++) {
            const part = partitions[i];
            rows.push(row(
                "  " + fmt.formatValue(part.name),
                fmt.formatValue(part.mountpoint) + "  " + fmt.formatValue(part.fstype) +
                    "  " + fmt.formatBytes(part.used_bytes) + " / " + fmt.formatBytes(part.total_bytes)
            ));
        }

        return block(fmt.formatValue(disk.name), rows);
    });

    return {
        key: "disks",
        title: "Disks",
        summary: disks.length + " device(s) · " + fmt.formatPercent(usage.percent) + " used",
        blocks: blocks.length > 0 ? blocks : [block("", [row("", "No disks detected")])],
    };
}

function networkSection(s) {
    const networks = s.networks || [];
    const rates = snap.netRates(networks);

    const blocks = networks.map((iface) => {
        const rows = [
            row("Kind", fmt.formatValue(iface.kind)),
            row("State", (iface.is_up ? "up" : "down") + " / " + fmt.formatValue(iface.oper_state)),
            row("MAC", fmt.formatValue(iface.hardware_addr)),
            row("Addresses", fmt.formatList(iface.addresses)),
            row("MTU", fmt.formatValue(iface.mtu)),
            row("Driver", fmt.formatValue(iface.driver)),
            row("Link", fmt.formatValue(iface.speed_mbps, "Mbps") + " " + fmt.formatValue(iface.duplex)),
            row("DNS", fmt.formatList(iface.dns_servers)),
            row("DNS search", fmt.formatList(iface.dns_search)),
            row("Throughput", "tx " + fmt.formatBits(iface.bytes_sent_rate) +
                ", rx " + fmt.formatBits(iface.bytes_recv_rate)),
            row("Transferred", "tx " + fmt.formatBytes(iface.bytes_sent) +
                ", rx " + fmt.formatBytes(iface.bytes_recv)),
            row("Packets", "tx " + fmt.formatValue(iface.packets_sent) +
                ", rx " + fmt.formatValue(iface.packets_recv)),
            row("Errors", "in " + fmt.formatValue(iface.errors_in) +
                ", out " + fmt.formatValue(iface.errors_out)),
            row("Drops", "in " + fmt.formatValue(iface.drops_in) +
                ", out " + fmt.formatValue(iface.drops_out)),
            row("Flags", fmt.formatList(iface.flags)),
        ];

        if (iface.bridge_ports && iface.bridge_ports.length > 0) {
            rows.push(row("Bridge ports", fmt.formatList(iface.bridge_ports)));
        }
        if (iface.bond_mode) {
            rows.push(row("Bond mode", fmt.formatValue(iface.bond_mode)));
            rows.push(row("Bond slaves", fmt.formatList(iface.bond_slaves)));
        }
        if (iface.bond_master) {
            rows.push(row("Bond master", fmt.formatValue(iface.bond_master)));
        }
        if (iface.wireless) {
            const w = iface.wireless;
            rows.push(row("SSID", fmt.formatValue(w.ssid)));
            rows.push(row("BSSID", fmt.formatValue(w.bssid)));
            rows.push(row("Signal", fmt.formatValue(w.signal_dbm, "dBm") +
                " (" + fmt.formatPercent(w.quality_percent) + ")"));
            rows.push(row("Frequency", fmt.formatValue(w.frequency_mhz, "MHz") +
                " channel " + fmt.formatValue(w.channel)));
            rows.push(row("Bitrate", fmt.formatValue(w.bitrate_mbps, "Mbps")));
        }

        return block(fmt.formatValue(iface.name), rows);
    });

    return {
        key: "network",
        title: "Network",
        summary: networks.length + " interface(s) · ↑" + fmt.formatBits(rates.sent) +
            " ↓" + fmt.formatBits(rates.recv),
        blocks: blocks.length > 0 ? blocks : [block("", [row("", "No interfaces detected")])],
    };
}

function sensorsSection(s) {
    const sensors = s.sensors || {};
    const coreTemps = sensors.core_temps || [];
    const zones = sensors.thermal_zones || [];
    const fans = sensors.fans || [];
    const power = sensors.package_power || [];
    const throttle = sensors.thermal_throttle || [];
    const blocks = [];

    if (coreTemps.length > 0) {
        blocks.push(block("Core temperatures", coreTemps.map((t) =>
            row(
                label(t.label, "core", t.core_id),
                fmt.formatTemp(t.temp_celsius) +
                    (t.high_celsius ? "  (high " + fmt.formatTemp(t.high_celsius) + ")" : "") +
                    (t.crit_celsius ? "  (crit " + fmt.formatTemp(t.crit_celsius) + ")" : "")
            )
        )));
    }
    if (power.length > 0) {
        blocks.push(block("Package power", power.map((p) =>
            row(
                label(p.package_name, "package", null),
                fmt.formatValue((p.power_w || 0).toFixed(1), "W") +
                    (p.max_power_w ? "  of " + p.max_power_w.toFixed(1) + " W" : "")
            )
        )));
    }
    if (zones.length > 0) {
        blocks.push(block("Thermal zones", zones.map((z) =>
            row(label(z.type || z.name, "zone", null), fmt.formatTemp(z.temp_celsius))
        )));
    }
    if (fans.length > 0) {
        blocks.push(block("Fans", fans.map((f) =>
            row(
                label(f.label || f.hwmon_name, "fan", null),
                fmt.formatValue(f.rpm, "rpm") +
                    (f.max_rpm ? "  (max " + f.max_rpm + ")" : "")
            )
        )));
    }
    if (throttle.length > 0) {
        blocks.push(block("Throttling", throttle.map((t) =>
            row(
                label(null, "cpu", t.cpu),
                fmt.formatValue(t.core_throttle_count) + " core, " +
                    fmt.formatValue(t.package_throttle_count) + " package"
            )
        )));
    }

    const maxTemp = snap.maxCpuTemp(coreTemps);

    return {
        key: "sensors",
        title: "Sensors",
        summary: coreTemps.length > 0
            ? "max " + fmt.formatTemp(maxTemp) + " · " + zones.length + " zone(s)"
            : zones.length + " zone(s)",
        blocks: blocks.length > 0 ? blocks : [block("", [row("", "No sensors detected")])],
    };
}

function processesSection(s) {
    const summary = s.processes || {};
    const top = snap.topProcesses(summary.process_list, TOP_PROCESS_COUNT);

    const blocks = [
        block("", [
            row("Total", fmt.formatValue(summary.total)),
            row("Running", fmt.formatValue(summary.running)),
            row("Sleeping", fmt.formatValue(summary.sleeping)),
            row("Idle", fmt.formatValue(summary.idle)),
            row("Stopped", fmt.formatValue(summary.stopped)),
            row("Zombie", fmt.formatValue(summary.zombie)),
        ]),
    ];

    if (top.length > 0) {
        blocks.push(block("Top by CPU", top.map((proc) =>
            row(
                fmt.formatValue(proc.name),
                fmt.formatPercent(proc.cpu_percent) + "   " +
                    fmt.formatBytes(proc.memory_bytes) + "   " +
                    "pid " + fmt.formatValue(proc.pid) + "   " +
                    fmt.formatValue(proc.username)
            )
        )));
    }

    return {
        key: "processes",
        title: "Processes",
        summary: fmt.formatValue(summary.total) + " total · " +
            fmt.formatValue(summary.running) + " running",
        blocks: blocks,
    };
}

function virtSection(s) {
    const virt = s.virtualization || {};
    const containers = virt.containers || [];
    const vms = virt.vms || [];
    const runtime = virt.runtime || {};
    const blocks = [];

    blocks.push(block("Runtime", [
        row("Engine", fmt.formatValue(runtime.engine)),
        row("Version", fmt.formatValue(runtime.version)),
        row("Available", runtime.available ? "yes" : "no"),
        row("Socket", fmt.formatValue(runtime.socket_path)),
        row("Storage driver", fmt.formatValue(runtime.storage_driver)),
        row("Cgroup version", fmt.formatValue(virt.cgroup_version)),
        row("Runtimes", fmt.formatList(virt.runtimes)),
    ]));

    if (containers.length > 0) {
        blocks.push(block("Containers (" + containers.length + ")", containers.map((c) =>
            row(
                fmt.formatValue(c.name),
                fmt.formatPercent(c.cpu_percent) + "   " +
                    fmt.formatBytes(c.memory_bytes) + "   " +
                    fmt.formatValue(c.pids) + " pids   " +
                    fmt.formatValue(c.runtime)
            )
        )));
    }

    if (vms.length > 0) {
        blocks.push(block("Virtual machines (" + vms.length + ")", vms.map((vm) =>
            row(
                fmt.formatValue(vm.name),
                fmt.formatPercent(vm.cpu_percent) + "   " +
                    fmt.formatBytes(vm.memory_bytes) + "   " +
                    fmt.formatValue(vm.vcpus) + " vCPU   " +
                    fmt.formatValue(vm.hypervisor) + "/" + fmt.formatValue(vm.accelerator)
            )
        )));
    }

    const notes = (virt.capability || {}).notes || [];
    if (notes.length > 0) {
        blocks.push(block("Notes", notes.map((note) => row("", note))));
    }

    return {
        key: "virt",
        title: "Virtualisation",
        summary: containers.length + " container(s) · " + vms.length + " VM(s)",
        blocks: blocks,
    };
}

// SECTION_BUILDERS is ordered as the popup renders it, and mirrors the CLI's
// section order so the two read the same way.
const SECTION_BUILDERS = [
    hostSection,
    cpuSection,
    gpuSection,
    memorySection,
    disksSection,
    networkSection,
    sensorsSection,
    processesSection,
    virtSection,
];

/**
 * buildSections turns a snapshot into the popup's section model.
 * @param {object|null} snapshot - a parsed snapshot, or null when none has
 *   arrived yet
 * @returns {Array<object>} the sections, empty when there is no snapshot
 */
function buildSections(snapshot) {
    if (!snapshot || typeof snapshot !== "object") {
        return [];
    }
    return SECTION_BUILDERS.map((build) => build(snapshot));
}

module.exports = {
    TOP_PROCESS_COUNT,
    buildSections,
};
