#!/usr/bin/env bash
# Tests for `check-ci-images.sh`, the first of its three harnesses: the images a job pulls before
# checkout (each service and job container held to the credentials that pull without a login,
# the shapes the check cannot read, a docker action, a call of a workflow outside this tree), the
# two modes, and the repository's own tree in the package mode, which is the one mode a case of
# it is left for. The rows of .github/ci-images.yaml are check-ci-images-rows.test.sh's, and the
# references CI's reach names are check-ci-images-references.test.sh's.
#
# The fixtures, the stand-in for the package's reads and the helpers all three share are
# check-ci-images-test-lib.sh's, whose header says how a case builds its tree, why no case
# reaches the network and why every case names its mode.
#
# Run from anywhere: .github/scripts/check-ci-images.test.sh
# CI runs it in ci.yaml's test job, in its own step, "Test the CI image check".
set -euo pipefail
# Every fixture is built inside `root=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a tree that failed to build is used anyway and
# the case reports on whatever is there.
shopt -s inherit_errexit

# shellcheck source=.github/scripts/check-ci-images-test-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-ci-images-test-lib.sh"

# The spy the mode case drives the check with: it answers 200 for every digest and writes each
# one it was asked about to $SPY_LOG. The check reads ghcr.io through this seam alone, so a run
# that leaves the log empty made no network read.
spy_script="$work/spy.sh"
cat > "$spy_script" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$1" >> "${SPY_LOG:?}"
echo 200
SH
chmod +x "$spy_script"

echo "case: a tree whose every image is a row of .github/ci-images.yaml"
root=$(fixture green)
run_check tree "$root"
check "the tree mode exits 0" "$(is "$status" 0)"
check "says how many rows it read" "$(contains "$out" 'holds 4 row(s);')"
check "counts the reach" "$(contains "$out" "reference(s) in .* file(s) of CI's reach")"
run_check package "$root"
check "the package mode exits 0" "$(is "$status" 0)"
check "says what the package holds" "$(contains "$out" 'holds 4 row(s), 4 of them in the package')"

