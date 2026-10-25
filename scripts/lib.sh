# shellcheck shell=sh disable=SC2034
# lib.sh — the helpers the release scripts share. Sourced, never run:
#
#   set -eu
#   # shellcheck source=lib.sh
#   . "${0%/*}/lib.sh"
#
# Defines (the caller's $0 is the script, so these describe the script):
#   here             the directory holding the scripts
#   prog             the script's name without .sh, the prefix of die()
#   NL               one newline
#   die MSG...       print "<prog>: MSG" to stderr and exit 1
#   matches VALUE ERE  the WHOLE value matches
LC_ALL=C
export LC_ALL

here=$(CDPATH='' cd -- "${0%/*}" && pwd)
prog=${0##*/}
prog=${prog%.sh}

NL='
'

die() { echo "$prog: $*" >&2; exit 1; }

# `grep` alone is line-oriented, so a value with an embedded newline would pass
# on any one matching line.
matches() {
  case $1 in *"$NL"*) return 1 ;; esac
  printf '%s\n' "$1" | grep -Eq "$2"
}
