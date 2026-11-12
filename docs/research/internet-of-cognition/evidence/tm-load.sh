#!/usr/bin/env bash
#  Every command of test-maslab.md, in order. Writes what it prints to maslab-load.txt:
#    bash tm-load.sh > maslab-load.txt 2>&1
#  This is the record of the run that wrote evidence/maslab-load.txt, and it names the paths of
#  the pod that ran it. The rows tm-* of claims.jsonl carry its commands, and
#  check.sh --replay runs them again anywhere, under the environment env.sh prepares.
set -u
export TMPDIR=/legion/scratch/tmp
export IOC_CLONES=$TMPDIR/ioc-clones
export OUT=$TMPDIR/infra3134/in/out
export UV_CACHE_DIR=$TMPDIR/infra3134/tm-cache/uv
export PIP_CACHE_DIR=$TMPDIR/infra3134/tm-cache/pip
export HF_HOME=$TMPDIR/infra3134/tm-cache/hf
export XDG_CACHE_HOME=$TMPDIR/infra3134/tm-cache/xdg
export VENV=$TMPDIR/infra3134/tm-venv
OURS=/legion/workspaces/cybermonklabs/legion/infra-3134

run() { echo; echo "\$ $*"; "$@" 2>&1; echo "exit=$?"; }

echo "### 0. the commits, and the two files of ours this evidence reads"
run git -C "$IOC_CLONES/mas-lab" rev-parse HEAD
run git -C "$OURS" log -1 --format=%H
echo
echo "\$ git show 9542c6f:...table.go | sha256sum; sha256sum the working tree's"
git -C "$OURS" show 9542c6fbbecd184ccee73523ab3defa39d6cf23a:packages/daemon/internal/workflow/table.go | sha256sum
sha256sum "$OURS/packages/daemon/internal/workflow/table.go"
git -C "$OURS" show 9542c6fbbecd184ccee73523ab3defa39d6cf23a:packages/daemon/internal/phase/phase.go | sha256sum
sha256sum "$OURS/packages/daemon/internal/phase/phase.go"

echo
echo "### 1. the install, from the clone at the pinned commit (mas-lab 0.2.0 is on PyPI as well)"
run uv venv --allow-existing "$VENV"
cd "$IOC_CLONES/mas-lab" || exit 1
VIRTUAL_ENV=$VENV run uv pip install ./runtime ./ctl ./library-standard
# A built wheel of mas-lab-ctl carries no docs/schemas, so validate falls back to
# "no schema file for kind 'agent'". An editable install of ctl and of the lab core
# component gives the validator the schemas and the registry it imports.
VIRTUAL_ENV=$VENV run uv pip install -e ./ctl -e ./lab/components/core
cd "$OUT" || exit 1

echo
echo "### 2. their validator over the two specifications"
run "$VENV/bin/mas-ctl" validate legion-workflow.yaml legion-workflow-kind.yaml

echo
echo "### 3. the compile-only path"
run "$VENV/bin/mas-ctl" compile legion-workflow.yaml --output "$TMPDIR/infra3134/tm-compiled.yaml"
echo
echo "\$ grep -c 'goto:' \$TMPDIR/infra3134/tm-compiled.yaml"
grep -c 'goto:' "$TMPDIR/infra3134/tm-compiled.yaml"
echo "\$ grep -c '^ *true:' \$TMPDIR/infra3134/tm-compiled.yaml"
grep -c '^ *true:' "$TMPDIR/infra3134/tm-compiled.yaml"

echo
echo "### 4. their loaders, from Python"
run "$VENV/bin/python" tm-load.py

echo
echo "### 5. the run itself, with no model key"
run "$VENV/bin/mas-ctl" chat legion-workflow.yaml -p "run the issue" --single-turn -I

echo
echo "### 6. what their format has no word for"
cd "$IOC_CLONES/mas-lab" || exit 1
SM=library-standard/src/mas/library/standard/plugins/design_patterns/state_machine.py
for pattern in "effect" "trigger" "guard:" "'wait'" "eval\(" "durable|persist" "transaction|atomic|rollback" "status"; do
  echo
  echo "\$ grep -rnE \"$pattern\" \$IOC_CLONES/mas-lab/$SM"
  grep -rnE "$pattern" "$SM"
  echo "exit=$?"
done
echo
echo "\$ grep -rni outbox --include=*.py \$IOC_CLONES/mas-lab"
grep -rni "outbox" --include=*.py .
echo "exit=$?"
echo
echo "\$ grep -rn 'class WorkflowSpec' --include=*.py \$IOC_CLONES/mas-lab"
grep -rn "class WorkflowSpec" --include=*.py .
echo "exit=$?"
echo
echo "\$ grep -rnE \"\\\"edges\\\"|'edges'\" --include=*.py \$IOC_CLONES/mas-lab/runtime/src \$IOC_CLONES/mas-lab/ctl/src"
grep -rnE "\"edges\"|'edges'" --include=*.py runtime/src ctl/src
echo "exit=$?"

echo
echo "### 7. the row count of our table, measured"
cd "$OURS/packages/daemon" || exit 1
echo "\$ grep -c '^	{From: phase\.' internal/workflow/table.go"
grep -c '^	{From: phase\.' internal/workflow/table.go
run go test -overlay="$OUT/tm-overlay.json" -run TestTmCountRows -v ./internal/workflow/

echo
echo "### 8. what their validator does from a plain wheel install"
WHEELVENV=$TMPDIR/infra3134/tm-venv-wheel
run uv venv --allow-existing "$WHEELVENV"
cd "$IOC_CLONES/mas-lab" || exit 1
VIRTUAL_ENV=$WHEELVENV run uv pip install ./runtime ./ctl ./library-standard
cd "$OUT" || exit 1
run "$WHEELVENV/bin/mas-ctl" validate legion-workflow.yaml
# The same ctl from a source checkout, without the lab core component its validator imports.
cd "$IOC_CLONES/mas-lab" || exit 1
VIRTUAL_ENV=$WHEELVENV run uv pip install -e ./ctl
cd "$OUT" || exit 1
run "$WHEELVENV/bin/mas-ctl" validate legion-workflow.yaml

echo
echo "### 9. mas-lab on PyPI, fetched 2026-11-12 into tm-pypi-mas-lab.json"
echo "\$ curl -s https://pypi.org/pypi/mas-lab/json -o tm-pypi-mas-lab.json; sha256sum tm-pypi-mas-lab.json"
sha256sum "$OUT/tm-pypi-mas-lab.json"
echo "\$ python -c 'the name, the version and the files of the record'"
"$VENV/bin/python" - <<'PY'
import json
from pathlib import Path

record = json.loads(Path("tm-pypi-mas-lab.json").read_text(encoding="utf-8"))
print("name:", record["info"]["name"], "version:", record["info"]["version"])
print("summary:", record["info"]["summary"])
print("releases:", sorted(record["releases"]))
print("files:", [f["filename"] for f in record["urls"]])
PY
