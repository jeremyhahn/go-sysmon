<script lang="ts">
  /**
   * The history view.
   *
   * Everything else in this dashboard answers "what is happening now", from a
   * live stream the browser holds sixty seconds of. This answers "what happened
   * then", from the Prometheus each node pushes its own metrics to, and it is
   * the only view whose data does not come from the event stream.
   *
   * A node with no prometheus.query_url does not get this tab at all: the
   * catalog reports itself disabled and App.svelte leaves it out, which is a
   * standalone node behaving exactly as it always has rather than one showing a
   * broken feature.
   */

  import HistoryChart from "$components/HistoryChart.svelte";
  import {
    RANGE_PRESETS,
    fetchHistory,
    fromLocalInput,
    toLocalInput,
    HistoryError,
    type RangePreset,
  } from "$lib/history";
  import { monitor } from "../stores/monitor.svelte";
  import type { History, HistoryMetric } from "$lib/types";

  interface Props {
    /** The catalog, already loaded by App: it decides whether this tab exists,
     * so fetching it again here would ask the same question twice. */
    metrics: HistoryMetric[];
  }

  let { metrics }: Props = $props();

  // The node whose history is drawn. Following the selected peer means drilling
  // into a node and opening History shows that node, not the aggregator.
  const node = $derived(monitor.selectedNode);

  /** The metrics shown by default: the four figures an operator looks at first
   * when asked what a machine was doing. */
  const DEFAULT_KEYS = ["cpu", "memory", "disk_used", "net_rx"];

  let selectedKeys = $state<string[]>(
    DEFAULT_KEYS.filter((k) => metrics.some((m) => m.key === k))
  );
  let pickerOpen = $state(false);

  let presetKey = $state<string>("1h");
  let customStart = $state("");
  let customEnd = $state("");

  // Bumped to re-run the load effect for a range that has not otherwise
  // changed -- a relative preset means something different a minute later.
  let refreshToken = $state(0);

  const preset = $derived<RangePreset | null>(
    RANGE_PRESETS.find((p) => p.key === presetKey) ?? null
  );

  /**
   * The window being drawn.
   *
   * A preset is resolved against the clock each time this recomputes, so
   * "1 hour" means the hour ending now. A custom range is absolute; an
   * incomplete custom range falls back to the last hour rather than issuing a
   * request the server would reject, since the inputs are incomplete on every
   * keystroke while a date is being typed.
   */
  const window = $derived.by(() => {
    void refreshToken;
    if (preset != null) {
      const end = Date.now();
      return { startMs: end - preset.ms, endMs: end };
    }
    const start = fromLocalInput(customStart);
    const end = fromLocalInput(customEnd);
    if (start == null || end == null || end <= start) {
      const now = Date.now();
      return { startMs: now - 3600_000, endMs: now };
    }
    return { startMs: start, endMs: end };
  });

  const customIncomplete = $derived(
    preset == null &&
      (fromLocalInput(customStart) == null ||
        fromLocalInput(customEnd) == null ||
        (fromLocalInput(customEnd) ?? 0) <= (fromLocalInput(customStart) ?? 0))
  );

  /**
   * The loaded ranges, carrying the window they were loaded for.
   *
   * The window is kept beside them on purpose. The heading and the axis change
   * the instant a new range is picked, while the data takes a round trip to
   * arrive; without this, the previous range's lines would keep rendering under
   * the new range's heading for as long as the request took. That is the
   * stale-data failure this dashboard already guards for fleet peers -- numbers
   * that look measured, under a label that says they are something else.
   */
  let loaded = $state<{
    startMs: number;
    endMs: number;
    byMetric: Record<string, History>;
  } | null>(null);
  let error = $state("");
  let loading = $state(false);

  /** The loaded data, but only while it still describes the window on screen. */
  const results = $derived<Record<string, History>>(
    loaded != null &&
      loaded.startMs === window.startMs &&
      loaded.endMs === window.endMs
      ? loaded.byMetric
      : {}
  );

  function selectPreset(key: string) {
    presetKey = key;
  }

  function selectCustom() {
    if (customStart === "" || customEnd === "") {
      const now = Date.now();
      customStart = toLocalInput(now - 3600_000);
      customEnd = toLocalInput(now);
    }
    presetKey = "custom";
  }

  function toggleMetric(key: string) {
    selectedKeys = selectedKeys.includes(key)
      ? selectedKeys.filter((k) => k !== key)
      : [...selectedKeys, key];
  }

  // Keeps the charts in catalog order however they were toggled on, so the
  // page does not reorder itself as metrics are added.
  const shown = $derived(metrics.filter((m) => selectedKeys.includes(m.key)));

  $effect(() => {
    const { startMs, endMs } = window;
    const keys = shown.map((m) => m.key);
    const forNode = node;
    if (keys.length === 0) {
      loaded = null;
      error = "";
      return;
    }

    // One controller for the whole batch: changing the range mid-flight must
    // abandon every request of the old one, or a slow chart can be filled in
    // with the previous window's data after the new one has already drawn.
    const controller = new AbortController();
    loading = true;
    error = "";

    Promise.all(
      keys.map((key) =>
        fetchHistory({ metric: key, node: forNode, startMs, endMs }, controller.signal)
          .then((h) => [key, h] as const)
      )
    )
      .then((entries) => {
        if (controller.signal.aborted) return;
        loaded = { startMs, endMs, byMetric: Object.fromEntries(entries) };
      })
      .catch((e: unknown) => {
        if (controller.signal.aborted) return;
        loaded = null;
        error =
          e instanceof HistoryError
            ? e.message
            : e instanceof Error
              ? e.message
              : "history is unavailable";
      })
      .finally(() => {
        if (!controller.signal.aborted) loading = false;
      });

    return () => controller.abort();
  });

  function pointCount(h: History | undefined): number {
    if (h == null) return 0;
    return h.series.reduce((sum, s) => sum + s.points.length, 0);
  }

  const rangeLabel = $derived(
    `${new Date(window.startMs).toLocaleString()} — ${new Date(window.endMs).toLocaleString()}`
  );
