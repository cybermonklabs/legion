#!/usr/bin/env bash
# priorities-seed-check.test.sh - Drives scripts/priorities-seed-check.sh against a stand-in
# Dispatch: a python3 server of this test's own, bound to a port the kernel picks on the loopback
# address, holding the priorities board, the table's versions and the issues as files the cases
# rewrite between runs. It proves what the check answers for a board that holds the table, for a
# table row no priority holds, for a board whose priorities do not run in the table's order, for a
# priority whose record names an issue that does not advance it and one no issue carries, and that
# the bearer reaches Dispatch as a header without ever standing in a command's arguments.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly project_root
readonly script="${project_root}/scripts/priorities-seed-check.sh"
temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/priorities-seed-check-test.XXXXXX")"
readonly temporary_dir
readonly state="${temporary_dir}/state"
readonly token_file="${temporary_dir}/token"
server_pid=''

# The stand-in is stopped by the process id this test started, and nothing else, and the run's own
# directory is removed by its own path. `wait` answers 143 for a server SIGTERM ended, which is
# this test's own doing and no failure of it, so neither call decides the test's exit status.
cleanup() {
  if [[ -n $server_pid ]]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf -- "$temporary_dir"
}
trap cleanup EXIT

mkdir -p "$state/versions" "$state/issues"
printf 'a-token-of-this-test' >"$token_file"
chmod 600 "$token_file"

cat >"${temporary_dir}/dispatch.py" <<'EOF'
"""A stand-in Dispatch for priorities-seed-check.test.sh: the five routes the check reads, each
answered from a file of the state directory the cases rewrite, and every Authorization header it
was presented appended to <state>/authorization."""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

state, port_file = sys.argv[1], sys.argv[2]


def body(path):
    with open(os.path.join(state, path), "rb") as handle:
        return handle.read()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def answer(self, status, payload):
        encoded = json.dumps(payload).encode() if not isinstance(payload, bytes) else payload
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def do_GET(self):
        with open(os.path.join(state, "authorization"), "a") as handle:
            handle.write((self.headers.get("Authorization") or "<none>") + "\n")
        parts = [part for part in self.path.split("?")[0].split("/") if part != ""]
        if parts == ["api", "v1", "priorities"]:
            self.answer(200, body("priorities.json"))
        elif len(parts) == 8 and parts[:3] in (["api", "v1", "issues"], ["api", "v1", "projects"]) \
                and parts[4] == "artifacts" and parts[6] == "versions":
            name = os.path.join("versions", parts[7] + ".md")
            if not os.path.exists(os.path.join(state, name)):
                self.answer(404, {"code": "NOT_FOUND"})
                return
            self.answer(200, {"markdown": body(name).decode(), "number": int(parts[7])})
        elif len(parts) == 7 and parts[:3] in (["api", "v1", "issues"], ["api", "v1", "projects"]) \
                and parts[4] == "artifacts" and parts[6] == "text":
            self.answer(200, {"markdown": body("text.md").decode(), "token": "t", "version": None})
        elif len(parts) == 4 and parts[:3] == ["api", "v1", "issues"]:
            name = os.path.join("issues", parts[3] + ".json")
            if not os.path.exists(os.path.join(state, name)):
                self.answer(404, {"code": "NOT_FOUND"})
                return
            self.answer(200, body(name))
        else:
            self.answer(404, {"code": "NO_ROUTE", "message": self.path})


server = HTTPServer(("127.0.0.1", 0), Handler)
with open(port_file + ".writing", "w") as handle:
    handle.write(str(server.server_port))
os.rename(port_file + ".writing", port_file)
server.serve_forever()
EOF

python3 "${temporary_dir}/dispatch.py" "$state" "${temporary_dir}/port" &
server_pid=$!
for _ in $(seq 1 100); do
  [[ -s "${temporary_dir}/port" ]] && break
  sleep 0.1
done
[[ -s "${temporary_dir}/port" ]] || {
  echo "the stand-in Dispatch did not bind a port" >&2
  exit 1
}
port="$(cat "${temporary_dir}/port")"
readonly port

readonly document='dispatch://INFRA-10/artifact/priorities-table'

# The table every case reads back, at version 7: three rows, the second of them DONE.
cat >"${state}/versions/7.md" <<'EOF'
# What this deployment is working towards

Some prose over the table.

| \# | Priority | Owner | Where |
| --- | --- | --- | --- |
| 1 | Ship the board | ana | INFRA-21 in flight |
| 2 | Cut the backlog in half | bo | DONE in October |
| 3 | Keep the lights on | cy | INFRA-22, waiting |
EOF
cp "${state}/versions/7.md" "${state}/text.md"

# priority <id> <row> <advances>: one priority of the board, as the seed's record writes it.
priority() {
  local id=$1 row=$2 advances=$3
  printf '{"id":"%s","title":"row %s","description":"row %s\\n\\n---\\n\\nTable row %s of %s, read at version 7.\\nAdvances: %s.\\nNot advanced: none."}' \
    "$id" "$row" "$row" "$row" "$document" "$advances"
}

# board <priority json>…: the board GET /api/v1/priorities answers, in the order given.
board() {
  local first=1 row
  {
    printf '{"priorities":['
    for row in "$@"; do
      ((first)) || printf ','
      first=0
      printf '%s' "$row"
    done
    printf '],"work_under_no_priority":{"count":0},"priority_work_parked":{"count":0}}'
  } >"${state}/priorities.json"
}

# issue <KEY> <priority id>…: an issue whose own advances names each priority given.
issue() {
  local key=$1 first=1 id
  shift
  {
    printf '{"key":"%s","status":"in_progress","advances":[' "$key"
    for id in "$@"; do
      ((first)) || printf ','
      first=0
      printf '{"id":"%s","inherited":false}' "$id"
    done
    printf '],"advances_revision":4}'
  } >"${state}/issues/${key}.json"
}

