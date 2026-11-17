#!/usr/bin/env bash
# Sourced by check-legion-version-inputs.test.sh: how its fixtures are built and run. A fixture is
# a miniature repository under the harness's temporary directory - a go.work over a daemon module
# whose cmd/legion imports packages of an envoy module, one of them on darwin alone, an envoy
# module with the three programs the listener release builds, the two workflows and the two
# target lists - with the real script and the modules it imports symlinked into it, so the script
# under test is the file CI runs. Every job of the two workflows that the check pins is rendered
# from release_version_inputs.py, so each fixture starts pinned as the check requires and each
# case of the harness changes one piece of it.
#
# The caller sets $script_dir (this directory) and $work (the temporary directory) before it
# sources this file, and sources test-lib.sh first; the variables below, and run_check's $out and
# $status, are the caller's to read. Not executable:
# `source "$script_dir/check-legion-version-inputs-fixtures.sh"`.
# shellcheck disable=SC2034,SC2154 # the variables this file sets are read by its caller, and the
# two it reads ($script_dir, $work) are set there

# The hand-written arguments and the version step every fixture starts from.
hand_args='    -- packages/daemon/ .github/go-release-targets.json'
handoff='.github/scripts/release-push.sh version legion-v $LEGION_VERSION_ARGS $LEGION_GO_INPUTS'

