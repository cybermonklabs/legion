#!/usr/bin/env bash
# Captures the first request a real Oh My Pi sends its model, for the three cases of
# requests/ (README.md says what each case is and what it showed). A python3 stand-in is the model:
# it records each POST body and answers every one with one text turn. Oh My Pi runs as
#
#   omp --mode rpc --extension <root> --extension <pi-envoy pack> --extension <pi-legion pack> \
#       --append-system-prompt <file> --no-lsp --no-title
#
# with DISPATCH_URL set so pi-envoy adds the dispatch-first skill to the request, under a fresh HOME
# and from a fresh working directory, so no user or project skill of this machine enters a fixture.
# A root is an --extension directory holding agents/ and skills/ and nothing else.
#
#   OMP=<the Oh My Pi .omp-pin names> bash capture.sh
#
# OMP defaults to LEGION_TEST_OMP, then LEGION_OMP_PATH. The pin is github:<owner>/<repo>@<version>
# in the repository's .omp-pin; .github/actions/install-omp/action.yml installs it (the release
# archive, held to ARG OMP_SHA256 of packages/daemon/docker/worker.Dockerfile). The script prints the
# version and the sha256 of the binary it ran, which README.md records.
#
# The plugins are packed as the release packs them (scripts/e2e/lib/pack-plugin.sh: the manifest's
# omp.extensions rewritten to the built bundle, `bun pm pack`, the committed package.json put back
# byte for byte) from this checkout, which needs `bun install --frozen-lockfile` at the repository
# root. Scratch is one mktemp -d under $TMPDIR, removed at the end by its exact path.
#
# Each fixture is the first request with a system prompt and tools, formatted by the repository's
# Biome, with the fresh working directory's path replaced by /work/cwd. With a settings layer named in PI_CONFIG_FILES
# Oh My Pi first sends one skill-description compression request for each skill (a request with no
# system prompt); those are skipped.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
repo=$(cd "$here/../../../../../.." && pwd -P)
omp=${OMP:-${LEGION_TEST_OMP:-${LEGION_OMP_PATH:-}}}
[ -n "$omp" ] && [ -x "$omp" ] || { echo "capture.sh: set OMP to the Oh My Pi binary .omp-pin names" >&2; exit 2; }
for tool in python3 jq bun tar; do
  command -v "$tool" >/dev/null || { echo "capture.sh: $tool is not on PATH" >&2; exit 2; }
done
[ -d "$repo/node_modules" ] || (cd "$repo" && bun install --frozen-lockfile >&2)

echo "pin: $(cat "$repo/.omp-pin")"
echo "omp: $("$omp" --version) sha256 $(sha256sum "$omp" | cut -d' ' -f1)"

