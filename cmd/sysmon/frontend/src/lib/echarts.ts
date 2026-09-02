// Tree-shaken ECharts build.
//
// The `echarts` package's default entry point registers every chart type,
// component and renderer it ships, which is most of a megabyte for a dashboard
// that draws exactly two kinds of chart. Importing from `echarts/core` and
// registering only what is drawn lets the bundler drop the rest.
//
// Anything added to a chart option later — a new series type, a legend, a data
// zoom — must be registered here first, or ECharts silently renders nothing for
// it. That is the tradeoff for the smaller bundle.

import { init, use, graphic } from "echarts/core";
import type { ComposeOption, ECharts } from "echarts/core";
import { GaugeChart, LineChart } from "echarts/charts";
import type { GaugeSeriesOption, LineSeriesOption } from "echarts/charts";
import { GridComponent, TooltipComponent } from "echarts/components";
import type {
  GridComponentOption,
  TooltipComponentOption,
} from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";

// GridComponent supplies the cartesian coordinate system used by the line
// chart, including its axes. TooltipComponent pulls in the axis pointer on its
// own, so `trigger: "axis"` works without registering that separately. The
// gauge is self-contained and needs no coordinate system.
use([GaugeChart, LineChart, GridComponent, TooltipComponent, CanvasRenderer]);

export { init, graphic };
export type { ECharts };

// ChartOption is the option type narrowed to the registered pieces, so a
// setting belonging to an unregistered component fails to compile rather than
// disappearing at runtime.
export type ChartOption = ComposeOption<
  | GaugeSeriesOption
  | LineSeriesOption
  | GridComponentOption
  | TooltipComponentOption
>;

// Chart surface colours.
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
  },
  light: {
    darkMode: false,
    surface: "#ffffff",
    border: "#d1d5db",
    track: "#e5e7eb",
    mutedText: "#4b5563",
    axisLabel: "#6b7280",
    axisPointer: "#9ca3af",
  },
};

/** chartPalette returns the surface colours for a theme. */
export function chartPalette(theme: ThemeName): ChartPalette {
  return PALETTES[theme];
}
