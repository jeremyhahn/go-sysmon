<script lang="ts">
  // A panel-level summary of the alerts the backend is firing for that panel.
  // Renders nothing when the list is empty, so callers can drop it in
  // unconditionally.
  import { alertClass, severityDotClass } from "$lib/alerts";
  import { formatRelativeTime } from "$lib/format";
  import type { Alert } from "$lib/types";

  interface Props {
    alerts: Alert[];
    /** Heading above the messages, e.g. "Disk Health Alert". */
    title: string;
  }

  let { alerts, title }: Props = $props();
</script>

{#if alerts.length > 0}
  <div role="alert" class="alert {alertClass(alerts)} shadow-sm items-start">
    <svg
      xmlns="http://www.w3.org/2000/svg"
      class="h-5 w-5 shrink-0 stroke-current"
      fill="none"
      viewBox="0 0 24 24"
    >
      <path
        stroke-linecap="round"
        stroke-linejoin="round"
        stroke-width="2"
        d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.538-1.333-3.308 0L3.732 16c-.77 1.333.192 3 1.732 3z"
      />
    </svg>
    <div class="min-w-0">
      <p class="font-semibold">{title}</p>
      <ul class="text-sm space-y-0.5 mt-0.5">
        {#each alerts as alert (alert.id)}
          <li class="flex items-baseline gap-2">
            <span
              class="inline-block w-1.5 h-1.5 rounded-full shrink-0 {severityDotClass(
                alert.severity
              )}"
              aria-hidden="true"
            ></span>
            <span class="min-w-0">
              {alert.message}
              <span class="opacity-60 whitespace-nowrap">
                &bull; {formatRelativeTime(alert.since)}
              </span>
            </span>
          </li>
        {/each}
      </ul>
    </div>
  </div>
{/if}
