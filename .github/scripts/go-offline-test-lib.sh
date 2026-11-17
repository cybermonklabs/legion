#!/usr/bin/env bash
# Sourced by the harnesses of the two checks whose go commands run through go_offline.py,
# check-ci-filters.test.sh and check-legion-version-inputs.test.sh: two stand-ins that let a case
# of each harness hold the check's go commands to the environment go_offline.py promises, so a
# change that breaks that environment fails both harnesses, not only a test of the module.
#
# go_spy <dir>: writes a stand-in `go` into <dir>/shim, for the caller to put first on PATH. It
# answers the toolchain lookup, `go env GOROOT`, with <dir>/root, and runs the real go for any
# other command. <dir>/root/bin/go, which the check then runs every go command with, appends one
# line per command to <dir>/log - its GOPROXY, GONOPROXY, GOENV and GOMODCACHE, and how many files
# that cache holds when the command starts - and runs the real toolchain's go. The real one is what
# `go env GOROOT` names where the harness runs.
#
# go_spy_verdict <dir>: `offline` when the log holds at least one command and every one ran with
# GOPROXY=off, GONOPROXY=none and GOENV=off over a module cache that is not this machine's and
# holds no file; else the first line that did not, or `no go command`.
#
# proxy_recorder <dir>: starts a stand-in HTTP proxy on 127.0.0.1 that answers every request, a
# CONNECT among them, with 502 and appends `<method> <target>` to <dir>/requests, and writes its
# URL to <dir>/url. A go command handed it as HTTPS_PROXY then records every fetch it tries
# instead of reaching the network. proxy_recorders_stop stops every recorder this shell started;
# each harness calls it from its EXIT trap.
#
# The network case both harnesses make, in three calls, so its procedure is kept once:
#   network_recorder <dir>  in the harness's own shell (the recorder must be this shell's to stop):
#                           starts a recorder in <dir> and sets GO_OFFLINE_INHERITED, the `env`
#                           command line a case runs its check under, the settings that send
#                           GOPROXY=off alone to the network: the recorder as every proxy
#                           variable, NO_PROXY unset, and GONOPROXY and GOPRIVATE matching
#                           GO_OFFLINE_VENDOR, a third-party module no cache of a case holds.
#   network_control <dir> <module dir> <package>
#                           the recorder's own control: `go list -e -deps <package>` in
#                           <module dir>, under those settings and GOPROXY=off alone over an empty
#                           cache of its own; prints true when the recorder saw it try
#                           example.org, else false, and then clears the recorder's log.
#   network_requests <dir>  how many requests the recorder has seen since the control.
# The case writes its fixture to require GO_OFFLINE_VENDOR (go_offline_vendor_sums writes the
# go.sum lines that make go look for it rather than stop at a missing sum), runs its check under
# "${GO_OFFLINE_INHERITED[@]}", and asserts its own verdict beside network_requests being 0.
#
# Not executable, and sourced after test-lib.sh, whose `contains` network_control reads with:
# `source "$(dirname "${BASH_SOURCE[0]}")/go-offline-test-lib.sh"`.

GO_OFFLINE_RECORDERS=()
GO_OFFLINE_INHERITED=()
GO_OFFLINE_VENDOR="example.org/vendorlib"

go_spy() {
  local dir=$1 real
  real="$(go env GOROOT)/bin/go"
  mkdir -p "$dir/shim" "$dir/root/bin"
  cat > "$dir/shim/go" <<SH
#!/usr/bin/env bash
if [ "\$*" = "env GOROOT" ]; then
  echo "$dir/root"
  exit 0
fi
exec "$real" "\$@"
SH
  cat > "$dir/root/bin/go" <<SH
#!/usr/bin/env bash
files=\$(find "\${GOMODCACHE:-/nonexistent}" -type f 2>/dev/null | wc -l)
printf '%s\t%s\t%s\t%s\t%s\n' "\${GOPROXY-unset}" "\${GONOPROXY-unset}" "\${GOENV-unset}" \\
  "\${GOMODCACHE-unset}" "\$files" >> "$dir/log"
exec "$real" "\$@"
SH
  chmod +x "$dir/shim/go" "$dir/root/bin/go"
}

go_spy_verdict() {
  local dir=$1 ambient proxy noproxy goenv cache files
  ambient=$(go env GOMODCACHE)
  if [ ! -s "$dir/log" ]; then
    echo "no go command"
    return
  fi
  while IFS=$'\t' read -r proxy noproxy goenv cache files; do
    if [ "$proxy" != off ] || [ "$noproxy" != none ] || [ "$goenv" != off ] || [ "$cache" = unset ] ||
      [ "$cache" = "$ambient" ] || [ "$files" != 0 ]; then
      echo "GOPROXY=$proxy GONOPROXY=$noproxy GOENV=$goenv GOMODCACHE=$cache holding $files files"
      return
    fi
  done < "$dir/log"
  echo offline
}

proxy_recorder() {
  local dir=$1
  mkdir -p "$dir"
  : > "$dir/requests"
  rm -f "$dir/port"
  python3 - "$dir/requests" "$dir/port" > /dev/null 2>&1 <<'PY' &
import http.server
import socketserver
import sys

requests = open(sys.argv[1], "a", buffering=1)


class Recorder(http.server.BaseHTTPRequestHandler):
    def record(self):
        requests.write(f"{self.command} {self.path}\n")
        self.send_error(502)

    do_CONNECT = do_GET = do_HEAD = do_POST = record

    def log_message(self, *args):
        pass


server = socketserver.TCPServer(("127.0.0.1", 0), Recorder)
open(sys.argv[2], "w").write(str(server.server_address[1]))
server.serve_forever()
PY
  GO_OFFLINE_RECORDERS+=("$!")
  local _
  for _ in $(seq 100); do
    [ -s "$dir/port" ] && break
    sleep 0.05
  done
  if [ ! -s "$dir/port" ]; then
    echo "  FAIL: the proxy recorder in $dir did not start" >&2
    return 1
  fi
  echo "http://127.0.0.1:$(<"$dir/port")" > "$dir/url"
}

proxy_recorders_stop() {
  local pid
  for pid in "${GO_OFFLINE_RECORDERS[@]}"; do
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  GO_OFFLINE_RECORDERS=()
}

go_offline_vendor_sums() {
  printf '%s v1.0.0 h1:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=\n%s v1.0.0/go.mod h1:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=\n' \
    "$GO_OFFLINE_VENDOR" "$GO_OFFLINE_VENDOR" > "$1"
}

network_recorder() {
  local dir=$1 url
  proxy_recorder "$dir"
  url=$(<"$dir/url")
  GO_OFFLINE_INHERITED=(env -u NO_PROXY -u no_proxy HTTPS_PROXY="$url" https_proxy="$url" HTTP_PROXY="$url"
    http_proxy="$url" GONOPROXY="${GO_OFFLINE_VENDOR%%/*}" GOPRIVATE="${GO_OFFLINE_VENDOR%%/*}")
}

network_control() {
  local dir=$1 module=$2 package=$3 seen
  rm -rf "$dir/cache"
  mkdir "$dir/cache"
  (cd "$module" &&
    "${GO_OFFLINE_INHERITED[@]}" GOPROXY=off GOFLAGS= GOMODCACHE="$dir/cache" go list -e -deps "$package" > /dev/null 2>&1) || true
  seen=$(contains "$(<"$dir/requests")" "^CONNECT ${GO_OFFLINE_VENDOR%%/*}:443\$")
  : > "$dir/requests"
  echo "$seen"
}

network_requests() {
  wc -l < "$1/requests"
}
