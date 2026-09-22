<script lang="ts" generics="T">
  import { untrack, type Snippet } from "svelte";
  import type { Column } from "$lib/table";

  interface Props {
    rows: T[];
    columns: Column<T>[];
    /** Renders the <td> cells for one row. */
    cells: Snippet<[T]>;
    /** Text searched by the filter box. */
    searchText: (row: T) => string;
    /** Stable key for keyed iteration. */
    rowKey: (row: T) => string | number;
    title?: string;
    initialSort?: string;
    initialDirection?: "asc" | "desc";
    pageSize?: number;
    searchPlaceholder?: string;
    emptyMessage?: string;
    /** Rows omitted upstream, disclosed so totals are not misread. */
    truncated?: number;
    /** Called when a row is clicked; makes the whole row interactive. */
    onRowClick?: (row: T) => void;
    /** Marks a row as selected. */
    isSelected?: (row: T) => boolean;
  }

  let {
    rows,
    columns,
    cells,
    searchText,
    rowKey,
    title = "",
    initialSort = "",
    initialDirection = "asc",
    pageSize = 25,
    searchPlaceholder = "Search…",
    emptyMessage = "Nothing to show.",
    truncated = 0,
    onRowClick,
    isSelected,
  }: Props = $props();

  let query = $state("");
  let sortKey = $state(untrack(() => initialSort));
  let sortDir = $state<"asc" | "desc">(untrack(() => initialDirection));
  let page = $state(0);
  let perPage = $state(untrack(() => pageSize));

  const pageSizeOptions = [10, 25, 50, 100];

  function toggleSort(col: Column<T>) {
    if (col.sortValue == null) return;
    if (sortKey === col.key) {
      sortDir = sortDir === "asc" ? "desc" : "asc";
    } else {
      sortKey = col.key;
      // Numeric columns are most useful largest-first on the first click.
      const sample = rows.length > 0 ? col.sortValue(rows[0]) : "";
      sortDir = typeof sample === "number" ? "desc" : "asc";
    }
    page = 0;
  }

  function indicator(col: Column<T>): string {
    if (col.sortValue == null || sortKey !== col.key) return "";
    return sortDir === "asc" ? "▲" : "▼";
  }

  // aria-sort communicates ordering without altering the header's accessible
  // name, which would otherwise change as the user sorts.
  function ariaSort(col: Column<T>): "ascending" | "descending" | "none" {
    if (col.sortValue == null || sortKey !== col.key) return "none";
    return sortDir === "asc" ? "ascending" : "descending";
  }

  const filtered = $derived.by(() => {
    const q = query.trim().toLowerCase();
    if (q === "") return rows;
    return rows.filter((r) => searchText(r).toLowerCase().includes(q));
  });

  const sorted = $derived.by(() => {
    const col = columns.find((c) => c.key === sortKey);
    if (col?.sortValue == null) return filtered;
    const dir = sortDir === "asc" ? 1 : -1;
    return [...filtered].sort((a, b) => {
      const av = col.sortValue!(a);
      const bv = col.sortValue!(b);
      if (typeof av === "string" || typeof bv === "string") {
        return String(av).localeCompare(String(bv)) * dir;
      }
      return (Number(av) - Number(bv)) * dir;
    });
  });

  const pageCount = $derived(Math.max(1, Math.ceil(sorted.length / perPage)));
  // A filter or page-size change can leave the current page past the end.
  const safePage = $derived(Math.min(page, pageCount - 1));
  const start = $derived(safePage * perPage);
  const visible = $derived(sorted.slice(start, start + perPage));

  const rangeLabel = $derived(
    sorted.length === 0
      ? "0"
      : `${start + 1}–${Math.min(start + perPage, sorted.length)} of ${sorted.length}`
  );

  function go(delta: number) {
    page = Math.min(Math.max(safePage + delta, 0), pageCount - 1);
  }
</script>

