// How a CPU core is named, on every surface that names one.
//
// A core number restarts on every package: a two-socket host has two "Core 0".
// Named by the number alone, the two share one label in a legend, one line in
// a history chart and one entry in search, and the reader cannot tell which
// socket is hot. So a core is its package and its number, and a host with
// more than one package says both: "P1 · Core 0". Package numbers are the
// kernel's, from 0, as the CPU panel's Package column and `sysmon cpu` show
// them. A single-package host keeps the plain "Core 0".

import type { CoreTemp } from "./types";

/** coreKey identifies a core on its host: "package:core", as the history API
 * names a core's series. */
export function coreKey(packageId: number, coreId: number): string {
  return `${packageId}:${coreId}`;
}

/** multiPackage reports whether the readings span more than one package. */
export function multiPackage(temps: readonly Pick<CoreTemp, "package_id">[]): boolean {
  return new Set(temps.map((t) => t.package_id)).size > 1;
}

/**
 * coreLabel names one reading. The driver's own label is kept -- "Core 3" from
 * coretemp, "Tccd1" from k10temp, where the number is a chiplet rather than a
 * core -- with the package in front when the host has more than one.
 */
export function coreLabel(t: Pick<CoreTemp, "package_id" | "core_id" | "label">, multi: boolean): string {
  const base = t.label !== "" ? t.label : `Core ${t.core_id}`;
  return multi ? `P${t.package_id} · ${base}` : base;
}

/**
 * coreLabelForKey names a core known only by its key -- a history series with
 * no live reading beside it -- the same way.
 */
export function coreLabelForKey(key: string, multi: boolean): string {
  const [packageId, coreId] = key.split(":");
  if (coreId === undefined) return `Core ${key}`;
  return multi ? `P${packageId} · Core ${coreId}` : `Core ${coreId}`;
}

/** multiPackageKeys reports whether the core keys span more than one package. */
export function multiPackageKeys(keys: readonly string[]): boolean {
  return new Set(keys.map((k) => k.split(":")[0])).size > 1;
}
