#!/usr/bin/env bash
# shellcheck disable=SC2016 # each single-quoted $name below is a jq variable, not the shell's
# packages/envoy/infra/scripts/operator-token-minters.test.sh
#
# Proves operator-token-minters.sh's verdicts without a cluster or a cloud. A plant on a real
# cluster or project needs RoleBinding and IAM writes, which no identity this check runs as holds,
# so every plant and every control of the check runs here, against a fake kubectl, gcloud and
# agent-secrets put first on PATH. The fakes answer from one world of files this test writes:
#
#   - kubectl answers a SubjectAccessReview as the RBAC authorizer does, from the world's grants
#     (each a subject, a verb, an API group, a resource and subresource, a namespace or none for
#     cluster-wide, and a resource name or none), with the built-in cluster-admin binding of the
#     group system:masters allowed everything; it lists ServiceAccounts and reads a namespace;
#   - gcloud describes roles from the world's role table (roles/container.admin's list is the
#     real role's, read with `gcloud iam roles describe roles/container.admin` on 2026-11-20, cut
#     to the permissions the check names and a few beside them), lists the project's clusters,
#     writes a kubeconfig for get-credentials, and answers Policy Analyzer as the API does: a
#     binding on the resource itself or on its project is seen from any scope, one on the
#     organization only from the organization's scope, a group is expanded only with
#     --expand-groups and --output-group-edges, and a role is given as its permissions only with
#     --expand-roles;
#   - agent-secrets shows a secret's tags as `secret show NAME --json` prints them, or answers as
#     Secret Manager's NotFound and PermissionDenied are printed.
#
# Each row resets the world (so each plant is removed after its row), plants one thing, and holds
# the run to its exit and to the line naming what it planted:
#
#   (a)  a world of people, system namespaces and the Workload Identity member alone exits 0, and a
#        project with the operator's cluster alone prints that it has no other;
#   (b)  each right of the RBAC list, allowed to one agent ServiceAccount alone, exits 1 naming that
#        ServiceAccount and that right, and nothing else;
#   (c)  a right allowed to the group system:serviceaccounts:legion exits 1 naming the group;
#   (d)  a container.* holder that is a CI account's or an agent's Google service account exits 1
#        naming it (the GKE IAM step's control);
#   (e)  each of the seven Google service account permissions held on the operator's account by an
#        agent's Google service account exits 1 naming the permission;
#   (f)  roles/iam.serviceAccountTokenCreator, roles/iam.serviceAccountKeyAdmin and
#        roles/iam.serviceAccountUser on the scratch account (--namespace envoy-operator-control
#        --gsa envoy-operator-control@...) each exit 1;
#   (g)  on a second cluster of the project, the namespace envoy-operator, the scratch namespace
#        envoy-operator-control, or a ServiceAccount outside the system namespaces allowed to make a
#        namespace each exit 1 naming that cluster; the system namespaces' own grants exit 0;
#   (h)  a permission name missing from roles/container.admin's list fails the run, naming it;
#   (i)  an empty listing exits 2 with UNKNOWN: agent namespaces holding no ServiceAccount, a Policy
#        Analyzer answer listing no one, an analysis not fully explored;
#   (j)  each live step's witness: the operator account's analysis without its Workload Identity
#        member, and an RBAC read whose cluster-admin binding does not answer, each read UNKNOWN;
#   (k)  with no identity that may run the live steps, each prints UNKNOWN naming the permission it
#        needs, and the run exits 2;
#   (l)  the admin tag: ENVOY_OPERATOR shown without "admin": "true", no such secret, and the scratch
#        secret ENVOY_OPERATOR_CONTROL at human tier without --admin each exit 1 naming the field;
#        a session that cannot show it reads UNKNOWN;
#   (m)  --report writes run_at, findings and unknown, with a stubbed UNKNOWN step listed under
#        unknown;
#   (n)  one holder of each hold-writer right (Cloud SQL users, the database Secret, the broker
#        program's state, the signing key) is a finding in the report;
#   (o)  the broker's one binding on the operator's account is allowed only when --broker-account
#        names it, and a minter of the broker's own account is a finding;
#   (p)  the code read: a grant the RBAC render or an IAM preview declares is a finding with every
#        live step refused; a binding to a role the render does not hold, a member a preview does
#        not know, an IAM resource type the check does not read, and a missing render or preview,
#        each read UNKNOWN;
#   (q)  a Policy Analyzer answer longer than a pipe holds (3000 people beside the Workload Identity
#        member) still finds its witness, and exits 0.
#
# Every name here is an example name: example-project, example.com, and the namespaces and
# accounts below. Run from anywhere: bash packages/envoy/infra/scripts/operator-token-minters.test.sh
# OPERATOR_MINTERS_TEST_ROWS="b p" runs those rows alone, as a control run by hand does, and
# OPERATOR_MINTERS_SCRIPT names another copy of the script to run them against. The rows of one
# plant per item ((b), (d), (e), (f)) run up to 8 at a time, each in a world of its own.
# CI runs it in envoy-go-checks (.github/workflows/ci-job-envoy-go-checks.yaml).
set -euo pipefail
shopt -s inherit_errexit

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly check="${OPERATOR_MINTERS_SCRIPT:-$script_dir/operator-token-minters.sh}"
temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/operator-token-minters-test.XXXXXX")"
readonly temporary_dir
trap 'rm -rf "$temporary_dir"' EXIT
readonly fake_bin="$temporary_dir/bin"
# world is the world the current row plants in; a row run beside others has one of its own.
world="$temporary_dir/world"
mkdir -p "$fake_bin"

readonly project=example-project
readonly number=123456789012
readonly org=100
readonly gsa=envoy-operator@example-project.iam.gserviceaccount.com
readonly control_gsa=envoy-operator-control@example-project.iam.gserviceaccount.com
readonly broker=secrets-broker@example-project.iam.gserviceaccount.com
readonly worker_gsa=worker@example-project.iam.gserviceaccount.com
readonly ci_gsa=ci-runner@example-project.iam.gserviceaccount.com
readonly wi_member="serviceAccount:example-project.svc.id.goog[envoy-operator/envoy-operator]"
readonly control_wi_member="serviceAccount:example-project.svc.id.goog[envoy-operator-control/envoy-operator]"
readonly broker_wi_member="serviceAccount:example-project.svc.id.goog[secrets-broker/secrets-broker]"
readonly project_res="//cloudresourcemanager.googleapis.com/projects/$project"
readonly org_res="//cloudresourcemanager.googleapis.com/organizations/$org"
readonly gsa_res="//iam.googleapis.com/projects/$project/serviceAccounts/$gsa"
readonly control_gsa_res="//iam.googleapis.com/projects/$project/serviceAccounts/$control_gsa"
readonly broker_res="//iam.googleapis.com/projects/$project/serviceAccounts/$broker"
readonly sql_res="//sqladmin.googleapis.com/projects/$project/instances/example-instance"
readonly signing_res="//secretmanager.googleapis.com/projects/$number/secrets/hold-signing-example"
readonly bucket_res="//storage.googleapis.com/projects/_/buckets/example-state"

# fake writes the fake command $1 from the body on stdin, after the header every fake shares: the
# world directory it reads, which each run names in OPERATOR_MINTERS_WORLD.
fake() {
  { printf '#!/usr/bin/env bash\nset -euo pipefail\nworld="${OPERATOR_MINTERS_WORLD:?the test names the world in OPERATOR_MINTERS_WORLD}"\n' && cat; } >"$fake_bin/$1"
  chmod +x "$fake_bin/$1"
}