# step <kind> <name> [<steps before>]: a piece of release_version_inputs.py as YAML lines of a
# fixture: `version <job>:<step id>`, one of VERSION_STEPS with its if:, env: and its run: as a
# `|` block; `before <name>`, one of STEPS_BEFORE_VERSION_STEPS; `outputs release|listener:<job>`,
# the job's JOB_OUTPUTS; `job-head release|listener:<job>`, the job's own if: and needs:;
# `chain release|listener`, the jobs whose entry is their own if: alone; `on release|listener`,
# the workflow's on:; or `readers release|listener:<job> <steps before>`, the job's
# DECISION_READERS after that many steps, each at its position when the module names it by one,
# with `run: "true"` fillers between. Every fixture's jobs are built from these, so each starts
# pinned as the check requires and each case of the harness changes one piece of it.
step() {
  PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$script_dir" python3 - "$@" <<'PY'
import json
import sys

import release_version_inputs as plan

kind, name = sys.argv[1], sys.argv[2]
if kind == "version":
    spec = next(s for s in plan.VERSION_STEPS if f"{s.job}:{s.step_id}" == name)
    fields = {"id": spec.step_id, "if": spec.if_, "env": spec.env, "run": spec.run}
elif kind == "outputs":
    workflow, job = name.split(":")
    wanted = plan.JOB_OUTPUTS[({"release": plan.RELEASE, "listener": plan.LISTENER}[workflow], job)]
    print("\n".join(["    outputs:", *(f"      {k}: {json.dumps(v)}" for k, v in wanted.items())]))
    sys.exit(0)
elif kind == "job-head":
    # The job's own if: when DECISION_READERS names one (position 0), and its needs: when JOB_NEEDS
    # names any, as lines of the job.
    workflow, job = name.split(":")
    key = ({"release": plan.RELEASE, "listener": plan.LISTENER}[workflow], job)
    lines = [f"    if: {json.dumps(r.if_)}" for r in plan.DECISION_READERS.get(key, ()) if r.position == 0 and r.if_]
    if plan.JOB_NEEDS.get(key):
        lines.append(f"    needs: {json.dumps(list(plan.JOB_NEEDS[key]))}")
    print("\n".join(lines))
    sys.exit(0)
elif kind == "on":
    # The workflow's on: as TRIGGERS, PUSH_KEYS and PUSH_BRANCHES name it, every push key but the
    # branches set to all paths.
    path = {"release": plan.RELEASE, "listener": plan.LISTENER}[name]
    out = ["on:"]
    for event in plan.TRIGGERS[path]:
        out.append(f"  {event}:")
        if event == "push":
            out += [f"    {key}: {json.dumps(list(plan.PUSH_BRANCHES) if key == 'branches' else ['**'])}"
                    for key in plan.PUSH_KEYS]
    print("\n".join(out))
    sys.exit(0)
elif kind == "chain":
    # Each job of the workflow whose DECISION_READERS entry is its own if: alone (the jobs the
    # decision jobs need, and the jobs that read a published decision), as a whole job with its
    # pinned if: and needs: and one step.
    path = {"release": plan.RELEASE, "listener": plan.LISTENER}[name]
    out = []
    for (workflow, job), readers in plan.DECISION_READERS.items():
        if workflow != path or any(r.position != 0 for r in readers):
            continue
        out.append(f"  {job}:\n    runs-on: ubuntu-24.04")
        out += [f"    if: {json.dumps(r.if_)}" for r in readers if r.if_]
        if plan.JOB_NEEDS.get((workflow, job)):
            out.append(f"    needs: {json.dumps(list(plan.JOB_NEEDS[(workflow, job)]))}")
        out.append('    steps:\n      - run: "true"')
    print("\n".join(out))
    sys.exit(0)
elif kind == "readers":
    workflow, job = name.split(":")
    readers = plan.DECISION_READERS[({"release": plan.RELEASE, "listener": plan.LISTENER}[workflow], job)]
    # In the module's order, which is the job's: a reader named by position waits behind fillers
    # for its place, a reader named by id: or name: takes the next one. Position 0 is the job's
    # own if:, which `step job-head` renders on the job.
    out, at = [], int(sys.argv[3]) + 1
    for reader in (r for r in readers if r.position != 0):
        if reader.id is None and reader.name is None:
            while at < reader.position:
                out.append('      - run: "true"')
                at += 1
            out += ['      - run: "true"', f"        if: {json.dumps(reader.if_)}"]
        else:
            head = f"- id: {reader.id}" if reader.id else f"- name: {json.dumps(reader.name)}"
            out += [f"      {head}", f"        if: {json.dumps(reader.if_)}", '        run: "true"']
        at += 1
    print("\n".join(out))
    sys.exit(0)
else:
    spec = next(s for s in plan.STEPS_BEFORE_VERSION_STEPS if s.name == name)
    fields = {"uses": spec.uses, "with": spec.with_, "env": spec.env, "run": spec.run}
lines = []
for key, value in fields.items():
    if value is None:
        continue
    if key == "run":
        lines += ["run: |", *(f"  {line}" for line in value.rstrip("\n").split("\n"))]
    elif isinstance(value, dict):
        lines += [f"{key}:", *(f"  {k}: {json.dumps(v)}" for k, v in value.items())]
    else:
        lines.append(f"{key}: {json.dumps(value) if key == 'if' else value}")
print("\n".join(f"      {'- ' if i == 0 else '  '}{line}" for i, line in enumerate(lines)))
PY
}
legion_step=$(step version legion_build:version)
envoy_build_step=$(step version release_build:version)
envoy_confirm_step=$(step version release_publish:confirm)
legion_outputs=$(step outputs release:legion_build)
envoy_build_outputs=$(step outputs listener:release_build)
publish_outputs=$(step outputs release:publish)
checkout_step=$(step before checkout)
checkout_sha_step=$(step before "checkout of the event's commit")
determine_step=$(step before "Determine latest")
login_step=$(step before "log in to GHCR")
advance_step=$(step before "Advance latest image")
legion_readers=$(step readers release:legion_build 2)
publish_readers=$(step readers release:publish 0)
envoy_build_readers=$(step readers listener:release_build 2)
envoy_publish_readers=$(step readers listener:release_publish 5)
legion_job_head=$(step job-head release:legion_build)
publish_job_head=$(step job-head release:publish)
envoy_build_job_head=$(step job-head listener:release_build)
envoy_publish_job_head=$(step job-head listener:release_publish)
release_chain=$(step chain release)
listener_chain=$(step chain listener)
release_on=$(step on release)
listener_on=$(step on listener)
login_uses=$(PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$script_dir" python3 -c 'import release_version_inputs as p; print(p.LOGIN)')
confirm_version='          VERSION: "${{ needs.release_build.outputs.version }}"'
# The listener's, as release-envoy-listener.yaml writes them: the envoy module but its excluded
# infra directory, and the workspace's module files.
envoy_args='    -- :(exclude)packages/envoy/infra packages/envoy/ .github/go-release-targets.json'
envoy_modules='    go.work go.work.sum packages/daemon/go.mod packages/daemon/go.sum'

# fixture <name>: a tree whose block is still empty, so `write` fills it. Echoes its root.
# Each workflow's build job comes first, its chain jobs next and its publish job last, so a case
# that changes the first `runs-on:` or `steps:` changes the build job, and one that appends a step
# with `>>` appends it to publish or release_publish.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/packages/daemon/cmd/legion" \
    "$root/packages/daemon/lib" "$root/packages/envoy/alpha" "$root/packages/envoy/beta" \
    "$root/packages/envoy/gamma" "$root/packages/envoy/internal/delta" "$root/packages/envoy/infra/x" \
    "$root/packages/envoy/cmd/listener" "$root/packages/envoy/cmd/agent-secrets" \
    "$root/packages/envoy/cmd/agent-secrets-helper"
  for linked in check-legion-version-inputs.sh go_offline.py release_push_paths.py release_version_inputs.py workflow_paths.py; do
    ln -s "$script_dir/$linked" "$root/.github/scripts/$linked"
  done
  printf 'go 1.22\n\nuse (\n\t./packages/daemon\n\t./packages/envoy\n)\n' > "$root/go.work"
  : > "$root/go.work.sum"
  printf 'module example.com/envoy\n\ngo 1.22\n' > "$root/packages/envoy/go.mod"
  : > "$root/packages/envoy/go.sum"
  printf 'package alpha\n\nimport _ "example.com/envoy/internal/delta"\n' > "$root/packages/envoy/alpha/alpha.go"
  printf 'package beta\n' > "$root/packages/envoy/beta/beta.go"
  printf 'package gamma\n' > "$root/packages/envoy/gamma/gamma.go"
  printf 'package delta\n' > "$root/packages/envoy/internal/delta/delta.go"
  printf 'package x\n' > "$root/packages/envoy/infra/x/x.go"
  printf 'package main\n\nimport _ "example.com/envoy/alpha"\n\nfunc main() {}\n' > "$root/packages/envoy/cmd/listener/main.go"
  printf 'package main\n\nfunc main() {}\n' > "$root/packages/envoy/cmd/agent-secrets/main.go"
  printf 'package main\n\nfunc main() {}\n' > "$root/packages/envoy/cmd/agent-secrets-helper/main.go"
  printf 'module example.com/daemon\n\ngo 1.22\n\nrequire example.com/envoy v0.0.0\n\nreplace example.com/envoy => ../envoy\n' \
    > "$root/packages/daemon/go.mod"
  : > "$root/packages/daemon/go.sum"
  printf 'package lib\n' > "$root/packages/daemon/lib/lib.go"
  printf 'package main\n\nimport _ "example.com/envoy/alpha"\n\nfunc main() {}\n' > "$root/packages/daemon/cmd/legion/main.go"
  printf '//go:build darwin\n\npackage main\n\nimport _ "example.com/envoy/beta"\n' > "$root/packages/daemon/cmd/legion/darwin.go"
  printf '{"legion": ["linux/amd64", "darwin/arm64"], "envoy": ["amd64"]}\n' > "$root/.github/go-release-targets.json"
  printf '{"agent-secrets-client": ["darwin/arm64"]}\n' > "$root/.github/agent-secrets-client-targets.json"
  cat > "$root/.github/workflows/release-envoy-listener.yaml" <<EOF
name: Release Envoy Listener
$listener_on
env:
  LEGION_ENVOY_VERSION_ARGS: >-
    --patch-path .github/go-release-targets.json
$envoy_args
$envoy_modules
EOF
  cat >> "$root/.github/workflows/release-envoy-listener.yaml" <<YAML
jobs:
  release_build:
    runs-on: ubuntu-24.04
$envoy_build_job_head
$envoy_build_outputs
    steps:
$checkout_step
$envoy_build_step
$envoy_build_readers
$listener_chain
  release_publish:
    runs-on: ubuntu-24.04
$envoy_publish_job_head
    steps:
$checkout_sha_step
$determine_step
$login_step
$advance_step
$envoy_confirm_step
$envoy_publish_readers
YAML
  cat > "$root/.github/workflows/release.yaml" <<EOF
name: Release Packages
$release_on
env:
  LEGION_VERSION_ARGS: >-
    --patch-path .github/go-release-targets.json
$hand_args
  # BEGIN generated by \`.github/scripts/check-legion-version-inputs.sh write\`
  # END generated
jobs:
  legion_build:
    runs-on: ubuntu-24.04
$legion_job_head
$legion_outputs
    steps:
$checkout_step
$legion_step
$legion_readers
$release_chain
  publish:
    runs-on: ubuntu-24.04
$publish_job_head
$publish_outputs
    steps:
$publish_readers
EOF
  echo "$root"
}

