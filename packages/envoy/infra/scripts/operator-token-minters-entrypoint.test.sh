#!/usr/bin/env bash
# Tests the no-argument minter-auditor entrypoint against fake gcloud, kubectl, curl,
# gke-gcloud-auth-plugin and minter check: no cloud, no cluster. The rows:
#   - each scalar variable unset or empty, and each array variable malformed, exits 2 naming it and
#     makes no gcloud call; so do an empty or duplicate cluster list, a writer list of another
#     length, and an argument;
#   - a full run builds a context for each of the pool's five clusters, reads each render object's
#     commit and run_id from the API's metadata, fetches that object at the generation it read, runs
#     the check with its flags in order (each cluster an --audited-cluster), and adds the objects to
#     the report;
#   - a cluster whose context cannot be built exits 2 naming it, before the check runs;
#   - an object without its commit metadata exits 2 naming it, before the check runs.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
entrypoint="$script_dir/operator-token-minters/entrypoint.sh"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/operator-token-minters-entrypoint.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
# The fake gcloud: get-credentials fails for the cluster MINTERS_AUDITOR_TEST_BAD_CLUSTER names; an
# object's raw description holds generation 7 and its commit and run id, but none for the object
# MINTERS_AUDITOR_TEST_NO_COMMIT names; cp copies only an object named at generation 7.
cat >"$tmp/bin/gcloud" <<'EOF'
#!/usr/bin/env bash
: "${MINTERS_AUDITOR_TEST_CALLS:?}"
printf 'gcloud %s\n' "$*" >>"$MINTERS_AUDITOR_TEST_CALLS"
case "$1 $2 $3" in
  "container clusters get-credentials")
    [ "$4" != "${MINTERS_AUDITOR_TEST_BAD_CLUSTER:-}" ] || { echo "ERROR: permission denied on $4" >&2; exit 1; }
    [ -n "${KUBECONFIG:-}" ] || { echo "fake gcloud: get-credentials with no KUBECONFIG" >&2; exit 2; }
    printf 'cluster: %s\n' "$4" >>"$KUBECONFIG"
    ;;
  "storage objects describe")
    [ "$5 $6" = "--raw --format=json" ] || { echo "fake gcloud: describe without --raw --format=json" >&2; exit 2; }
    object="${4#gs://example-renders/}"
    if [ "$object" = "${MINTERS_AUDITOR_TEST_NO_COMMIT:-}" ]; then
      jq -n '{generation: "7", metadata: {run_id: "run-1"}}'
    else
      jq -n --arg o "$object" '{generation: "7", metadata: {commit: ("commit-of-" + $o), run_id: "run-1"}}'
    fi
    ;;
  "storage cp gs://example-renders/"*)
    case "$3" in *"#7") ;; *) echo "fake gcloud: cp of an object not pinned to its generation" >&2; exit 2 ;; esac
    printf '{"object": "%s"}\n' "${3#gs://example-renders/}" >"$4"
    ;;
  *) echo "fake gcloud: unexpected call: gcloud $*" >&2; exit 2 ;;
esac
EOF
for tool in kubectl curl gke-gcloud-auth-plugin; do
  printf '#!/usr/bin/env bash\necho "fake %s: unexpected call" >&2\nexit 2\n' "$tool" >"$tmp/bin/$tool"
done
# The fake check: records its arguments, one a line, and writes the --report it was given.
cat >"$tmp/bin/operator-token-minters.sh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$MINTERS_AUDITOR_TEST_ARGS"
while [ "$#" -gt 0 ]; do
  [ "$1" != --report ] || printf '{"findings": [], "unknown": []}\n' >"$2"
  shift
done
EOF
chmod +x "$tmp/bin/"*

scalars=(
  MINTERS_AUDITOR_RENDERS_BUCKET MINTERS_AUDITOR_RENDERS_PROJECT MINTERS_AUDITOR_RENDERS_ADMIN MINTERS_AUDITOR_PROJECT
  MINTERS_AUDITOR_ANALYSIS_SCOPE MINTERS_AUDITOR_SECRETS_PREFIX
  MINTERS_AUDITOR_NAMESPACE MINTERS_AUDITOR_SERVICE_ACCOUNT MINTERS_AUDITOR_GSA
  MINTERS_AUDITOR_BROKER_ACCOUNT MINTERS_AUDITOR_BROKER_WORKLOAD MINTERS_AUDITOR_SQL_INSTANCE
  MINTERS_AUDITOR_DB_SECRET MINTERS_AUDITOR_BROKER_STATE MINTERS_AUDITOR_SIGNING_SECRET
  MINTERS_AUDITOR_SECRET MINTERS_AUDITOR_POOL_PROJECT MINTERS_AUDITOR_FLEET
  MINTERS_AUDITOR_OPERATOR_CLUSTER MINTERS_AUDITOR_OPERATOR_LOCATION MINTERS_AUDITOR_REPORT
)
arrays=(MINTERS_AUDITOR_CLUSTERS MINTERS_AUDITOR_AGENT_NAMESPACES MINTERS_AUDITOR_GUARDED_PAIRS
  MINTERS_AUDITOR_RBAC_ONLY_MEMBERS MINTERS_AUDITOR_SERVICE_AGENTS MINTERS_AUDITOR_RENDER_WRITERS)
