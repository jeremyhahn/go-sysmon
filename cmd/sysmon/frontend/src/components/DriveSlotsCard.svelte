<script lang="ts">
  import type { DriveSlot, DriveSlots } from "$lib/types";

  // The node's drive bays as its BMC reports them (#289). A bay's fault LED
  // is the BMC's judgement and can be lit while every block device the kernel
  // sees is healthy, so this is shown beside the disks rather than merged
  // into them.
  //
  // Undefined is a server that predates the collector: nothing is drawn,
  // because "no BMC" would be a claim this build cannot make about it.
  let { slots }: { slots: DriveSlots | undefined } = $props();

  const SOURCE_LABEL: Record<string, string> = {
    redfish: "via Redfish",
    ipmi: "via IPMI",
  };

  const HEALTH_PRESENTATION: Record<string, { label: string; badge: string }> = {
    ok: { label: "OK", badge: "badge-success" },
    warning: { label: "Warning", badge: "badge-warning" },
    critical: { label: "Critical", badge: "badge-error" },
  };

  // The fault flag wins over the health word: IPMI reports a drive fault with
  // no health at all, and that bay must not read as "unknown".
  function healthPresentation(slot: DriveSlot): { label: string; badge: string } {
    if (slot.fault) return { label: "FAULT", badge: "badge-error" };
    return HEALTH_PRESENTATION[slot.health] ?? { label: "unknown", badge: "badge-ghost" };
  }
</script>

{#if slots != null}
  <div class="card bg-base-200 shadow-sm" data-testid="drive-slots">
    <div class="card-body p-4">
      <div class="flex flex-wrap items-center justify-between gap-2 mb-2">
        <h3 class="font-semibold">Drive bays</h3>
        {#if slots.available}
          <span class="badge badge-ghost badge-sm">{SOURCE_LABEL[slots.source] ?? slots.source}</span>
        {:else}
          <span class="badge badge-ghost badge-sm" title={slots.reason || undefined}>BMC not read</span>
        {/if}
      </div>

      {#if !slots.available}
        <p class="text-xs text-base-content/60" data-testid="drive-slots-reason">
          {slots.reason || "No reason was given."}
        </p>
      {:else}
        <!-- Redfish failed and IPMI answered: the reading is real, but the
             configured source is broken and the operator should hear why. -->
        {#if slots.reason}
          <p class="text-xs text-warning mb-2" data-testid="drive-slots-reason">{slots.reason}</p>
        {/if}
        {#if (slots.slots ?? []).length === 0}
          <p class="text-xs text-base-content/60">The BMC reported no drive bays.</p>
        {:else}
          <div class="overflow-x-auto">
            <table class="table table-xs">
              <thead>
                <tr>
                  <th>Bay</th>
                  <th>Drive</th>
                  <th>Health</th>
                  <th>State</th>
                  <th class="text-right">Life left</th>
                  <th>Predicted failure</th>
                </tr>
              </thead>
              <tbody>
                {#each slots.slots as slot (slot.id)}
                  {@const health = healthPresentation(slot)}
                  <tr>
                    <td class="font-mono" title={slot.id}>{slot.slot || slot.id}</td>
                    <td>{slot.present ? "present" : "empty"}</td>
                    <td>
                      <span class="badge {health.badge} badge-sm">{health.label}</span>
                      {#if slot.rebuilding}
                        <span class="badge badge-info badge-sm">rebuilding</span>
                      {/if}
                    </td>
                    <td class="text-base-content/70">{slot.state || "—"}</td>
                    <td class="text-right font-mono">
                      {slot.media_life_reported ? `${slot.media_life_left_percent}%` : "—"}
                    </td>
                    <td>
                      {#if slot.failure_predicted}
                        <span class="badge badge-warning badge-sm">predicted</span>
                      {:else}
                        no
                      {/if}
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {/if}
      {/if}
    </div>
  </div>
{/if}