# --- Fake gcloud. Every flag the check passes is --name=value or a bare --name, and the words
# before the first flag are the command. ---
fake gcloud <<'EOF'
read -r project <"$world/project"
read -r number <"$world/number"
words=()
declare -A flag=()
for arg in "$@"; do
  case "$arg" in
    --*=*) name="${arg%%=*}"; flag[${name#--}]="${arg#*=}" ;;
    --*) flag[${arg#--}]=true ;;
    *) words+=("$arg") ;;
  esac
done
refuse() { echo "ERROR: (gcloud.$1) PERMISSION_DENIED: $2" >&2; exit 1; }
case "${words[*]}" in
  "iam roles describe "*)
    role="${words[3]}"
    [ ! -e "$world/refuse-describe" ] || refuse iam.roles.describe "Permission 'iam.roles.get' denied on resource (or it may not exist)."
    if ! jq -e --arg r "$role" 'has($r)' "$world/roles.json" >/dev/null; then
      echo "ERROR: (gcloud.iam.roles.describe) NOT_FOUND: The role named $role was not found." >&2
      exit 1
    fi
    jq --arg r "$role" '{name: $r, title: $r, stage: "GA", etag: "AA==", includedPermissions: .[$r]}' "$world/roles.json"
    ;;
  "projects describe $project")
    jq -n --arg p "$project" --arg n "$number" \
      '{projectId: $p, projectNumber: $n, name: $p, lifecycleState: "ACTIVE", parent: {type: "organization", id: "100"}}'
    ;;
  "projects get-ancestors $project")
    jq -n --arg p "$project" '[{id: $p, type: "project"}, {id: "100", type: "organization"}]'
    ;;
  "container clusters list")
    [ "${flag[project]:-}" = "$project" ] || { echo "fake gcloud: clusters list of project ${flag[project]:-none}" >&2; exit 2; }
    [ ! -e "$world/refuse-clusters" ] || refuse container.clusters.list "Required \"container.clusters.list\" permission(s) for \"projects/$project\"."
    cat "$world/clusters.json"
    ;;
  "container clusters get-credentials "*)
    cluster="${words[3]}"
    jq -e --arg c "$cluster" --arg l "${flag[location]:-}" 'any(.[]; .name == $c and .location == $l)' "$world/clusters.json" >/dev/null ||
      { echo "ERROR: (gcloud.container.clusters.get-credentials) NOT_FOUND: cluster $cluster in ${flag[location]:-} not found" >&2; exit 1; }
    [ -n "${KUBECONFIG:-}" ] || { echo "fake gcloud: get-credentials with no KUBECONFIG would write the caller's own kubeconfig" >&2; exit 2; }
    printf 'cluster: %s\n' "$cluster" >"$KUBECONFIG"
    echo "Fetching cluster endpoint and auth data." >&2
    ;;
  "asset analyze-iam-policy")
    [ "${flag[show-response]:-}" = true ] || { echo "fake gcloud: without --show-response gcloud prints its own summary, which this fake does not render" >&2; exit 2; }
    scope=""
    for kind in organization folder project; do
      [ -z "${flag[$kind]:-}" ] || scope="$kind/${flag[$kind]}"
    done
    [ ! -e "$world/refuse-analyze" ] || refuse asset.analyze-iam-policy "Permission 'cloudasset.assets.analyzeIamPolicy' denied on resource '//cloudresourcemanager.googleapis.com/${scope/\//s/}' (or it may not exist)."
    jq -n --arg scope "$scope" --arg res "${flag[full-resource-name]:-}" --arg perms "${flag[permissions]:-}" \
      --arg project_res "//cloudresourcemanager.googleapis.com/projects/$project" \
      --arg org_res "//cloudresourcemanager.googleapis.com/organizations/100" \
      --arg expand_groups "${flag[expand-groups]:-}${flag[output-group-edges]:-}" --arg expand_roles "${flag[expand-roles]:-}" \
      --arg explored "$([ -e "$world/not-explored" ] && echo false || echo true)" \
      --arg empty "$([ -e "$world/empty-analysis" ] && echo true || echo false)" \
      --slurpfile iam "$world/iam.json" --slurpfile roles "$world/roles.json" --slurpfile groups "$world/groups.json" '
      ($perms | split(",")) as $asked
      | [ $iam[0][]
          | select(.attached == $res or .attached == $project_res or (.attached == $org_res and $scope == "organization/100"))
          | . as $b
          | ([($roles[0][$b.role] // [])[] | select(. as $p | any($asked[]; . == $p))]) as $held
          | select(($held | length) > 0 and $empty != "true")
          | ([$b.members[] | select(startswith("group:")) | . as $g | ($groups[0][$g] // [])[] | {sourceNode: $g, targetNode: .}]) as $edges
          | {
              attachedResourceFullName: $b.attached,
              iamBinding: {role: $b.role, members: $b.members},
              accessControlLists: [{
                resources: [{fullResourceName: $res}],
                accesses: (if $expand_roles == "true" then [$held[] | {permission: .}] else [{role: $b.role}] end)
              }],
              identityList: (if $expand_groups == "truetrue"
                then {identities: ([$b.members[], $edges[].targetNode] | unique | map({name: .})), groupEdges: $edges}
                else {identities: ($b.members | map({name: .}))} end),
              fullyExplored: true
            } ] as $results
      | {mainAnalysis: {analysisQuery: {scope: ($scope | sub("/"; "s/")), resourceSelector: {fullResourceName: $res}},
                        analysisResults: $results, fullyExplored: ($explored == "true")},
         fullyExplored: ($explored == "true")}'
    ;;
  *)
    echo "fake gcloud: unexpected call: gcloud $*" >&2
    exit 2
    ;;
esac
EOF

# --- Fake kubectl: the cluster is the one the kubeconfig get-credentials wrote names. ---
fake kubectl <<'EOF'
kubeconfig=""
args=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --kubeconfig) kubeconfig="$2"; shift 2 ;;
    *) args+=("$1"); shift ;;
  esac
done
[ -n "$kubeconfig" ] || { echo "fake kubectl: called without --kubeconfig, so it would reach the caller's own cluster" >&2; exit 2; }
read -r _ cluster <"$kubeconfig"
dir="$world/clusters/$cluster"
[ -d "$dir" ] || { echo "fake kubectl: no cluster $cluster" >&2; exit 2; }
forbidden() {
  echo "Error from server (Forbidden): $1 is forbidden: User \"reader@example-project.iam.gserviceaccount.com\" cannot $2 at the cluster scope: requires one of [\"$3\"] permission(s)." >&2
  exit 1
}
case "${args[*]}" in
  "create -f - -o json")
    [ ! -e "$dir/refuse-sar" ] || forbidden subjectaccessreviews.authorization.k8s.io 'create resource "subjectaccessreviews" in API group "authorization.k8s.io"' container.subjectAccessReviews.create
    jq --slurpfile grants "$dir/grants.json" --arg masters "$([ -e "$dir/no-masters" ] && echo off || echo on)" '
      def answer:
        .spec as $s | ($s.resourceAttributes // {}) as $a
        | ($s.user // "") as $user | ($s.groups // []) as $groups
        | [ $grants[0][]
            | select(
                (if .subject.kind == "ServiceAccount" then $user == "system:serviceaccount:\(.subject.namespace):\(.subject.name)"
                 elif .subject.kind == "Group" then (.subject.name as $name | any($groups[]; . == $name))
                 else $user == .subject.name end)
                and .verb == ($a.verb // "") and .group == ($a.group // "") and .resource == ($a.resource // "")
                and .subresource == ($a.subresource // "")
                and (.namespace == null or .namespace == ($a.namespace // ""))
                and (.name == null or .name == ($a.name // "")))
            | .binding ] as $by
        | (($masters == "on" and any($groups[]; . == "system:masters"))) as $master
        | . + {status: (if $master then {allowed: true, reason: "RBAC: allowed by ClusterRoleBinding \"cluster-admin\" of ClusterRole \"cluster-admin\" to Group \"system:masters\""}
                        elif ($by | length) > 0 then {allowed: true, reason: "RBAC: allowed by \($by[0])"}
                        else {allowed: false} end)};
      if .kind == "List" then .items |= map(answer) else answer end'
    ;;
  "get serviceaccounts --all-namespaces -o json")
    [ ! -e "$dir/refuse-list" ] || forbidden serviceaccounts 'list resource "serviceaccounts" in API group ""' container.serviceAccounts.list
    jq '{apiVersion: "v1", kind: "List", items: [.[] | {apiVersion: "v1", kind: "ServiceAccount", metadata: ({namespace: .namespace, name: .name, uid: "uid-\(.namespace)-\(.name)"} + (if .gsa then {annotations: {"iam.gke.io/gcp-service-account": .gsa}} else {} end))}]}' "$dir/serviceaccounts.json"
    ;;
  "get namespace "*" -o name")
    namespace="${args[2]}"
    if grep -qxF -- "$namespace" "$dir/namespaces"; then
      echo "namespace/$namespace"
    else
      echo "Error from server (NotFound): namespaces \"$namespace\" not found" >&2
      exit 1
    fi
    ;;
  *)
    echo "fake kubectl: unexpected call: kubectl ${args[*]}" >&2
    exit 2
    ;;
esac
EOF

# --- Fake agent-secrets: `secret show NAME --json`, as its failures print on stderr: a line naming
# the sign-in first, then the refusal (read from legion-envoy-v30.43.0 on 2026-11-21, which words a
# missing secret as gRPC's own `rpc error: code = NotFound` where the current source writes
# `GetSecret answered NotFound`; old-words picks the first wording). ---
fake agent-secrets <<'EOF'
[ "$#" -eq 4 ] && [ "$1 $2 $4" = "secret show --json" ] || { echo "fake agent-secrets: unexpected call: agent-secrets $*" >&2; exit 2; }
signed_in="agent-secrets secret show: using the active gcloud account (gcloud configuration example): reader@example-project.iam.gserviceaccount.com"
if [ -e "$world/refuse-secret-show" ]; then
  printf '%s\n' "$signed_in" "agent-secrets secret show: Secret Manager GetSecret answered PermissionDenied (missing permission: secretmanager.secrets.get; its message is not printed); looked in GCP Secret Manager project 123456789012" >&2
  exit 1
fi
if [ ! -e "$world/secrets/$3.json" ]; then
  if [ -e "$world/old-words" ]; then
    printf '%s\n' "$signed_in" "agent-secrets secret show: rpc error: code = NotFound desc = Secret [projects/123456789012/secrets/agent-secrets-$3] not found.; looked in GCP Secret Manager project 123456789012" >&2
  else
    printf '%s\n' "$signed_in" "agent-secrets secret show: Secret Manager GetSecret answered NotFound (its message is not printed); looked in GCP Secret Manager project 123456789012" >&2
  fi
  exit 1
fi
cat "$world/secrets/$3.json"
EOF

# --- The world. ---

# edit FILE JQ [ARGS...]: rewrites a world JSON file through a jq program. The program and the file
# come first: after --args, jq takes every word as a positional string.
edit() {
  local file="$1" program="$2"
  shift 2
  jq "$program" "$file" "$@" >"$file.next"
  mv "$file.next" "$file"
}
# grant CLUSTER JSON: one RBAC grant on that cluster.
grant() { edit "$world/clusters/$1/grants.json" '. + [$g]' --argjson g "$2"; }
# sa_grant CLUSTER NAMESPACE NAME VERB GROUP RESOURCE SUBRESOURCE GRANT_NAMESPACE RESOURCE_NAME:
# one grant to a ServiceAccount; an empty GRANT_NAMESPACE is a ClusterRoleBinding's, and an empty
# RESOURCE_NAME names no resource.
subject_grant() {
  local cluster="$1" subject="$2" verb="$3" group="$4" resource="$5" sub="$6" namespace="$7" name="$8"
  grant "$cluster" "$(jq -n --argjson s "$subject" --arg v "$verb" --arg g "$group" --arg r "$resource" --arg sub "$sub" --arg ns "$namespace" --arg n "$name" \
    '{subject: $s, verb: $v, group: $g, resource: $r, subresource: $sub,
      namespace: (if $ns == "" then null else $ns end), name: (if $n == "" then null else $n end),
      binding: "a planted binding"}')"
}
sa_subject() { jq -nc --arg ns "$1" --arg n "$2" '{kind: "ServiceAccount", namespace: $ns, name: $n}'; }
group_subject() { jq -nc --arg n "$1" '{kind: "Group", name: $n}'; }
# iam ATTACHED ROLE MEMBER...: one IAM binding.
iam() {
  local attached="$1" role="$2"
  shift 2
  edit "$world/iam.json" '. + [{attached: $a, role: $r, members: $ARGS.positional}]' --arg a "$attached" --arg r "$role" --args "$@"
}
# unbind ATTACHED ROLE: removes the world's bindings of that role on that resource.
unbind() { edit "$world/iam.json" 'map(select((.attached == $a and .role == $r) | not))' --arg a "$1" --arg r "$2"; }
role() { edit "$world/roles.json" '. + {($r): $ARGS.positional}' --arg r "$1" --args "${@:2}"; }
# add_cluster NAME LOCATION: a second cluster, whose system namespaces' controllers may make
# namespaces and ServiceAccounts, as a cluster's own controllers may.
add_cluster() {
  edit "$world/clusters.json" '. + [{name: $n, location: $l, status: "RUNNING"}]' --arg n "$1" --arg l "$2"
  mkdir -p "$world/clusters/$1"
  printf '%s\n' default kube-system gke-managed-system gmp-system apps >"$world/clusters/$1/namespaces"
  jq -n '[{namespace: "kube-system", name: "namespace-controller"}, {namespace: "gke-managed-system", name: "addon-manager"},
          {namespace: "gmp-system", name: "collector"}, {namespace: "default", name: "default"}, {namespace: "apps", name: "deployer"}]' \
    >"$world/clusters/$1/serviceaccounts.json"
  echo '[]' >"$world/clusters/$1/grants.json"
  subject_grant "$1" "$(sa_subject kube-system namespace-controller)" create "" namespaces "" "" ""
  subject_grant "$1" "$(sa_subject gke-managed-system addon-manager)" create "" serviceaccounts "" "" ""
  subject_grant "$1" "$(sa_subject gmp-system collector)" create "" namespaces "" "" ""
}

# The checked permissions of roles/container.admin, as `gcloud iam roles describe` lists them, with
# a few beside them it also holds.
readonly container_admin=(
  container.clusterRoleBindings.create container.clusterRoleBindings.get container.clusterRoleBindings.update
  container.clusterRoles.bind container.clusterRoles.escalate container.clusters.get container.clusters.impersonate
  container.cronJobs.create container.cronJobs.update container.daemonSets.create container.daemonSets.update
  container.deployments.create container.deployments.update container.jobs.create container.jobs.update
  container.namespaces.create container.pods.attach container.pods.create container.pods.exec container.pods.get
  container.pods.portForward container.pods.update container.replicaSets.create container.replicaSets.update
  container.roleBindings.create container.roleBindings.update container.roles.bind container.roles.escalate
  container.secrets.get container.secrets.list container.serviceAccounts.create container.serviceAccounts.createToken
  container.statefulSets.create container.statefulSets.update container.subjectAccessReviews.create
  resourcemanager.projects.get
)

build_world() {
  mkdir -p "$world/clusters/operator-cluster" "$world/secrets"
  printf '%s\n' "$project" >"$world/project"
  printf '%s\n' "$number" >"$world/number"
  jq -n '[{name: "operator-cluster", location: "us-central1", status: "RUNNING"}]' >"$world/clusters.json"
  printf '%s\n' default kube-system legion agent-sessions legion-abc123-sessions envoy-operator secrets-broker example-db \
    >"$world/clusters/operator-cluster/namespaces"
  jq -n --arg worker "$worker_gsa" --arg gsa "$gsa" --arg broker "$broker" '[
    {namespace: "legion", name: "default"}, {namespace: "legion", name: "worker", gsa: $worker},
    {namespace: "agent-sessions", name: "agent-session"}, {namespace: "legion-abc123-sessions", name: "default"},
    {namespace: "kube-system", name: "generic-garbage-collector"}, {namespace: "envoy-operator", name: "envoy-operator", gsa: $gsa},
    {namespace: "secrets-broker", name: "secrets-broker", gsa: $broker}]' >"$world/clusters/operator-cluster/serviceaccounts.json"
  echo '[]' >"$world/clusters/operator-cluster/grants.json"
  # The operator's own Role, bound to its Google service account alone.
  local user
  user="$(jq -nc --arg n "$gsa" '{kind: "User", name: $n}')"
  subject_grant operator-cluster "$user" create "" serviceaccounts token envoy-operator envoy-operator
  subject_grant operator-cluster "$user" create "" pods "" envoy-operator ""
  subject_grant operator-cluster "$user" create batch jobs "" envoy-operator ""

  local owner_perms=("${container_admin[@]}" iam.serviceAccounts.getAccessToken iam.serviceAccounts.getOpenIdToken
    iam.serviceAccounts.signJwt iam.serviceAccounts.signBlob iam.serviceAccounts.actAs iam.serviceAccounts.implicitDelegation
    iam.serviceAccountKeys.create cloudsql.users.create cloudsql.users.update secretmanager.versions.access storage.objects.get)
  echo '{}' >"$world/roles.json"
  role roles/container.admin "${container_admin[@]}"
  role roles/owner "${owner_perms[@]}"
  role roles/iam.serviceAccountTokenCreator iam.serviceAccounts.get iam.serviceAccounts.getAccessToken iam.serviceAccounts.getOpenIdToken \
    iam.serviceAccounts.implicitDelegation iam.serviceAccounts.list iam.serviceAccounts.signBlob iam.serviceAccounts.signJwt \
    resourcemanager.projects.get resourcemanager.projects.list
  role roles/iam.serviceAccountKeyAdmin iam.serviceAccountKeys.create iam.serviceAccountKeys.delete iam.serviceAccountKeys.disable \
    iam.serviceAccountKeys.enable iam.serviceAccountKeys.get iam.serviceAccountKeys.list iam.serviceAccounts.get iam.serviceAccounts.list \
    resourcemanager.projects.get resourcemanager.projects.list
  role roles/iam.serviceAccountUser iam.serviceAccounts.actAs iam.serviceAccounts.get iam.serviceAccounts.list resourcemanager.projects.get \
    resourcemanager.projects.list
  role roles/iam.workloadIdentityUser iam.serviceAccounts.get iam.serviceAccounts.getAccessToken iam.serviceAccounts.getOpenIdToken \
    iam.serviceAccounts.list
  role roles/cloudsql.admin cloudsql.instances.get cloudsql.users.create cloudsql.users.delete cloudsql.users.get cloudsql.users.list \
    cloudsql.users.update resourcemanager.projects.get
  role roles/secretmanager.secretAccessor resourcemanager.projects.get resourcemanager.projects.list secretmanager.versions.access
  role roles/storage.objectViewer resourcemanager.projects.get resourcemanager.projects.list storage.objects.get storage.objects.list
  role roles/container.serviceAgent container.pods.create container.pods.update container.serviceAccounts.createToken container.secrets.get
  role roles/viewer container.pods.get resourcemanager.projects.get storage.objects.list

  echo '[]' >"$world/iam.json"
  jq -n '{"group:cluster-admins@example.com": ["user:admin@example.com"]}' >"$world/groups.json"
  iam "$org_res" roles/owner user:owner@example.com
  iam "$project_res" roles/container.admin group:cluster-admins@example.com
  iam "$project_res" roles/container.serviceAgent "serviceAccount:service-$number@container-engine-robot.iam.gserviceaccount.com"
  iam "$project_res" roles/cloudsql.admin user:dba@example.com
  iam "$project_res" roles/viewer "serviceAccount:$ci_gsa"
  iam "$gsa_res" roles/iam.workloadIdentityUser "$wi_member"
  iam "$gsa_res" roles/iam.serviceAccountTokenCreator "serviceAccount:$broker"
  iam "$control_gsa_res" roles/iam.workloadIdentityUser "$control_wi_member"
  iam "$broker_res" roles/iam.workloadIdentityUser "$broker_wi_member"
  iam "$signing_res" roles/secretmanager.secretAccessor "serviceAccount:$gsa"
  iam "$bucket_res" roles/storage.objectViewer user:admin@example.com

  local name admin
  for name in ENVOY_OPERATOR ENVOY_OPERATOR_CONTROL; do
    admin=""
    [ "$name" = ENVOY_OPERATOR_CONTROL ] || admin=true
    jq -n --arg name "$name" --arg admin "$admin" --arg slug "agent-secrets-$(printf '%s' "$name" | tr 'A-Z_' 'a-z-')" '{
      name: $name, store: "GCP Secret Manager project 123456789012", secret_name: "projects/123456789012/secrets/\($slug)",
      owner: "owner@example.com", tier: "human", type: "google-service-account", github_owners: "",
      requesters: "owner@example.com", scopes: "", admin: $admin, max_grant: "1h", default_grant: "", has_value: true,
      created: "2026-11-17T12:00:00Z", last_changed: "2026-11-17T12:00:00Z", versions: {"1": ["ENABLED"]}}' >"$world/secrets/$name.json"
  done

  # The code: the RBAC render and the IAM program's preview, each declaring only what (a) allows.
  jq -n --arg gsa "$gsa" '{apiVersion: "v1", kind: "List", items: [
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "Role", metadata: {name: "envoy-operator-runtime", namespace: "envoy-operator"},
     rules: [{apiGroups: [""], resources: ["serviceaccounts/token"], resourceNames: ["envoy-operator"], verbs: ["create"]},
             {apiGroups: [""], resources: ["pods"], verbs: ["create"]}, {apiGroups: ["batch"], resources: ["jobs"], verbs: ["create"]}]},
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", metadata: {name: "envoy-operator-runtime", namespace: "envoy-operator"},
     roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "Role", name: "envoy-operator-runtime"},
     subjects: [{apiGroup: "rbac.authorization.k8s.io", kind: "User", name: $gsa}]},
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", metadata: {name: "everyone-views"},
     roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "view"},
     subjects: [{apiGroup: "rbac.authorization.k8s.io", kind: "Group", name: "system:authenticated"}]},
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "Role", metadata: {name: "sessions", namespace: "legion"},
     rules: [{apiGroups: [""], resources: ["pods", "pods/exec"], verbs: ["create", "delete"]}]},
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", metadata: {name: "sessions", namespace: "legion"},
     roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "Role", name: "sessions"},
     subjects: [{kind: "ServiceAccount", name: "worker", namespace: "legion"}]}]}' >"$world/render.json"
  jq -n --arg p "$project" --arg gsa "$gsa" --arg broker "$broker" --arg wi "$wi_member" --arg ci "$ci_gsa" '
    def step($name; $type; $inputs): {op: "same", urn: "urn:pulumi:example::iam::\($type)::\($name)", provider: "example-provider",
                                      newState: {urn: "urn:pulumi:example::iam::\($type)::\($name)", custom: true, type: $type, inputs: $inputs}};
    {steps: [
      step("admin-container"; "gcp:projects/iAMMember:IAMMember"; {project: $p, role: "roles/container.admin", member: "user:admin@example.com"}),
      step("admins-container"; "gcp:projects/iAMMember:IAMMember"; {project: $p, role: "roles/container.admin", member: "group:cluster-admins@example.com"}),
      step("operator-workload"; "gcp:serviceaccount/iAMMember:IAMMember";
           {serviceAccountId: "projects/\($p)/serviceAccounts/\($gsa)", role: "roles/iam.workloadIdentityUser", member: $wi}),
      step("broker-mints-operator"; "gcp:serviceaccount/iAMMember:IAMMember";
           {serviceAccountId: "projects/\($p)/serviceAccounts/\($gsa)", role: "roles/iam.serviceAccountTokenCreator", member: "serviceAccount:\($broker)"}),
      step("operator-reads-signing-key"; "gcp:secretmanager/secretIamMember:SecretIamMember";
           {project: $p, secretId: "projects/\($p)/secrets/hold-signing-example", role: "roles/secretmanager.secretAccessor", member: "serviceAccount:\($gsa)"}),
      step("ci-uses-a-key"; "gcp:kms/cryptoKeyIAMMember:CryptoKeyIAMMember";
           {cryptoKeyId: "projects/\($p)/locations/global/keyRings/example/cryptoKeys/example", role: "roles/cloudkms.cryptoKeyEncrypterDecrypter", member: "serviceAccount:\($ci)"}),
      {op: "delete", urn: "urn:pulumi:example::iam::gcp:projects/iAMMember:IAMMember::gone", provider: "example-provider",
       oldState: {type: "gcp:projects/iAMMember:IAMMember", inputs: {project: $p, role: "roles/owner", member: "serviceAccount:\($ci)"}}}
    ]}' >"$world/preview.json"
}
# The world (a) runs against, built once; base_world gives each row a fresh copy of it.
readonly base="$temporary_dir/base"
build_world
mv "$world" "$base"
base_world() {
  rm -rf "$world"
  cp -a "$base" "$world"
}

