// Chart surface colours.
//
// Split out from echarts.ts so it can be read without loading the charting
// bundle: these are plain data, and tests/contrast.spec.ts measures them.
//
// The charts draw to a canvas, so they inherit nothing from the stylesheet: a
// theme switch that repaints every daisyUI surface leaves an ECharts axis the
// colour it was compiled with. Every colour that describes the *surface* --
// axes, gridlines, tooltip chrome, the gauge track -- therefore lives here,
// per theme, and the chart components read it rather than spelling hex codes
// inline. Series colours are not here: they encode meaning (green healthy, red
// saturated) and are legible on both surfaces.
//
// ECharts' stock "dark" theme is not used, because it is distributed as a UMD
// bundle that pulls in the full build and so cannot coexist with the tree
// shaking above.

export type ThemeName = "light" | "dark";

export interface ChartPalette {
  /** darkMode tells ECharts which way to compute the few contrast-derived
   * colours the options below do not state outright. */
  darkMode: boolean;
  /** surface backs the tooltip popup. */
  surface: string;
  /** border outlines the tooltip and draws the category axis line. */
  border: string;
  /** track draws horizontal gridlines and the unfilled part of a gauge. */
  track: string;
  /** mutedText is for secondary lettering: tooltip body, gauge caption. */
  mutedText: string;
  /** axisLabel is for tick labels, which sit outside the plot area. */
  axisLabel: string;
  /** axisPointer is the crosshair the tooltip drags across the plot. */
  axisPointer: string;

  /**
   * usage colours a reading by how alarming it is: green, amber, red.
   *
   * These are text as well as fill -- a gauge letters its percentage in them --
   * so the light values are the ones that reach WCAG AA on a card, not the
   * ones that look right on black. The dark values are unchanged.
   */
  usage: { ok: string; warn: string; crit: string };

  /**
   * series are the line colours, in the order a chart assigns them.
   *
   * A stroke is a graphical object rather than text, so the bar is 3:1 rather
   * than 4.5:1 -- but the stock palette was picked against a dark surface and
   * does not reach even that on white. Every one of these is the same hue at a
   * lightness that does.
   */
  series: string[];
}

const PALETTES: Record<ThemeName, ChartPalette> = {
  dark: {
    darkMode: true,
    surface: "#1d232a",
    border: "#3d4451",
    track: "#2a323c",
    mutedText: "#a6adbb",
    axisLabel: "#6b7280",
    // The stock dark theme's axis pointer, the one tooltip element the chart
    // options did not already state for themselves.
    axisPointer: "#817f91",
    usage: { ok: "#36d399", warn: "#fbbd23", crit: "#f87272" },
    series: [
      "#36d399",
      "#38bdf8",
      "#fb923c",
      "#a78bfa",
      "#f472b6",
      "#facc15",
      "#f87272",
      "#2dd4bf",
    ],
  },
  light: {
    darkMode: false,
    surface: "#ffffff",
    border: "#d1d5db",
    track: "#e5e7eb",
    mutedText: "#4b5563",
    axisLabel: "#6b7280",
    axisPointer: "#9ca3af",
    // Measured against base-200 (#f8f8f8), the card these are drawn on:
    // 4.60:1 each, against 1.81 / 1.59 / 2.59 for the dark palette's values.
    usage: { ok: "#1d805b", warn: "#906b0f", crit: "#b65252" },
    // 3.10:1 each, against 1.44-2.56 before. Same hues, darker.
    series: [
      "#27a073",
      "#2b97c7",
      "#cf7830",
      "#977de3",
      "#da65a2",
      "#aa8a0a",
      "#e26767",
      "#1f9f8e",
    ],
  },
};

/** chartPalette returns the surface colours for a theme. */
export function chartPalette(theme: ThemeName): ChartPalette {
  return PALETTES[theme];
}
