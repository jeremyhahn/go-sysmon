<script lang="ts">
  // Read-only view of the effective configuration, in a dialog rather than a
  // tab: settings are a reference you consult while looking at something else,
  // not a place you spend time. There is deliberately no input and no save
  // button — configuration is edited on disk and reloaded, and a form here
  // would imply otherwise.
  import { FLEET_URL } from "$lib/fleetpage";
  import type { ConfigEntry, ConfigSource, ConfigView } from "$lib/types";
  import { monitor } from "$stores/monitor.svelte";

  interface Props {
    open: boolean;
    onClose: () => void;
  }

  const { open, onClose }: Props = $props();

  // Settings follow the selected node. Showing the aggregator's own
  // configuration while a peer is selected would read as that peer's settings
  // and be wrong.
  const selectedNode = $derived(monitor.selectedNode);
  const configURL = $derived(
    selectedNode == null
      ? "api/config"
      : `${FLEET_URL}/${encodeURIComponent(selectedNode)}/config`
  );

  let dialogEl = $state<HTMLDialogElement | null>(null);
  let config = $state<ConfigView | null>(null);
  let loadError = $state<string | null>(null);
  let loading = $state(false);

  /** Entries grouped by section, sections and keys in a stable order. */
  const sections = $derived.by(() => {
    const bySection = new Map<string, ConfigEntry[]>();
    for (const entry of config?.entries ?? []) {
      const list = bySection.get(entry.section);
      if (list == null) {
        bySection.set(entry.section, [entry]);
      } else {
        list.push(entry);
      }
    }
    return [...bySection.entries()]
      .map(([name, entries]) => ({ name, entries }))
      .sort((a, b) => a.name.localeCompare(b.name));
  });

  /**
   * `config_file` says WHETHER a file is in force, not where it is: the
   * endpoint has no authentication of its own, so the server withholds the
   * path rather than handing out its filesystem layout. Anything else is a
   * peer still running a build that sent the path, and is shown verbatim
   * rather than described wrongly.
   */
  function configFileLabel(state: string): string {
    switch (state) {
      case "(configured)":
        return "reading a configuration file";
      case "(not set)":
      case "":
        return "no config file — running on defaults";
      default:
        return state;
    }
  }

  /**
   * The badge answers "how surprising is this line": a default is unremarkable,
   * a flag or env var is something someone deliberately did.
   */
  function sourceBadge(source: ConfigSource): string {
    switch (source) {
      case "config file":
        return "badge-info";
      case "env":
        return "badge-warning";
      case "flag":
        return "badge-success";
      case "runtime":
        // Distinct from every configuration layer: the value is genuinely in
        // force but no layer accounts for it, and a reload will replace it.
        return "badge-accent";
      default:
        return "badge-neutral";
    }
  }

  // Drive the native dialog from the prop, so Esc and the backdrop stay
  // functional while the parent still owns the state.
  $effect(() => {
    const el = dialogEl;
    if (el == null) return;
    if (open && !el.open) {
      el.showModal();
    } else if (!open && el.open) {
      el.close();
    }
  });

  // Fetch when opened, and again if the selected node changes while open, so
  // the dialog can never show a different node's settings than its title says.
  $effect(() => {
    if (!open) return;
    const url = configURL;
    let cancelled = false;

    loading = true;
    loadError = null;

    void (async () => {
      try {
        const res = await fetch(url);
        if (cancelled) return;
        if (!res.ok) {
          loadError = `The server returned ${res.status} for ${url}.`;
          config = null;
          return;
        }
        // Parse first, then re-check. The guard above has already passed by
        // the time this await resolves, so without a second check a config
        // parsed for the previous node lands in a dialog now titled another.
        const view = (await res.json()) as ConfigView;
        if (cancelled) return;
        config = view;
      } catch {
        if (cancelled) return;
        loadError = "Could not reach the configuration endpoint.";
        config = null;
      } finally {
        if (!cancelled) loading = false;
      }
    })();

    return () => {
      cancelled = true;
    };
  });
</script>

<dialog bind:this={dialogEl} class="modal" onclose={onClose}>
  <div class="modal-box max-w-3xl p-0 overflow-hidden">
    <!-- Header -->
    <div class="flex items-start justify-between gap-3 p-4 border-b border-base-300">
      <div class="min-w-0">
        <h2 class="font-semibold text-base flex items-center gap-2">
          Configuration
          {#if selectedNode != null}
            <span class="badge badge-primary badge-sm">{selectedNode}</span>
          {/if}
          <span class="badge badge-ghost badge-sm">read-only</span>
        </h2>
        {#if config != null}
          <p class="text-xs text-base-content/60 mt-1 truncate">
            {configFileLabel(config.config_file)}
          </p>
        {/if}
      </div>
      <button
        type="button"
        class="btn btn-sm btn-circle btn-ghost shrink-0"
        aria-label="Close settings"
        onclick={onClose}
      >
        ✕
      </button>
    </div>

    <!-- Body -->
    <div class="p-4 max-h-[65vh] overflow-y-auto space-y-4">
      {#if loading}
        <div class="flex justify-center items-center h-24">
          <span class="loading loading-spinner loading-md text-primary"></span>
        </div>
      {:else if loadError != null}
        <div role="alert" class="alert alert-warning">
          <div>
            <p class="font-semibold">Configuration unavailable</p>
            <p class="text-sm">{loadError}</p>
          </div>
        </div>
      {:else if sections.length === 0}
        <p class="text-sm text-base-content/70">
          The server reported no configuration entries.
        </p>
      {:else}
        {#each sections as section (section.name)}
          <section>
            <h3
              class="text-xs font-semibold uppercase tracking-wider text-base-content/60 mb-1"
            >
              {section.name}
            </h3>
            <!-- table-fixed with explicit column widths: every section shares
                 one layout, so the columns line up down the whole dialog
                 instead of each table sizing itself to its own longest key. -->
            <table class="table table-xs table-fixed w-full">
              <thead>
                <tr class="text-base-content/60">
                  <th class="w-[52%] font-medium">Setting</th>
                  <th class="w-[28%] font-medium">Value</th>
                  <th class="w-[20%] font-medium">Source</th>
                </tr>
              </thead>
              <tbody>
                {#each section.entries as entry (entry.key)}
                  <tr class="hover">
                    <td class="font-mono text-xs truncate" title={entry.key}>
                      {entry.key}
                    </td>
                    <td class="font-mono text-xs break-all">
                      {entry.value === "" ? "—" : entry.value}
                    </td>
                    <td>
                      <span class="badge {sourceBadge(entry.source)} badge-sm whitespace-nowrap">
                        {entry.source}
                      </span>
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </section>
        {/each}
      {/if}
    </div>

    <!-- Footer: how to actually change any of this -->
    <div class="p-3 border-t border-base-300 bg-base-200/40">
      <p class="text-xs text-base-content/60">
        Edit <span class="font-mono">/etc/go-sysmon/sysmon.yaml</span> (or
        <span class="font-mono">sysmon.env</span>), then
        <span class="font-mono">systemctl reload go-sysmon</span>. Later layers
        win: default → config file → env → flag. A
        <span class="badge badge-accent badge-xs align-middle">runtime</span>
        value was changed on the running service and a reload will replace it.
      </p>
    </div>
  </div>

  <!-- Clicking the backdrop closes, matching the Esc key. -->
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close settings">close</button>
  </form>
</dialog>