echo "case: a service image without the no-login credentials"
root=$(fixture service-no-credentials)
sed -i '/^        credentials:/d' "$root/.github/workflows/ci.yaml"
run_check tree "$root"
check "the gate rejects a service with no credentials" "$(is "$status" 1)"
check "the gate names its anonymous credentials" "$(contains "$out" 'credentials:.*fromJSON')"
# Every declaration copies the form this refusal names, so the form is held here to what makes
# the runner pull without a login: a username, so it fills in no token, and no password.
form=$(python3 -c 'import json, re, sys
named = re.search(r"fromJSON\(\x27(\{.*?\})\x27\)", sys.stdin.read())
value = json.loads(named.group(1)) if named else {}
print(bool(value.get("username")) and not value.get("password"))' <<<"$out" || echo unreadable)
check "the form it names gives a username and no password" "$(is "$form" True)"

echo "case: a container image without the no-login credentials"
root=$(fixture container-no-credentials)
browser_row "$root"
browser_job "$root" "$BROWSER"
sed -i '/^      credentials:/d' "$root/.github/workflows/ci.yaml"
run_check tree "$root"
check "the gate rejects a job container with no credentials" "$(is "$status" 1)"
check "the gate names the job container" "$(contains "$out" "this job's container image")"

echo "case: credentials with a password, or with an empty username the runner fills with its token"
n=0
for form in '{"username":"anonymous","password":"unused"}' '{"username":""}'; do
  n=$((n + 1))
  root=$(fixture "service-credentials-$n")
  sed -i "s|credentials:.*|credentials: \${{ fromJSON('$form') }}|" "$root/.github/workflows/ci.yaml"
  run_check tree "$root"
  check "the gate rejects credentials $form" "$(is "$status" 1)"
done

echo "case: a scalar job container, which has no credentials to set"
root=$(fixture scalar-container)
browser_row "$root"
cat >> "$root/.github/workflows/ci.yaml" <<YAML
  scalar:
    runs-on: ubuntu-24.04
    container: $REGISTRY@$BROWSER
    steps:
      - run: bun test
YAML
run_check tree "$root"
check "the gate rejects a scalar job container" "$(is "$status" 1)"
check "the gate names the scalar job container" \
  "$(contains "$out" "this job's container is $REGISTRY@$BROWSER, a bare image, which cannot set credentials:")"
check "and does not say it cannot read the image it prints" "$(is "$(contains "$out" 'cannot read')" false)"

echo "case: one service written as a bare image, which has no credentials to set"
root=$(fixture scalar-service)
sed -i "/^      nats:/,/^        credentials:/c\\      nats: $REGISTRY@$NATS" "$root/.github/workflows/ci.yaml"
run_check tree "$root"
check "the gate rejects one service written as a bare image" "$(is "$status" 1)"
check "and names it, a bare image" \
  "$(contains "$out" "ci.yaml:7: the nats service is $REGISTRY@$NATS, a bare image, which cannot set credentials:")"
check "and does not say it cannot read the image it prints" "$(is "$(contains "$out" 'cannot read')" false)"

echo "case: services, or a container, that is neither a mapping nor an expression"
root=$(fixture services-scalar)
sed -i "/^    services:/,/^        credentials:/c\\    services: $REGISTRY@$NATS" "$root/.github/workflows/ci.yaml"
run_check tree "$root"
check "the gate rejects services: as one plain string" "$(is "$status" 1)"
check "and says it names no image this check can read" \
  "$(contains "$out" "ci.yaml:6: this job's services are $REGISTRY@$NATS, neither a mapping nor an expression, so it names no image this check can read")"
root=$(fixture container-list)
browser_row "$root"
cat >> "$root/.github/workflows/ci.yaml" <<YAML
  listed:
    runs-on: ubuntu-24.04
    container: ["$REGISTRY@$BROWSER"]
    steps:
      - run: bun test
YAML
run_check tree "$root"
check "the gate rejects a container written as a list" "$(is "$status" 1)"
check "and says it names no image this check can read" \
  "$(contains "$out" "this job's container is .*, neither a mapping nor an expression, so it names no image this check can read")"

echo "case: services, or one service, written as one expression"
# The runner's workflow schema admits an expression for a job's whole services: and for one
# service, which the runner evaluates before checkout and this check cannot read. Each image breaks
# a rule a mapping is refused by: the credentials rule, then rule 4 (registry), 5 (tag) and 6 (row).
n=0
for image in "$REGISTRY@$NATS" docker.io/library/nats:2.10 "$REGISTRY:nats-2.10-0a1b2c3d" "$REGISTRY@$UNKNOWN"; do
  n=$((n + 1))
  root=$(fixture "services-expression-$n")
  sed -i "/^    services:/,/^        credentials:/c\\    services: \${{ fromJSON('{\"nats\":{\"image\":\"$image\"}}') }}" "$root/.github/workflows/ci.yaml"
  run_check tree "$root"
  check "the gate rejects services: as one expression naming $image" "$(is "$status" 1)"
  check "and names them, an expression" \
    "$(contains "$out" "ci.yaml:6: this job's services are .*, an expression, which the runner evaluates and this check cannot")"
done
root=$(fixture service-expression)
sed -i "/^      nats:/,/^        credentials:/c\\      nats: \${{ fromJSON('{\"image\":\"docker.io/library/nats:2.10\"}') }}" "$root/.github/workflows/ci.yaml"
run_check tree "$root"
check "the gate rejects one service written as an expression" "$(is "$status" 1)"
check "and names that service, an expression" \
  "$(contains "$out" "ci.yaml:7: the nats service is .*, an expression, which the runner evaluates and this check cannot")"

echo "case: a docker action can restore the pre-checkout pull"
root=$(fixture docker-action)
cat >> "$root/.github/workflows/ci.yaml" <<YAML
      - uses: docker://$REGISTRY@$NATS
YAML
run_check tree "$root"
check "the gate rejects a docker action" "$(is "$status" 1)"
check "the gate names the docker action" "$(contains "$out" 'docker:// actions can pull before checkout')"

echo "case: a job that calls a workflow outside this tree, whose services the gate cannot read"
# The runner pulls the services and the container a called workflow's jobs declare before
# checkout, as it does a job's own, and this check reads only this tree's files: a call of
# another repository's workflow, or of this repository's at another ref, is refused by name.
n=0
for called in "octo-org/shared-ci/.github/workflows/postgres-tests.yaml@0123456789abcdef0123456789abcdef01234567" \
  "cybermonklabs/legion/.github/workflows/ci-job-envoy-smoke.yaml@0123456789abcdef0123456789abcdef01234567"; do
  n=$((n + 1))
  root=$(fixture "foreign-workflow-$n")
  cat >> "$root/.github/workflows/ci.yaml" <<YAML
  called:
    uses: $called
YAML
  run_check tree "$root"
  check "the gate rejects a job that calls $called" "$(is "$status" 1)"
  check "and names the job, the line and the workflow" \
    "$(contains "$out" "ci.yaml:14: the called job calls $called, a workflow outside this tree")"
  check "and says it cannot read that workflow's services or container" \
    "$(contains "$out" "the image gate cannot read that workflow's services or container")"
done
root=$(fixture local-workflow)
cat >> "$root/.github/workflows/ci.yaml" <<YAML
  called:
    uses: ./.github/workflows/ci-job-envoy-smoke.yaml
YAML
run_check tree "$root"
check "a job that calls this tree's own workflow passes" "$(is "$status" 0)"

echo "case: each mode runs its own half"
# The tree mode's refusals read files, and the package mode's one refusal reads ghcr.io. This
# tree carries a refusal of the tree half (a Go string on another registry, rule 4) and a package
# that holds every row, so each mode answers for its half alone.
root=$(fixture mode-halves)
cat > "$root/packages/envoy/internal/testpg/testpg.go" <<'YAML'
package testpg

const Image = "docker.io/pgvector/pgvector:pg17"
YAML
spy_log="$work/spy.log"
: > "$spy_log"
set +e
out=$(CI_IMAGES_MANIFEST_HEAD="$spy_script" SPY_LOG="$spy_log" \
  "$root/.github/scripts/check-ci-images.sh" tree 2>&1)
status=$?
set -e
check "the tree mode refuses the reference" "$(is "$status" 1)"
check "naming the registry it found" "$(contains "$out" 'this string names docker.io/pgvector/pgvector')"
check "and reads the package for no row" "$(is "$(wc -l < "$spy_log")" 0)"

set +e
out=$(CI_IMAGES_MANIFEST_HEAD="$spy_script" SPY_LOG="$spy_log" \
  "$root/.github/scripts/check-ci-images.sh" package 2>&1)
status=$?
set -e
check "the package mode passes the same tree" "$(is "$status" 0)"
check "naming that reference nowhere" "$(is "$(contains "$out" 'docker.io/pgvector')" false)"
check "and reads the package once per row" "$(is "$(wc -l < "$spy_log")" 4)"

echo "case: a call that names no mode, and one that names both"
set +e
out=$("$check_script" 2>&1)
status=$?
set -e
check "no mode fails" "$(is "$status" 1)"
check "naming the tree mode" "$(contains "$out" 'this check takes one mode: .tree. reads the files of this tree')"
check "and the package mode" "$(contains "$out" '.package. reads ghcr.io/cybermonklabs/legion/ci')"

set +e
out=$("$check_script" tree package 2>&1)
status=$?
set -e
check "both modes in one call fails" "$(is "$status" 1)"
check "naming them" "$(contains "$out" 'this check takes one mode')"

# The package mode alone. ci.yaml's lint job runs `check-ci-images.sh tree` over this
# repository on every change (.github/workflows/ci-job-lint.yaml, "Check every image a CI job
# fetches is a row of .github/ci-images.yaml"), so a tree-mode case here would run that same
# check a second time in the same CI run. The package mode runs in a CI run too: ci.yaml's
# ci-images job runs `check-ci-images.sh package` (.github/workflows/ci-job-ci-images.yaml,
# "Check the package holds every row of .github/ci-images.yaml") on every event ci.yaml runs
# on - a pull request, a merge group and a dispatch - whenever the ci_images filter selects
# it, and there it reads ghcr.io itself. This case runs that mode over this tree's rows against
# a stand-in for the registry instead (CI_IMAGES_MANIFEST_HEAD and LABELS below), so the mode's
# own reading of the rows is held in every run of this suite, which reaches no registry.
echo "case: the repository's own tree, in the package mode"

# This tree's own `built` row carries the digest of an image this repository published, and
# rule 10 holds that image's label to the recipe this tree holds. The row is read out of
# .github/ci-images.yaml rather than written here, so a row that moves takes this case with
# it, and the label the stand-in answers with is the hash the build stamps into what it
# pushes: the row passes here for the reason it passes in CI.
repository=$(cd "$script_dir/../.." && pwd)
built_digest=$(cd "$repository" && PYTHONDONTWRITEBYTECODE=1 python3 -c '
import yaml

rows = yaml.safe_load(open(".github/ci-images.yaml"))["images"]
print(next(row["digest"] for row in rows if "built" in row))
')
set +e
out=$(CI_IMAGES_MANIFEST_HEAD="$head_script" LABELS="$built_digest=$(recipe "$repository")" \
  "$check_script" package 2>&1)
status=$?
set -e
check "the real tree passes the package mode, with every row held" "$(is "$status" 0)"
check "says so" "$(contains "$out" "holds .* row(s), .* of them in the package")"
check "and compared its built row" \
  "$(contains "$out" '1 built row(s) at the build recipe this tree holds')"

summary "check-ci-images.sh (pulls before checkout, and the modes)"
