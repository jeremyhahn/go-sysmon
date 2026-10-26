### Fixed

- **The Gitea image is pushed where the release runner may write.** The
  `Gitea Image` job now pushes `agent-ci-images/go-sysmon`, the namespace of
  the release runner's registry user (ruling 169), instead of
  `jeremyhahn/go-sysmon`, which only that user or a site admin may write. The
  registry refused v0.3.1-alpha's push there with 401 (claude-agents #342).
