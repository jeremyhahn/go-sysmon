import { test, expect } from "@playwright/test";
import { searchHistory, hitKey, MAX_HITS, type Resources } from "../src/lib/search";
import type { HistoryMetric } from "../src/lib/types";

// The catalog as the server publishes it, trimmed to the cases that matter.
const catalog: HistoryMetric[] = [
  { key: "cpu", label: "CPU", unit: "percent", split_by: "" },
  { key: "memory", label: "Memory", unit: "percent", split_by: "" },
  { key: "swap", label: "Swap", unit: "percent", split_by: "" },
  { key: "disk_util", label: "Disk utilisation", unit: "percent", split_by: "device" },
  { key: "disk_read", label: "Disk read", unit: "bytes_per_second", split_by: "device" },
  { key: "net_rx", label: "Network in", unit: "bytes_per_second", split_by: "interface" },
  { key: "gpu_util", label: "GPU utilisation", unit: "percent", split_by: "gpu" },
];

const resources: Resources = {
  device: ["sda", "sdb", "nvme0n1"],
  interface: ["eth0", "lo"],
  gpu: ["0"],
};

test.describe("history search", () => {
  test("a resource name finds every metric about that resource", () => {
    const hits = searchHistory("sda", catalog, resources);
    const titles = hits.map((h) => h.title);

    expect(titles).toContain("Disk utilisation \u2014 sda");
    expect(titles).toContain("Disk read \u2014 sda");
    // Another drive is not this drive.
    expect(titles.some((t) => t.includes("sdb"))).toBe(false);
    // And nothing about the network came back for a disk.
    expect(titles.some((t) => t.includes("eth0"))).toBe(false);
  });

  test("an exact resource ranks above a partial one", () => {
    // "sda" is exactly one device and a prefix of none; "sd" is a prefix of two
    // and exactly none, so the ordering is only visible with a query that is
    // both.
    const hits = searchHistory("nvme0n1", catalog, { device: ["nvme0n1p1", "nvme0n1"] });
    expect(hits[0].resource).toBe("nvme0n1");
  });

  test("a metric name finds the metric on every resource it has", () => {
    const hits = searchHistory("disk read", catalog, resources);
    expect(hits.map((h) => h.resource)).toEqual(["sda", "sdb", "nvme0n1"]);
  });

  test("an unsplit metric is found by name and carries no resource", () => {
    const hits = searchHistory("swap", catalog, resources);
    expect(hits).toHaveLength(1);
    expect(hits[0].metric).toBe("swap");
    expect(hits[0].resource).toBeNull();
    expect(hits[0].title).toBe("Swap");
  });

  test("a named metric with no resources on this node still answers", () => {
    // The distinction is the point: "this node records GPU utilisation and has
    // no GPU" is a different claim from "there is no such metric", and the
    // second is what an empty result would say.
    const hits = searchHistory("gpu", catalog, { device: ["sda"] });
    expect(hits).toHaveLength(1);
    expect(hits[0].metric).toBe("gpu_util");
    expect(hits[0].resource).toBeNull();
  });

  test("an empty query returns nothing rather than everything", () => {
    // A search box that lists the whole catalog on focus would fetch every
    // range on the server the moment someone clicks it.
    expect(searchHistory("", catalog, resources)).toEqual([]);
    expect(searchHistory("   ", catalog, resources)).toEqual([]);
  });

  test("a query matching nothing returns nothing", () => {
    expect(searchHistory("kernel entropy", catalog, resources)).toEqual([]);
  });

  test("the result count is bounded", () => {
    // A hundred containers must not turn one keystroke into a hundred range
    // queries.
    const many: Resources = { device: Array.from({ length: 200 }, (_, i) => "sd" + i) };
    const hits = searchHistory("sd", catalog, many);
    expect(hits.length).toBe(MAX_HITS);
  });

  test("search is case-insensitive on both sides", () => {
    expect(searchHistory("SDA", catalog, resources).length).toBeGreaterThan(0);
    expect(searchHistory("cpu", catalog, resources).length).toBeGreaterThan(0);
    expect(searchHistory("CPU", catalog, resources)).toEqual(searchHistory("cpu", catalog, resources));
  });

  test("hits are identified uniquely, so one cannot be pinned twice", () => {
    const hits = searchHistory("sda", catalog, resources);
    const keys = hits.map(hitKey);
    expect(new Set(keys).size).toBe(keys.length);
  });
});
