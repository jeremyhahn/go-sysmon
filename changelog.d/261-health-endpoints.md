### Added

- `GET /healthz` (liveness) and `GET /readyz` (readiness). `/readyz` answers
  503 with a reason when the latest snapshot is older than three stream
  intervals (never less than 10s) or the server is draining (#261).

### Changed

- The container `HEALTHCHECK` and `docker-compose.yml` probe `/readyz` instead
  of `/`, so a stalled collector marks the container unhealthy (#261).