status=0
output=''
# run <argument>…: the check, against the stand-in, with its output and status kept.
run() {
  set +e
  output=$(DISPATCH_URL="http://127.0.0.1:${port}" DISPATCH_TOKEN_FILE="$token_file" \
    bash "$script" "$@" 2>&1)
  status=$?
  set -e
}

failures=0
case_name=''
# fail <text>: one expectation the case did not meet, with what the check printed.
fail() {
  echo "FAIL ${case_name}: $*" >&2
  echo "--- the check printed (exit ${status}):" >&2
  echo "$output" >&2
  echo "---" >&2
  failures=$((failures + 1))
}

expect_status() {
  [[ $status == "$1" ]] || fail "expected exit $1, got $status"
}

expect_output() {
  [[ $output == *"$1"* ]] || fail "expected the output to name \"$1\""
}

refute_output() {
  [[ $output != *"$1"* ]] && return 0
  fail "expected the output not to name \"$1\""
}

case_name='--help prints the usage block and exits 0'
run --help
expect_status 0
expect_output 'usage: scripts/priorities-seed-check.sh --artifact <document reference>'
expect_output 'DISPATCH_TOKEN_FILE'
expect_output 'exit: 0 the board holds the table'

case_name='no --artifact is a usage error'
run
expect_status 2
expect_output 'no --artifact: name the priorities table'
expect_output 'usage: scripts/priorities-seed-check.sh'

case_name='a reference of no grammar this check reads is a usage error'
run --artifact 'https://example.org/documents/7'
expect_status 2
expect_output 'is no document reference'

case_name='an argument the check does not take is a usage error'
run --artifact "$document" --loudly
expect_status 2
expect_output '--loudly is no argument of this check'

case_name='a board that holds the table passes'
board "$(priority p1 1 'INFRA-21')" "$(priority p3 3 'INFRA-22')"
issue INFRA-21 p1
issue INFRA-22 p3
run --artifact "$document"
expect_status 0
expect_output '2 rows stand on the board in the table'
expect_output '1 rows passed over (DONE)'
expect_output '2 relations read back'

case_name='the version pinned on the argument is passed over for the one the board records'
run --artifact "${document}@v3"
expect_status 0
expect_output 'read at version 7'

case_name='a row no priority holds fails, naming that row'
board "$(priority p1 1 'INFRA-21')"
run --artifact "$document"
expect_status 1
expect_output 'row 3 of '"$document"' (Keep the lights on) stands on no priority of the board'
refute_output 'row 1 of '"$document"' ('
refute_output 'row 2 of '"$document"' ('

case_name='every row the board does not hold is named, not only the first'
board "$(priority p2 2 'none')"
run --artifact "$document"
expect_status 1
expect_output 'row 1 of '"$document"' (Ship the board) stands on no priority of the board'
expect_output 'row 3 of '"$document"' (Keep the lights on) stands on no priority of the board'
expect_output '2 faults'

case_name='priorities that do not run in the table order fail'
board "$(priority p3 3 'INFRA-22')" "$(priority p1 1 'INFRA-21')"
run --artifact "$document"
expect_status 1
expect_output 'the board draws row 3 of '"$document"' where the table writes row 1'

case_name='a priority whose record names an issue that does not advance it fails, naming both'
board "$(priority p1 1 'INFRA-21')" "$(priority p3 3 'INFRA-22')"
issue INFRA-22 p9
run --artifact "$document"
expect_status 1
expect_output "priority p3 (row 3 of ${document}) records that INFRA-22 advances it, and INFRA-22's own advances does not name it"
refute_output 'INFRA-21 advances it, and'

case_name='a relation an issue only inherits is no relation of its own'
printf '{"key":"INFRA-22","status":"todo","advances":[{"id":"p3","inherited":true}],"advances_revision":0}' \
  >"${state}/issues/INFRA-22.json"
run --artifact "$document"
expect_status 1
expect_output "INFRA-22's own advances does not name it"

case_name='a key no issue carries fails, naming the priority and the key'
rm -f -- "${state}/issues/INFRA-22.json"
run --artifact "$document"
expect_status 1
expect_output "priority p3 (row 3 of ${document}) records that INFRA-22 advances it, and no issue carries that key"

case_name='a board holding no row of this document is not a board to read back'
issue INFRA-22 p3
board '{"id":"p7","title":"written by hand","description":"no record of any table."}'
run --artifact "$document"
expect_status 1
expect_output 'no priority of the board records a row of '"$document"

case_name='a priority of another table is passed over'
board "$(priority p1 1 'INFRA-21')" "$(priority p3 3 'INFRA-22')" \
  '{"id":"pX","title":"another table","description":"x\n\n---\n\nTable row 1 of dispatch://INFRA-99/artifact/other-table, read at version 2.\nAdvances: INFRA-98.\nNot advanced: none."}'
run --artifact "$document"
expect_status 0
expect_output '2 rows stand on the board'
refute_output 'INFRA-98'

case_name='every read presents the bearer as a header, and no run puts it in an argument'
[[ -s "${state}/authorization" ]] || fail 'the stand-in recorded no Authorization header'
while read -r line; do
  [[ $line == "Bearer a-token-of-this-test" ]] || fail "a read presented ${line}"
done <"${state}/authorization"
if grep -rq 'a-token-of-this-test' "$script"; then
  fail 'the script holds the test token'
fi

if ((failures > 0)); then
  echo "priorities-seed-check.test.sh: ${failures} failures" >&2
  exit 1
fi
echo 'priorities-seed-check.test.sh: every case passed'
