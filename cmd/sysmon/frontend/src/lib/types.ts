// The wire types, as one import.
//
// Every interface here is GENERATED from pkg/types/types.go -- see
// types.generated.ts, and run `make generate` after changing a Go wire type.
// This file exists so that several hundred
// `import type { Snapshot } from "$lib/types"` lines do not have to know that.
//
// It used to be the mirror itself: 780 lines of hand-written interfaces
// restating the Go structs, kept in step by remembering to. They were not in
// step. Regenerating them turned up ten fields and one whole interface the
// server had been sending that the dashboard did not declare -- NetworkInfo's
// `index` and `wireless`, DiskInfo's and DIMMInfo's `index`, DiskMember's
// `online`, RuntimeInfo's `images_filtered`, Snapshot's `version`,
// `git_commit` and `build_date`, and WirelessInfo entirely -- none of it
// visible to the page or to svelte-check, because a mirror that omits a field
// omits the evidence with it (#38).
//
// The unions in ./unions are the exception, and say there why they are one.

export * from "./types.generated";
export * from "./unions";
