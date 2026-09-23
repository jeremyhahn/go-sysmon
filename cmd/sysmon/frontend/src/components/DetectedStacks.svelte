<script lang="ts">
  import type { StackDetection, StackEvidence } from "$lib/types";

  /**
   * What the node found installed or running for one tab: the container
   * runtimes behind Containers, or the VM stacks behind Virtualization (#187).
   *
   * The tab exists because of this evidence, so the panel says what it was --
   * otherwise a tab that appears on a host with nothing running looks like a
   * mistake.
   */
  let {
    detection,
    title,
    subject,
  }: {
    /** Undefined when the node predates detection. */
    detection: StackDetection | undefined;
    title: string;
    /** What could not be confirmed, in a sentence: "a container runtime". */
    subject: string;
  } = $props();

  const kindLabels: Record<StackEvidence["kind"], string> = {
    binary: "binary",
    socket: "socket",
    module: "kernel module",
    control_domain: "Xen control domain",
    running: "running",
  };

  interface StackRow {
    stack: string;
    found: string[];
  }

  // One row per stack, each listing what proved it: the paths found, and
  // "running" when a workload was seen.
  const rows = $derived.by<StackRow[]>(() => {
    const byStack = new Map<string, string[]>();
    for (const e of detection?.evidence ?? []) {
      const found = byStack.get(e.stack) ?? [];
      found.push(e.path ? `${kindLabels[e.kind] ?? e.kind} ${e.path}` : (kindLabels[e.kind] ?? e.kind));
      byStack.set(e.stack, found);
    }
    return [...byStack].map(([stack, found]) => ({ stack, found }));
  });
</script>

{#if detection?.state === "present" && rows.length > 0}
  <div class="card bg-base-200 shadow-sm" data-testid="detected-stacks">
    <div class="card-body p-4">
      <h3 class="card-title text-sm text-base-content/70">{title}</h3>
      <ul class="text-sm space-y-1">
        {#each rows as row (row.stack)}
          <li class="flex flex-wrap items-baseline gap-x-2">
            <span class="font-semibold">{row.stack}</span>
            <span class="text-xs text-base-content/60 font-mono break-all">{row.found.join(", ")}</span>
          </li>
        {/each}
      </ul>
    </div>
  </div>
{:else if detection?.state === "unknown"}
  <div class="card bg-base-200 shadow-sm" data-testid="detected-stacks">
    <div class="card-body p-4">
      <h3 class="card-title text-sm text-base-content/70">{title}</h3>
      <p class="text-xs text-base-content/60">
        Whether this node has {subject} could not be confirmed: {detection.reason}.
      </p>
    </div>
  </div>
{/if}
