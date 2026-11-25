#!/usr/bin/env bash
# Captures shipped-names.txt: every agent name and every skill name a real Oh My Pi discovers over
# both packed plugins, one `agent <name>` or `skill <name>` per line, sorted. Run it again after a
# change to a skill, an agent or the Oh My Pi pin (README.md says why and what compares against it).
#
#   OMP=<the Oh My Pi .omp-pin names> bash capture-names.sh
#
# Oh My Pi runs as the boot gate's pod lane runs its load probe (bootgate.go, `models` then the
# lane's flags), with list-names.mjs in the probe's place and the two packed plugins as its explicit
# roots, in the order a pod's argv names them:
#
#   omp models --extension <pi-envoy pack> --extension <pi-legion pack> --extension list-names.mjs --json
#
# with LEGION_PROMPT_ROOTS naming the same two roots, under a fresh HOME and from a fresh working
# directory, so no user or project skill or agent of this machine enters the list. The flags come
# after `models`: `omp --extension <module> models` takes them as the root command's and loads none
# of them for `models`, so it prints nothing. OMP defaults to
# LEGION_TEST_OMP, then LEGION_OMP_PATH. The pin is github:<owner>/<repo>@<version> in the
# repository's .omp-pin; .github/actions/install-omp/action.yml installs it. The script prints the
# version and the sha256 of the binary it ran, which README.md records.
#
# The plugins are packed as the release packs them (scripts/e2e/lib/pack-plugin.sh), from this
# checkout, which needs `bun install --frozen-lockfile` at the repository root. Scratch is one
# mktemp -d under $TMPDIR, removed at the end by its exact path.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
repo=$(cd "$here/../../../../../.." && pwd -P)
omp=${OMP:-${LEGION_TEST_OMP:-${LEGION_OMP_PATH:-}}}
[ -n "$omp" ] && [ -x "$omp" ] || { echo "capture-names.sh: set OMP to the Oh My Pi binary .omp-pin names" >&2; exit 2; }
for tool in bun jq tar sort; do
  command -v "$tool" >/dev/null || { echo "capture-names.sh: $tool is not on PATH" >&2; exit 2; }
done
[ -d "$repo/node_modules" ] || (cd "$repo" && bun install --frozen-lockfile >&2)

echo "pin: $(cat "$repo/.omp-pin")"
echo "omp: $("$omp" --version) sha256 $(sha256sum "$omp" | cut -d' ' -f1)"

work=$(mktemp -d "${TMPDIR:-/tmp}/shipped-names.XXXXXX")
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/plugins" "$work/home" "$work/cwd"
for plugin in pi-envoy pi-legion; do
  tarball=$(bash "$repo/scripts/e2e/lib/pack-plugin.sh" "$plugin" "$work/tgz-$plugin" 2>/dev/null)
  mkdir "$work/plugins/$plugin"
  tar -xzf "$tarball" -C "$work/plugins/$plugin" --strip-components=1
done
envoy=$work/plugins/pi-envoy
legion=$work/plugins/pi-legion

# `omp models` loads its extensions only for a profile with a model: one operator provider that
# nothing listens on, which no run here calls (the probe tests' own profile).
mkdir -p "$work/home/.omp/agent"
cat >"$work/home/.omp/agent/models.yml" <<'YML'
providers:
  operator:
    baseUrl: http://127.0.0.1:9
    auth: apiKey
    api: anthropic-messages
    apiKey: unused
    models:
      - id: probe-test
        name: Probe test
YML
printf 'modelRoles:\n  default: operator/probe-test\n' >"$work/home/.omp/agent/config.yml"

(cd "$work/cwd" && env -i HOME="$work/home" PATH=/usr/local/bin:/usr/bin:/bin AWS_EC2_METADATA_DISABLED=true \
  LEGION_PROMPT_ROOTS="$envoy:$legion" timeout 180 "$omp" \
  models --extension "$envoy" --extension "$legion" --extension "$here/list-names.mjs" --json) \
  >"$work/stdout" 2>"$work/stderr" || { echo "capture-names.sh: omp models failed:" >&2; cat "$work/stderr" >&2; exit 1; }

# Only the module's own lines, which no other line Oh My Pi prints has the shape of.
grep -E '^(agent|skill) [a-z0-9][a-z0-9._-]*$' "$work/stderr" | LC_ALL=C sort -u >"$work/names"
for expected in "agent scout" "agent reviewer" "agent deep-worker" "skill legion-worker" "skill dispatch-first"; do
  grep -qxF "$expected" "$work/names" || { echo "capture-names.sh: the list lacks '$expected':" >&2; cat "$work/stderr" >&2; exit 1; }
done
cp "$work/names" "$here/shipped-names.txt"
echo "wrote shipped-names.txt: $(grep -c '^agent ' "$here/shipped-names.txt") agents, $(grep -c '^skill ' "$here/shipped-names.txt") skills"
