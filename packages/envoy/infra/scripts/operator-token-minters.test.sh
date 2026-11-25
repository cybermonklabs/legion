#!/usr/bin/env bash
# shellcheck disable=SC2016 # each single-quoted $name below is a jq variable, not the shell's
# packages/envoy/infra/scripts/operator-token-minters.test.sh
#
# Proves operator-token-minters.sh's verdicts without a cluster or a cloud. A plant on a real
# cluster or project needs RoleBinding and IAM writes, which no identity this check runs as holds,
# so every plant and every control of the check runs here, against a fake kubectl, gcloud, curl and
# agent-secrets put first on PATH (operator-token-minters/test-fakes.sh writes them). The fakes
# answer from one world of files this test writes:
#
#   - kubectl answers a SubjectAccessReview as the RBAC authorizer does, from the world's grants
#     (each a subject, a verb, an API group, a resource and subresource, a namespace or none for
#     cluster-wide, and a resource name or none), with the built-in cluster-admin binding of the
#     group system:masters allowed everything; it lists ServiceAccounts and reads a namespace;
#   - gcloud describes roles from the world's role table (roles/container.admin's list is the
#     real role's, read with `gcloud iam roles describe roles/container.admin` on 2026-11-20, cut
#     to the permissions the check names and a few beside them), lists the project's clusters (and
#     says on stderr, as gcloud 593.0.0 does, which zones did not respond), writes a kubeconfig for
#     a GKE cluster's get-credentials and for a fleet membership's, prints an access token, and
#     answers Policy Analyzer as the API does: a binding on the resource itself or on its project is
#     seen from any scope, one on the organization only from the organization's scope, a group is
#     expanded only with --expand-groups and --output-group-edges, and a role is given as its
#     permissions only with --expand-roles;
#   - curl answers the GKE Hub API's list of the fleet's memberships, one membership a page, the
#     locations it could not reach on the first page, so a reader that stops at one page, or reads
#     `unreachable` from another, misses what the world holds;
#   - agent-secrets shows a secret's tags as `secret show NAME --json` prints them, or answers as
#     Secret Manager's NotFound and PermissionDenied are printed.
#
# Each row resets the world (so each plant is removed after its row), plants one thing, and holds
# the run to its exit and to the line naming what it planted. The rows are in seven files of
# operator-token-minters/, by concern, each sourced below after the world and its helpers, and
# each listing its rows in its header:
#
#   - test-rows-run.sh: the run as a whole: a clean world, the permission names, reads that saw
#     nothing, each step's witness, a refused identity, the admin tag, the report and its fixture,
#     and a copy missing a part;
#   - test-rows-code.sh: the code read of the RBAC render and the IAM preview;
#   - test-rows-rbac.sh: Kubernetes RBAC: each right of the list, who can become a holder, rights
#     limited by name, custom resources, and the subjects a binding names;
#   - test-rows-iam.sh: Google IAM: GKE IAM, the minters of the operator's and the broker's
#     accounts, the hold writers, IAM impersonation and IAM writers, and a fleet member's project;
#   - test-rows-pool.sh: the pool's other clusters: the project's other clusters, the pool
#     project's own clusters, and the fleet's members;
#   - test-rows-admission.sh: admission power outside the admission objects: a webhook's backend,
#     a policy engine's resources, and an admission policy's parameter objects;
#   - test-rows-become.sh: who can become a principal with a finding through a right a live role
#     limits by name.
#
# Every name here is an example name: example-project, example.com, and the namespaces and
# accounts below. Run from anywhere: bash packages/envoy/infra/scripts/operator-token-minters.test.sh
# OPERATOR_MINTERS_TEST_ROWS="b p" runs those rows alone, as a control run by hand does, and
# OPERATOR_MINTERS_SCRIPT names another copy of the script to run them against; a copy keeps the
# directory operator-token-minters/ beside it, whose parts it sources (the rows files are this
# test's own, beside it). A row of one plant per item runs up to 8 items at a time, each in a world
# of its own (each).
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
# Every row asks the reviews in one kubectl call, so a world that answers the nth call its own way
# (the fake's review-follow-* markers) names a round, not a worker's share of one; row (cy)'s
# chunked item sets REVIEW_WORKERS itself and wants the same finding.
export REVIEW_WORKERS=1

