// Unit tests for the shared snapshot accessors.

const test = require("node:test");
const assert = require("node:assert");

const snap = require("../shared/snapshot.js");

test("parseSnapshot returns the parsed object", () => {
    const result = snap.parseSnapshot('{"host":{"hostname":"x1"}}');

    assert.strictEqual(result.host.hostname, "x1");
});

test("parseSnapshot returns null for anything that is not an object", () => {
    // A truncated stream, a bare scalar and an array are all "valid JSON" in
    // some sense but none of them is a snapshot.
    assert.strictEqual(snap.parseSnapshot('{"a":'), null);
    assert.strictEqual(snap.parseSnapshot("not json"), null);
    assert.strictEqual(snap.parseSnapshot("42"), null);
    assert.strictEqual(snap.parseSnapshot('"text"'), null);
    assert.strictEqual(snap.parseSnapshot("null"), null);
    assert.strictEqual(snap.parseSnapshot("[1,2]"), null);
    assert.strictEqual(snap.parseSnapshot(""), null);
    assert.strictEqual(snap.parseSnapshot(undefined), null);
});

test("avgCpuUsage averages across cores", () => {
    const cpus = [{ usage_percent: 20 }, { usage_percent: 40 }, { usage_percent: 60 }];

    assert.strictEqual(snap.avgCpuUsage(cpus), 40);
});

test("avgCpuUsage treats a missing reading as zero", () => {
    assert.strictEqual(snap.avgCpuUsage([{ usage_percent: 50 }, {}]), 25);
});

test("avgCpuUsage returns zero for an empty or absent list", () => {
    assert.strictEqual(snap.avgCpuUsage([]), 0);
    assert.strictEqual(snap.avgCpuUsage(undefined), 0);
    assert.strictEqual(snap.avgCpuUsage(null), 0);
});

test("maxCpuTemp returns the hottest core", () => {
    const temps = [{ temp_celsius: 45 }, { temp_celsius: 71 }, { temp_celsius: 52 }];

    assert.strictEqual(snap.maxCpuTemp(temps), 71);
});

test("maxCpuTemp returns zero when no cores report", () => {
    assert.strictEqual(snap.maxCpuTemp([]), 0);
    assert.strictEqual(snap.maxCpuTemp(undefined), 0);
    assert.strictEqual(snap.maxCpuTemp([{}]), 0);
});

test("totalNetRate sums both directions across interfaces", () => {
    const networks = [
        { bytes_sent_rate: 100, bytes_recv_rate: 200 },
        { bytes_sent_rate: 50, bytes_recv_rate: 25 },
    ];

    assert.strictEqual(snap.totalNetRate(networks), 375);
});

test("totalNetRate returns zero for an empty or absent list", () => {
    assert.strictEqual(snap.totalNetRate([]), 0);
    assert.strictEqual(snap.totalNetRate(undefined), 0);
});

test("netRates separates the directions and skips loopback", () => {
    // Loopback traffic is not traffic leaving the machine, so counting it
    // makes an idle host look busy.
    const networks = [
        { name: "lo", is_loopback: true, bytes_sent_rate: 9999, bytes_recv_rate: 9999 },
        { name: "eth0", bytes_sent_rate: 100, bytes_recv_rate: 200 },
        { name: "wlan0", bytes_sent_rate: 5, bytes_recv_rate: 10 },
    ];

    assert.deepStrictEqual(snap.netRates(networks), { sent: 105, recv: 210 });
});

test("netRates returns zeroes for an empty or absent list", () => {
    assert.deepStrictEqual(snap.netRates([]), { sent: 0, recv: 0 });
    assert.deepStrictEqual(snap.netRates(undefined), { sent: 0, recv: 0 });
});

test("diskUsage aggregates capacity and computes a percentage", () => {
    const disks = [
        { total_bytes: 100, used_bytes: 50 },
        { total_bytes: 100, used_bytes: 30 },
    ];

    assert.deepStrictEqual(snap.diskUsage(disks), { total: 200, used: 80, percent: 40 });
});

test("diskUsage does not divide by zero when no disk reports capacity", () => {
    assert.deepStrictEqual(snap.diskUsage([{}, {}]), { total: 0, used: 0, percent: 0 });
    assert.deepStrictEqual(snap.diskUsage(undefined), { total: 0, used: 0, percent: 0 });
});

test("diskIORates sums read and write throughput", () => {
    const disks = [
        { read_bytes_rate: 10, write_bytes_rate: 20 },
        { read_bytes_rate: 5, write_bytes_rate: 1 },
    ];

    assert.deepStrictEqual(snap.diskIORates(disks), { read: 15, write: 21 });
});

test("diskIORates returns zeroes for an absent list", () => {
    assert.deepStrictEqual(snap.diskIORates(undefined), { read: 0, write: 0 });
});

test("topProcesses sorts by CPU descending and honours the limit", () => {
    const processes = [
        { name: "a", cpu_percent: 5 },
        { name: "b", cpu_percent: 50 },
        { name: "c", cpu_percent: 20 },
    ];

    const top = snap.topProcesses(processes, 2);

    assert.deepStrictEqual(top.map((p) => p.name), ["b", "c"]);
});

test("topProcesses does not modify the caller's array", () => {
    // The applet holds the snapshot it was given; sorting in place would
    // silently reorder what every other reader sees.
    const processes = [
        { name: "a", cpu_percent: 5 },
        { name: "b", cpu_percent: 50 },
    ];

    snap.topProcesses(processes, 2);

    assert.deepStrictEqual(processes.map((p) => p.name), ["a", "b"]);
});

test("topProcesses handles absent, empty and over-long requests", () => {
    assert.deepStrictEqual(snap.topProcesses(undefined, 5), []);
    assert.deepStrictEqual(snap.topProcesses([], 5), []);
    assert.deepStrictEqual(snap.topProcesses([{ name: "a" }], 0), []);
    assert.deepStrictEqual(snap.topProcesses([{ name: "a" }], -1), []);
    assert.strictEqual(snap.topProcesses([{ name: "a" }], 99).length, 1);
});

test("usageLevel classifies the three bands", () => {
    assert.strictEqual(snap.usageLevel(0), "normal");
    assert.strictEqual(snap.usageLevel(0.49), "normal");
    assert.strictEqual(snap.usageLevel(0.5), "warn");
    assert.strictEqual(snap.usageLevel(0.79), "warn");
    assert.strictEqual(snap.usageLevel(0.8), "critical");
    assert.strictEqual(snap.usageLevel(1), "critical");
});

test("usageLevel treats absent input as normal", () => {
    assert.strictEqual(snap.usageLevel(undefined), "normal");
    assert.strictEqual(snap.usageLevel(NaN), "normal");
    assert.strictEqual(snap.usageLevel(-1), "normal");
});
