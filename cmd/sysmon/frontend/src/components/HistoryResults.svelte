<script lang="ts">
  /**
   * The charts a search pinned.
   *
   * One card per pinned result, above the tabs, so the answer to "show me sda
   * over the last 24 hours" appears where the question was asked rather than
   * on a page of its own.
   *
   * Each card asks the history store for its metric exactly as a panel does --
   * the same request, the same cache, the same window -- so searching for a
   * metric a panel is already showing costs nothing extra.
   */

  import HistoryChart from "./HistoryChart.svelte";
  import { history } from "$stores/history.svelte";
  import { search } from "$stores/search.svelte";
  import { hitKey, type SearchHit } from "$lib/search";
  import type { HistoryLine } from "$lib/history";

  const pinned = $derived(search.pinned);
  const active = $derived(history.active);

  $effect(() => {
    for (const hit of pinned) history.need(hit.metric);
  });

  /** seriesFor narrows a metric's series to the resource the hit names. */
  function seriesFor(hit: SearchHit): HistoryLine[] {
    const loaded = history.get(hit.metric);
    if (loaded == null) return [];
    const series = loaded.series ?? [];
    const matching =
      hit.resource == null ? series : series.filter((s) => s.name === hit.resource);
    return matching.map((s) => ({
      name: hit.resourceName ?? s.name,
      points: s.points,
      stepMs: loaded.step_ms,
    }));
  }

  function windowFor(hit: SearchHit): { startMs: number; endMs: number } {
    const loaded = history.get(hit.metric);
    if (loaded != null) return { startMs: loaded.start_ms, endMs: loaded.end_ms };
    const range = history.range;
    return { startMs: range?.startMs ?? 0, endMs: range?.endMs ?? 0 };
  }
</script>

{#if pinned.length > 0}
  <section class="px-4 pt-3" aria-label="History search results">
    {#if !active}
      <!-- A pinned chart with no range selected would be an empty box with no
           reason given. The range is what makes these charts possible, so the
           absence of one is the thing to say. -->
      <p class="text-xs text-warning mb-2">
        Pick a date range to chart these.
      </p>
    {/if}

    <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
      {#each pinned as hit (hitKey(hit))}
        <div class="card bg-base-100 border border-base-300">
          <div class="card-body p-3">
            <div class="flex items-center justify-between gap-2">
              <h3 class="text-sm font-medium">{hit.title}</h3>
              <button
                type="button"
                class="btn btn-ghost btn-xs"
                aria-label={"Remove " + hit.title}
                onclick={() => search.unpin(hit)}
              >
                Remove
              </button>
            </div>

            {#if !active}
              <p class="text-xs text-base-content/60">No range selected.</p>
            {:else if !history.settled(hit.metric)}
              <p class="text-xs text-base-content/60">Loading...</p>
            {:else if history.failed(hit.metric)}
              <p class="text-xs text-error">{history.error || "This range could not be read."}</p>
            {:else if seriesFor(hit).length === 0}
              <!-- The metric answered and this resource was not in it: nothing
                   was recorded for it in this window, which is a different fact
                   from a failed query and is said differently. -->
              <p class="text-xs text-base-content/60">Nothing recorded for this in the selected range.</p>
            {:else}
              <HistoryChart
                series={seriesFor(hit)}
                unit={hit.unit}
                startMs={windowFor(hit).startMs}
                endMs={windowFor(hit).endMs}
                height="180px"
              />
            {/if}
          </div>
        </div>
      {/each}
    </div>
  </section>
{/if}