readonly project=example-project
readonly number=123456789012
readonly org=100
readonly gsa=envoy-operator@example-project.iam.gserviceaccount.com
readonly control_gsa=envoy-operator-control@example-project.iam.gserviceaccount.com
readonly broker=secrets-broker@example-project.iam.gserviceaccount.com
readonly worker_gsa=worker@example-project.iam.gserviceaccount.com
readonly ci_gsa=ci-runner@example-project.iam.gserviceaccount.com
readonly grade_ci=grade-ci@example-grade.iam.gserviceaccount.com
readonly grade_res="//cloudresourcemanager.googleapis.com/projects/example-grade"
readonly applier=applier@example-project.iam.gserviceaccount.com
readonly applier_res="//iam.googleapis.com/projects/example-project/serviceAccounts/applier@example-project.iam.gserviceaccount.com"
readonly relay=relay@example-project.iam.gserviceaccount.com
readonly relay_res="//iam.googleapis.com/projects/example-project/serviceAccounts/relay@example-project.iam.gserviceaccount.com"
readonly route_pair=envoy-route/envoy-route-password-reader
readonly fixture="${OPERATOR_MINTERS_FIXTURE:-$script_dir/testdata/operator-token-minters-report.json}"
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

# The fakes, written into $fake_bin.
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/test-fakes.sh
. "$script_dir/operator-token-minters/test-fakes.sh"

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
user_subject() { jq -nc --arg n "$1" '{kind: "User", name: $n}'; }
# iam ATTACHED ROLE MEMBER...: one IAM binding.
iam() {
  local attached="$1" role="$2"
  shift 2
  edit "$world/iam.json" '. + [{attached: $a, role: $r, members: $ARGS.positional}]' --arg a "$attached" --arg r "$role" --args "$@"
}
# unbind ATTACHED ROLE: removes the world's bindings of that role on that resource.
unbind() { edit "$world/iam.json" 'map(select((.attached == $a and .role == $r) | not))' --arg a "$1" --arg r "$2"; }
role() { edit "$world/roles.json" '. + {($r): $ARGS.positional}' --arg r "$1" --args "${@:2}"; }
# add_sa CLUSTER NAMESPACE NAME: one more ServiceAccount, and its namespace, on a cluster's world.
add_sa() {
  edit "$world/clusters/$1/serviceaccounts.json" '. + [{namespace: $ns, name: $n}]' --arg ns "$2" --arg n "$3"
  grep -qxF -- "$2" "$world/clusters/$1/namespaces" || echo "$2" >>"$world/clusters/$1/namespaces"
}
# add_cluster NAME LOCATION: a second cluster, whose system namespaces' controllers may make
# namespaces and ServiceAccounts, as a cluster's own controllers may.
add_cluster() {
  edit "$world/clusters.json" '. + [{name: $n, location: $l, status: "RUNNING"}]' --arg n "$1" --arg l "$2"
  make_cluster "$1"
}
# make_cluster NAME: a cluster's own world (its namespaces, ServiceAccounts and grants) and no
# entry in the project's list, as a fleet member in another project or outside GKE has.
make_cluster() {
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
# add_member JSON: one more membership of the fleet, last in its list: {id, location, gke:
# {project, location, cluster}} for a GKE cluster, else {id, location, cluster: <its world's name>}.
add_member() { edit "$world/fleet.json" '.memberships += [$m]' --argjson m "$1"; }
# A GKE member of the fleet in another project, example-grade (project number 222222222222), whose
# cluster is grade-cluster.
grade_member() {
  make_cluster grade-cluster
  add_member '{"id": "grade", "location": "global", "gke": {"project": "example-grade", "location": "us-east1", "cluster": "grade-cluster"}}'
  mkdir -p "$world/numbers"
  echo 222222222222 >"$world/numbers/example-grade"
}
# cluster_admin CLUSTER SUBJECT: the built-in cluster-admin role bound to SUBJECT there.
cluster_admin() { subject_grant "$1" "$2" "*" "*" "*" "*" "" ""; }
grade_step="live: Workload Identity on fleet member grade (cluster example-grade/us-east1/grade-cluster)"
# A fleet member that is no GKE cluster, edge, on the cluster edge-cluster.
edge_member() {
  make_cluster edge-cluster
  add_member '{"id": "edge", "location": "global", "cluster": "edge-cluster"}'
}
# refuse_live: every live step refused (the operator's cluster's reviews and Policy Analyzer), so
# each finding a run prints is the code read's.
refuse_live() {
  : >"$world/clusters/operator-cluster/refuse-sar"
  : >"$world/refuse-analyze"
}

# The checked permissions of roles/container.admin, as `gcloud iam roles describe` lists them, with
# a few beside them it also holds.
readonly container_admin=(
  container.clusterRoleBindings.create container.clusterRoleBindings.get container.clusterRoleBindings.update
  container.clusterRoles.bind container.clusterRoles.escalate container.clusters.get container.clusters.impersonate
  container.cronJobs.create container.cronJobs.update container.daemonSets.create container.daemonSets.update
  container.deployments.create container.deployments.update container.jobs.create container.jobs.update
  container.namespaces.create container.namespaces.update container.nodes.proxy container.pods.attach container.pods.create container.pods.exec container.pods.get
  container.pods.portForward container.pods.update container.replicaSets.create container.replicaSets.update
  container.replicationControllers.create container.replicationControllers.update
  container.mutatingWebhookConfigurations.create container.mutatingWebhookConfigurations.update
  container.validatingWebhookConfigurations.create container.validatingWebhookConfigurations.update
  container.certificateSigningRequests.approve container.certificateSigningRequests.create container.clusters.update
  container.roleBindings.create container.roleBindings.update container.roles.bind container.roles.escalate
  container.secrets.create container.secrets.get container.secrets.list container.secrets.update container.serviceAccounts.create
  container.serviceAccounts.createToken container.serviceAccounts.update
  container.statefulSets.create container.statefulSets.update container.subjectAccessReviews.create
  resourcemanager.projects.get
)

build_world() {
  mkdir -p "$world/clusters/operator-cluster" "$world/secrets"
  printf '%s\n' "$project" >"$world/project"
  printf '%s\n' "$number" >"$world/number"
  jq -n '[{name: "operator-cluster", location: "us-central1", status: "RUNNING", autopilot: {enabled: true}}]' >"$world/clusters.json"
  # The pool project's fleet, holding the operator's cluster alone.
  jq -n --arg p "$project" '{project: $p, unreachable: [],
    memberships: [{id: "operator-cluster", location: "global", gke: {project: $p, location: "us-central1", cluster: "operator-cluster"}}]}' \
    >"$world/fleet.json"
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
    iam.serviceAccountKeys.create cloudsql.users.create cloudsql.users.update secretmanager.versions.access storage.objects.get
    gkehub.memberships.create gkehub.memberships.update resourcemanager.projects.setIamPolicy resourcemanager.folders.setIamPolicy
    resourcemanager.organizations.setIamPolicy iam.serviceAccounts.setIamPolicy secretmanager.secrets.setIamPolicy iam.roles.update)
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
  # Its names checked against `gcloud iam roles describe roles/gkehub.admin` on 2026-11-21.
  role roles/gkehub.admin gkehub.features.create gkehub.features.update gkehub.memberships.create gkehub.memberships.delete \
    gkehub.memberships.get gkehub.memberships.list gkehub.memberships.update gkehub.membershipbindings.create \
    gkehub.membershipbindings.update gkehub.namespaces.create gkehub.namespaces.update gkehub.rbacrolebindings.create \
    gkehub.rbacrolebindings.update gkehub.scopes.create gkehub.scopes.list gkehub.scopes.setIamPolicy gkehub.scopes.update \
    resourcemanager.projects.get
  # The IAM writers' roles, each name checked against `gcloud iam roles describe` on 2026-11-21, cut
  # to the permission the check names and a few beside it.
  role roles/resourcemanager.projectIamAdmin resourcemanager.projects.get resourcemanager.projects.getIamPolicy \
    resourcemanager.projects.setIamPolicy
  role roles/resourcemanager.folderIamAdmin resourcemanager.folders.get resourcemanager.folders.getIamPolicy \
    resourcemanager.folders.setIamPolicy
  role roles/resourcemanager.organizationAdmin resourcemanager.organizations.get resourcemanager.organizations.getIamPolicy \
    resourcemanager.organizations.setIamPolicy
  role roles/iam.serviceAccountAdmin iam.serviceAccounts.create iam.serviceAccounts.get iam.serviceAccounts.getIamPolicy \
    iam.serviceAccounts.setIamPolicy
  role roles/secretmanager.admin secretmanager.secrets.get secretmanager.secrets.getIamPolicy secretmanager.secrets.setIamPolicy \
    secretmanager.versions.access
  role roles/iam.roleAdmin iam.roles.create iam.roles.get iam.roles.list iam.roles.update

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
# run_only ARGS...: the check with ARGS alone; run_script names another copy of the script to run, as
# a row breaking one of its parts does (run_script=<copy> run).
run_only() {
  status=0
  output="$(env PATH="$fake_bin:$PATH" OPERATOR_MINTERS_WORLD="$world" TMPDIR="$temporary_dir" bash "${run_script:-$check}" "$@" 2>&1)" || status=$?
}
# script_copy NAME: a copy of the script and its parts under the test's temporary directory, for a
# row that breaks or removes one part; prints the copy's script path, for run_script.
script_copy() {
  local copy="$temporary_dir/copy-$1"
  rm -rf "$copy"
  mkdir -p "$copy"
  cp "$check" "$copy/operator-token-minters.sh"
  cp -r "$(dirname "$check")/operator-token-minters" "$copy/"
  printf '%s' "$copy/operator-token-minters.sh"
}
# plant FILE OLD NEW: FILE with its one OLD made NEW; fails the row when FILE does not hold OLD once,
# so a plant that missed its line cannot pass as a control.
plant() {
  local text
  text="$(<"$1")"
  [ "$(grep -cF -- "$2" "$1")" -eq 1 ] || fail "the plant's line is not in $(basename "$1") once: $2"
  printf '%s\n' "${text/"$2"/"$3"}" >"$1"
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
# want_step_unknown STEP TEXT WHAT: the step STEP (as its lines name it: `RBAC on operator-cluster`)
# read UNKNOWN holding TEXT, the run reached its end with no finding (its last line says so, which a
# run that stopped mid-way does not), exit 2, and the step did not pass. The rows that break a read
# share it, so a text one of them changes is fixed here once, and a run that stopped fails it by its
# own last line. want_rbac_unknown TEXT WHAT is it for the operator's RBAC step.
want_step_unknown() {
  want_status 2 "$3"
  want_text "UNKNOWN live: $1: $2" "$3"
  has_prefix "operator-token-minters: no finding, " ||
    fail "$3: the run did not reach its end with no finding; its last line is '$(tail -n 1 <<<"$output")'"
  ! has_prefix "PASS live: $1: " || fail "$3: the step $1 passed"
}
want_rbac_unknown() { want_step_unknown "RBAC on operator-cluster" "$1" "$2"; }
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

# The rows, one file of operator-token-minters/ per concern, as the header lists them.
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/test-rows-run.sh
. "$script_dir/operator-token-minters/test-rows-run.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/test-rows-code.sh
. "$script_dir/operator-token-minters/test-rows-code.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/test-rows-rbac.sh
. "$script_dir/operator-token-minters/test-rows-rbac.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/test-rows-iam.sh
. "$script_dir/operator-token-minters/test-rows-iam.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/test-rows-pool.sh
. "$script_dir/operator-token-minters/test-rows-pool.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/test-rows-admission.sh
. "$script_dir/operator-token-minters/test-rows-admission.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/test-rows-become.sh
. "$script_dir/operator-token-minters/test-rows-become.sh"

[ "$rows" -gt 0 ] || { echo "FAIL: OPERATOR_MINTERS_TEST_ROWS=${OPERATOR_MINTERS_TEST_ROWS:-} names no row" >&2; exit 1; }
echo "all $rows rows passed"
