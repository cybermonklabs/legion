#!/usr/bin/env bash
# Tests for `dispatch-browser-fetch-binaries.sh`, the second read of the browser suite's binaries
# artifact. Each case starts a stand-in GitHub on 127.0.0.1: the run's artifact list, which holds
# the other caller's binaries beside this caller's, as a run of a pull request labelled full-ci
# does, and answers a `name=` query with the artifacts of that name, as the REST API does; the
# artifact's API address, which redirects to a blob address on another host name (localhost);
# and the blob, which serves a fixture zip and honours a byte range. A case's mode makes the blob
# stall partway through its first answer, serve bytes whose digest is not the artifact's, the list
# hold only the other caller's artifact, hold this caller's three times as a re-run does (out of
# id order, the lower ids serving an older build), or hold it expired. The script runs against it
# as the step runs it, for the caller dispatch-browser-pr, with the low-speed bound shortened to
# the stand-in's scale.
#
# The stall case has a control: the same stall with a low-speed bound longer than the case's
# limit never completes, which shows the stand-in really stalls and the bound is what ends it.
#
# Run from anywhere: .github/scripts/dispatch-browser-fetch-binaries.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"
fetch="$script_dir/dispatch-browser-fetch-binaries.sh"
work=$(mktemp -d)
server=""
trap '[ -z "$server" ] || kill "$server" 2>/dev/null || true; rm -rf "$work"' EXIT

# The fixture: four files of random bytes, zipped as upload-artifact zips them (deflate), and an
# older build's zip of the same four names with other bytes, which a re-run case's lower-id
# artifacts serve.
python3 - "$work" <<'PY'
import os, sys, zipfile
work = sys.argv[1]
os.makedirs(f"{work}/files")
os.makedirs(f"{work}/old-files")
for name, size in (("broker", 400_000), ("dispatch", 300_000), ("dev-oidc-issuer", 90_000), ("requestseed", 80_000)):
    for directory in ("files", "old-files"):
        with open(f"{work}/{directory}/{name}", "wb") as f:
            f.write(os.urandom(size))
for directory, zip_name in (("files", "fixture.zip"), ("old-files", "old.zip")):
    with zipfile.ZipFile(f"{work}/{zip_name}", "w", zipfile.ZIP_DEFLATED) as z:
        for name in sorted(os.listdir(f"{work}/{directory}")):
            z.write(f"{work}/{directory}/{name}", name)
PY