<div class="space-y-2">
  <div class="flex flex-wrap items-center justify-between gap-2">
    {#if title}
      <h3 class="card-title text-sm text-base-content/70">
        {title}
        <span class="text-xs font-normal text-base-content/60">({sorted.length})</span>
      </h3>
    {/if}

    <div class="flex items-center gap-2 ml-auto">
      <input
        type="search"
        class="input input-bordered input-sm w-48"
        placeholder={searchPlaceholder}
        bind:value={query}
        oninput={() => (page = 0)}
      />
      <select
        class="select select-bordered select-sm"
        bind:value={perPage}
        onchange={() => (page = 0)}
        aria-label="Rows per page"
      >
        {#each pageSizeOptions as size (size)}
          <option value={size}>{size} / page</option>
        {/each}
      </select>
    </div>
  </div>

  <div class="overflow-x-auto">
    <table class="table table-sm">
      <thead>
        <tr>
          {#each columns as col (col.key)}
            <th
              class="bg-base-300 whitespace-nowrap {col.align === 'right' ? 'text-right' : ''} {col.sortValue != null ? 'cursor-pointer select-none hover:bg-base-200' : ''}"
              title={col.title ?? (col.sortValue != null ? `Sort by ${col.label}` : undefined)}
              onclick={() => toggleSort(col)}
              aria-sort={ariaSort(col)}
            >
              {col.label}<span aria-hidden="true" class="ml-0.5">{indicator(col)}</span>
            </th>
          {/each}
        </tr>
      </thead>
      <tbody>
        {#each visible as row (rowKey(row))}
          <tr
            class:clickable={onRowClick != null}
            class:selected={onRowClick != null && (isSelected?.(row) ?? false)}
            onclick={onRowClick != null ? () => onRowClick(row) : undefined}
          >
            {@render cells(row)}
          </tr>
        {:else}
          <tr>
            <td colspan={columns.length} class="text-center text-base-content/60 py-6 text-sm">
              {query.trim() !== "" ? `No match for "${query}".` : emptyMessage}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>

  <div class="flex flex-wrap items-center justify-between gap-2 text-xs text-base-content/60">
    <span>
      Showing {rangeLabel}
      {#if truncated > 0}
        <span class="text-warning">
          &bull; {truncated} more not sent by the server
        </span>
      {/if}
    </span>

    {#if pageCount > 1}
      <div class="join">
        <button
          class="join-item btn btn-xs"
          onclick={() => go(-1)}
          disabled={safePage === 0}
          aria-label="Previous page"
        >
          «
        </button>
        <span class="join-item btn btn-xs btn-disabled">
          {safePage + 1} / {pageCount}
        </span>
        <button
          class="join-item btn btn-xs"
          onclick={() => go(1)}
          disabled={safePage >= pageCount - 1}
          aria-label="Next page"
        >
          »
        </button>
      </div>
    {/if}
  </div>
</div>

<style>
  /* A clickable row marks hover and selection with rules, never with a fill.
     The row keeps the card's own surface, base-200, which is the surface the
     semantic text colours in app.css are measured against -- and a row's
     status cells are exactly where those colours appear. A base-300 fill took
     them below AA in the light theme (#147).

     The rules are inset shadows rather than borders, so a row does not shift
     when its state changes. Cells come from the caller's snippet, so they are
     reached through :global. */
  tr.clickable {
    cursor: pointer;
  }

  /* Hover is a pointer affordance. Guarded like Tailwind's hover: variant, so
     a touch browser that keeps :hover after a tap does not leave a deselected
     row looking marked. */
  @media (hover: hover) {
    tr.clickable:hover > :global(td:first-child) {
      box-shadow: inset 3px 0 0 color-mix(in oklab, var(--color-base-content) 40%, transparent);
    }
  }

  /* Selection edges every cell, top and bottom, so it stays visible when a
     wide table is scrolled sideways and the first cell is out of view. The
     first cell adds the heavier left rule. Selection outranks hover: a
     selected row under the pointer keeps its marker. */
  tr.selected > :global(td) {
    box-shadow:
      inset 0 1px 0 var(--color-primary),
      inset 0 -1px 0 var(--color-primary);
  }

  tr.selected > :global(td:first-child),
  tr.selected:hover > :global(td:first-child) {
    box-shadow:
      inset 3px 0 0 var(--color-primary),
      inset 0 1px 0 var(--color-primary),
      inset 0 -1px 0 var(--color-primary);
  }
</style>
