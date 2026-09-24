// Chart tooltip content, built as DOM nodes rather than as an HTML string.
//
// ECharts assigns a string formatter's result to innerHTML. Two things follow,
// and both are why this module exists:
//
//  - A colour swatch written as <span style="color:..."> is an inline style
//    attribute parsed from markup, which the server's Content-Security-Policy
//    (style-src 'self', no 'unsafe-inline') refuses. The swatch would silently
//    lose its colour. A colour set through the CSSOM -- element.style.color --
//    is not governed by style-src, so it is applied.
//  - Series names are data: interface, disk and peer names from snapshots,
//    some of them relayed from other nodes. Interpolated into an HTML string
//    they are markup; set as textContent they are only ever text.
//
// ECharts accepts an HTMLElement from a tooltip formatter and appends it as
// is, so nothing is parsed from a string anywhere on this path.

/** One series line in a tooltip: its colour swatch, name and formatted value. */
export interface TooltipRow {
  color: string;
  name: string;
  value: string;
}

/**
 * tooltipContent returns the tooltip body for rows, headed by title when one
 * is given. Each row reads "● name: value" with the value in bold, one row per
 * line, exactly as the string formatters it replaces rendered.
 */
export function tooltipContent(rows: TooltipRow[], title?: string): HTMLElement {
  const root = document.createElement("div");
  if (title) {
    const heading = document.createElement("div");
    heading.textContent = title;
    root.append(heading);
  }
  for (const row of rows) {
    const line = document.createElement("div");

    const swatch = document.createElement("span");
    swatch.textContent = "●";
    swatch.style.color = row.color;

    const value = document.createElement("b");
    value.textContent = row.value;

    line.append(swatch, ` ${row.name}: `, value);
    root.append(line);
  }
  return root;
}
