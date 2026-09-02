<script lang="ts">
  // Theme control for the navbar.
  //
  // A select rather than a two-state switch, because the choice has three
  // values: a switch can express light and dark but has nowhere to put "follow
  // the operating system", so once it is touched there is no way back to
  // following the OS. It is also the same control the polling interval beside
  // it uses, which keeps the navbar's two preferences looking like one idea.

  import { theme, isThemeMode } from "$lib/theme.svelte";

  const options: { value: "system" | "light" | "dark"; label: string }[] = [
    { value: "system", label: "Auto" },
    { value: "light", label: "Light" },
    { value: "dark", label: "Dark" },
  ];

  const mode = $derived(theme.mode);

  function onChange(e: Event) {
    const value = (e.target as HTMLSelectElement).value;
    if (isThemeMode(value)) theme.setMode(value);
  }
</script>

<select
  class="select select-xs select-bordered bg-base-200 text-base-content/80 font-medium"
  aria-label="Theme"
  title="Theme"
  value={mode}
  onchange={onChange}
>
  {#each options as opt (opt.value)}
    <option value={opt.value}>{opt.label}</option>
  {/each}
</select>
