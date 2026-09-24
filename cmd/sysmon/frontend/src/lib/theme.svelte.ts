// Theme selection for the dashboard.
//
// Two states, light and dark, matching go-netmon exactly. The two tools are run
// by the same person and their theme controls should not disagree about what a
// theme control is.
//
// There was a third state, "follow the operating system", and it was working
// -- it read prefers-color-scheme and tracked OS changes live. It was removed
// anyway, on the ground that decides it: a user cannot tell which state they
// are in without changing their operating system. On a light-mode machine
// "system" and "light" render identically, so a third of the control was
// invisible, unverifiable, and indistinguishable from a bug.
//
// The sensible part of it survives without being a mode. With nothing stored,
// the first load takes its theme from prefers-color-scheme, so a dark-mode
// machine opens dark. The first explicit choice replaces that for good.

import { readStored, writeStored } from "$lib/storage";

/**
 * THEME_KEY is the localStorage key holding the chosen theme.
 *
 * public/theme-init.js, loaded from the <head> of index.html, is a pre-paint
 * bootstrap that reads this same key by its literal name. It has to: a module script runs after first paint, so applying
 * the theme from here alone would render one frame of the wrong theme on every
 * load. The two must be changed together; tests/theme.spec.ts covers the
 * bootstrap by asserting the theme is already correct on the first frame.
 */
export const THEME_KEY = "sysmon.theme";

/** ThemeMode is the theme in use. There is no third value. */
export type ThemeMode = "light" | "dark";

const DARK_QUERY = "(prefers-color-scheme: dark)";

/** isThemeMode validates a stored value, which is user-editable and survives
 * upgrades that may have changed its vocabulary -- including the upgrade that
 * removed "system", whose stored value is treated as no choice at all. */
export function isThemeMode(value: unknown): value is ThemeMode {
  return value === "light" || value === "dark";
}

/** prefersDark reports the OS preference, defaulting to dark where the query is
 * unavailable so an environment without matchMedia keeps the look this
 * dashboard has always had. */
function prefersDark(): boolean {
  return globalThis.matchMedia?.(DARK_QUERY).matches ?? true;
}

/** initialMode is the stored choice, or the OS preference when there is none. */
function initialMode(): ThemeMode {
  const stored = readStored(THEME_KEY);
  if (isThemeMode(stored)) return stored;
  return prefersDark() ? "dark" : "light";
}

/**
 * ThemeStore holds the theme and keeps the document's data-theme attribute in
 * step with it.
 *
 * The attribute is written imperatively rather than from an $effect because
 * this is module-level state with no owning component: effects need a component
 * to be created in, and the theme has to be right before any component mounts.
 */
class ThemeStore {
  #mode = $state<ThemeMode>("dark");

  constructor() {
    this.#mode = initialMode();
    this.#apply();
  }

  /** mode is the theme in use. Charts read it: they draw to a canvas and
   * cannot inherit CSS custom properties. */
  get mode(): ThemeMode {
    return this.#mode;
  }

  /** resolved is the theme on screen. It is the same thing as mode now that
   * there is no indirection, and is kept because every chart reads it. */
  get resolved(): ThemeMode {
    return this.#mode;
  }

  /** other is the theme a toggle would switch to. */
  get other(): ThemeMode {
    return this.#mode === "dark" ? "light" : "dark";
  }

  /** setMode records a choice and applies it immediately. */
  setMode(mode: ThemeMode): void {
    this.#mode = mode;
    writeStored(THEME_KEY, mode);
    this.#apply();
  }

  /** toggle switches to the other theme. */
  toggle(): void {
    this.setMode(this.other);
  }

  #apply(): void {
    globalThis.document?.documentElement.setAttribute("data-theme", this.#mode);
  }
}

export const theme = new ThemeStore();
