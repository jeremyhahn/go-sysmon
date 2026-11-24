### Fixed

- **The README describes what ships.** It now covers NPUs (the Metrics table
  and `sysmon npu`), drive-bay health from the BMC, md and MegaRAID arrays,
  alerts, the fleet, Prometheus export and history, and configuration reload.
  The command list gains `host`, `npu` and `version`, the project layout lists
  every `cmd/`, `pkg/` and `internal/` directory, and the documentation table
  links the SPEC, the alerts/fleet/config page, delta streaming, remote
  deployment and testing. It no longer says there is no configuration file.
- **`docs/cli/reference.md` documents `sysmon npu`**, and
  `docs/collector/user.md` says what NPU and drive-bay readings need.
- **`docs/SPEC.md` describes the release pipeline as it runs**: a green merge
  to `main` that bumps `VERSION` is tagged and released by
  `auto-release.yml`, not by a signed tag pushed by hand. `ROADMAP.md`
  records the change, names v0.3.5-alpha as the latest release, links the
  GitHub release-candidate milestone, and stops linking issues that only
  existed on the retired Gitea forge.
