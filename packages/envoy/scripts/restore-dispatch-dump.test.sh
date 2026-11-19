#!/usr/bin/env bash
# Locale test for restore-dispatch-dump.sh (fakes psql and pg_restore; no Postgres).
#
# The script refuses a database name that is no PostgreSQL identifier "no longer than 63
# characters". It sets LC_ALL=C.UTF-8 itself, so its check reads alike in every caller's locale:
# [a-zA-Z] admits the ASCII letters alone, and ${#name} counts code points, which for ASCII are the
# bytes PostgreSQL's 63 counts. Each row runs the script under en_US.UTF-8 and under C.UTF-8 with
# stubs that record a call, and reads whether the name check refused the name (exit 2, its
# refusal, no stub called) or passed it (a stub was called).
#
# Rows: a name of 40 "é" (80 bytes) is refused in both locales, 64 ASCII letters are refused in
# both, and 63 pass in both. The control runs a copy of the script without its LC_ALL line and
# wants the 80-byte name to pass under en_US.UTF-8, the fault the line closes, so a run that
# cannot see the fault fails. A box with no en_US.UTF-8 locale fails, naming it, since the rows
# would prove nothing there.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
script="$here/restore-dispatch-dump.sh"
locale -a 2>/dev/null | grep -qix 'en_US.utf8' || { echo "FAIL: this box has no en_US.UTF-8 locale, so the rows prove nothing"; exit 1; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"
for stub in psql pg_restore; do
  printf '#!/bin/sh\necho called >>"%s/called"\nexit 1\n' "$work" >"$work/bin/$stub"
  chmod +x "$work/bin/$stub"
done
: >"$work/dump"
grep -v '^export LC_ALL=' "$script" >"$work/without-locale.sh"
cmp -s "$script" "$work/without-locale.sh" && { echo "FAIL: the script sets no LC_ALL of its own"; exit 1; }

# run <script> <locale> <name>: "refused" when the name check refused the name, else "passed".
run() {
  rm -f "$work/called"
  LC_ALL=$2 PATH="$work/bin:$PATH" bash "$1" "$work/dump" "$3" >"$work/out" 2>&1
  local code=$?
  if [ "$code" = 2 ] && grep -q 'database name must be a PostgreSQL identifier' "$work/out" && [ ! -e "$work/called" ]; then
    echo refused
  elif [ -e "$work/called" ]; then
    echo passed
  else
    echo "neither (exit $code: $(head -c 200 "$work/out"))"
  fi
}

accented=$(printf 'é%.0s' $(seq 1 40))
failed=0
check() { # <row> <got> <want>
  if [ "$2" = "$3" ]; then
    echo "ok    $1: $2"
  else
    echo "FAIL  $1: $2, want $3"
    failed=1
  fi
}
for locale in en_US.UTF-8 C.UTF-8; do
  check "40 é, 80 bytes, under $locale" "$(run "$script" "$locale" "$accented")" refused
  check "64 ASCII letters under $locale" "$(run "$script" "$locale" "$(printf 'a%.0s' $(seq 1 64))")" refused
  check "63 ASCII letters under $locale" "$(run "$script" "$locale" "$(printf 'a%.0s' $(seq 1 63))")" passed
done
check "control: without its LC_ALL line, 40 é under en_US.UTF-8" "$(run "$work/without-locale.sh" en_US.UTF-8 "$accented")" passed
exit "$failed"
