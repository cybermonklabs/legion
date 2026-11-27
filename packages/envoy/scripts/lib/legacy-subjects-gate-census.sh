# shellcheck shell=sh
# packages/envoy/scripts/lib/legacy-subjects-gate-census.sh
#
# The census of connections of legacy-subjects-gate.sh's second reading, run by a one-off Job of
# its own that the gate creates in the cluster: the gate hands this file to the Job's container as
# its `sh -c` script, so it runs in the nats-box image (busybox sh, the nats CLI and jq). The Job
# runs as the ServiceAccount of the legacy-cutover user and mounts that user's nkey seed, nothing
# of the listener's, so the census is asked as that user alone.
#
# It requests $SYS.REQ.SERVER.PING.CONNZ at NATS_URL as the nkey user of CENSUS_NKEY, with
# CENSUS_INBOX_PREFIX as its inbox prefix and CENSUS_NAME, which the gate drew for this reading and
# leaves out of the census, as its connection name. Every server of the hub answers it, each for
# the account the asking connection sits in, the clients' own for the legacy-cutover user (measured
# on a three-server nats-server 2.15.1 cluster). Every reply is read until none comes for 2 s
# (`--replies=0`), whatever number of servers answers; a next page is asked while a server holds
# more connections than it has answered, at most 16 pages, each from the last connection of the
# page before (offset + limit - 1). A server lists its open connections by id, so a connection
# that closes below a page already read shifts every later one down by one; the page that then
# does not begin with the connection the page before ended on shows it (the judge reads it), where
# a page asked at offset + limit would pass the shifted connection over unseen.
#
# Each page is printed as one line of five tab-separated fields, as lib/legacy-subjects-gate-nats.sh
# prints its reads: `census`, the page's offset, the nats CLI's exit status, and its standard output
# and standard error, each in base64 on one line. Of each answer only the server's name and id, its
# counts and, of each connection, its id, name, account, client library and NATS user reach the log;
# an answer that is no JSON object is logged as "no JSON", never as it came. The last line is `end`,
# so a log cut short is told from one that is whole.
set -u

work=$(mktemp -d "${WORK_DIR:-/tmp}/census.XXXXXX")
trap 'rm -rf "$work"' EXIT

# emit LEG ARG STATUS prints one read's line from the output and error the read left in $work.
emit() {
  printf '%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$(base64 -w 0 <"$work/out")" "$(base64 -w 0 <"$work/err")"
}

offset=0
limit=1024
pages=0
while [ "$pages" -lt 16 ]; do
  pages=$((pages + 1))
  nats req --raw --replies=0 --timeout=10s --reply-timeout=2s \
    --nkey "${CENSUS_NKEY:-}" --connection-name "${CENSUS_NAME:-}" --inbox-prefix "${CENSUS_INBOX_PREFIX:-}" \
    "\$SYS.REQ.SERVER.PING.CONNZ" "{\"offset\":$offset,\"limit\":$limit,\"auth\":true}" >"$work/census" 2>"$work/err"
  status=$?
  jq -c -R 'select(length > 0) | (fromjson? // "no JSON") | if type == "object"
    then {server: {name: .server.name, id: .server.id}, error,
          data: {total: .data.total, offset: .data.offset, limit: .data.limit,
            connections: [.data.connections[]? | {cid, name, account, lang, version, authorized_user}]}}
    else "no JSON" end' "$work/census" >"$work/out" 2>/dev/null || : >"$work/out"
  emit census "$offset" "$status"
  more=$(jq -s '[.[] | objects | select((.data.offset // 0) + (.data.connections | length) < (.data.total // 0))] | length' "$work/out" 2>/dev/null)
  [ "${more:-0}" -gt 0 ] || break
  offset=$((offset + limit - 1))
done

printf 'end\t-\t0\t\t\n'
