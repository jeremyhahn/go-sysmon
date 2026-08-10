// Unit tests for the shared formatting helpers.
// Run with: node --test extensions/tests/

const test = require("node:test");
const assert = require("node:assert");

const fmt = require("../shared/format.js");

test("formatBytes scales through every unit", () => {
    assert.strictEqual(fmt.formatBytes(0), "0 B");
    assert.strictEqual(fmt.formatBytes(512), "512 B");
    assert.strictEqual(fmt.formatBytes(1024), "1.0 KB");
    assert.strictEqual(fmt.formatBytes(1536), "1.5 KB");
    assert.strictEqual(fmt.formatBytes(1024 * 1024), "1.0 MB");
    assert.strictEqual(fmt.formatBytes(1024 * 1024 * 1024), "1.0 GB");
    assert.strictEqual(fmt.formatBytes(1024 * 1024 * 1024 * 1024), "1.0 TB");
});

test("formatBytes treats absent values as zero rather than NaN", () => {
    assert.strictEqual(fmt.formatBytes(undefined), "0 B");
    assert.strictEqual(fmt.formatBytes(null), "0 B");
    assert.strictEqual(fmt.formatBytes(NaN), "0 B");
});

test("formatRate appends a per-second suffix", () => {
    assert.strictEqual(fmt.formatRate(1024), "1.0 KB/s");
    assert.strictEqual(fmt.formatRate(0), "0 B/s");
});

test("formatBits reports network rates in bits", () => {
    // 125 MB/s is a saturated gigabit link.
    assert.strictEqual(fmt.formatBits(125000000), "1.00 Gbps");
    assert.strictEqual(fmt.formatBits(125000), "1.00 Mbps");
    assert.strictEqual(fmt.formatBits(125), "1.0 Kbps");
    assert.strictEqual(fmt.formatBits(10), "80 bps");
});

test("formatBits handles absent and zero rates", () => {
    assert.strictEqual(fmt.formatBits(0), "0 bps");
    assert.strictEqual(fmt.formatBits(undefined), "0 bps");
    assert.strictEqual(fmt.formatBits(NaN), "0 bps");
});

test("formatCompactRate stays short enough for a panel", () => {
    assert.strictEqual(fmt.formatCompactRate(0), "0B");
    assert.strictEqual(fmt.formatCompactRate(2048), "2K");
    assert.strictEqual(fmt.formatCompactRate(1024 * 1024 * 1.5), "1.5M");
    assert.strictEqual(fmt.formatCompactRate(1024 * 1024 * 1024 * 2), "2.0G");

    // Every output must fit the panel's narrow text column.
    for (const value of [0, 999, 1e6, 1e9, 1e12]) {
        assert.ok(
            fmt.formatCompactRate(value).length <= 6,
            `formatCompactRate(${value}) = ${fmt.formatCompactRate(value)} is too wide`
        );
    }
});

test("formatCompactRate rejects negative and absent rates", () => {
    assert.strictEqual(fmt.formatCompactRate(-1), "0B");
    assert.strictEqual(fmt.formatCompactRate(undefined), "0B");
    assert.strictEqual(fmt.formatCompactRate(NaN), "0B");
});

test("formatPercent renders one decimal place", () => {
    assert.strictEqual(fmt.formatPercent(0), "0.0%");
    // 42.35 is not exactly representable, and toFixed rounds the stored value
    // up rather than the decimal literal down.
    assert.strictEqual(fmt.formatPercent(42.35), "42.4%");
    assert.strictEqual(fmt.formatPercent(42.31), "42.3%");
    assert.strictEqual(fmt.formatPercent(100), "100.0%");
});

test("formatPercent falls back to zero for absent values", () => {
    assert.strictEqual(fmt.formatPercent(undefined), "0.0%");
    assert.strictEqual(fmt.formatPercent(null), "0.0%");
    assert.strictEqual(fmt.formatPercent(NaN), "0.0%");
});

test("formatTemp distinguishes a missing reading from a real one", () => {
    assert.strictEqual(fmt.formatTemp(65.4), "65°C");
    assert.strictEqual(fmt.formatTemp(0), fmt.DASH);
    assert.strictEqual(fmt.formatTemp(undefined), fmt.DASH);
    assert.strictEqual(fmt.formatTemp(NaN), fmt.DASH);
});

test("formatMHz switches to GHz at a thousand", () => {
    assert.strictEqual(fmt.formatMHz(800), "800 MHz");
    assert.strictEqual(fmt.formatMHz(999), "999 MHz");
    assert.strictEqual(fmt.formatMHz(1000), "1.0 GHz");
    assert.strictEqual(fmt.formatMHz(3500), "3.5 GHz");
});

test("formatMHz handles absent frequencies", () => {
    assert.strictEqual(fmt.formatMHz(undefined), "0 MHz");
    assert.strictEqual(fmt.formatMHz(NaN), "0 MHz");
});

test("formatDuration renders days, hours and minutes", () => {
    assert.strictEqual(fmt.formatDuration(90), "1m");
    // Zero components are omitted, so a whole hour reads "1h", not "1h 0m".
    assert.strictEqual(fmt.formatDuration(3600), "1h");
    assert.strictEqual(fmt.formatDuration(86400), "1d");
    assert.strictEqual(fmt.formatDuration(90061), "1d 1h 1m");
    assert.strictEqual(fmt.formatDuration(86460), "1d 1m");
});

test("formatDuration never renders an empty string for a short uptime", () => {
    // A machine up for ten seconds must still show something.
    assert.strictEqual(fmt.formatDuration(10), "0m");
});

test("formatDuration rejects absent and negative spans", () => {
    assert.strictEqual(fmt.formatDuration(0), fmt.DASH);
    assert.strictEqual(fmt.formatDuration(-5), fmt.DASH);
    assert.strictEqual(fmt.formatDuration(undefined), fmt.DASH);
    assert.strictEqual(fmt.formatDuration(NaN), fmt.DASH);
});

test("formatValue substitutes a dash rather than printing undefined", () => {
    assert.strictEqual(fmt.formatValue("x1carbon"), "x1carbon");
    assert.strictEqual(fmt.formatValue(42), "42");
    assert.strictEqual(fmt.formatValue(0), "0");
    assert.strictEqual(fmt.formatValue(undefined), fmt.DASH);
    assert.strictEqual(fmt.formatValue(null), fmt.DASH);
    assert.strictEqual(fmt.formatValue(""), fmt.DASH);
    assert.strictEqual(fmt.formatValue(NaN), fmt.DASH);
});

test("formatValue appends a unit only to present values", () => {
    assert.strictEqual(fmt.formatValue(1600, "MT/s"), "1600 MT/s");
    assert.strictEqual(fmt.formatValue(undefined, "MT/s"), fmt.DASH);
});

test("formatList joins values and dashes an empty list", () => {
    assert.strictEqual(fmt.formatList(["a", "b"]), "a, b");
    assert.strictEqual(fmt.formatList(["a", "b"], " "), "a b");
    assert.strictEqual(fmt.formatList([]), fmt.DASH);
    assert.strictEqual(fmt.formatList(undefined), fmt.DASH);
});
