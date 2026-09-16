#!/usr/bin/env python3
"""Tell "the dependencies have advisories" from "the audit never ran".

`npm audit` exits non-zero for both, and the difference matters: the first is
ours to fix, the second is somebody else's availability. Classifying it by
matching error strings does not work -- an HTTP 429, a 5xx, a TLS or auth failure
and npm's own "audit endpoint returned an error" all read differently, and the
list has no end -- so this detects the SUCCESS case positively instead. A report
that parses and carries `auditReportVersion` and `metadata.vulnerabilities` is an
audit that completed; anything else did not, whatever it says.

Usage: audit-gate.py <npm-audit-json> [min-severity]
Exit:  0 no findings at or above min-severity (default: low)
       1 findings -- ours to fix
       2 the audit did not complete -- not a finding, and not a pass either

Set AUDIT_STATUS_FILE to have that code written to a file as well. GNU make
returns 2 for ANY failed recipe, so a caller invoking this through `make audit`
cannot recover the distinction from the process status -- 1 and 2 arrive
identically. The file is how CI reads the verdict it is meant to act on.
"""

import json
import os
import sys

# npm's own ordering, lowest first. A threshold includes everything above it.
SEVERITIES = ["info", "low", "moderate", "high", "critical"]
# npm accepts two more values for --audit-level that name no severity: "none",
# meaning nothing is worth failing for, and "null", its way of spelling unset.
# Rejecting them made a valid `make audit AUDIT_LEVEL=none` report an outage,
# which is the gate accusing a third party of being down over our own flag.
NO_THRESHOLD = "none"
UNSET = "null"
DID_NOT_RUN = 2


def main(path: str, minimum: str) -> int:
    try:
        with open(path) as fh:
            report = json.load(fh)
    except (OSError, json.JSONDecodeError) as err:
        print(f"npm audit did not produce a report: {err}")
        return DID_NOT_RUN

    # A document that parses is not necessarily a report: `null`, `[]` and a bare
    # string are all valid JSON, and reaching .get on them raised AttributeError
    # -- which escaped before the verdict was recorded, so the status file stayed
    # empty and Python's exit 1 read as "findings" under this contract.
    if not isinstance(report, dict):
        print(f"npm audit produced {type(report).__name__} rather than a report object; "
              "the audit did not complete")
        return DID_NOT_RUN

    # Both keys, not either: a truncated or unrelated JSON document can carry one.
    counts = (report.get("metadata") or {}).get("vulnerabilities")
    if "auditReportVersion" not in report or not isinstance(counts, dict):
        print("npm audit produced no report body; the audit did not complete")
        return DID_NOT_RUN

    if minimum in ("", UNSET):
        minimum = "low"
    if minimum not in SEVERITIES and minimum != NO_THRESHOLD:
        print(f"unknown severity {minimum!r}; expected one of "
              f"{', '.join(SEVERITIES + [NO_THRESHOLD, UNSET])}")
        return DID_NOT_RUN

    deps_total = ((report.get("metadata") or {}).get("dependencies") or {}).get("total", "?")
    if minimum == NO_THRESHOLD:
        # The audit ran and the caller asked for no threshold, so the counts are
        # reported and nothing fails. Saying "no advisories" here would be a lie
        # when there are some.
        total = int(counts.get("total") or 0)
        print(f"npm audit completed: {total} advisor{'y' if total == 1 else 'ies'} "
              f"across {deps_total} dependencies; --audit-level=none, so none of "
              "them fail this check")
        return 0

    at_or_above = SEVERITIES[SEVERITIES.index(minimum):]
    findings = {s: int(counts.get(s) or 0) for s in at_or_above}
    total = sum(findings.values())

    deps = (report.get("metadata") or {}).get("dependencies") or {}
    scanned = deps.get("total", "?")
    if total == 0:
        print(f"npm audit completed: no advisories at or above {minimum} "
              f"across {scanned} dependencies")
        return 0

    print(f"npm audit completed: {total} advisor{'y' if total == 1 else 'ies'} "
          f"at or above {minimum} across {scanned} dependencies")
    for severity in reversed(at_or_above):
        if findings[severity]:
            print(f"  {severity:9s} {findings[severity]}")
    # Name them, so the log is actionable without re-running anything.
    for name, vuln in sorted((report.get("vulnerabilities") or {}).items()):
        if vuln.get("severity") in findings and findings[vuln["severity"]]:
            fix = vuln.get("fixAvailable")
            fixable = "fix available" if fix else "no fix available"
            print(f"  {vuln.get('severity', '?'):9s} {name:30s} {fixable}")
    return 1


def record(code: int) -> int:
    """Write the verdict where a caller behind make can still read it."""
    path = os.environ.get("AUDIT_STATUS_FILE")
    if path:
        try:
            with open(path, "w") as fh:
                fh.write(f"{code}\n")
        except OSError as err:
            # Reported, never fatal: failing the audit because a status file
            # could not be written would be the gate inventing its own outage.
            print(f"could not write AUDIT_STATUS_FILE={path}: {err}")
    return code


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print(__doc__, file=sys.stderr)
        raise SystemExit(record(DID_NOT_RUN))
    try:
        code = main(sys.argv[1], sys.argv[2] if len(sys.argv) > 2 else "low")
    except Exception as err:  # noqa: BLE001 -- see below
        # The contract is that a verdict is ALWAYS recorded. An unhandled
        # exception would leave the status file empty and exit 1, which this
        # contract reads as "findings" -- turning a crash in the gate into a
        # false accusation against the dependencies.
        print(f"audit gate failed: {type(err).__name__}: {err}")
        code = DID_NOT_RUN
    raise SystemExit(record(code))
