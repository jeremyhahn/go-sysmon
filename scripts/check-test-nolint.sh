#!/usr/bin/env bash
#
# Fails when a Go test file silences errcheck with a //nolint directive.
#
# errcheck runs over test code in full (.golangci.yml, #196): a test that drops
# the error from its own cleanup, or from its own fake server, can pass over the
# very failure it exists to catch. A //nolint directive is the one way around
# that which the linter cannot report, because the directive IS the linter being
# told to be quiet -- seven `defer s.Close() //nolint:errcheck` sat in
# pkg/server that way. So the directive itself is what this rejects:
#
#   //nolint:errcheck            names errcheck
#   //nolint:gosec,errcheck      names it among others
#   //nolint:all                 silences every linter, errcheck included
#   //nolint  or  //nolint // x  no linter list at all, which also means all
#
# A directive naming only other linters (//nolint:noctx) is left alone.
#
# Untracked files are scanned too, so a new test file fails before it is ever
# committed rather than after.
set -euo pipefail

readonly DIRECTIVE_RE='//nolint($|[^:[:alnum:]_-]|:([[:alnum:]_-]+,)*(errcheck|all)($|[^[:alnum:]_-]))'

if git grep -nE --untracked "$DIRECTIVE_RE" -- '*_test.go'; then
    echo "::error::a test silences errcheck with //nolint, listed above."
    echo "         Check the error instead: a t.Cleanup that t.Errorf's when Close fails."
    exit 1
fi
echo "ok   no test silences errcheck"
