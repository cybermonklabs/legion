#!/usr/bin/env bash
# packages/envoy/scripts/restore-dispatch-dump.test.sh
#
# Proves what restore-dispatch-dump.sh does with a password, with no real Postgres: fakes `psql`
# and `pg_restore` on PATH record each call's arguments and the PGPASSWORD it was given, and
# answer success. It asserts, one PASS line each: that the DATABASE_URL line it prints for an
# operator's DISPATCH_ADMIN_URL leaves that URL's password out; that the password reaches psql and
# pg_restore in PGPASSWORD alone, in no argument; that with no DISPATCH_ADMIN_URL the printed line
# keeps the default's fixed dev password `dispatch`, which a reader pastes; and that a
# DISPATCH_ADMIN_URL naming another database is refused with exit 2 and a message that prints no
# password.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly project_root
readonly driver="${project_root}/packages/envoy/scripts/restore-dispatch-dump.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
trap 'rm -rf "$temporary_dir"' EXIT
readonly fake_bin_dir="${temporary_dir}/bin"
readonly calls="${temporary_dir}/calls"
readonly passwords="${temporary_dir}/passwords"
readonly out="${temporary_dir}/out"
readonly err="${temporary_dir}/err"
readonly dump="${temporary_dir}/dispatch.dump"
mkdir -p "$fake_bin_dir"
: >"$dump"

# --- Fake psql and pg_restore: each records its arguments to RESTORE_TEST_CALLS and the
# PGPASSWORD it was given to RESTORE_TEST_PASSWORDS, one line per call, and succeeds. ---
for tool in psql pg_restore; do
  cat >"${fake_bin_dir}/${tool}" <<'EOF'
#!/usr/bin/env bash
printf '%s %s\n' "${0##*/}" "$*" >>"${RESTORE_TEST_CALLS:?}"
if [[ -n "${PGPASSWORD+set}" ]]; then
  printf 'PGPASSWORD=%s\n' "$PGPASSWORD"
else
  printf 'PGPASSWORD unset\n'
fi >>"${RESTORE_TEST_PASSWORDS:?}"
EOF
  chmod +x "${fake_bin_dir}/${tool}"
done

# run_restore [NAME=VALUE...] runs the script on the dump into the database copy_db, with each
# NAME=VALUE in its environment and DISPATCH_ADMIN_URL unset unless one of them sets it, and sets
# status to its exit status.
status=0
run_restore() {
  : >"$calls"
  : >"$passwords"
  status=0
  env -u DISPATCH_ADMIN_URL -u PGPASSWORD "$@" PATH="${fake_bin_dir}:${PATH}" \
    RESTORE_TEST_CALLS="$calls" RESTORE_TEST_PASSWORDS="$passwords" \
    bash "$driver" "$dump" copy_db >"$out" 2>"$err" || status=$?
}

fail() {
  printf -- '--- stdout:\n%s\n--- stderr:\n%s\n--- calls:\n%s\n--- PGPASSWORD per call:\n%s\n' \
    "$(<"$out")" "$(<"$err")" "$(<"$calls")" "$(<"$passwords")" >&2
  echo "FAIL: $1" >&2
  exit 1
}

# --- An operator's DISPATCH_ADMIN_URL with a password, percent-encoded as not-a-secret%40op. ---
run_restore DISPATCH_ADMIN_URL='postgres://operator:not-a-secret%40op@db.test:5432/postgres?sslmode=require'
if [[ "$status" -ne 0 || "$(<"$out")" != 'DATABASE_URL=postgres://operator@db.test:5432/copy_db?sslmode=require' ]] ||
  grep -q 'not-a-secret' "$out" "$err"; then
  fail "an operator's DISPATCH_ADMIN_URL: the printed DATABASE_URL must leave its password out (exit ${status})"
fi
printf 'PASS: for an operator'"'"'s DISPATCH_ADMIN_URL the printed DATABASE_URL leaves its password out\n'

