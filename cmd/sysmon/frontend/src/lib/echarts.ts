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

// The chart surface colours live in palette.ts and are re-exported here, so a
// chart component has one import for everything it draws with.
export { chartPalette } from "./palette";
export type { ChartPalette, ThemeName } from "./palette";