</script>

<div class="space-y-4">
  <!-- Range picker -->
  <div class="card bg-base-200 shadow-sm">
    <div class="card-body p-4 gap-3">
      <div class="flex flex-wrap items-center gap-3">
        <h3 class="card-title text-sm text-base-content/70 shrink-0">
          History{node != null ? ` · ${node}` : ""}
        </h3>

        <div class="join">
          {#each RANGE_PRESETS as p (p.key)}
            <button
              type="button"
              class="btn btn-xs join-item {presetKey === p.key ? 'btn-primary' : 'btn-ghost'}"
              onclick={() => selectPreset(p.key)}
            >
              {p.label}
            </button>
          {/each}
          <button
            type="button"
            class="btn btn-xs join-item {presetKey === 'custom' ? 'btn-primary' : 'btn-ghost'}"
            onclick={selectCustom}
          >
            Custom
          </button>
        </div>

        <button
          type="button"
          class="btn btn-xs btn-ghost"
          onclick={() => (refreshToken += 1)}
          disabled={loading}
        >
          {loading ? "Loading…" : "Refresh"}
        </button>

        <button
          type="button"
          class="btn btn-xs btn-ghost ml-auto"
          onclick={() => (pickerOpen = !pickerOpen)}
        >
          Metrics ({selectedKeys.length})
        </button>
      </div>

      {#if presetKey === "custom"}
        <div class="flex flex-wrap items-center gap-2 text-xs">
          <label class="flex items-center gap-1.5">
            <span class="text-base-content/60">From</span>
            <input
              type="datetime-local"
              class="input input-xs input-bordered"
              aria-label="Range start"
              bind:value={customStart}
            />
          </label>
          <label class="flex items-center gap-1.5">
            <span class="text-base-content/60">To</span>
            <input
              type="datetime-local"
              class="input input-xs input-bordered"
              aria-label="Range end"
              bind:value={customEnd}
            />
          </label>
          {#if customIncomplete}
            <!-- Said plainly rather than silently drawing something else: the
                 charts below are showing the last hour, not the dates typed. -->
            <span class="text-warning">
              Enter a start and an end, with the end later. Showing the last hour
              until then.
            </span>
          {/if}
        </div>
      {/if}

      {#if pickerOpen}
        <div class="flex flex-wrap gap-x-4 gap-y-1.5 pt-1">
          {#each metrics as m (m.key)}
            <label class="flex items-center gap-1.5 text-xs cursor-pointer">
              <input
                type="checkbox"
                class="checkbox checkbox-xs"
                checked={selectedKeys.includes(m.key)}
                onchange={() => toggleMetric(m.key)}
              />
              <span>{m.label}</span>
            </label>
          {/each}
        </div>
      {/if}

      <p class="text-xs text-base-content/50">{rangeLabel}</p>
    </div>
  </div>

  {#if error !== ""}
    <div class="alert alert-error text-sm" role="alert">
      <span>{error}</span>
    </div>
  {/if}

  {#if shown.length === 0}
    <div class="card bg-base-200 shadow-sm">
      <div class="card-body p-6 items-center text-sm text-base-content/60">
        Pick at least one metric to draw.
      </div>
    </div>
  {/if}

  {#each shown as m (m.key)}
    {@const h = results[m.key]}
    <div class="card bg-base-200 shadow-sm">
      <div class="card-body p-4">
        <div class="flex items-baseline justify-between gap-3">
          <h3 class="card-title text-sm text-base-content/70">{m.label}</h3>
          {#if h != null}
            <span class="text-xs text-base-content/40">
              {h.series.length} series · {Math.round(h.step_ms / 1000)}s step
            </span>
          {/if}
        </div>

        {#if h != null && pointCount(h) > 0}
          <HistoryChart
            series={h.series}
            unit={h.unit}
            startMs={h.start_ms}
            endMs={h.end_ms}
          />
          {#if h.series.length > 1}
            <div class="flex flex-wrap gap-x-3 gap-y-1 text-xs text-base-content/60">
              {#each h.series as s (s.name)}
                <span>{s.name}</span>
              {/each}
            </div>
          {/if}
        {:else if loading}
          <div class="h-[220px] flex items-center justify-center text-sm text-base-content/40">
            Loading…
          </div>
        {:else}
          <!-- An empty range is a real answer: the node may not have been
               running, or may not have been pushing yet. It is not an error,
               and must not be dressed as one. -->
          <div class="h-[220px] flex items-center justify-center text-sm text-base-content/40">
            No data recorded for this range.
          </div>
        {/if}
      </div>
    </div>
  {/each}
</div>
