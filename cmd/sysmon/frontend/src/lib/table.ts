/**
 * Column describes one column of a DataTable.
 *
 * Providing sortValue makes the column sortable; omit it for display-only
 * columns such as rendered bars or action buttons.
 */
export interface Column<Row> {
  /** Stable identifier, also used as the sort key. */
  key: string;
  label: string;
  align?: "left" | "right";
  /** Comparable value for sorting. Omit to disable sorting on this column. */
  sortValue?: (row: Row) => string | number;
  /** Tooltip explaining what the column means. */
  title?: string;
}
