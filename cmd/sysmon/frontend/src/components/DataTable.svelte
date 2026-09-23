<script lang="ts" generics="T">
  import { tick, untrack, type Snippet } from "svelte";
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
    /**
     * Accessible name of the table. Defaults to the title; a table without a
     * title that has selectable rows needs one, since a grid must be named.
     */
    label?: string;
    initialSort?: string;
    initialDirection?: "asc" | "desc";
    pageSize?: number;
    searchPlaceholder?: string;
    emptyMessage?: string;
    /** Rows omitted upstream, disclosed so totals are not misread. */
    truncated?: number;
    /**
     * Called when a row is clicked, or chosen with Enter or Space. Makes the
     * table a grid whose rows are reached from the keyboard.
     */
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
    label = "",
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

  // Keyboard access to selectable rows: the ARIA grid pattern with a roving
  // tabindex (#165). The table is one tab stop; the arrow keys move within it.
  const selectable = $derived(onRowClick != null);
  const paged = $derived(pageCount > 1);

  let tbody = $state<HTMLTableSectionElement>();

  // The row that holds the tab stop, by key rather than by position, so a
  // refresh that reorders rows keeps it on the same row. When that row is no
  // longer on the page the first row takes the stop, so there always is one.
  let activeKey = $state<string | null>(null);
  const tabStopKey = $derived.by(() => {
    const keys = visible.map((r) => String(rowKey(r)));
    return activeKey != null && keys.includes(activeKey) ? activeKey : (keys[0] ?? null);
  });

  async function focusRow(index: number) {
    await tick();
    const count = visible.length;
    if (count === 0) return;
    tbody?.rows[Math.min(Math.max(index, 0), count - 1)]?.focus();
  }

  // PageUp and PageDown turn the pager and keep the row's position on the
  // new page. On the first or last page they go to the first or last row.
  function turnPage(delta: number, index: number) {
    const before = safePage;
    go(delta);
    const edge = delta < 0 ? 0 : visible.length - 1;
    focusRow(safePage === before ? edge : index);
  }

  const keyActions: Record<string, (index: number, row: T) => void> = {
    ArrowDown: (i) => focusRow(i + 1),
    ArrowUp: (i) => focusRow(i - 1),
    Home: () => focusRow(0),
    End: () => focusRow(visible.length - 1),
    PageDown: (i) => turnPage(1, i),
    PageUp: (i) => turnPage(-1, i),
    Enter: (_, row) => onRowClick?.(row),
    " ": (_, row) => onRowClick?.(row),
  };

  function onRowKeydown(event: KeyboardEvent, index: number, row: T) {
    // Keys pressed inside a cell's own control belong to that control.
    if (event.target !== event.currentTarget) return;
    if (event.altKey || event.ctrlKey || event.metaKey) return;
    const action = keyActions[event.key];
    if (action == null) return;
    event.preventDefault();
    action(index, row);
  }

  // A live refresh must not throw focus back to the page. Keyed rows keep
  // their elements, but a reorder moves them, and moving a focused element
  // blurs it. So before the DOM changes, note the focused row; after, if focus
  // was lost, put it back:
  //   - on the same row while it is on the page, whoever reordered it --
  //     focus follows the item, not the position, so Enter never lands on a
  //     different container;
  //   - on the same row on another page, when a refresh (not the user) moved
  //     it there: sorted onto another page, or past a page clamped away;
  //   - on the row now in its position, when the row has gone (a container
  //     stopped) or the user's own change of view took it off the page.
  //
  // "The user changed the view" is read from the state only the user sets --
  // the requested page, the sort, the search and the page size -- compared
  // with the last update. A refresh or a clamp changes none of it, and every
  // control changes some of it, including a sort header clicked in a browser
  // that leaves focus on the row (Safari does not focus buttons on click).
  const userView = () => `${page}|${sortKey}|${sortDir}|${query}|${perPage}`;
  let lastView = untrack(userView);
  let lostFocus: { key: string; index: number } | null = null;
  $effect.pre(() => {
    void visible;
    const focused = document.activeElement;
    if (tbody == null || !(focused instanceof HTMLTableRowElement)) return;
    if (focused.parentElement !== tbody) return;
    lostFocus = { key: focused.dataset.key ?? "", index: focused.sectionRowIndex };
  });
  $effect(() => {
    void visible;
    const view = untrack(userView);
    const userChangedView = view !== lastView;
    lastView = view;
    const pending = lostFocus;
    lostFocus = null;
    if (pending == null || tbody == null || visible.length === 0) return;
    if (tbody.contains(document.activeElement)) return;
    const rows = Array.from(tbody.rows);
    const same = rows.find((r) => r.dataset.key === pending.key);
    if (same != null) {
      same.focus({ preventScroll: true });
      return;
    }
    const at = userChangedView
      ? -1
      : untrack(() => sorted.findIndex((r) => String(rowKey(r)) === pending.key));
    if (at >= 0) {
      // Following the row is not a user change of view.
      page = Math.floor(at / perPage);
      lastView = untrack(userView);
      focusRow(at % perPage);
      return;
    }
    rows[Math.min(pending.index, visible.length - 1)].focus({ preventScroll: true });
  });
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
    <table
      class="table table-sm"
      role={selectable ? "grid" : undefined}
      aria-label={label || title || undefined}
      aria-rowcount={paged ? sorted.length + 1 : undefined}
    >
      <thead>
        <tr aria-rowindex={paged ? 1 : undefined}>
          <!-- The header sits on base-300, where daisyUI's /60 header text
               measures 4.45:1 in the light theme. /70 reads (#156). -->
          {#each columns as col (col.key)}
            <th
              class="bg-base-300 text-base-content/70 whitespace-nowrap {col.align === 'right' ? 'text-right' : ''} {col.sortValue != null ? 'p-0 hover:bg-base-200' : ''}"
              title={col.sortValue == null ? col.title : undefined}
              aria-sort={ariaSort(col)}
            >
              {#if col.sortValue != null}
                <button
                  type="button"
                  class="sort px-3 py-2"
                  title={col.title ?? `Sort by ${col.label}`}
                  onclick={() => toggleSort(col)}
                >
                  {col.label}<span aria-hidden="true" class="ml-0.5">{indicator(col)}</span>
                </button>
              {:else}
                {col.label}
              {/if}
            </th>
          {/each}
        </tr>
      </thead>
      <tbody bind:this={tbody}>
        {#each visible as row, i (rowKey(row))}
          {@const key = String(rowKey(row))}
          {@const isRowSelected = selectable && (isSelected?.(row) ?? false)}
          <tr
            data-key={key}
            aria-rowindex={paged ? start + i + 2 : undefined}
            class:clickable={selectable}
            class:selected={isRowSelected}
            tabindex={selectable ? (key === tabStopKey ? 0 : -1) : undefined}
            aria-selected={selectable ? isRowSelected : undefined}
            onclick={selectable ? () => onRowClick?.(row) : undefined}
            onkeydown={selectable ? (e) => onRowKeydown(e, i, row) : undefined}
            onfocus={selectable ? () => (activeKey = key) : undefined}
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

  /* Keyboard focus is an outline around the whole row: a different shape from
     both hover's left rule and selection's primary edges, so the three states
     read apart and combine without hiding one another. It is drawn in
     base-content, which clears the 3:1 non-text floor on the card in both
     themes; the contrast suite measures it. :focus-visible keeps it off rows
     focused by a click, which the selection rule already marks. */
  tr.clickable:focus-visible {
    outline: 2px solid var(--color-base-content);
    outline-offset: -2px;
  }

  /* Forced colours (Windows High Contrast) drop box-shadows and author
     colours, which took the hover and selection rules with them. Each state
     is restated with system colours, in shapes that stay distinct:
       hover      a 1px dashed CanvasText outline
       selection  the system's Highlight / HighlightText pair on every cell
       focus      a 3px solid outline, drawn over either: CanvasText, or
                  HighlightText on a selected row
     forced-color-adjust: none keeps the selection pair from being forced back
     to Canvas; the cells' own text is then set to HighlightText, since their
     author colours would otherwise survive onto Highlight. */
  @media (forced-colors: active) and (hover: hover) {
    tr.clickable:hover {
      outline: 1px dashed CanvasText;
      outline-offset: -1px;
    }
  }

  @media (forced-colors: active) {
    tr.selected > :global(td) {
      forced-color-adjust: none;
      background-color: Highlight;
      color: HighlightText;
    }

    tr.selected > :global(td *) {
      color: HighlightText;
    }

    tr.clickable:focus-visible {
      outline: 3px solid CanvasText;
      outline-offset: -3px;
    }

    /* On a selected row the outline sits on Highlight, and CanvasText is not
       a contrast pair for it; HighlightText is. */
    tr.selected:focus-visible {
      outline-color: HighlightText;
    }
  }

  /* A sort control fills its header cell, so the whole cell stays the click
     target it was before it became a button. The cell's padding moves onto the
     button (p-0 on the cell, table-sm's px-3 py-2 on the button), or a tap on
     the padding would miss it. */
  .sort {
    display: block;
    width: 100%;
    text-align: inherit;
    font: inherit;
    color: inherit;
    cursor: pointer;
    user-select: none;
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