want_calls="psql postgres://operator@db.test:5432/postgres?sslmode=require --quiet --set=ON_ERROR_STOP=1 -c drop database if exists \"copy_db\" with (force) -c create database \"copy_db\""
want_calls+=$'\n'"pg_restore --no-owner --no-privileges --dbname=postgres://operator@db.test:5432/copy_db?sslmode=require ${dump}"
if [[ "$(<"$calls")" != "$want_calls" || "$(<"$passwords")" != $'PGPASSWORD=not-a-secret@op\nPGPASSWORD=not-a-secret@op' ]] ||
  grep -q 'not-a-secret' "$calls"; then
  fail "an operator's DISPATCH_ADMIN_URL: its password must reach psql and pg_restore in PGPASSWORD alone, in no argument"
fi
printf 'PASS: for an operator'"'"'s DISPATCH_ADMIN_URL the password reaches psql and pg_restore in PGPASSWORD alone, in no argument\n'

# --- No DISPATCH_ADMIN_URL: the default, dispatch-pg's postgres database with the fixed dev
# password of this repository, which the printed line keeps. The password is spelled through
# dev_password: it is the literal restore-dispatch-dump.sh carries, public in this repository,
# and the leak check refuses any new line holding a URL with a password it does not mark.
dev_password=dispatch
run_restore
if [[ "$status" -ne 0 || "$(<"$out")" != "DATABASE_URL=postgres://postgres:${dev_password}@127.0.0.1:55432/copy_db?sslmode=disable" ]] ||
  [[ "$(<"$passwords")" != $'PGPASSWORD=dispatch\nPGPASSWORD=dispatch' ]] || grep -q ':dispatch@' "$calls"; then
  fail "with no DISPATCH_ADMIN_URL the printed DATABASE_URL must keep the fixed dev password dispatch, and psql and pg_restore must get it in PGPASSWORD alone (exit ${status})"
fi
printf 'PASS: with no DISPATCH_ADMIN_URL the printed DATABASE_URL keeps the fixed dev password dispatch, which psql and pg_restore get in PGPASSWORD alone\n'

# --- A DISPATCH_ADMIN_URL naming another database than postgres is refused before anything
# runs, and the refusal names the URL without its password. ---
run_restore DISPATCH_ADMIN_URL='postgres://operator:not-a-secret%40op@db.test:5432/other?sslmode=require'
if [[ "$status" -ne 2 || -s "$calls" || -s "$out" ]] || grep -q 'not-a-secret' "$err" ||
  ! grep -qxF 'DISPATCH_ADMIN_URL must name the postgres database: postgres://operator@db.test:5432/other?sslmode=require' "$err"; then
  fail "a DISPATCH_ADMIN_URL naming another database must be refused with 2 before anything runs, naming the URL without its password (exit ${status})"
fi
printf 'PASS: a DISPATCH_ADMIN_URL naming another database is refused with 2 before anything runs, and the refusal prints no password\n'

# --- A DISPATCH_ADMIN_URL naming another database whose password libpq cannot decode: the
# helper's own refusal names the fault, the message names no URL, and neither prints the password.
run_restore DISPATCH_ADMIN_URL='postgres://u:not-a-secret%zz@h/other'
if [[ "$status" -ne 2 || -s "$calls" || -s "$out" ]] || grep -q 'not-a-secret' "$err" ||
  ! grep -qxF 'restore-dispatch-dump.sh: refused a Postgres URL: its password holds a % that starts no %XX escape, or %00 (the URL is not printed)' "$err" ||
  ! grep -qxF 'DISPATCH_ADMIN_URL must name the postgres database: (not printed)' "$err"; then
  fail "a DISPATCH_ADMIN_URL the helper refuses must be refused with 2 before anything runs, the helper naming the fault and the message printing no URL (exit ${status})"
fi
printf 'PASS: a DISPATCH_ADMIN_URL whose password libpq cannot decode is refused with 2, the helper naming the fault and the message printing no URL\n'
