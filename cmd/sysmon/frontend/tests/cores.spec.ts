import { test, expect } from "@playwright/test";
import { coreKey, coreLabel, coreLabelForKey, multiPackage, multiPackageKeys } from "../src/lib/cores";
import { searchHistory } from "../src/lib/search";
import type { HistoryMetric } from "../src/lib/types";

/**
 * How a core is named. Every package numbers its cores from 0, so a two-socket
 * host has two "Core 0": named by the number alone they share one label, one
 * history line and one search result. A single-package host must read exactly
 * as it always has.
 */

const single = [
  { package_id: 0, core_id: 0, label: "Core 0" },
  { package_id: 0, core_id: 1, label: "Core 1" },
];
const dual = [
  { package_id: 0, core_id: 0, label: "Core 0" },
  { package_id: 1, core_id: 0, label: "Core 0" },
];

test.describe("core names", () => {
  test("a single-package host keeps the driver's label", () => {
    expect(multiPackage(single)).toBe(false);
    expect(single.map((t) => coreLabel(t, false))).toEqual(["Core 0", "Core 1"]);
  });

  test("a two-package host names each core by its package, from 0 as the kernel does", () => {
    expect(multiPackage(dual)).toBe(true);
    const labels = dual.map((t) => coreLabel(t, true));
    expect(labels).toEqual(["P0 · Core 0", "P1 · Core 0"]);
    expect(new Set(labels).size).toBe(2);
  });

  test("a chiplet keeps its own name, and an unlabelled core gets its number", () => {
    expect(coreLabel({ package_id: 1, core_id: 1, label: "Tccd1" }, true)).toBe("P1 · Tccd1");
    expect(coreLabel({ package_id: 0, core_id: 5, label: "" }, false)).toBe("Core 5");
  });

  test("a history key names the same core the same way", () => {
    expect(coreKey(1, 0)).toBe("1:0");
    expect(multiPackageKeys(["0:0", "1:0"])).toBe(true);
    expect(multiPackageKeys(["0:0", "0:1"])).toBe(false);
    expect(coreLabelForKey("1:0", true)).toBe("P1 · Core 0");
    expect(coreLabelForKey("0:3", false)).toBe("Core 3");
    expect(coreLabelForKey("7", false)).toBe("Core 7");
  });
});

test.describe("core search", () => {
  const catalog: HistoryMetric[] = [
    { key: "cpu_temperature", label: "CPU temperature", unit: "celsius", split_by: "core" },
  ];
  const resources = { core: ["0:0", "1:0"] };
  const labels = { core: { "0:0": "P0 · Core 0", "1:0": "P1 · Core 0" } };

  test("results are titled by the core's name and keyed by its history name", () => {
    const hits = searchHistory("temperature", catalog, resources, labels);
    expect(hits.map((h) => h.title)).toEqual(["CPU temperature — P0 · Core 0", "CPU temperature — P1 · Core 0"]);
    expect(hits.map((h) => h.resource)).toEqual(["0:0", "1:0"]);
  });

  test("a package's name finds its cores", () => {
    const hits = searchHistory("p1", catalog, resources, labels);
    expect(hits.map((h) => h.resource)).toEqual(["1:0"]);
  });
});
