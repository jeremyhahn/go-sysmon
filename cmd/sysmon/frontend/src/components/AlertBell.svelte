<script lang="ts">
  // Navbar alert bell: a badge of alerts the user has not seen yet, and a
  // dropdown that lists everything currently firing.
  //
  // Open/close is driven by :focus-within (daisyUI's focus dropdown) rather
  // than a boolean, because that gets dismiss-on-click-outside and
  // dismiss-on-Escape from the browser instead of from hand-written listeners.
  import { severityDotClass, severityTextClass } from "$lib/alerts";
  import { formatRelativeTime } from "$lib/format";
  import { monitor } from "$stores/monitor.svelte";

  interface Props {
    /** Switches the dashboard to the tab an alert belongs to. */
    onNavigate: (category: string) => void;
  }

  let { onNavigate }: Props = $props();

  const alerts = $derived(monitor.alerts);
  const newAlerts = $derived(monitor.newAlerts);
  const criticalCount = $derived(monitor.criticalCount);
  const warningCount = $derived(monitor.warningCount);

  const bellColor = $derived(
    criticalCount > 0
      ? "text-error"
      : warningCount > 0
        ? "text-warning"
        : "text-base-content/60"
  );

  // The badge takes the colour of the worst *unseen* alert, not of the worst
  // alert overall — the badge is about what is new.
  const badgeColor = $derived(
    newAlerts.some((a) => a.severity === "critical")
      ? "badge-error"
      : newAlerts.some((a) => a.severity === "warning")
        ? "badge-warning"
        : "badge-info"
  );

  function closeDropdown(): void {
    // Dropping focus is what closes a :focus-within dropdown.
    const active = document.activeElement;
    if (active instanceof HTMLElement) active.blur();
  }

  function selectAlert(category: string): void {
    onNavigate(category);
    closeDropdown();
  }
</script>

<!-- focusin marks everything currently firing as seen; it is idempotent, so
     the extra fires from focusing a row inside the panel are harmless. -->
<div
  class="dropdown dropdown-end"
  onfocusin={() => monitor.markAlertsSeen()}
>
  <button
    type="button"
    tabindex="0"
    class="btn btn-ghost btn-xs btn-circle relative {bellColor}"
    aria-label="Alerts"
    title="{alerts.length} active alert{alerts.length === 1 ? '' : 's'}"
  >
    <svg
      xmlns="http://www.w3.org/2000/svg"
      class="h-4 w-4"
      fill="none"
      viewBox="0 0 24 24"
      stroke="currentColor"
      stroke-width="2"
    >
      <path
        stroke-linecap="round"
        stroke-linejoin="round"
        d="M15 17h5l-1.405-1.405A2.032 2.032 0 0118 14.158V11a6.002 6.002 0 00-4-5.659V5a2 2 0 10-4 0v.341C7.67 6.165 6 8.388 6 11v3.159c0 .538-.214 1.055-.595 1.436L4 17h5m6 0v1a3 3 0 11-6 0v-1m6 0H9"
      />
    </svg>
    {#if newAlerts.length > 0}
      <span
        class="badge badge-xs {badgeColor} absolute -top-1 -right-1 px-1 font-semibold"
      >
        {newAlerts.length > 99 ? "99+" : newAlerts.length}
      </span>
    {/if}
  </button>

  <div
    tabindex="-1"
    class="dropdown-content mt-2 w-96 max-w-[90vw] rounded-box bg-base-100 shadow-lg border border-base-300"
  >
    <div
      class="flex items-baseline justify-between px-3 py-2 border-b border-base-300"
    >
      <span class="text-xs font-semibold uppercase tracking-wider">Alerts</span>
      {#if alerts.length > 0}
        <span class="text-xs text-base-content/60">
          {criticalCount} critical &bull; {warningCount} warning
        </span>
      {/if}
    </div>

    {#if alerts.length === 0}
      <p class="px-3 py-4 text-sm text-base-content/60">No active alerts</p>
    {:else}
      <ul class="max-h-96 overflow-y-auto py-1">
        {#each alerts as alert (alert.id)}
          <li>
            <button
              type="button"
              class="w-full text-left px-3 py-2 hover:bg-base-200 flex items-start gap-2"
              onclick={() => selectAlert(alert.category)}
            >
              <span
                class="inline-block w-2 h-2 rounded-full mt-1.5 shrink-0 {severityDotClass(
                  alert.severity
                )}"
                aria-hidden="true"
              ></span>
              <span class="min-w-0 flex-1">
                <span class="block text-sm {severityTextClass(alert.severity)}">
                  {alert.message}
                </span>
                <span class="block text-xs text-base-content/60">
                  {#if alert.resource}
                    <span class="font-mono">{alert.resource}</span> &bull;
                  {/if}
                  {alert.category} &bull; {formatRelativeTime(alert.since)}
                </span>
              </span>
            </button>
          </li>
        {/each}
      </ul>
    {/if}
  </div>
</div>
