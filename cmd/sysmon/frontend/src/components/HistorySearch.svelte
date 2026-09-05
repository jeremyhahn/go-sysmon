<script lang="ts">
  /**
   * The history search box.
   *
   * It sits beside the date range, because the two are one thought: the range
   * says WHEN and this says WHAT. There is no separate history destination --
   * #31 settled that, and a search that opened one would re-introduce it.
   *
   * Rendered only on a node that records history. On a node with no Prometheus
   * there is nothing to search, and an input that returns nothing whatever you
   * type is worse than no input.
   */

  import { search } from "$stores/search.svelte";
  import { hitKey, type SearchHit } from "$lib/search";

  const available = $derived(search.available);
  const hits = $derived(search.hits);
  const query = $derived(search.query);

  let open = $state(false);

  function onInput(e: Event) {
    search.query = (e.target as HTMLInputElement).value;
    open = true;
  }

  function choose(hit: SearchHit) {
    search.pin(hit);
    open = false;
  }

  function onKey(e: KeyboardEvent) {
    if (e.key === "Escape") {
      open = false;
    } else if (e.key === "Enter" && hits.length > 0) {
      choose(hits[0]);
    }
  }
</script>

{#if available}
  <div class="relative">
    <input
      type="search"
      class="input input-xs input-bordered w-44 bg-base-200"
      aria-label="Search history"
      placeholder="Search history"
      value={query}
      oninput={onInput}
      onfocus={() => (open = true)}
      onkeydown={onKey}
    />

    {#if open && query.trim() !== ""}
      <div
        class="absolute right-0 top-7 z-50 w-80 max-h-80 overflow-y-auto rounded-box border border-base-300 bg-base-100 shadow-lg"
        role="listbox"
        aria-label="History search results"
      >
        {#if hits.length === 0}
          <!-- Said, not left blank: an empty dropdown reads as a broken control
               rather than as an answer. -->
          <p class="px-3 py-2 text-xs text-base-content/60">
            Nothing recorded matches that.
          </p>
        {:else}
          {#if search.pinnedFull}
            <p class="px-3 py-2 text-xs text-warning">
              Six charts is the limit. Remove one to add another.
            </p>
          {/if}
          {#each hits as hit (hitKey(hit))}
            <button
              type="button"
              role="option"
              aria-selected="false"
              class="w-full text-left px-3 py-2 text-xs hover:bg-base-200 disabled:opacity-40"
              disabled={search.pinnedFull}
              onclick={() => choose(hit)}
            >
              {hit.title}
            </button>
          {/each}
        {/if}
      </div>
    {/if}
  </div>
{/if}
