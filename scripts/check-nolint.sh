#!/usr/bin/env bash
#
# Fails when any Go file silences a guard linter with a //nolint directive.
#
# Three linters in .golangci.yml hold rules with no exceptions, and a //nolint
# directive is the one way around each that the linter cannot report, because
# the directive IS the linter being told to be quiet:
#
#   errcheck   every error is checked, in production and test code alike
#              (#196, #207). Seven `defer s.Close() //nolint:errcheck` sat in
#              pkg/server's tests that way, and two more in cmd/sysmon.
#   errname    an error type is named *Error and never Err* (#194).
#   forbidigo  production code has no errors.New or fmt.Errorf: every error
#              is a named type, with no sentinels (#194).
#
# So the directive itself is what this rejects:
#
#   //nolint:errcheck            names a guard linter
#   //nolint:gosec,forbidigo     names one among others
#   //nolint:all                 silences every linter, the guards included
#   //nolint  or  //nolint // x  no linter list at all, which also means all
#
# A directive naming only other linters (//nolint:unused) is left alone.
#
# Untracked files are scanned too, so a new file fails before it is ever
# committed rather than after.
set -euo pipefail

readonly GUARDS='errcheck|errname|forbidigo|all'
readonly DIRECTIVE_RE="//nolint(\$|[^:[:alnum:]_-]|:([[:alnum:]_-]+,)*(${GUARDS})(\$|[^[:alnum:]_-]))"

if git grep -nE --untracked "$DIRECTIVE_RE" -- '*.go' ':!:cmd/sysmon/frontend/node_modules'; then
    echo "::error::a Go file silences errcheck, errname or forbidigo with //nolint, listed above."
    echo "         Fix the finding instead: handle the error, or give it a named *Error type."
    exit 1
fi
echo "ok   no Go file silences errcheck, errname or forbidigo"