# --- Running the check. ---

# The flags every run passes, naming every target, so that a run of (a)'s world can exit 0.
full_args() {
  printf '%s\n' --project "$project" --cluster operator-cluster --gsa "$gsa" \
    --agent-namespace legion --agent-namespace agent-sessions --agent-namespace 'legion-*-sessions' \
    --broker-account "$broker" --broker-workload secrets-broker/secrets-broker \
    --sql-instance example-instance --db-secret example-db/broker-db-login --broker-state gs://example-state/broker \
    --signing-secret hold-signing-example \
    --rbac-render "$world/render.json" --iam-preview "$world/preview.json"
}
output=""
status=0
# run ARGS...: the check with the full flags, then ARGS (a later flag of the same name wins for a
# single-value flag), against the current world; sets output and status.
run() {
  local args
  mapfile -t args < <(full_args)
  run_only "${args[@]}" "$@"
}
# args_without FLAG...: the full flags less each named flag and its value, one a line.
args_without() {
  local args drop i keep=()
  mapfile -t args < <(full_args)
  for ((i = 0; i < ${#args[@]}; i++)); do
    for drop in "$@"; do
      if [ "${args[$i]}" = "$drop" ]; then
        i=$((i + 1))
        continue 2
      fi
    done
    keep+=("${args[$i]}")
  done
  printf '%s\n' "${keep[@]}"
}
# run_only ARGS...: the check with ARGS alone.
run_only() {
  status=0
  output="$(env PATH="$fake_bin:$PATH" OPERATOR_MINTERS_WORLD="$world" TMPDIR="$temporary_dir" bash "$check" "$@" 2>&1)" || status=$?
}
fail() {
  echo "FAIL: $*" >&2
  printf 'the check printed (exit %s):\n%s\n' "$status" "$output" >&2
  exit 1
}
# Each reads the output whole from a here-string, never a pipe into grep -q: under pipefail grep
# stops at its first match, and a long output's next write then fails the pipeline with SIGPIPE,
# reading a line that is there as one that is not (a row failed so under load on 2026-11-21).
has_line() { grep -qxF -- "$1" <<<"$output"; }
has_prefix() { grep -qF -- "$1" <<<"$output"; }
count_prefix() { grep -c "^$1" <<<"$output" || true; }
want_status() { [ "$status" -eq "$1" ] || fail "$2: exit $status, want $1"; }
want_line() { has_line "$1" || fail "$2: no line '$1'"; }
want_text() { has_prefix "$1" || fail "$2: no line holding '$1'"; }
rows=0
passed() {
  rows=$((rows + 1))
  echo "PASS: $1"
}
# row LETTER: whether a row runs: every row, or those OPERATOR_MINTERS_TEST_ROWS names.
row() { [ -z "${OPERATOR_MINTERS_TEST_ROWS:-}" ] || [[ " $OPERATOR_MINTERS_TEST_ROWS " == *" $1 "* ]]; }
# each FUNCTION ITEM...: FUNCTION ITEM for each item, each in a subshell with a world of its own,
# up to 8 at a time; fails naming each item whose run failed, with what it printed.
each() {
  local fn="$1" i=0 running=0 failed=0
  shift
  for item in "$@"; do
    i=$((i + 1))
    {
      set +e
      (
        set -e
        world="$temporary_dir/world-$fn-$i" "$fn" "$item"
      ) >"$temporary_dir/$fn-$i.out" 2>&1
      echo "$?" >"$temporary_dir/$fn-$i.status"
    } &
    running=$((running + 1))
    if [ "$running" -ge 8 ]; then
      wait -n
      running=$((running - 1))
    fi
  done
  wait
  for ((i = 1; i <= $#; i++)); do
    if [ "$(cat "$temporary_dir/$fn-$i.status")" != 0 ]; then
      cat "$temporary_dir/$fn-$i.out" >&2
      failed=$((failed + 1))
    fi
  done
  [ "$failed" -eq 0 ] || { echo "FAIL: $failed of the $# items of $fn failed" >&2; exit 1; }
}

# --- (a) People, the system namespaces and the Workload Identity member alone. ---
if row a; then
  base_world
  run
  want_status 0 "a clean world"
  [ "$(count_prefix 'FINDING ')" -eq 0 ] && [ "$(count_prefix 'UNKNOWN ')" -eq 0 ] || fail "a clean world printed a FINDING or an UNKNOWN"
  want_line "PASS live: Workload Identity: the project runs the operator's cluster operator-cluster alone" "a clean world"
  for step in "permission names" "code: RBAC" "code: IAM" "live: RBAC on operator-cluster" "live: GKE IAM on project $project" \
    "live: minters of $gsa" "live: minters of the broker account $broker" "live: hold writers: Cloud SQL users of example-instance" \
    "live: hold writers: Secret example-db/broker-db-login" "live: hold writers: the broker program's state in gs://example-state" \
    "live: hold writers: the signing key hold-signing-example" "admin tag of ENVOY_OPERATOR"; do
    want_text "PASS $step: " "a clean world's $step"
  done
  passed "(a) people, the system namespaces and the Workload Identity member alone exit 0, and a project with the operator's cluster alone says so"
fi

# --- (b) Each right of the RBAC list, allowed to one agent ServiceAccount. Each entry is the grant
# (verb, API group, resource, subresource, the binding's namespace or none for cluster-wide, the
# resource name or none) and the right the run must name. ---
rbac_rights=(
  "create||serviceaccounts|token|envoy-operator|envoy-operator=create serviceaccounts/token envoy-operator in envoy-operator"
  "impersonate||serviceaccounts||envoy-operator|envoy-operator=impersonate serviceaccounts envoy-operator in envoy-operator"
  "impersonate||users||||=impersonate users cluster-wide"
  "impersonate||groups||||=impersonate groups cluster-wide"
  "escalate|rbac.authorization.k8s.io|roles||envoy-operator|=escalate roles.rbac.authorization.k8s.io in envoy-operator"
  "bind|rbac.authorization.k8s.io|roles||envoy-operator|=bind roles.rbac.authorization.k8s.io in envoy-operator"
  "escalate|rbac.authorization.k8s.io|clusterroles||||=escalate clusterroles.rbac.authorization.k8s.io cluster-wide"
  "bind|rbac.authorization.k8s.io|clusterroles||envoy-operator|=bind clusterroles.rbac.authorization.k8s.io in envoy-operator"
  "create|rbac.authorization.k8s.io|rolebindings||envoy-operator|=create rolebindings.rbac.authorization.k8s.io in envoy-operator"
  "patch|rbac.authorization.k8s.io|rolebindings||envoy-operator|=patch rolebindings.rbac.authorization.k8s.io in envoy-operator"
  "create|rbac.authorization.k8s.io|clusterrolebindings||||=create clusterrolebindings.rbac.authorization.k8s.io cluster-wide"
  "patch|rbac.authorization.k8s.io|clusterrolebindings||||=patch clusterrolebindings.rbac.authorization.k8s.io cluster-wide"
  "create||pods|exec|envoy-operator|=create pods/exec in envoy-operator"
  "create||pods|attach|envoy-operator|=create pods/attach in envoy-operator"
  "create||pods|portforward|envoy-operator|=create pods/portforward in envoy-operator"
  "get||pods|exec|envoy-operator|=get pods/exec in envoy-operator"
  "get||pods|attach|envoy-operator|=get pods/attach in envoy-operator"
  "get||pods|portforward|envoy-operator|=get pods/portforward in envoy-operator"
  "create||pods||envoy-operator|=create pods in envoy-operator"
  "patch||pods||envoy-operator|=patch pods in envoy-operator"
  "update||pods||envoy-operator|=update pods in envoy-operator"
  "patch||pods|ephemeralcontainers|envoy-operator|=patch pods/ephemeralcontainers in envoy-operator"
  "update||pods|ephemeralcontainers|envoy-operator|=update pods/ephemeralcontainers in envoy-operator"
  "create|apps|deployments||envoy-operator|=create deployments.apps in envoy-operator"
  "patch|apps|deployments||envoy-operator|=patch deployments.apps in envoy-operator"
  "update|apps|deployments||envoy-operator|=update deployments.apps in envoy-operator"
  "create|apps|replicasets||envoy-operator|=create replicasets.apps in envoy-operator"
  "patch|apps|replicasets||envoy-operator|=patch replicasets.apps in envoy-operator"
  "update|apps|replicasets||envoy-operator|=update replicasets.apps in envoy-operator"
  "create|apps|statefulsets||envoy-operator|=create statefulsets.apps in envoy-operator"
  "patch|apps|statefulsets||envoy-operator|=patch statefulsets.apps in envoy-operator"
  "update|apps|statefulsets||envoy-operator|=update statefulsets.apps in envoy-operator"
  "create|apps|daemonsets||envoy-operator|=create daemonsets.apps in envoy-operator"
  "patch|apps|daemonsets||envoy-operator|=patch daemonsets.apps in envoy-operator"
  "update|apps|daemonsets||envoy-operator|=update daemonsets.apps in envoy-operator"
  "create|batch|jobs||envoy-operator|=create jobs.batch in envoy-operator"
  "patch|batch|jobs||envoy-operator|=patch jobs.batch in envoy-operator"
  "update|batch|jobs||envoy-operator|=update jobs.batch in envoy-operator"
  "create|batch|cronjobs||envoy-operator|=create cronjobs.batch in envoy-operator"
  "patch|batch|cronjobs||envoy-operator|=patch cronjobs.batch in envoy-operator"
  "update|batch|cronjobs||envoy-operator|=update cronjobs.batch in envoy-operator"
  "get||secrets||envoy-operator|=get secrets in envoy-operator"
  "list||secrets||envoy-operator|=list secrets in envoy-operator"
)
rbac_right_row() {
  local right="${1#*=}" verb group resource sub namespace name
  IFS='|' read -r verb group resource sub namespace name <<<"${1%%=*}"
  base_world
  subject_grant operator-cluster "$(sa_subject legion worker)" "$verb" "$group" "$resource" "$sub" "$namespace" "$name"
  run
  want_status 1 "$right allowed to legion/worker"
  want_line "FINDING system:serviceaccount:legion:worker: $right" "$right allowed to legion/worker"
  [ "$(count_prefix 'FINDING ')" -eq 1 ] || fail "$right allowed to legion/worker: want that one finding"
}
if row b; then
  each rbac_right_row "${rbac_rights[@]}"
  passed "(b) each of the ${#rbac_rights[@]} rights of the RBAC list, allowed to one agent ServiceAccount alone, exits 1 naming it and the right"
fi

# --- (c) A right allowed to the group of every ServiceAccount of namespace legion. ---
if row c; then
  base_world
  subject_grant operator-cluster "$(group_subject system:serviceaccounts:legion)" create "" serviceaccounts token envoy-operator envoy-operator
  run
  want_status 1 "a grant to the group system:serviceaccounts:legion"
  want_line "FINDING group:system:serviceaccounts:legion: create serviceaccounts/token envoy-operator in envoy-operator" "a grant to the group"
  want_line "FINDING system:serviceaccount:legion:default: create serviceaccounts/token envoy-operator in envoy-operator" "a grant to the group"
  passed "(c) a right allowed to the group system:serviceaccounts:legion exits 1 naming the group and each of its ServiceAccounts"
fi

# --- (d) A project-level container.admin held by a CI account, and by an agent's Google service
# account (the GKE IAM step's control). ---
container_admin_row() {
  base_world
  iam "$project_res" roles/container.admin "serviceAccount:$1"
  run
  want_status 1 "roles/container.admin held by $1"
  want_text "FINDING serviceAccount:$1: container.serviceAccounts.createToken," "roles/container.admin held by $1"
  want_text " on $project_res through roles/container.admin, granted at $project_res" "roles/container.admin held by $1"
}
if row d; then
  each container_admin_row "$ci_gsa" "$worker_gsa"
  passed "(d) roles/container.admin held by a CI account's or an agent's Google service account exits 1 naming it"
fi

# --- (e) Each of the seven Google service account permissions, held on the operator's account. ---
gsa_permissions=(iam.serviceAccounts.getAccessToken iam.serviceAccounts.getOpenIdToken iam.serviceAccounts.signJwt
  iam.serviceAccounts.signBlob iam.serviceAccounts.actAs iam.serviceAccounts.implicitDelegation iam.serviceAccountKeys.create)
gsa_permission_row() {
  base_world
  role "projects/$project/roles/only.$1" "$1"
  iam "$gsa_res" "projects/$project/roles/only.$1" "serviceAccount:$worker_gsa"
  run
  want_status 1 "$1 held by $worker_gsa"
  want_line "FINDING serviceAccount:$worker_gsa: $1 on $gsa_res through projects/$project/roles/only.$1, granted at $gsa_res" \
    "$1 held by $worker_gsa"
}
if row e; then
  each gsa_permission_row "${gsa_permissions[@]}"
  passed "(e) each of the seven Google service account permissions held on the operator's account exits 1 naming it"
fi

# --- (f) The plan's controls on the scratch account. ---
scratch_role_row() {
  base_world
  iam "$control_gsa_res" "$1" "serviceAccount:$worker_gsa"
  run --namespace envoy-operator-control --gsa "$control_gsa"
  want_status 1 "$1 on the scratch account"
  want_text "FINDING serviceAccount:$worker_gsa: " "$1 on the scratch account"
  want_text " on $control_gsa_res through $1, granted at $control_gsa_res" "$1 on the scratch account"
}
if row f; then
  each scratch_role_row roles/iam.serviceAccountTokenCreator roles/iam.serviceAccountKeyAdmin roles/iam.serviceAccountUser
  passed "(f) roles/iam.serviceAccountTokenCreator, roles/iam.serviceAccountKeyAdmin and roles/iam.serviceAccountUser on the scratch account each exit 1"
fi

# --- (g) A second cluster of the project. ---
if row g; then
  base_world
  add_cluster other-cluster us-east1
  run
  want_status 0 "a second cluster whose system namespaces alone may make namespaces"
  want_text "PASS live: Workload Identity on other-cluster: " "a clean second cluster"
  passed "(g) a second cluster whose system namespaces alone may make namespaces and ServiceAccounts exits 0"

  base_world
  add_cluster other-cluster us-east1
  echo envoy-operator >>"$world/clusters/other-cluster/namespaces"
  run
  want_status 1 "the namespace envoy-operator on a second cluster"
  want_text "FINDING cluster:other-cluster: namespace envoy-operator exists there, so its ServiceAccount envoy-operator/envoy-operator acts as $gsa" \
    "the namespace envoy-operator on a second cluster"
  passed "(g) the namespace envoy-operator on a second cluster exits 1 naming the cluster"

  base_world
  add_cluster other-cluster us-east1
  echo envoy-operator-control >>"$world/clusters/other-cluster/namespaces"
  run --namespace envoy-operator-control --gsa "$control_gsa"
  want_status 1 "the scratch namespace on a second cluster"
  want_text "FINDING cluster:other-cluster: namespace envoy-operator-control exists there" "the scratch namespace on a second cluster"
  passed "(g) the scratch namespace envoy-operator-control made on a second cluster exits 1 naming the cluster (the plan's control)"

  base_world
  add_cluster other-cluster us-east1
  subject_grant other-cluster "$(sa_subject apps deployer)" create "" namespaces "" "" ""
  run
  want_status 1 "a ServiceAccount outside the system namespaces that may make a namespace"
  want_line "FINDING system:serviceaccount:apps:deployer: create namespaces cluster-wide on cluster other-cluster" "apps/deployer making namespaces"
  passed "(g) a ServiceAccount outside the system namespaces of a second cluster that may make a namespace exits 1 naming the cluster"
fi

# --- (h) A permission name roles/container.admin's list does not hold. ---
if row h; then
  base_world
  edit "$world/roles.json" '.["roles/container.admin"] -= ["container.pods.exec"]'
  run
  want_status 2 "roles/container.admin without container.pods.exec"
  want_line "UNKNOWN permission names: container.pods.exec is not in roles/container.admin's includedPermissions, so no check that names it can find a holder" \
    "roles/container.admin without container.pods.exec"
  passed "(h) a permission name missing from roles/container.admin's list fails the run, naming it"
fi

# --- (i) Empty listings. ---
if row i; then
  base_world
  mapfile -t args < <(args_without --agent-namespace)
  run_only "${args[@]}" --agent-namespace 'nobody-*'
  want_status 2 "agent namespaces holding no ServiceAccount"
  want_text "UNKNOWN live: RBAC on operator-cluster: the cluster lists no ServiceAccount in the agent namespaces" "agent namespaces holding no ServiceAccount"
  passed "(i) agent namespaces holding no ServiceAccount exit 2 with UNKNOWN"

  base_world
  : >"$world/empty-analysis"
  run
  want_status 2 "a Policy Analyzer that lists no one"
  want_text "UNKNOWN live: GKE IAM on project $project: the analysis lists no holder" "a Policy Analyzer that lists no one"
  passed "(i) a Policy Analyzer answer that lists no one exits 2 with UNKNOWN"

  base_world
  : >"$world/not-explored"
  run
  want_status 2 "an analysis not fully explored"
  want_text "UNKNOWN live: GKE IAM on project $project: the analysis was not fully explored" "an analysis not fully explored"
  passed "(i) an analysis not fully explored exits 2 with UNKNOWN"
fi

# --- (j) Each live step's positive witness. ---
if row j; then
  base_world
  unbind "$gsa_res" roles/iam.workloadIdentityUser
  run
  want_status 2 "the operator's account without its Workload Identity member"
  want_text "UNKNOWN live: minters of $gsa: the analysis does not list $wi_member" "no Workload Identity member"
  passed "(j) the operator account's analysis without its Workload Identity member reads UNKNOWN"

  base_world
  : >"$world/clusters/operator-cluster/no-masters"
  run
  want_status 2 "an RBAC read whose cluster-admin binding does not answer"
  want_text "UNKNOWN live: RBAC on operator-cluster: the witness review, the cluster-admin binding's group system:masters, was not allowed" "no witness"
  passed "(j) an RBAC read whose cluster-admin binding does not answer reads UNKNOWN"
fi

# --- (k) No identity that may run the live steps. ---
if row k; then
  base_world
  : >"$world/clusters/operator-cluster/refuse-sar"
  : >"$world/refuse-analyze"
  run
  want_status 2 "no identity that may run the live steps"
  want_text "UNKNOWN live: RBAC on operator-cluster: the API server refused the review (it needs create on subjectaccessreviews, container.subjectAccessReviews.create on GKE)" \
    "a refused review"
  for step in "live: GKE IAM on project $project" "live: minters of $gsa" "live: hold writers: Cloud SQL users of example-instance"; do
    want_text "UNKNOWN $step: Policy Analyzer refused the analysis (it needs cloudasset.assets.analyzeIamPolicy on organizations/$org)" "a refused analysis"
  done
  [ "$(count_prefix 'FINDING ')" -eq 0 ] || fail "no identity that may run the live steps: a finding"
  passed "(k) with no identity that may run them, the live steps print UNKNOWN naming the permission, and the run exits 2"
fi

# --- (l) The admin tag. ---
if row l; then
  base_world
  edit "$world/secrets/ENVOY_OPERATOR.json" '.admin = ""'
  run
  want_status 1 "ENVOY_OPERATOR without the admin tag"
  want_line "FINDING secret:ENVOY_OPERATOR: admin is \"\", not \"true\": without the administrator tag a model judge may decide its release" "no admin tag"
  passed "(l) ENVOY_OPERATOR shown without \"admin\": \"true\" exits 1 naming admin"

  base_world
  edit "$world/secrets/ENVOY_OPERATOR.json" '.tier = "agent"'
  run
  want_status 1 "ENVOY_OPERATOR at agent tier"
  want_line "FINDING secret:ENVOY_OPERATOR: tier is \"agent\", not \"human\"" "agent tier"
  passed "(l) ENVOY_OPERATOR at agent tier exits 1 naming tier"

  base_world
  rm "$world/secrets/ENVOY_OPERATOR.json"
  run
  want_status 1 "no ENVOY_OPERATOR"
  want_text "FINDING secret:ENVOY_OPERATOR: no such secret" "no such secret"
  passed "(l) no secret ENVOY_OPERATOR exits 1"

  base_world
  rm "$world/secrets/ENVOY_OPERATOR.json"
  : >"$world/old-words"
  run
  want_status 1 "no ENVOY_OPERATOR, in the released agent-secrets' words"
  want_text "FINDING secret:ENVOY_OPERATOR: no such secret" "no such secret, in the released agent-secrets' words"
  passed "(l) no secret ENVOY_OPERATOR exits 1 in gRPC's words too, as legion-envoy-v30.43.0 prints them"

  base_world
  run --secret ENVOY_OPERATOR_CONTROL
  want_status 1 "the scratch secret at human tier without --admin"
  want_line "FINDING secret:ENVOY_OPERATOR_CONTROL: admin is \"\", not \"true\": without the administrator tag a model judge may decide its release" \
    "the scratch secret"
  passed "(l) the scratch secret ENVOY_OPERATOR_CONTROL at human tier without --admin exits 1 naming admin (the plan's control)"

  base_world
  : >"$world/refuse-secret-show"
  run
  want_status 2 "a session that cannot show the secret"
  want_text "UNKNOWN admin tag of ENVOY_OPERATOR: agent-secrets secret show ENVOY_OPERATOR --json was refused: agent-secrets secret show: Secret Manager GetSecret answered PermissionDenied" \
    "a refused show"
  passed "(l) a session that cannot show the secret's tags reads UNKNOWN naming the refusal"
fi

# --- (m) The report. ---
if row m; then
  base_world
  iam "$project_res" roles/container.admin "serviceAccount:$ci_gsa"
  : >"$world/refuse-secret-show"
  before="$(date -u +%s)"
  run --report "$temporary_dir/report.json"
  want_status 1 "a run with a finding and an unknown step"
  report="$temporary_dir/report.json"
  [ -s "$report" ] || fail "--report wrote nothing"
  run_at="$(jq -r '.run_at' "$report")"
  [[ "$run_at" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || fail "the report's run_at is '$run_at', not an RFC 3339 UTC time"
  at="$(date -u -d "$run_at" +%s)"
  [ "$at" -ge "$((before - 1))" ] && [ "$at" -le "$(date -u +%s)" ] || fail "the report's run_at $run_at is not the run's"
  jq -e --arg p "serviceAccount:$ci_gsa" 'any(.findings[]; .principal == $p and (.right | startswith("container.serviceAccounts.createToken")))' "$report" >/dev/null ||
    fail "the report's findings do not name serviceAccount:$ci_gsa: $(jq -c .findings "$report")"
  jq -e 'any(.unknown[]?; .step == "admin tag of ENVOY_OPERATOR" and (.reason | contains("PermissionDenied")))' "$report" >/dev/null ||
    fail "the report's unknown does not list the step 'admin tag of ENVOY_OPERATOR': $(jq -c '.unknown // "absent"' "$report")"
  jq -e 'keys == ["findings", "run_at", "unknown"] and all(.findings[]; keys == ["principal", "right"]) and all(.unknown[]; keys == ["reason", "step"])' "$report" >/dev/null ||
    fail "the report holds other fields than run_at, findings and unknown: $(jq -c . "$report")"
  passed "(m) --report writes run_at, findings and unknown, with the stubbed UNKNOWN step under unknown"
fi

# --- (n) One holder of each hold-writer right. ---
if row n; then
  base_world
  iam "$project_res" roles/cloudsql.admin "serviceAccount:$worker_gsa"
  subject_grant operator-cluster "$(sa_subject legion worker)" get "" secrets "" example-db broker-db-login
  iam "$bucket_res" roles/storage.objectViewer "serviceAccount:$worker_gsa"
  iam "$signing_res" roles/secretmanager.secretAccessor "serviceAccount:$worker_gsa"
  run --report "$temporary_dir/report.json"
  want_status 1 "one holder of each hold-writer right"
  report="$temporary_dir/report.json"
  for right in "cloudsql.users.create, cloudsql.users.update on $sql_res" "storage.objects.get on $bucket_res" "secretmanager.versions.access on $signing_res"; do
    jq -e --arg p "serviceAccount:$worker_gsa" --arg r "$right" 'any(.findings[]; .principal == $p and (.right | startswith($r)))' "$report" >/dev/null ||
      fail "the report does not list serviceAccount:$worker_gsa with $right: $(jq -c .findings "$report")"
  done
  jq -e 'any(.findings[]; .principal == "system:serviceaccount:legion:worker" and .right == "get secrets broker-db-login in example-db")' "$report" >/dev/null ||
    fail "the report does not list legion/worker getting the database Secret: $(jq -c .findings "$report")"
  passed "(n) one holder of each hold-writer right is a finding in the report"
fi

# --- (o) The broker's one binding, and the broker account's own minters. ---
if row o; then
  base_world
  mapfile -t args < <(args_without --broker-account --broker-workload)
  run_only "${args[@]}"
  want_status 1 "the broker's binding without --broker-account"
  want_text "FINDING serviceAccount:$broker: iam.serviceAccounts.getAccessToken," "the broker's binding without --broker-account"
  passed "(o) the broker's roles/iam.serviceAccountTokenCreator on the operator's account is a finding unless --broker-account names it"

  base_world
  iam "$broker_res" roles/iam.serviceAccountTokenCreator "serviceAccount:$worker_gsa"
  run
  want_status 1 "a minter of the broker's own account"
  want_text "FINDING serviceAccount:$worker_gsa: iam.serviceAccounts.getAccessToken," "a minter of the broker's account"
  want_text " on $broker_res through roles/iam.serviceAccountTokenCreator" "a minter of the broker's account"
  passed "(o) a minter of the broker account's own tokens is a holder of the operator's power"

  base_world
  iam "$project_res" roles/iam.serviceAccountTokenCreator "serviceAccount:$broker"
  run
  want_status 1 "the broker minting through another binding"
  want_text "FINDING serviceAccount:$broker: iam.serviceAccounts.getAccessToken," "the broker minting through another binding"
  # On the operator's own account, so the finding is the operator check's: the broker account's
  # own minters check finds the same binding on the broker account, and a row reading that one
  # passed with the broker allowed in any role (the o-broker control, 2026-11-21).
  want_text " on $gsa_res through roles/iam.serviceAccountTokenCreator, granted at $project_res" "the broker minting through another binding"
  passed "(o) the broker's allowance is its one binding on the operator's account, by name: the same role on the project is a finding"
fi

# --- (p) The code read. Every live step is refused, so each finding is the code's. ---
refuse_live() {
  : >"$world/clusters/operator-cluster/refuse-sar"
  : >"$world/refuse-analyze"
}
if row p; then
  base_world
  refuse_live
  edit "$world/render.json" '.items += [
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", metadata: {name: "worker-runs-pods", namespace: "envoy-operator"},
     roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "pod-runner"},
     subjects: [{kind: "ServiceAccount", name: "worker", namespace: "legion"}]},
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole", metadata: {name: "pod-runner"},
     rules: [{apiGroups: ["", "apps"], resources: ["pods", "deployments"], verbs: ["create"]}]}]'
  run
  want_status 1 "a RoleBinding the render declares"
  want_line "FINDING system:serviceaccount:legion:worker: create pods in envoy-operator, declared by RoleBinding envoy-operator/worker-runs-pods" "the render's grant"
  want_line "FINDING system:serviceaccount:legion:worker: create deployments.apps in envoy-operator, declared by RoleBinding envoy-operator/worker-runs-pods" "the render's grant"
  passed "(p) a grant the RBAC render declares is a finding with every live step refused"

  base_world
  refuse_live
  edit "$world/render.json" '.items += [
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", metadata: {name: "sessions-edit"},
     roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "edit"},
     subjects: [{kind: "Group", name: "system:serviceaccounts:legion"}]}]'
  run
  want_status 2 "a binding to a role the render does not hold"
  want_text "UNKNOWN code: RBAC: ClusterRoleBinding sessions-edit binds group:system:serviceaccounts:legion to ClusterRole edit, which the render does not hold" \
    "a role the render does not hold"
  passed "(p) a binding to a role the render does not hold reads UNKNOWN"

  base_world
  refuse_live
  edit "$world/preview.json" '.steps += [{op: "same", urn: "urn:pulumi:example::iam::gcp:projects/iAMMember:IAMMember::ci-container",
    newState: {type: "gcp:projects/iAMMember:IAMMember", inputs: {project: "example-project", role: "roles/container.admin", member: "serviceAccount:ci-runner@example-project.iam.gserviceaccount.com"}}}]'
  run
  want_status 1 "a binding an IAM preview declares"
  want_text "FINDING serviceAccount:$ci_gsa: container.serviceAccounts.createToken," "the preview's binding"
  want_text " on $project_res through roles/container.admin, declared by urn:pulumi:example::iam::gcp:projects/iAMMember:IAMMember::ci-container" "the preview's binding"
  passed "(p) a binding an IAM preview declares is a finding with every live step refused"

  base_world
  refuse_live
  edit "$world/preview.json" '.steps += [{op: "create", urn: "urn:pulumi:example::iam::gcp:serviceaccount/iAMMember:IAMMember::new-minter",
    newState: {type: "gcp:serviceaccount/iAMMember:IAMMember", inputs: {serviceAccountId: "projects/example-project/serviceAccounts/envoy-operator@example-project.iam.gserviceaccount.com",
      role: "roles/iam.serviceAccountTokenCreator", member: "04da6b54-80e4-46f7-96ec-b56ff0331ba9"}}}]'
  run
  want_status 2 "a member the preview does not know"
  want_text "UNKNOWN code: IAM: urn:pulumi:example::iam::gcp:serviceaccount/iAMMember:IAMMember::new-minter: the preview does not know its member" \
    "a member the preview does not know"
  passed "(p) a binding whose member the preview does not know reads UNKNOWN"

  base_world
  refuse_live
  edit "$world/preview.json" '.steps += [{op: "same", urn: "urn:pulumi:example::iam::gcp:storage/defaultObjectAccessControl:DefaultObjectAccessControl::state",
    newState: {type: "gcp:storage/defaultObjectAccessControl:DefaultObjectAccessControl", inputs: {bucket: "example-state", entity: "allAuthenticatedUsers", role: "READER"}}}]'
  run
  want_status 2 "an IAM resource type the check does not read"
  want_text "UNKNOWN code: IAM: urn:pulumi:example::iam::gcp:storage/defaultObjectAccessControl:DefaultObjectAccessControl::state: the check does not read gcp:storage/defaultObjectAccessControl:DefaultObjectAccessControl" \
    "an unread type"
  passed "(p) an IAM resource type the check does not read reads UNKNOWN"

  base_world
  mapfile -t args < <(args_without --rbac-render --iam-preview)
  run_only "${args[@]}"
  want_status 2 "no render and no preview"
  want_text "UNKNOWN code: RBAC: no --rbac-render" "no render"
  want_text "UNKNOWN code: IAM: no --iam-preview" "no preview"
  passed "(p) a run given no render and no preview reads both code steps UNKNOWN"
fi

# --- (q) A long answer. Past the 64 KiB a pipe holds, `cut | grep -q` fails under pipefail with
# SIGPIPE once grep has matched, reading the witness as missing; the check must read the answer
# whole. ---
if row q; then
  base_world
  mapfile -t people < <(seq -f 'user:person-%04g@example.com' 1 3000)
  iam "$gsa_res" roles/iam.serviceAccountTokenCreator "${people[@]}"
  run
  want_status 0 "3000 people beside the Workload Identity member"
  want_text "PASS live: minters of $gsa: 3003 holders listed" "3000 people beside the Workload Identity member"
  passed "(q) a Policy Analyzer answer longer than a pipe holds still finds its witness"
fi

[ "$rows" -gt 0 ] || { echo "FAIL: OPERATOR_MINTERS_TEST_ROWS=${OPERATOR_MINTERS_TEST_ROWS:-} names no row" >&2; exit 1; }
echo "all $rows rows passed"
