// Unit tests for the popup section model.
//
// These are the parity tests: the popup must surface the same information the
// CLI and the web UI show. A field silently missing from a section is exactly
// the regression that is invisible in a screenshot.

const test = require("node:test");
const assert = require("node:assert");

const sections = require("../shared/sections.js");
// The web dashboard's Playwright fixture is the single source of truth for a
// representative snapshot; sharing it keeps the two suites from drifting.
const fixture = require("../../cmd/sysmon/frontend/tests/fixtures/snapshot.json");

/** build returns the section model for the fixture snapshot. */
function build(overrides) {
    return sections.buildSections(Object.assign({}, fixture, overrides || {}));
}

/** sectionByKey finds one section in a model. */
function sectionByKey(model, key) {
    const found = model.filter((s) => s.key === key);
    assert.strictEqual(found.length, 1, `expected exactly one ${key} section`);
    return found[0];
}

/** allText flattens every label and value in a section into one string. */
function allText(section) {
    return section.blocks
        .map((b) => b.title + " " + b.rows.map((r) => r.label + " " + r.value).join(" "))
        .join(" ");
}

/** labels returns every row label in a section. */
function labels(section) {
    return section.blocks.reduce((acc, b) => acc.concat(b.rows.map((r) => r.label)), []);
}

// ---- structure -------------------------------------------------------------

test("every section the CLI covers is present, in order", () => {
    const model = build();

    assert.deepStrictEqual(
        model.map((s) => s.key),
        ["host", "cpu", "gpu", "memory", "disks", "network", "sensors", "processes", "virt"]
    );
});

test("every section carries a title and a non-empty summary", () => {
    for (const section of build()) {
        assert.ok(section.title, `${section.key} has no title`);
        assert.ok(section.summary, `${section.key} has no summary`);
        assert.ok(Array.isArray(section.blocks), `${section.key} has no blocks`);
        assert.ok(section.blocks.length > 0, `${section.key} has no content`);
    }
});

test("no rendered value is ever the string undefined or null", () => {
    // Absent fields must render as a dash; anything else is a formatting bug
    // that only shows up on hardware that does not report the field.
    for (const section of build()) {
        const text = allText(section);
        assert.ok(!text.includes("undefined"), `${section.key} renders "undefined": ${text}`);
        assert.ok(!text.includes("null"), `${section.key} renders "null": ${text}`);
        assert.ok(!text.includes("NaN"), `${section.key} renders "NaN": ${text}`);
    }
});

test("buildSections returns nothing before the first snapshot arrives", () => {
    assert.deepStrictEqual(sections.buildSections(null), []);
    assert.deepStrictEqual(sections.buildSections(undefined), []);
    assert.deepStrictEqual(sections.buildSections("not an object"), []);
});

test("an entirely empty snapshot still produces every section", () => {
    // A server that reports nothing must not collapse the menu; each section
    // renders its empty state instead.
    const model = sections.buildSections({});

    assert.strictEqual(model.length, 9);
    for (const section of model) {
        assert.ok(section.summary, `${section.key} has no summary for an empty snapshot`);
        const text = allText(section);
        assert.ok(!text.includes("undefined"), `${section.key} renders "undefined": ${text}`);
    }
});

// ---- host ------------------------------------------------------------------

test("host section reports identity, board and BIOS", () => {
    const host = sectionByKey(build(), "host");

    assert.deepStrictEqual(
        labels(host),
        ["Hostname", "OS", "Platform", "Kernel", "Uptime", "Vendor", "Model", "Version",
            "Vendor", "Version", "Date"]
    );
    assert.ok(allText(host).includes(fixture.host.hostname));
});

// ---- cpu -------------------------------------------------------------------

test("cpu section reports one row per core", () => {
    const cpu = sectionByKey(build(), "cpu");
    const perCore = cpu.blocks.filter((b) => b.title === "Per-core");

    assert.strictEqual(perCore.length, 1);
    assert.strictEqual(perCore[0].rows.length, fixture.cpus.length);
    assert.strictEqual(perCore[0].rows[0].label, "core" + fixture.cpus[0].index);
});

test("cpu section reports identity, topology and load average", () => {
    const cpu = sectionByKey(build(), "cpu");
    const rowLabels = labels(cpu);

    for (const want of ["Model", "Vendor", "Microcode", "Topology", "Cores/Threads",
        "Frequency", "Cache", "Usage", "Load average"]) {
        assert.ok(rowLabels.includes(want), `cpu section is missing the ${want} row`);
    }
});

