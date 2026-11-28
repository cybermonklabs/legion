#!/usr/bin/env bash
# packages/envoy/scripts/restore-dispatch-dump.sh
#
# Restores a Dispatch dump (pg_dump -Fc) into a database of its own on the server
# DISPATCH_ADMIN_URL names (default: dispatch-pg's postgres database), dropping and creating that
# database first, and prints its DATABASE_URL. psql and pg_restore are given each URL without its
# password, and the password in PGPASSWORD (lib/pg-url.sh).
# The printed DATABASE_URL leaves out any password DISPATCH_ADMIN_URL carries, which its operator
# already holds; the default URL's password is the fixed dev password `dispatch`, and stays.
set -euo pipefail

if (($# < 1 || $# > 2)); then
  printf 'usage: %s <dump-file> [database-name]\n' "$0" >&2
  exit 2
fi

for command in psql pg_restore; do
  command -v "$command" >/dev/null || {
    printf '%s is required\n' "$command" >&2
    exit 1
  }
done
# shellcheck source-path=SCRIPTDIR source=lib/pg-url.sh
. "$(dirname "$0")/lib/pg-url.sh"

readonly dump_file="$1"
if [[ ! -f "$dump_file" ]]; then
  printf 'dump file not found: %s\n' "$dump_file" >&2
  exit 1
fi

if (($# == 2)); then
  database_name="$2"
else
  basename="$(basename "$dump_file")"
  database_name="dispatch_copy_${basename//[^a-z0-9]/_}"
fi

if [[ ! "$database_name" =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ]] || ((${#database_name} > 63)); then
  printf 'database name must be a PostgreSQL identifier no longer than 63 characters: %s\n' "$database_name" >&2
  exit 2
fi
readonly database_name

readonly admin_url="${DISPATCH_ADMIN_URL:-postgres://postgres:dispatch@127.0.0.1:55432/postgres?sslmode=disable}"
case "$admin_url" in
  */postgres\?*) database_url="${admin_url%/postgres\?*}/${database_name}?${admin_url#*\?}" ;;
  */postgres) database_url="${admin_url%/postgres}/${database_name}" ;;
  *)
    # A URL the helper refuses is named by its refusal on stderr, which prints nothing of the URL.
    shown_url="$(pg_url_without_password "$admin_url")" || shown_url="(not printed)"
    printf 'DISPATCH_ADMIN_URL must name the postgres database: %s\n' "$shown_url" >&2
    exit 2
    ;;
esac
readonly database_url

# restore_into URL restores the dump into the database URL names. pg_url_run hands the URL to its
# command as the first argument, and pg_restore takes it only as --dbname=, so this function takes
# the URL where pg_url_run puts it, and the PGPASSWORD pg_url_run sets reaches pg_restore through it.
restore_into() { pg_restore --no-owner --no-privileges --dbname="$1" "$dump_file"; }

pg_url_run "$admin_url" psql --quiet --set=ON_ERROR_STOP=1 \
  -c "drop database if exists \"${database_name}\" with (force)" \
  -c "create database \"${database_name}\""
pg_url_run "$database_url" restore_into
if [[ -n "${DISPATCH_ADMIN_URL:-}" ]]; then
  printed_url="$(pg_url_without_password "$database_url")"
else
  printed_url="$database_url"
fi
printf 'DATABASE_URL=%s\n' "$printed_url"