cluster() { printf '{"project":"example-project","location":"example-region1","name":"example-cluster-%s"}' "$1"; }
clusters="[$(cluster a),$(cluster b),$(cluster c),$(cluster d),$(cluster e)]"
writers='["writer-a@example-project.iam.gserviceaccount.com=rbac/cluster-grants","writer-b@example-project.iam.gserviceaccount.com=secrets/dispatch","writer-c@example-project.iam.gserviceaccount.com=secrets/broker","writer-d@example-project.iam.gserviceaccount.com=iam/bootstrap"]'

run() {
  status=0
  rm -f "$tmp/calls" "$tmp/args" "$tmp/report.json"
  output="$(env PATH="$tmp/bin:$PATH" MINTERS_AUDITOR_TEST_CALLS="$tmp/calls" MINTERS_AUDITOR_TEST_ARGS="$tmp/args" TMPDIR="$tmp" \
    MINTERS_AUDITOR_RENDERS_BUCKET=gs://example-renders MINTERS_AUDITOR_RENDERS_PROJECT=example-root \
    MINTERS_AUDITOR_SECRETS_PREFIX=projects/000000000000/secrets/agent-secrets- \
    MINTERS_AUDITOR_RENDERS_ADMIN=renders-admin@example-project.iam.gserviceaccount.com MINTERS_AUDITOR_PROJECT=example-project \
    MINTERS_AUDITOR_ANALYSIS_SCOPE=organizations/100 MINTERS_AUDITOR_NAMESPACE=example-namespace \
    MINTERS_AUDITOR_SERVICE_ACCOUNT=example-service-account MINTERS_AUDITOR_GSA=example@example-project.iam.gserviceaccount.com \
    MINTERS_AUDITOR_BROKER_ACCOUNT=broker@example-project.iam.gserviceaccount.com \
    MINTERS_AUDITOR_BROKER_WORKLOAD=example-broker/example-broker \
    MINTERS_AUDITOR_SQL_INSTANCE=example-instance MINTERS_AUDITOR_DB_SECRET=example-secrets/example-db \
    MINTERS_AUDITOR_BROKER_STATE=gs://example-state MINTERS_AUDITOR_SIGNING_SECRET=EXAMPLE_SIGNING \
    MINTERS_AUDITOR_SECRET=EXAMPLE_OPERATOR MINTERS_AUDITOR_POOL_PROJECT=example-project MINTERS_AUDITOR_FLEET=none \
    MINTERS_AUDITOR_OPERATOR_CLUSTER=example-cluster-a MINTERS_AUDITOR_OPERATOR_LOCATION=example-region1 \
    MINTERS_AUDITOR_REPORT="$tmp/report.json" MINTERS_AUDITOR_CLUSTERS="$clusters" \
    MINTERS_AUDITOR_AGENT_NAMESPACES='["example-agents"]' \
    MINTERS_AUDITOR_GUARDED_PAIRS='["example-auditor/example-service-account"]' \
    MINTERS_AUDITOR_RBAC_ONLY_MEMBERS='[]' MINTERS_AUDITOR_SERVICE_AGENTS='[]' MINTERS_AUDITOR_RENDER_WRITERS="$writers" \
    "$@" "$entrypoint" 2>&1)" || status=$?
}
fail() { echo "FAIL: $*" >&2; echo "$output" >&2; exit 1; }
refused_before_gcloud() {
  [ "$status" -eq 2 ] || fail "$1 exits $status, want 2"
  [[ "$output" == *"$2"* ]] || fail "$1 does not name $2"
  [ ! -e "$tmp/calls" ] || fail "$1 called gcloud"
}

# --- Validation, before any call. ---
for name in "${scalars[@]}"; do
  run env -u "$name"
  refused_before_gcloud "unset $name" "$name"
  run env "$name="
  refused_before_gcloud "empty $name" "$name"
done
for name in "${arrays[@]}"; do
  run env "$name=not-json"
  refused_before_gcloud "malformed $name" "$name"