test("cpu flags are listed and wrapped", () => {
    const flags = Array.from({ length: 20 }, (_, i) => "flag" + i);
    const model = build({ cpus: [Object.assign({}, fixture.cpus[0], { flags: flags })] });
    const cpu = sectionByKey(model, "cpu");

    const flagBlock = cpu.blocks.filter((b) => b.title.startsWith("Flags"))[0];
    assert.ok(flagBlock, "cpu section has no flags block");
    assert.ok(flagBlock.title.includes("20"), "flags block does not report the count");
    // 20 flags at 8 per line is three rows.
    assert.strictEqual(flagBlock.rows.length, 3);
    assert.ok(allText(cpu).includes("flag19"), "the last flag was dropped");
});

test("a CPU with no flags has no flags block", () => {
    const model = build({ cpus: [Object.assign({}, fixture.cpus[0], { flags: [] })] });
    const cpu = sectionByKey(model, "cpu");

    assert.strictEqual(cpu.blocks.filter((b) => b.title.startsWith("Flags")).length, 0);
});

// ---- gpu -------------------------------------------------------------------

test("gpu section reports one block per device", () => {
    const gpu = sectionByKey(build(), "gpu");

    assert.strictEqual(gpu.blocks.length, fixture.gpus.length);
    for (const want of ["Driver", "Utilisation", "Memory", "Temperature", "Power"]) {
        assert.ok(labels(gpu).includes(want), `gpu section is missing the ${want} row`);
    }
});

test("gpu section states plainly when there are no GPUs", () => {
    const gpu = sectionByKey(build({ gpus: [] }), "gpu");

    assert.strictEqual(gpu.summary, "none detected");
    assert.ok(allText(gpu).includes("No GPUs detected"));
});

// ---- memory ----------------------------------------------------------------

test("memory section reports usage, swap and every DIMM", () => {
    const memory = sectionByKey(build(), "memory");
    const rowLabels = labels(memory);

    for (const want of ["Total", "Used", "Available", "Free", "Buffers", "Cached",
        "Shared", "Slab"]) {
        assert.ok(rowLabels.includes(want), `memory section is missing the ${want} row`);
    }
    assert.ok(memory.blocks.some((b) => b.title === "Swap"), "no swap block");

    const dimms = fixture.memory.dimms || [];
    const dimmBlocks = memory.blocks.filter(
        (b) => b.rows.some((r) => r.label === "Part number")
    );
    assert.strictEqual(dimmBlocks.length, dimms.length);
});

// ---- disks -----------------------------------------------------------------

test("disks section reports identity, usage, I/O, queue and peaks per device", () => {
    const disks = sectionByKey(build(), "disks");

    assert.strictEqual(disks.blocks.length, fixture.disks.length);
    const rowLabels = labels(disks);
    for (const want of ["Model", "Serial", "Firmware", "Type", "Transport", "Capacity",
        "Filesystem", "I/O rate", "I/O total", "Queue", "Utilisation", "Peaks"]) {
        assert.ok(rowLabels.includes(want), `disks section is missing the ${want} row`);
    }
});

test("disks section reports SMART only when the device supports it", () => {
    const withSmart = build({
        disks: [Object.assign({}, fixture.disks[0], { smart_enabled: true, smart_healthy: true })],
    });
    assert.ok(allText(sectionByKey(withSmart, "disks")).includes("healthy"));

    const withoutSmart = build({
        disks: [Object.assign({}, fixture.disks[0], { smart_enabled: false })],
    });
    assert.ok(!labels(sectionByKey(withoutSmart, "disks")).includes("SMART"));
});

test("a failing SMART status is stated unambiguously", () => {
    const failing = build({
        disks: [Object.assign({}, fixture.disks[0], { smart_enabled: true, smart_healthy: false })],
    });

    assert.ok(allText(sectionByKey(failing, "disks")).includes("FAILING"));
});

test("partitions are listed under their disk", () => {
    const disk = Object.assign({}, fixture.disks[0], {
        partitions: [{ name: "nvme0n1p1", mountpoint: "/boot", fstype: "vfat", total_bytes: 100, used_bytes: 50 }],
    });
    const disks = sectionByKey(build({ disks: [disk] }), "disks");

    assert.ok(allText(disks).includes("nvme0n1p1"));
    assert.ok(allText(disks).includes("/boot"));
});

test("disks section states plainly when there are no disks", () => {
    const disks = sectionByKey(build({ disks: [] }), "disks");

    assert.ok(allText(disks).includes("No disks detected"));
});

// ---- network ---------------------------------------------------------------

test("network section reports one block per interface with addressing and counters", () => {
    const network = sectionByKey(build(), "network");

    assert.strictEqual(network.blocks.length, fixture.networks.length);
    const rowLabels = labels(network);
    for (const want of ["Kind", "State", "MAC", "Addresses", "MTU", "Driver", "Link",
        "DNS", "Throughput", "Packets", "Errors", "Drops", "Flags"]) {
        assert.ok(rowLabels.includes(want), `network section is missing the ${want} row`);
    }
});