work=$(mktemp -d "${TMPDIR:-/tmp}/omprequest-capture.XXXXXX")
standin_pid=
cleanup() {
  [ -z "$standin_pid" ] || kill "$standin_pid" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

# The two plugins, packed and unpacked into directories named as the worker image names them.
mkdir -p "$work/plugins"
for plugin in pi-envoy pi-legion; do
  tarball=$(bash "$repo/scripts/e2e/lib/pack-plugin.sh" "$plugin" "$work/tgz-$plugin" 2>/dev/null)
  mkdir "$work/plugins/$plugin"
  tar -xzf "$tarball" -C "$work/plugins/$plugin" --strip-components=1
done
envoy=$work/plugins/pi-envoy
legion=$work/plugins/pi-legion

# Root 1: a changed legion-worker/SKILL.md and a changed goal-red-team agent, each marked at the
# front of its description, which is what a request shows of either. Root 3: the one skill with
# only references/plan-tests.md changed, its SKILL.md the plugin's own bytes.
marker="Changed by the arm."
mkdir -p "$work/root-1/agents" "$work/root-1/skills" "$work/root-3/skills"
cp -r "$legion/dist/skills/legion-worker" "$work/root-1/skills/legion-worker"
cp -r "$legion/dist/skills/legion-worker" "$work/root-3/skills/legion-worker"
python3 - "$legion" "$work" "$marker" <<'PY'
import sys
legion, work, marker = sys.argv[1:]

def changed(source, target, old, new):
    text = open(source, encoding="utf-8").read()
    if text.count(old) != 1:
        sys.exit("capture.sh: %s holds %r %d times, want once" % (source, old, text.count(old)))
    open(target, "w", encoding="utf-8").write(text.replace(old, new))

changed(legion + "/dist/skills/legion-worker/SKILL.md", work + "/root-1/skills/legion-worker/SKILL.md",
        "\ndescription: Use when", "\ndescription: " + marker + " Use when")
changed(legion + "/agents/goal-red-team.md", work + "/root-1/agents/goal-red-team.md",
        "\n  Goal red team.", "\n  " + marker + " Goal red team.")
plan_tests = work + "/root-3/skills/legion-worker/references/plan-tests.md"
open(plan_tests, "a", encoding="utf-8").write("\n" + marker + "\n")
PY

# The hiding layer of decision 9: the replaced skill's collision aliases, never its plain name.
cat >"$work/hide.yml" <<'YML'
skills:
  ignoredSkills:
    - "*/legion-worker"
    - "*/legion-worker~*"
YML

# The model stand-in: records each POST body to out/request-<n>.json, writes out/conversation.json
# for the first with a system prompt and tools, and answers every Messages request with one text
# turn. Every other route answers an empty JSON object, a GET an empty list.
cat >"$work/standin.py" <<'PY'
import http.server, json, os, sys

out = sys.argv[1]
count = 0

class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def reply(self, content_type, data):
        self.send_response(200)
        self.send_header("content-type", content_type)
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.reply("application/json", b'{"data": []}')

    def do_POST(self):
        global count
        raw = self.rfile.read(int(self.headers.get("content-length") or 0))
        if self.path != "/anthropic/v1/messages":
            self.reply("application/json", b"{}")
            return
        count += 1
        with open(os.path.join(out, "request-%d.json" % count), "wb") as file:
            file.write(raw)
        body = json.loads(raw)
        if "system" in body and "tools" in body and not os.path.exists(os.path.join(out, "conversation.json")):
            with open(os.path.join(out, "conversation.json"), "wb") as file:
                file.write(raw)
        events = []

        def event(name, data):
            events.append("event: %s\ndata: %s\n\n" % (name, json.dumps(data)))

        event("message_start", {"type": "message_start", "message": {
            "id": "msg_%d" % count, "type": "message", "role": "assistant", "model": "standin-model",
            "content": [], "stop_reason": None, "stop_sequence": None,
            "usage": {"input_tokens": 5, "output_tokens": 1}}})
        event("content_block_start", {"type": "content_block_start", "index": 0,
                                      "content_block": {"type": "text", "text": ""}})
        event("content_block_delta", {"type": "content_block_delta", "index": 0,
                                      "delta": {"type": "text_delta", "text": "Answer."}})
        event("content_block_stop", {"type": "content_block_stop", "index": 0})
        event("message_delta", {"type": "message_delta",
                                "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                                "usage": {"output_tokens": 1}})
        event("message_stop", {"type": "message_stop"})
        self.reply("text/event-stream", "".join(events).encode())

server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
with open(os.path.join(out, "port"), "w") as file:
    file.write(str(server.server_address[1]))
server.serve_forever()
PY

# Oh My Pi extracts its native module (about 123 MiB) into a HOME that lacks it: once here, and
# every case's HOME then links that copy.
mkdir -p "$work/natives-home/work"
(cd "$work/natives-home/work" && env -i HOME="$work/natives-home" PATH=/usr/local/bin:/usr/bin:/bin \
  "$omp" config get compaction.remoteEndpoint --json >/dev/null)

capture() { # <name> <root> <settings layer or "">
  local name=$1 root=$2 layer=$3
  local case=$work/case-$name
  mkdir -p "$case/home/.omp/agent" "$case/cwd" "$case/out"
  cp -al "$work/natives-home/.omp/natives" "$case/home/.omp/natives" 2>/dev/null ||
    cp -a "$work/natives-home/.omp/natives" "$case/home/.omp/natives"
  python3 "$work/standin.py" "$case/out" &
  standin_pid=$!
  for _ in $(seq 1 100); do [ -s "$case/out/port" ] && break || sleep 0.1; done
  local port
  port=$(cat "$case/out/port")
  cat >"$case/home/.omp/agent/models.yml" <<YML
providers:
  standin:
    baseUrl: http://127.0.0.1:$port/anthropic
    auth: apiKey
    api: anthropic-messages
    apiKey: standin-key
    models:
      - id: standin-model
        name: Stand-in model
        reasoning: false
        input: [text]
        contextWindow: 200000
        maxTokens: 8000
        cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0}
YML
  {
    echo "enabledModels:"
    echo "  - standin/*"
    echo "disabledProviders: [amazon-bedrock, bedrock-mantle, google, google-vertex, ollama, llama.cpp, lm-studio]"
    echo "modelRoles:"
    for role in default smol slow plan task commit tiny vision advisor; do echo "  $role: standin/standin-model"; done
  } >"$case/home/.omp/agent/config.yml"
  echo "The case's appended system prompt." >"$case/append.md"

  echo "standin" >"$case/dispatch-bearer"
  local -a environ=(HOME="$case/home" PATH=/usr/local/bin:/usr/bin:/bin AWS_EC2_METADATA_DISABLED=true
    DISPATCH_URL="http://127.0.0.1:$port" DISPATCH_TOKEN_FILE="$case/dispatch-bearer")
  [ -z "$layer" ] || environ+=(PI_CONFIG_FILES="$layer")
  {
    printf '%s\n' '{"type":"prompt","message":"First question.","id":"1"}'
    for _ in $(seq 1 600); do [ -s "$case/out/conversation.json" ] && break || sleep 0.2; done
    sleep 3
  } | (cd "$case/cwd" && env -i "${environ[@]}" timeout 120 "$omp" --mode rpc \
    --extension "$root" --extension "$envoy" --extension "$legion" \
    --append-system-prompt "$case/append.md" --no-lsp --no-title) >"$case/rpc.out" 2>"$case/rpc.err"
  kill "$standin_pid" 2>/dev/null || true
  wait "$standin_pid" 2>/dev/null || true
  standin_pid=
  # Oh My Pi's own stderr, which README.md quotes: a warning that its default model is unavailable.
  while IFS= read -r line; do echo "case $name: omp stderr: $line" >&2; done <"$case/rpc.err"
  [ -s "$case/out/conversation.json" ] || { echo "capture.sh: case $name sent no request with a system prompt" >&2; cat "$case/rpc.err" >&2; exit 1; }
  python3 - "$case/out/conversation.json" "$here/requests/$name.json" "$case/cwd" <<'PY'
import json, sys
source, target, cwd = sys.argv[1:]
text = open(source, encoding="utf-8").read().replace(cwd, "/work/cwd")
with open(target, "w", encoding="utf-8") as file:
    json.dump(json.loads(text), file, indent=2, ensure_ascii=False)
    file.write("\n")
PY
  echo "captured requests/$name.json"
}

mkdir -p "$here/requests"
capture 1-changed-skill "$work/root-1" ""
capture 2-changed-skill-hidden "$work/root-1" "$work/hide.yml"
capture 3-changed-reference "$work/root-3" ""

# The repository's own formatter holds the committed JSON (lint's `biome check`), which writes the
# short arrays on one line: format the fixtures the way a commit holds them.
"$repo/node_modules/.bin/biome" format --write "$here/requests" >/dev/null