cat > "$work/server.py" <<'PY'
# The stand-in GitHub: python3 server.py <mode> <zip> <port-file> <log>.
import hashlib, http.server, json, sys, time, urllib.parse
mode, zip_path, port_file, log_path = sys.argv[1:5]
blob = open(zip_path, "rb").read()
digest = "sha256:" + hashlib.sha256(blob).hexdigest()
old = open(zip_path.replace("fixture.zip", "old.zip"), "rb").read()
old_digest = "sha256:" + hashlib.sha256(old).hexdigest()
served = blob if mode != "corrupt" else blob[:100] + bytes([blob[100] ^ 0xFF]) + blob[101:]
STALL_AT = 65_536
blob_reads = 0

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def note(self, what):
        with open(log_path, "a") as log:
            auth = "yes" if self.headers.get("Authorization") == "Bearer test-token" else (
                "other" if self.headers.get("Authorization") else "no")
            log.write(f"{what} auth={auth} range={self.headers.get('Range', 'none')}\n")

    def do_GET(self):
        global blob_reads
        port = self.server.server_address[1]
        url = urllib.parse.urlsplit(self.path)
        if url.path == "/repos/o/r/actions/runs/7/artifacts":
            self.note("list")
            if self.headers.get("Authorization") != "Bearer test-token":
                return self.send_error(401)
            # The other caller's binaries, whose address serves nothing: a read of them fails.
            held = [{"id": 900, "name": "dispatch-browser-binaries", "expired": False, "size_in_bytes": 4,
                     "created_at": "2026-11-24T02:00:00Z",
                     "digest": "sha256:" + hashlib.sha256(b"none").hexdigest(),
                     "archive_download_url": f"http://127.0.0.1:{port}/other-zip"}]
            if mode == "rerun":
                # A re-run's attempts, each holding this caller's binaries under the same name,
                # listed out of id order. The highest id is the one actions/download-artifact
                # takes, though its created_at is not the newest, as in run 45306887784; the two
                # others serve an older build whose bytes are not the fixture's.
                for id_, at, path in ((1001, "2026-11-24T01:47:58Z", "/old-zip"),
                                      (1003, "2026-11-24T02:03:33Z", "/zip"),
                                      (1002, "2026-11-24T02:14:39Z", "/old-zip")):
                    held.append({"id": id_, "name": "dispatch-browser-pr-binaries", "expired": False,
                                 "created_at": at,
                                 "size_in_bytes": len(blob) if path == "/zip" else len(old),
                                 "digest": digest if path == "/zip" else old_digest,
                                 "archive_download_url": f"http://127.0.0.1:{port}{path}"})
            elif mode != "missing":
                held.append({"id": 1001, "name": "dispatch-browser-pr-binaries", "expired": mode == "expired",
                             "created_at": "2026-11-24T01:47:58Z", "size_in_bytes": len(blob),
                             "digest": digest, "archive_download_url": f"http://127.0.0.1:{port}/zip"})
            asked = urllib.parse.parse_qs(url.query).get("name")
            artifacts = [a for a in held if asked is None or a["name"] in asked]
            body = json.dumps({"total_count": len(artifacts), "artifacts": artifacts}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            return self.wfile.write(body)
        if self.path in ("/zip", "/old-zip"):
            self.note(self.path[1:])
            if self.headers.get("Authorization") != "Bearer test-token":
                return self.send_error(401)
            self.send_response(302)
            self.send_header("Location", f"http://localhost:{port}/{'blob' if self.path == '/zip' else 'old-blob'}")
            self.send_header("Content-Length", "0")
            return self.end_headers()
        if self.path == "/old-blob":
            self.note("old-blob")
            self.send_response(200)
            self.send_header("Content-Type", "application/zip")
            self.send_header("Content-Length", str(len(old)))
            self.end_headers()
            return self.wfile.write(old)
        if self.path == "/blob":
            self.note("blob")
            blob_reads += 1
            start = 0
            ranged = self.headers.get("Range", "")
            if ranged.startswith("bytes="):
                start = int(ranged[len("bytes="):].split("-")[0])
            body = served[start:]
            self.send_response(206 if start else 200)
            self.send_header("Content-Type", "application/zip")
            self.send_header("Content-Length", str(len(body)))
            if start:
                self.send_header("Content-Range", f"bytes {start}-{len(served) - 1}/{len(served)}")
            self.end_headers()
            if mode == "stall" and blob_reads == 1:
                self.wfile.write(body[:STALL_AT])
                self.wfile.flush()
                time.sleep(30)  # the stall: the connection stays open and sends nothing
                return
            return self.wfile.write(body)
        self.send_error(404)

httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
httpd.daemon_threads = True
with open(port_file + ".tmp", "w") as f:
    f.write(str(httpd.server_address[1]))
import os
os.rename(port_file + ".tmp", port_file)
httpd.serve_forever()
PY

# start <mode>: starts the stand-in for one case and sets $port, $server, $log and $out.
start() {
  local case_dir
  case_dir=$(mktemp -d "$work/case.XXXXXX")
  log="$case_dir/requests.log"
  out="$case_dir/out"
  : > "$log"
  python3 "$work/server.py" "$1" "$work/fixture.zip" "$case_dir/port" "$log" &
  server=$!
  for _ in $(seq 1 100); do
    [ -f "$case_dir/port" ] && break
    sleep 0.05
  done
  port=$(cat "$case_dir/port")
}

stop() {
  kill "$server" 2>/dev/null || true
  wait "$server" 2>/dev/null || true
  server=""
}

# fetch_as_step [timeout] [env...]: runs the script as the step of the caller dispatch-browser-pr
# does, against this case's stand-in; prints its output and sets $status.
fetch_as_step() {
  local limit=$1
  shift
  status=0
  output=$(env GITHUB_API_URL="http://127.0.0.1:$port" GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=7 \
    GH_TOKEN=test-token DISPATCH_BROWSER_FETCH_SPEED_LIMIT=1000 DISPATCH_BROWSER_FETCH_SPEED_TIME=1 \
    "$@" timeout "$limit" "$fetch" "$out" dispatch-browser-pr 2>&1) || status=$?
}

same_files() {
  local name
  for name in broker dispatch dev-oidc-issuer requestseed; do
    cmp -s "$work/files/$name" "$out/$name" || { echo false; return; }
  done
  echo true
}

echo "A read that completes: one attempt, this caller's artifact, the token kept to the API's host"
start clean
fetch_as_step 30
check "exits 0 ($status)" "$(is "$status" 0)"
check "unzips its own caller's artifact's four files, byte for byte" "$(same_files)"
check "reads in one attempt" "$(is "$(grep -c '^attempt ' <<<"$output")" 1)"
check "sends the token to the artifact list and to the artifact's API address" \
  "$(is "$(grep -c '^\(list\|zip\) auth=yes' "$log")" 2)"
check "sends no token to the blob's host" "$(contains "$(grep '^blob' "$log")" '^blob auth=no range=none$')"
stop

echo "A read that stalls partway: the low-speed bound ends it, and the next attempt resumes"
start stall
fetch_as_step 30
check "exits 0 ($status)" "$(is "$status" 0)"
check "unzips the artifact's four files, byte for byte" "$(same_files)"
check "reads in two attempts" "$(is "$(grep -c '^attempt ' <<<"$output")" 2)"
check "the first attempt says where it stopped" "$(contains "$output" '^attempt 1: curl exit 28, HTTP 200, 65536 bytes')"
check "the second attempt asks for the bytes after the stall" \
  "$(is "$(grep '^blob' "$log" | sed -n 2p)" 'blob auth=no range=bytes=65536-')"
check "each attempt names the TCP segments retransmitted during it" "$(contains "$output" 'TCP segments retransmitted')"
stop

echo "Control: the same stall with a low-speed bound past the case's limit never completes"
start stall
fetch_as_step 4 DISPATCH_BROWSER_FETCH_SPEED_TIME=60
check "is ended by the case's limit (status 124, was $status)" "$(is "$status" 124)"
check "reads the blob once" "$(is "$(grep -c '^blob' "$log")" 1)"
stop

echo "A blob whose bytes are not the artifact's digest: refused, and read again from byte 0"
start corrupt
fetch_as_step 30 DISPATCH_BROWSER_FETCH_ATTEMPTS=2
check "exits 1 ($status)" "$(is "$status" 1)"
check "says the sha256 is not the artifact's" "$(contains "$output" 'their sha256 is not the artifact')"
check "says the artifact was not read whole, and that no row failed" \
  "$(contains "$output" 'was not read whole in 2 attempts.*not a failed browser row')"
check "starts the second attempt from byte 0" "$(is "$(grep '^blob' "$log" | sed -n 2p)" 'blob auth=no range=none')"
check "unzips nothing" "$(is "$(find "$out" -type f | wc -l)" 0)"
stop

echo "A run that holds only the other caller's binaries: refused, naming this caller's"
start missing
fetch_as_step 30
check "exits 1 ($status)" "$(is "$status" 1)"
check "names the artifact it found none of" "$(contains "$output" 'holds no artifact named dispatch-browser-pr-binaries')"
stop

# A re-run uploads the binaries again under the same name. actions/download-artifact, given a
# name and no run id, takes the highest id of the run's artifacts of that name, so the second
# read must take that one too, and its address, size and digest from it alone.
echo "A re-run that holds this caller's binaries three times: the highest id is read, as the action reads it"
start rerun
fetch_as_step 30
check "exits 0 ($status)" "$(is "$status" 0)"
check "unzips the highest id's four files, byte for byte" "$(same_files)"
check "says which of the three it read" \
  "$(contains "$output" '^artifact dispatch-browser-pr-binaries, id 1003, the highest of the 3 the run holds under that name')"
check "reads the highest id's address alone, never an older attempt's" \
  "$(is "$(grep -c '^zip auth=yes' "$log"):$(grep -c '^old-zip' "$log")" '1:0')"
check "reads in one attempt, held to that artifact's own size and digest" "$(is "$(grep -c '^attempt ' <<<"$output")" 1)"
stop

echo "A run whose only artifact of the name has expired: refused, naming it"
start expired
fetch_as_step 30
check "exits 1 ($status)" "$(is "$status" 1)"
check "names the expired artifact by its id" "$(contains "$output" 'dispatch-browser-pr-binaries in run 7, id 1001, has expired')"
check "reads no blob" "$(is "$(grep -c '^\(zip\|blob\)' "$log")" 0)"
stop

echo "Called without the calling job's id: refused"
status=0
output=$("$fetch" "$work/unused" 2>&1) || status=$?
check "exits 1 ($status)" "$(is "$status" 1)"
check "says what it takes" "$(contains "$output" "takes the directory to unzip into and the calling job's id")"

summary "dispatch-browser-fetch-binaries.sh"
