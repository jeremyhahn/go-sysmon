#!/usr/bin/env python3
"""Fail a build on container vulnerabilities that can actually be fixed.

Trivy reports every known CVE in the image, including ones the distribution
has chosen not to patch. Those cannot be resolved by any change to this
repository, so blocking on them would wedge every build permanently and teach
everyone to ignore the scanner. Findings that carry a FixedVersion are a
different matter: they mean a base image or package bump is available.

Usage: trivy-gate.py <trivy-json>
Exit:  1 if any finding has a fix available, 0 otherwise.
"""

import json
import sys


def main(path: str) -> int:
    with open(path) as fh:
        report = json.load(fh)

    fixable = []
    unfixable = 0

    for result in report.get("Results") or []:
        for vuln in result.get("Vulnerabilities") or []:
            if vuln.get("FixedVersion"):
                fixable.append(
                    "{severity:9s} {id:20s} {pkg:26s} {installed} -> {fixed}".format(
                        severity=vuln.get("Severity", "?"),
                        id=vuln.get("VulnerabilityID", "?"),
                        pkg=vuln.get("PkgName", "?"),
                        installed=vuln.get("InstalledVersion", "?"),
                        fixed=vuln["FixedVersion"],
                    )
                )
            else:
                unfixable += 1

    print("unfixable, reported only: {}".format(unfixable))
    print("fixable: {}".format(len(fixable)))
    for line in fixable:
        print("   " + line)

    if fixable:
        print("::error::{} vulnerabilities have fixes available".format(len(fixable)))
        return 1
    return 0


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print(__doc__)
        sys.exit(2)
    sys.exit(main(sys.argv[1]))
