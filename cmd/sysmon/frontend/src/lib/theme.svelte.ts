// Theme selection for the dashboard.
//
// Three modes, not two. "System" is a state the user can be in, not the absence
// of a choice: a user who has not picked follows the operating system and keeps
// following it live, so a laptop that flips to dark at sunset flips the
// dashboard with it. A user who has picked keeps that choice on every machine
// sharing the browser profile, and can get back to following the OS -- which a
// plain two-way toggle makes impossible once it has been touched.

import { readStored, writeStored } from "$lib/storage";

/**
 * THEME_KEY is the localStorage key holding the chosen mode.
 *
 * index.html contains a pre-paint bootstrap that reads this same key by its
 * literal name. It has to: a module script runs after first paint, so applying
 * the theme from here alone would render one frame of the wrong theme on every
 * load. The two must be changed together; tests/theme.spec.ts covers the
 * bootstrap by asserting the theme is already correct on the first frame.
 */
export const THEME_KEY = "sysmon.theme";

/** ThemeMode is what the user selected. */
export type ThemeMode = "system" | "light" | "dark";

/** ResolvedTheme is what is actually applied to the document. */
export type ResolvedTheme = "light" | "dark";

const DARK_QUERY = "(prefers-color-scheme: dark)";

/** isThemeMode validates a stored value, which is user-editable and survives
 * upgrades that may have changed its vocabulary. */
export function isThemeMode(value: unknown): value is ThemeMode {
  return value === "system" || value === "light" || value === "dark";
}

/**
 * resolveTheme collapses a mode and the OS preference into the theme actually
 * applied. It is pure and exported so the rule can be stated once and tested
 * without a DOM.
 */
export function resolveTheme(
  mode: ThemeMode,
  systemPrefersDark: boolean
): ResolvedTheme {
  if (mode === "system") return systemPrefersDark ? "dark" : "light";
  return mode;
}

/** prefersDark reports the OS preference, defaulting to dark where the query is
 * unavailable so an environment without matchMedia keeps the look this
 * dashboard has always had. */
function prefersDark(): boolean {
  return globalThis.matchMedia?.(DARK_QUERY).matches ?? true;
}

function loadMode(): ThemeMode {
  const stored = readStored(THEME_KEY);
  return isThemeMode(stored) ? stored : "system";
}

/**
 * ThemeStore holds the selected mode and the current OS preference, and keeps
 * the document's data-theme attribute in step with both.
 *
 * The attribute is written imperatively rather than from an $effect because
 * this is module-level state with no owning component: effects need a component
 * to be created in, and the theme has to be right before any component mounts.
 */
class ThemeStore {
  #mode = $state<ThemeMode>("system");
  #systemDark = $state(true);

  constructor() {
    this.#mode = loadMode();
    this.#systemDark = prefersDark();
    this.#apply();

    // Only fires while the page is open; a change made while it is closed is
    // picked up by prefersDark() on the next load. The listener is never
    // removed, which is correct here: this store is a module singleton that
    // lives as long as the document, so there is nothing to remove it for and
    // no caller that could.
    globalThis
      .matchMedia?.(DARK_QUERY)
      .addEventListener("change", (e: MediaQueryListEvent) => {
        this.#systemDark = e.matches;
        this.#apply();
      });
  }

  /** mode is the user's selection, including "system". */
  get mode(): ThemeMode {
    return this.#mode;
  }

  /** resolved is the theme on screen. Charts read this: they draw to a canvas
   * and cannot inherit CSS custom properties. */
  get resolved(): ResolvedTheme {
    return resolveTheme(this.#mode, this.#systemDark);
  }

  /** setMode records a selection and applies it immediately. */
  setMode(mode: ThemeMode): void {
    this.#mode = mode;
    writeStored(THEME_KEY, mode);
    this.#apply();
  }

  #apply(): void {
    globalThis.document?.documentElement.setAttribute("data-theme", this.resolved);
  }
}

export const theme = new ThemeStore();
