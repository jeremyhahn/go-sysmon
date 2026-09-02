<script lang="ts">
  /**
   * Says, unmistakably, that the charts are not live.
   *
   * A chart that has silently stopped updating is the same lie as a frozen peer
   * reading — numbers that look measured and current and are neither — and here
   * it would be self-inflicted, so this is a full-width bar rather than a
   * subtle label. It also states the part that is easy to get wrong by
   * omission: what the gauges and tables beside the charts are showing, since
   * a gauge has no historical value to display.
   *
   * That second sentence used to read "still show live values", flatly. It is
   * false in two states this dashboard already knows about and already reports
   * elsewhere: an unreachable peer, whose last snapshot keeps rendering under a
   * "stale" badge, and a dropped stream, where every panel freezes on its last
   * frame. Putting "live" beside either of those is the failure this banner
   * exists to prevent, printed by the banner itself.
   */

  import { history, LIVE } from "$stores/history.svelte";
  import { monitor } from "$stores/monitor.svelte";

  const range = $derived(history.range);
  const error = $derived(history.error);
  const loading = $derived(history.loading);

  // One question, one answer, asked in the store: see showingLiveData.
  const live = $derived(monitor.showingLiveData);

  const window = $derived(
    range == null
      ? ""
      : `${new Date(range.startMs).toLocaleString()} — ${new Date(range.endMs).toLocaleString()}`
  );
</script>

{#if range != null}
  <div
    class="alert alert-warning rounded-none py-2 px-4 flex flex-wrap items-center gap-x-3 gap-y-1 text-sm"
    role="status"
  >
    <span class="font-semibold">Historical view — charts are not live.</span>
    <span class="opacity-80">{window}</span>
    {#if loading}
      <span class="loading loading-spinner loading-xs"></span>
    {/if}
    <span class="opacity-70 text-xs">
      {live
        ? "Gauges, tables and counters still show live values."
        : "Gauges, tables and counters show the last snapshot received, not live values."}
    </span>
    <button
      type="button"
      class="btn btn-xs ml-auto"
      onclick={() => history.refresh()}
      disabled={loading}
    >
      Refresh
    </button>
    <button
      type="button"
      class="btn btn-xs"
      onclick={() => history.selectPreset(LIVE)}
    >
      Return to live
    </button>
  </div>
{/if}

{#if error !== ""}
  <div class="alert alert-error rounded-none py-2 px-4 text-sm" role="alert">
    <span>{error}</span>
  </div>
{/if}
