import type { CPUInfo } from "./types";

/**
 * coreUsage is a core's measured usage, or null when it was not measured.
 *
 * Usage is the difference of two /proc/stat readings, so a node's first
 * collection has none, and nor does a core whose line could not be read. The
 * backend says so with `usage_available`; `usage_percent` is then 0, which is
 * not an idle core and must never be drawn as one.
 */
export function coreUsage(cpu: CPUInfo): number | null {
  return cpu.usage_available ? cpu.usage_percent : null;
}

/**
 * meanCpuUsage is the mean usage over the cores that were measured, or null
 * when none was. It is pkg/types MeanCPUUsage's rule on this side of the wire:
 * an unmeasured core is left out rather than averaged in as idle.
 */
export function meanCpuUsage(cpus: readonly CPUInfo[]): number | null {
  let sum = 0;
  let measured = 0;
  for (const cpu of cpus) {
    const usage = coreUsage(cpu);
    if (usage === null) continue;
    sum += usage;
    measured++;
  }
  return measured === 0 ? null : sum / measured;
}