done
run env MINTERS_AUDITOR_CLUSTERS="[$(cluster a),$(cluster a),$(cluster c),$(cluster d)]"
refused_before_gcloud "a duplicate cluster" MINTERS_AUDITOR_CLUSTERS
run env MINTERS_AUDITOR_CLUSTERS='[]'
refused_before_gcloud "an empty cluster list" MINTERS_AUDITOR_CLUSTERS
run env MINTERS_AUDITOR_RENDER_WRITERS='["writer-a@example-project.iam.gserviceaccount.com=rbac/cluster-grants"]'
refused_before_gcloud "one writer" MINTERS_AUDITOR_RENDER_WRITERS
status=0
output="$(env PATH="$tmp/bin:$PATH" "$entrypoint" an-argument 2>&1)" || status=$?
[ "$status" -eq 2 ] && [[ "$output" == *"takes no argument"* ]] || fail "an argument exits $status, want 2 naming it"
echo "PASS: each invalid variable, and an argument, exits 2 naming it before any call"

# --- A full run. ---
run
[ "$status" -eq 0 ] || fail "a full run exits $status, want 0"
for c in a b c d e; do
  grep -qxF "gcloud container clusters get-credentials example-cluster-$c --project=example-project --location=example-region1 --quiet" "$tmp/calls" ||
    fail "a full run built no context for example-cluster-$c"
done
want_args="$tmp/want-args"
{
  printf '%s\n' --project example-project --cluster example-cluster-a --location example-region1 --pool-project example-project \
    --namespace example-namespace --service-account example-service-account --gsa example@example-project.iam.gserviceaccount.com \
    --broker-account broker@example-project.iam.gserviceaccount.com --broker-workload example-broker/example-broker \
    --sql-instance example-instance --db-secret example-secrets/example-db --broker-state gs://example-state \
    --signing-secret EXAMPLE_SIGNING --secret EXAMPLE_OPERATOR --secrets-prefix projects/000000000000/secrets/agent-secrets- \
    --analysis-scope organizations/100 --fleet none \
    --renders-bucket gs://example-renders --renders-project example-root --renders-admin renders-admin@example-project.iam.gserviceaccount.com
  for object in rbac-cluster-grants secrets-dispatch secrets-broker iam-bootstrap iam-root; do
    case "$object" in rbac-*) printf '%s\n' --rbac-render ;; secrets-*) printf '%s\n' --secret-render ;; iam-*) printf '%s\n' --iam-preview ;; esac
    printf '%s\n' "WORK/$object.json"
  done
  printf '%s\n' --report WORK/report.json --agent-namespace example-agents --guarded example-auditor/example-service-account
  jq -r '.[] | "--render-writer\n\(.)"' <<<"$writers"
  for c in a b c d e; do printf '%s\n' --audited-cluster "example-project/example-region1/example-cluster-$c"; done
} >"$want_args"
sed -E 's#^.*/minters-auditor\.[^/]+/#WORK/#' "$tmp/args" | diff "$want_args" - >"$tmp/args.diff" ||
  fail "a full run passed the check other flags: $(cat "$tmp/args.diff")"
jq -e '.renders == [
  {object: "rbac/cluster-grants", commit: "commit-of-rbac/cluster-grants", run_id: "run-1", generation: "7"},
  {object: "secrets/dispatch", commit: "commit-of-secrets/dispatch", run_id: "run-1", generation: "7"},
  {object: "secrets/broker", commit: "commit-of-secrets/broker", run_id: "run-1", generation: "7"},
  {object: "iam/bootstrap", commit: "commit-of-iam/bootstrap", run_id: "run-1", generation: "7"},
  {object: "iam/root", commit: "commit-of-iam/root", run_id: "run-1", generation: "7"}]' "$tmp/report.json" >/dev/null ||
  fail "a full run's report does not list each object's commit, run id and generation: $(cat "$tmp/report.json" 2>/dev/null)"
echo "PASS: a full run builds five contexts, fetches each object at its generation, runs the check with its flags and reports each object"

# --- A cluster whose context cannot be built. ---
run env MINTERS_AUDITOR_TEST_BAD_CLUSTER=example-cluster-c
[ "$status" -eq 2 ] || fail "a cluster without a context exits $status, want 2"
[[ "$output" == *"UNKNOWN entrypoint: cannot build the gcloud context for cluster example-project/example-region1/example-cluster-c"* ]] ||
  fail "a cluster without a context is not named"
[ ! -e "$tmp/args" ] || fail "the check ran though a cluster had no context"
echo "PASS: a cluster without a context exits 2 naming it, and the check does not run"

# --- An object without its commit. ---
run env MINTERS_AUDITOR_TEST_NO_COMMIT=secrets/broker
[ "$status" -eq 2 ] || fail "an object without its commit exits $status, want 2"
[[ "$output" == *"UNKNOWN entrypoint: render object secrets/broker has no metadata commit"* ]] || fail "the object without its commit is not named"
[ ! -e "$tmp/args" ] || fail "the check ran though an object had no commit"
echo "PASS: an object without its commit metadata exits 2 naming it, and the check does not run"

echo "all entrypoint rows passed"
