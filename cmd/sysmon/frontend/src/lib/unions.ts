// The string unions the Go types do not name.
//
// Everything else in $lib/types is generated from pkg/types/types.go. These
// five are not, because the Go side declares them as plain `string` fields
// whose legal values are untyped constants no Go type collects — there is
// nothing for the generator to read. `SMARTStatus` shows the difference: it IS
// a named Go type with typed constants, so its union is generated and does not
// appear here.
//
// internal/tsgen names each of these against the field it narrows, in one
// table of seven entries. Adding an eighth means adding it there too, and the
// emitted file will not compile until the alias exists here — which is the
// point of keeping them in a module of their own rather than loose in a
// generated file that says DO NOT EDIT.

/** Alert severities, ordered by urgency. Mirrors the Severity* constants. */
export type Severity = "info" | "warning" | "critical";

/** A peer's rolled-up health, as the local node last saw it. */
export type NodeStatus = "healthy" | "warning" | "critical" | "unreachable";

/**
 * Which layer won for a configuration key: default < config file < env < flag,
 * or "runtime" when the running service was changed after startup.
 *
 * Widened with `string` at the use site, deliberately: the server decides what
 * a source is called, and a value this list has not caught up with should
 * render as itself rather than fail to type-check.
 */
export type ConfigSource = "default" | "config file" | "env" | "flag" | "runtime";

/** The unit a history series is measured in, which is what picks its axis. */
export type HistoryUnit =
  | "percent"
  | "bytes_per_second"
  | "celsius"
  | "watts"
  | "count";

/**
 * Whether a peer runs the same build as the aggregator serving the view.
 *
 * NOT a health signal: the server reports divergence as a fact to display, and
 * "unknown" is its own answer that never collapses into "same".
 */
export type BuildMatch = "same" | "different" | "unknown";
