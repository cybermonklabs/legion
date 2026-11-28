#!/usr/bin/env bash
# worker-image-contract-order.test.sh - Runs the `contract_order` step of worker-image.yaml's
# build job, its own `run:` text read out of the workflow, over a shallow clone of a scratch
# repository, as that job runs it over its one-commit checkout. On a pull request the workflow
# comes from the merge ref while the checkout is the pull request's head, so the step meets heads
# from before the cutover to entry files (dispatch://INFRA-1569): in such a head, version.go
# declares `const DaemonAPIVersion = <n>`, no scripts/daemon-contract-order.sh exists, and its
# Dockerfile takes no stamp. The cases require:
#
#   - a head from before the cutover: the step exits 0 and writes an empty stamp;
#   - a head after it: the step writes the stamp its scripts/daemon-contract-order.sh prints;
#   - a head after it whose order script prints nothing, or that lacks the script: the step
#     fails, so no image of such a head is built unstamped.
#
# Run from the repository root: bash .github/scripts/worker-image-contract-order.test.sh
set -euo pipefail

root=$(git rev-parse --show-toplevel)
readonly workflow="${root}/.github/workflows/worker-image.yaml"
scratch=$(mktemp -d "${TMPDIR:-/tmp}/worker-image-contract-order-test.XXXXXX")
readonly scratch
trap 'rm -rf -- "$scratch"' EXIT

# git reads no configuration of the machine or the user, so a signing key, a hook or a default
# branch name of theirs changes nothing here, and every commit carries one fixed identity.
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
export GIT_AUTHOR_NAME='Image order test' GIT_AUTHOR_EMAIL=image-order-test@example.invalid
export GIT_COMMITTER_NAME='Image order test' GIT_COMMITTER_EMAIL=image-order-test@example.invalid

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

# The step's script exactly as the workflow holds it, so the test runs what CI runs.
python3 - "$workflow" > "${scratch}/step.sh" <<'PY'
import sys
import yaml

jobs = yaml.safe_load(open(sys.argv[1]))["jobs"]
steps = [step for step in jobs["build"]["steps"] if step.get("id") == "contract_order"]
if len(steps) != 1:
    sys.exit(f"worker-image.yaml's build job holds {len(steps)} steps of id contract_order, not one")
print(steps[0]["run"])
PY

readonly origin="${scratch}/origin"
git init -q -b main "$origin"
# The build fetches the head by its sha, blobless, as GitHub serves it.
git -C "$origin" config uploadpack.allowAnySHA1InWant true
git -C "$origin" config uploadpack.allowFilter true
mkdir -p "${origin}/packages/daemon/internal/api" "${origin}/scripts"
version="${origin}/packages/daemon/internal/api/version.go"
order="${origin}/scripts/daemon-contract-order.sh"

# A head from before the cutover: one numbered constant, no order script.
printf 'package api\n\nconst DaemonAPIVersion = 255\n' > "$version"
git -C "$origin" add -A
git -C "$origin" commit -q -m 'before the cutover'
before=$(git -C "$origin" rev-parse HEAD)

# A head after it: version.go keeps only a commented copy of the old line, as this repository's
# does for main's old check, and an order script stands in for the real one.
printf 'package api\n\n// The old check reads this line.\n//\n// const\tDaemonAPIVersion = 256\n' > "$version"
printf '#!/usr/bin/env bash\necho "v1:20261101000000"\n' > "$order"
git -C "$origin" add -A
git -C "$origin" commit -q -m 'after the cutover'
after=$(git -C "$origin" rev-parse HEAD)

# A head after it whose order script prints no stamp.
printf '#!/usr/bin/env bash\nexit 0\n' > "$order"
git -C "$origin" add -A
git -C "$origin" commit -q -m 'an order script that prints nothing'
silent=$(git -C "$origin" rev-parse HEAD)

# A head after it that lacks the order script.
git -C "$origin" rm -q scripts/daemon-contract-order.sh
git -C "$origin" commit -q -m 'no order script'
missing=$(git -C "$origin" rev-parse HEAD)

# run_step <name> <sha>: clones origin one commit deep at <sha>, as actions/checkout does, and
# runs the step there under the shell GitHub runs a `run:` step in. It leaves the step's exit
# status in $status and what it wrote to $GITHUB_OUTPUT in $output.
run_step() {
  local name=$1 sha=$2 clone="${scratch}/$1"
  git init -q "$clone"
  git -C "$clone" remote add origin "file://${origin}"
  git -C "$clone" fetch -q --depth=1 origin "$sha"
  git -C "$clone" checkout -q FETCH_HEAD
  : > "${scratch}/${name}.output"
  status=0
  (cd "$clone" && BUILT_SHA=$sha GITHUB_OUTPUT="${scratch}/${name}.output" \
    bash --noprofile --norc -eo pipefail "${scratch}/step.sh") > "${scratch}/${name}.log" 2>&1 || status=$?
  output=$(cat "${scratch}/${name}.output")
}

run_step before "$before"
if [[ $status -ne 0 ]]; then
  fail "a head from before the cutover: the step exited $status, not 0: $(cat "${scratch}/before.log")"
elif [[ $output != 'stamp=' ]]; then
  fail "a head from before the cutover: the step wrote '$output', not an empty stamp"
else
  echo "ok: a head from before the cutover is built with no stamp"
fi

run_step after "$after"
if [[ $status -ne 0 ]]; then
  fail "a head after the cutover: the step exited $status, not 0: $(cat "${scratch}/after.log")"
elif [[ $output != 'stamp=v1:20261101000000' ]]; then
  fail "a head after the cutover: the step wrote '$output', not its order script's stamp"
else
  echo "ok: a head after the cutover is built with its stamp"
fi

run_step silent "$silent"
if [[ $status -eq 0 ]]; then
  fail "a head after the cutover whose order script prints no stamp: the step exited 0 and wrote '$output'"
else
  echo "ok: a head after the cutover with no stamp is refused"
fi

run_step missing "$missing"
if [[ $status -eq 0 ]]; then
  fail "a head after the cutover with no order script: the step exited 0 and wrote '$output'"
else
  echo "ok: a head after the cutover with no order script is refused"
fi

if [[ $failures -ne 0 ]]; then
  echo "${failures} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
