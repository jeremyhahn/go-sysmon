<script lang="ts">
  /**
   * Theme control: one icon button that flips between light and dark.
   *
   * The sun and moon are go-netmon's, path for path, and so is the behaviour:
   * two states, one click between them. The icon shows the theme you are in,
   * and the accessible name says what clicking will do -- an icon alone cannot.
   */

  import { theme } from "$lib/theme.svelte";

  const mode = $derived(theme.mode);
  const other = $derived(theme.other);
</script>

<button
  type="button"
  class="btn btn-ghost btn-xs btn-circle"
  aria-label="{mode === 'dark' ? 'Dark' : 'Light'} theme. Switch to: {other} theme"
  title="Switch to the {other} theme"
  onclick={() => theme.toggle()}
>
  {#if mode === "light"}
    <!-- go-netmon's sun. -->
    <svg class="h-4 w-4" width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <circle cx="8" cy="8" r="3.2" stroke="currentColor" stroke-width="1.5" />
      <path
        d="M8 1v1.8M8 13.2V15M15 8h-1.8M2.8 8H1M12.9 3.1l-1.3 1.3M4.4 11.6l-1.3 1.3M12.9 12.9l-1.3-1.3M4.4 4.4 3.1 3.1"
        stroke="currentColor"
        stroke-width="1.5"
        stroke-linecap="round"
      />
    </svg>
  {:else}
    <!-- go-netmon's moon. -->
    <svg class="h-4 w-4" width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <path
        d="M13.5 9.6A5.8 5.8 0 0 1 6.4 2.5a5.8 5.8 0 1 0 7.1 7.1Z"
        stroke="currentColor"
        stroke-width="1.5"
        stroke-linejoin="round"
      />
    </svg>
  {/if}
</button>