test("bridge, bond and wireless details appear only on the relevant interface", () => {
    const model = build({
        networks: [
            { name: "br0", kind: "bridge", bridge_ports: ["eth0", "eth1"] },
            { name: "bond0", kind: "bond", bond_mode: "802.3ad", bond_slaves: ["eth2"] },
            { name: "eth2", kind: "ethernet", bond_master: "bond0" },
            {
                name: "wlan0",
                kind: "wifi",
                wireless: { ssid: "homenet", bssid: "aa:bb", signal_dbm: -45, quality_percent: 80, frequency_mhz: 5180, channel: 36, bitrate_mbps: 866 },
            },
        ],
    });
    const network = sectionByKey(model, "network");
    const text = allText(network);

    assert.ok(text.includes("Bridge ports"), "bridge ports missing");
    assert.ok(text.includes("802.3ad"), "bond mode missing");
    assert.ok(text.includes("Bond master"), "bond master missing");
    assert.ok(text.includes("homenet"), "wireless SSID missing");
    assert.ok(text.includes("866"), "wireless bitrate missing");

    // A plain ethernet interface must not sprout wireless rows.
    const ethBlock = network.blocks.filter((b) => b.title === "eth2")[0];
    assert.ok(!ethBlock.rows.some((r) => r.label === "SSID"));
});

test("network section states plainly when there are no interfaces", () => {
    const network = sectionByKey(build({ networks: [] }), "network");

    assert.ok(allText(network).includes("No interfaces detected"));
});

// ---- sensors ---------------------------------------------------------------

test("sensors section reports temperatures, power, zones and fans", () => {
    const sensors = sectionByKey(build(), "sensors");
    const titles = sensors.blocks.map((b) => b.title);

    if ((fixture.sensors.core_temps || []).length > 0) {
        assert.ok(titles.includes("Core temperatures"));
    }
    if ((fixture.sensors.package_power || []).length > 0) {
        assert.ok(titles.includes("Package power"));
    }
    if ((fixture.sensors.thermal_zones || []).length > 0) {
        assert.ok(titles.includes("Thermal zones"));
    }
});

test("sensors section states plainly when nothing reports", () => {
    const sensors = sectionByKey(build({ sensors: {} }), "sensors");

    assert.ok(allText(sensors).includes("No sensors detected"));
});

// ---- processes -------------------------------------------------------------

test("processes section reports the state counts and the top consumers", () => {
    const processes = sectionByKey(build(), "processes");
    const rowLabels = labels(processes);

    for (const want of ["Total", "Running", "Sleeping", "Idle", "Stopped", "Zombie"]) {
        assert.ok(rowLabels.includes(want), `processes section is missing the ${want} row`);
    }
});

test("the process block is capped so the menu cannot grow without bound", () => {
    const many = Array.from({ length: 500 }, (_, i) => ({
        pid: i,
        name: "proc" + i,
        username: "root",
        cpu_percent: i,
        memory_bytes: 1024,
    }));
    const model = build({ processes: { total: 500, process_list: many } });
    const processes = sectionByKey(model, "processes");

    const topBlock = processes.blocks.filter((b) => b.title === "Top by CPU")[0];
    assert.strictEqual(topBlock.rows.length, sections.TOP_PROCESS_COUNT);
    // Sorted descending, so the busiest process is first.
    assert.strictEqual(topBlock.rows[0].label, "proc499");
});

test("a snapshot with no process list omits the top block rather than failing", () => {
    const processes = sectionByKey(build({ processes: { total: 0 } }), "processes");

    assert.strictEqual(processes.blocks.filter((b) => b.title === "Top by CPU").length, 0);
});

// ---- virtualisation --------------------------------------------------------

test("virtualisation section reports the runtime, containers and VMs", () => {
    const virt = sectionByKey(build(), "virt");
    const titles = virt.blocks.map((b) => b.title);

    assert.ok(titles.includes("Runtime"));
    if ((fixture.virtualization.containers || []).length > 0) {
        assert.ok(titles.some((t) => t.startsWith("Containers")));
    }
    if ((fixture.virtualization.vms || []).length > 0) {
        assert.ok(titles.some((t) => t.startsWith("Virtual machines")));
    }
});

test("virtualisation section survives a host with no containers or VMs", () => {
    const virt = sectionByKey(build({ virtualization: {} }), "virt");

    assert.strictEqual(virt.summary, "0 container(s) · 0 VM(s)");
    assert.ok(virt.blocks.map((b) => b.title).includes("Runtime"));
});

test("capability notes are surfaced when the collector reports them", () => {
    const model = build({
        virtualization: { capability: { notes: ["docker socket not readable"] } },
    });
    const virt = sectionByKey(model, "virt");

    assert.ok(allText(virt).includes("docker socket not readable"));
});
