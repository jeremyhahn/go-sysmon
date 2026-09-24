# Build shapes

go-sysmon is compiled with several build-tag sets. `go vet` and `go test`
report success over the files their tags select and say nothing about the rest,
so a file behind a tag that no gate passes can stop compiling with every check
green. That happened: the desktop build's test files did not compile, and
nothing noticed (#204).

So every tag set the project ships is a named **shape** in the Makefile, and
the gates run over all of them.

## The shapes

| Shape | Tags | What ships with it |
|---|---|---|
| `default` | none | What an embedder of `pkg/*` gets. Also built with `CGO_ENABLED=0` (`check-cgo-clean`) and in FIPS mode (`test-fips`) |
| `image` | `nvml` | The published container image (`Dockerfile`) |
| `server` | `prometheus,nvml` | `sysmon-server`: releases and `make deploy` |
| `desktop` | `desktop,production,webkit2_41,nvml,systray` | `sysmon`, the desktop binary, with the tray |
| `desktop-notray` | `desktop,production,webkit2_41,nvml` | `sysmon` built with `WITH_SYSTRAY=0` |

Two more are compiled but ship nothing:

| Shape | Tags | Why |
|---|---|---|
| `nomegaraid` | `nomegaraid` | Stands in for a target without the RAID passthrough, which cannot otherwise be compiled on this host |
| `integration` | `integration` | The integration suite. Vetted here; it runs in its container (`make integration-test`) |

## What runs where

| Target | Shapes |
|---|---|
| `make vet` | all seven |
| `make lint` | golangci-lint in all seven, plus `check-build-tags` |
| `make test` | the five shipped shapes and `nomegaraid`, with `-race` |
| `make test-fips` | `default` and `prometheus`, with `GOFIPS140=latest` |
| `make gosec` | all seven |
| `make vulncheck` | the five shipped shapes |

Each shape is also a target of its own: `make vet-shape-desktop`,
`make lint-shape-server`, `make test-shape-image`, `make gosec-shape-desktop`,
`make vulncheck-shape-server`.

The desktop shapes link GTK and WebKit, so vet, lint and test need the
development packages `make deps` installs. CI installs them in the lint, test
and security jobs.

## The guard

`make check-build-tags` (part of `make lint`) fails when a build constraint
names a tag that no shape sets, and names the file and line:

```
buildtags: 1 use(s) of a build tag that no shipped build shape sets, so no gate compiles the files behind them.
Add the tag set to BUILD_SHAPES in the Makefile, or remove the constraint:
  pkg/collector/foo.go:1: newtag
```

Tags the toolchain sets itself (every GOOS and GOARCH, `unix`, `cgo`, release
tags) are accepted. The scan walks the module the way `./...` does, skipping
`testdata`, `vendor`, dot and underscore directories and nested modules. It
also fails when the `Dockerfile` builds the image with tags other than
`IMAGE_TAGS`.

The implementation is `internal/buildtags`, run by `cmd/buildtags`.

## Adding a tag

1. Add the shape that sets it to `BUILD_SHAPES` (or `ALL_SHAPES` for one that
   ships nothing) at the top of the `Makefile`.
2. Put a test beside any file the tag selects, under the same constraint. A test
   file must carry the constraint of the code it tests: a test of shared code
   carries none, so every shape runs it.