# written <name>: a fixture with its block written; fails the run when `write` fails on it.
written() {
  local root
  root=$(fixture "$1")
  "$root/.github/scripts/check-legion-version-inputs.sh" write > /dev/null
  echo "$root"
}

run_check() {
  local root=$1
  shift
  set +e
  out=$("$root/.github/scripts/check-legion-version-inputs.sh" "$@" 2>&1)
  status=$?
  set -e
}

# replace <file> <old> <new>: one literal replacement, failing the case loudly if <old> is absent.
replace() {
  local file=$1 old=$2 new=$3 text
  text=$(<"$file")
  if [[ $text != *"$old"* ]]; then
    echo "  FAIL: $file has no '$old' to replace" >&2
    return 1
  fi
  printf '%s\n' "${text/"$old"/"$new"}" > "$file"
}

release() { cat "$1/.github/workflows/release.yaml"; }

# swap_steps <file> <first> <second>: swaps two steps of a fixture's workflow, each named by the
# first line it starts with (`      - id: legion_pair`), every line of both kept as it is; fails the
# case loudly when either is not there exactly once.
swap_steps() {
  python3 - "$@" <<'PY'
import re
import sys
from pathlib import Path

path, first, second = Path(sys.argv[1]), sys.argv[2], sys.argv[3]
text = path.read_text()
blocks = re.split(r"(?m)^(?=      - )", text)
heads = [i for i, b in enumerate(blocks) if b.startswith(first + "\n")], [i for i, b in enumerate(blocks) if b.startswith(second + "\n")]
if any(len(h) != 1 for h in heads):
    sys.exit(f"  FAIL: swap_steps found {len(heads[0])} and {len(heads[1])} of the two steps")
i, j = heads[0][0], heads[1][0]
tail = lambda b: b if b.endswith("\n") else b + "\n"  # noqa: E731
blocks[i], blocks[j] = tail(blocks[j]), tail(blocks[i])
path.write_text("".join(blocks))
PY
}
