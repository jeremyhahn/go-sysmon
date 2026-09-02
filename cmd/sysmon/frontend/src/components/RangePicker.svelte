<script lang="ts">
  /**
   * The date range control.
   *
   * One control, in the header, beside the polling interval — the two settings
   * that decide what the whole dashboard is showing sit together. Its "Live"
   * value is not a mode of its own: it is the absence of a range, and choosing
   * it puts every chart back on the stream immediately.
   *
   * A node with no Prometheus configured does not render this at all, and is
   * exactly the dashboard it was before any of this existed.
   */

  import { history, LIVE, CUSTOM } from "$stores/history.svelte";
  import { RANGE_PRESETS } from "$lib/history";

  const available = $derived(history.available);
  const selection = $derived(history.selection);
  const showCustom = $derived(selection === CUSTOM);

  function onChange(e: Event) {
    const value = (e.target as HTMLSelectElement).value;
    // Custom is not a window; it is the state of specifying one. The store
    // decides what that means for whatever is currently applied.
    history.selectPreset(value);
  }

  function onStart(e: Event) {
    history.setCustom((e.target as HTMLInputElement).value, history.customEnd);
  }

  function onEnd(e: Event) {
    history.setCustom(history.customStart, (e.target as HTMLInputElement).value);
  }
</script>

{#if available}
  <div class="flex items-center gap-2">
    <select
      class="select select-xs select-bordered bg-base-200 text-base-content/80 font-medium"
      aria-label="Date range"
      title="Date range"
      value={selection}
      onchange={onChange}
    >
      <option value={LIVE}>Live</option>
      {#each RANGE_PRESETS as preset (preset.key)}
        <option value={preset.key}>Last {preset.label}</option>
      {/each}
      <option value={CUSTOM}>Custom…</option>
    </select>

    {#if showCustom}
      <input
        type="datetime-local"
        class="input input-xs input-bordered"
        aria-label="Range start"
        value={history.customStart}
        onchange={onStart}
      />
      <input
        type="datetime-local"
        class="input input-xs input-bordered"
        aria-label="Range end"
        value={history.customEnd}
        onchange={onEnd}
      />
      {#if !history.customComplete}
        <!-- Said rather than silently ignored: the charts are still live, and
             the reason is that the pair is not yet a window. -->
        <span class="text-warning whitespace-nowrap">
          Enter a start and an end, with the end later.
        </span>
      {/if}
    {/if}
  </div>
{/if}
