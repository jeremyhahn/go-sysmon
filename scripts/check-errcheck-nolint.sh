#!/usr/bin/env bash
#
# Fails when any Go file silences errcheck with a //nolint directive.
#
# errcheck runs over every file in full (.golangci.yml): test code since #196,
# production code since #207, with no exemption for Close, Flush, writes or the
# blank identifier. A //nolint directive is the one way around that which the
# linter cannot report, because the directive IS the linter being told to be
# quiet -- seven `defer s.Close() //nolint:errcheck` sat in pkg/server's tests
# that way, and two more in cmd/sysmon's production code. So the directive
# itself is what this rejects:
#
#   //nolint:errcheck            names errcheck
#   //nolint:gosec,errcheck      names it among others
#   //nolint:all                 silences every linter, errcheck included
#   //nolint  or  //nolint // x  no linter list at all, which also means all
#
# A directive naming only other linters (//nolint:unused) is left alone.
#
# Untracked files are scanned too, so a new file fails before it is ever
# committed rather than after.
set -euo pipefail

readonly DIRECTIVE_RE='//nolint($|[^:[:alnum:]_-]|:([[:alnum:]_-]+,)*(errcheck|all)($|[^[:alnum:]_-]))'

if git grep -nE --untracked "$DIRECTIVE_RE" -- '*.go' ':!:cmd/sysmon/frontend/node_modules'; then
    echo "::error::a Go file silences errcheck with //nolint, listed above."
    echo "         Handle the error instead: return it, or log it where nothing can receive it."
    exit 1
fi
echo "ok   no Go file silences errcheck"
