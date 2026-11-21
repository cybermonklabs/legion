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
#        member) still finds its witness, and exits 0;
#   (r)  the pool project's fleet: a member that is no GKE cluster, listed on the fleet's second
#        page, whose cluster lets a ServiceAccount make a namespace, exits 1 naming the member (the
#        control is the script before this step, which misses it); a GKE member in another project
#        holding the namespace envoy-operator exits 1 naming the cluster; a fleet the API answers
#        empty exits 0 and says so; a listing with an unreachable location, a refused listing, a
#        member whose credentials are refused, and a project's cluster list gcloud says is
#        incomplete each exit 2 with UNKNOWN; --pool-project naming another pool cannot read clean;
#   (s)  a binding subject on a fleet member: grade-ci, a Google service account, bound to
#        cluster-admin on a GKE member in another project exits 1 naming grade-ci and create
#        namespaces; a person's email bound the same way, and a Group bound the same way, exit 0;
#        a refused list of bindings exits 2 with UNKNOWN;
#   (t)  grade-ci holding roles/container.admin on that member's project exits 1, since GKE
#        authorizes IAM beside RBAC;
#   (u)  a binding subject on the operator's own cluster: grade-ci bound to create the operator's
#        token exits 1;
#   (v)  the report's findings carry role, resource, permissions and granted_at beside principal
#        and right, on an IAM finding and on an RBAC finding;
#   (w)  two guarded pairs (--guarded envoy-route/envoy-route-password-reader beside the
#        operator's): grade-ci bound to cluster-admin on a fleet member is reported for both;
#   (x)  a ServiceAccount on a fleet member that can create pods, and nothing else, in the
#        operator's namespace is reported, naming the operator's pair;
#   (y)  the same in the route password reader's namespace, naming that pair;
#   (z)  the fixture testdata/operator-token-minters-report.json is this script's --report from a
#        world with an IAM finding, grade-ci's RBAC findings and an UNKNOWN, run_at fixed; the row
#        fails when the committed file differs (OPERATOR_MINTERS_WRITE_FIXTURE=1 rewrites it, and
#        OPERATOR_MINTERS_FIXTURE names another file to compare, as the row's control does);
#   (aa) a second cluster of the project named operator-cluster, in us-east1, holding the namespace
#        envoy-operator, is a finding with --location us-central1, and UNKNOWN without it;
#   (ab) the broker's pair is guarded: an agent ServiceAccount that may create pods in the broker's
#        namespace, or create the broker ServiceAccount's token, and another cluster of the pool
#        holding the broker's namespace, each exit 1;
#   (ac) on the operator's cluster, ci/runner, outside the agent namespaces, with create pods in
#        envoy-operator, and with cluster-admin, each exits 1;
#   (ad) container.replicationControllers.create and update and container.nodes.proxy, each held on
#        the project by an agent's Google service account, exit 1 (their RBAC rights are row b's);
#   (ae) a CI account holding roles/gkehub.admin on the pool project exits 1;
#   (af) a render binding the Group cluster-admins@example.com to cluster-admin exits 0: a Group
#        other than the system ones is no finding in the code step, as in the live steps;
#   (ag) an agent's Google service account holding roles/iam.serviceAccountTokenCreator on a holder
#        of roles/container.admin is a finding naming the chain, through one service account and
#        through two;
#   (ah) a holder of roles/resourcemanager.projectIamAdmin is a finding, and so is a holder of each
#        of iam.serviceAccounts.setIamPolicy, secretmanager.secrets.setIamPolicy,
#        resourcemanager.organizations.setIamPolicy and iam.roles.update over a checked resource;
#   (ai) a ServiceAccount that can become ci/runner, which can create pods in envoy-operator, by
#        each of create pods, create serviceaccounts/token, impersonate, create pods/exec and get
#        secrets in ci, is a finding naming the chain;
#   (aj) the Group system:unauthenticated bound to create namespaces on another cluster is a
#        finding for anyone unauthenticated;
#   (ak) create on a custom resource (sandboxes.agents.x-k8s.io) in envoy-operator is a finding,
#        create on Kubernetes' own leases is not, and a discovery that cannot be read is UNKNOWN;
#   (al) a run that stops mid-way leaves an UNKNOWN report, never the old one, and a usage error
#        leaves none;
#   (am) an --agent-namespace pattern that matches nothing is UNKNOWN, naming it, beside two that
#        match.
#
# Every name here is an example name: example-project, example.com, and the namespaces and
# accounts below. Run from anywhere: bash packages/envoy/infra/scripts/operator-token-minters.test.sh
# OPERATOR_MINTERS_TEST_ROWS="b p" runs those rows alone, as a control run by hand does, and
# OPERATOR_MINTERS_SCRIPT names another copy of the script to run them against; a copy keeps the
# directory operator-token-minters/ beside it, whose parts it sources. The rows of one plant per
# item ((b), (d), (e), (f)) run up to 8 at a time, each in a world of its own.
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

# The checked permissions of roles/container.admin, as `gcloud iam roles describe` lists them, with
# a few beside them it also holds.
readonly container_admin=(
  container.clusterRoleBindings.create container.clusterRoleBindings.get container.clusterRoleBindings.update
  container.clusterRoles.bind container.clusterRoles.escalate container.clusters.get container.clusters.impersonate
  container.cronJobs.create container.cronJobs.update container.daemonSets.create container.daemonSets.update
  container.deployments.create container.deployments.update container.jobs.create container.jobs.update
  container.namespaces.create container.nodes.proxy container.pods.attach container.pods.create container.pods.exec container.pods.get
  container.pods.portForward container.pods.update container.replicaSets.create container.replicaSets.update
  container.replicationControllers.create container.replicationControllers.update
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
  role roles/gkehub.admin gkehub.memberships.create gkehub.memberships.delete gkehub.memberships.get gkehub.memberships.list \
    gkehub.memberships.update resourcemanager.projects.get
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
    "live: hold writers: the signing key hold-signing-example" "admin tag of ENVOY_OPERATOR" \
    "live: who can register a cluster to the fleet of project $project" "live: who can set IAM on organizations/100"; do
    want_text "PASS $step: " "a clean world's $step"
  done
  want_line "PASS live: Workload Identity: the fleet of project $project: 1 membership(s) listed, every location reached: 1 the operator's cluster, 0 read above as clusters of the project, 0 read here" \
    "a clean world's fleet"
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
  "watch||secrets||envoy-operator|=watch secrets in envoy-operator"
  "create||replicationcontrollers||envoy-operator|=create replicationcontrollers in envoy-operator"
  "patch||replicationcontrollers||envoy-operator|=patch replicationcontrollers in envoy-operator"
  "update||replicationcontrollers||envoy-operator|=update replicationcontrollers in envoy-operator"
  "create||nodes|proxy|||=create nodes/proxy cluster-wide"
  "get||nodes|proxy|||=get nodes/proxy cluster-wide"
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
  want_text "PASS live: Workload Identity on example-project/us-east1/other-cluster: " "a clean second cluster"
  passed "(g) a second cluster whose system namespaces alone may make namespaces and ServiceAccounts exits 0"

  base_world
  add_cluster other-cluster us-east1
  echo envoy-operator >>"$world/clusters/other-cluster/namespaces"
  run
  want_status 1 "the namespace envoy-operator on a second cluster"
  want_text "FINDING cluster:example-project/us-east1/other-cluster: namespace envoy-operator exists there, so its ServiceAccount envoy-operator/envoy-operator acts as $gsa" \
    "the namespace envoy-operator on a second cluster"
  passed "(g) the namespace envoy-operator on a second cluster exits 1 naming the cluster"

  base_world
  add_cluster other-cluster us-east1
  echo envoy-operator-control >>"$world/clusters/other-cluster/namespaces"
  run --namespace envoy-operator-control --gsa "$control_gsa"
  want_status 1 "the scratch namespace on a second cluster"
  want_text "FINDING cluster:example-project/us-east1/other-cluster: namespace envoy-operator-control exists there" "the scratch namespace on a second cluster"
  passed "(g) the scratch namespace envoy-operator-control made on a second cluster exits 1 naming the cluster (the plan's control)"

  base_world
  add_cluster other-cluster us-east1
  subject_grant other-cluster "$(sa_subject apps deployer)" create "" namespaces "" "" ""
  run
  want_status 1 "a ServiceAccount outside the system namespaces that may make a namespace"
  want_line "FINDING system:serviceaccount:apps:deployer: create namespaces cluster-wide on cluster example-project/us-east1/other-cluster" "apps/deployer making namespaces"
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
  jq -e 'keys == ["findings", "run_at", "unknown"]
    and all(.findings[]; keys == ["granted_at", "permissions", "principal", "resource", "right", "role"])
    and all(.unknown[]; keys == ["reason", "step"])' "$report" >/dev/null ||
    fail "the report holds other fields than run_at, findings (principal, right, role, resource, permissions, granted_at) and unknown: $(jq -c . "$report")"
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

# --- (r) The pool project's fleet. Every member cluster uses the pool, so a ServiceAccount
# envoy-operator/envoy-operator there acts as the operator's Google service account. ---
fleet_step="live: Workload Identity: the fleet of project $project"
if row r; then
  # A member that is no GKE cluster, second in the list, so on the API's second page, whose cluster
  # lets a ServiceAccount outside its system namespaces make a namespace.
  base_world
  make_cluster edge-cluster
  subject_grant edge-cluster "$(sa_subject apps deployer)" create "" namespaces "" "" ""
  add_member '{"id": "edge", "location": "global", "cluster": "edge-cluster"}'
  run
  want_status 1 "a fleet member whose cluster has a minter"
  want_line "FINDING system:serviceaccount:apps:deployer: create namespaces cluster-wide on fleet member $project/global/edge" \
    "a fleet member whose cluster has a minter"
  passed "(r) a fleet member that is no GKE cluster, on the listing's second page, whose cluster lets a ServiceAccount make a namespace, exits 1 naming it"

  # A GKE member in another project, read in that project, holding the operator's namespace.
  base_world
  make_cluster shared-cluster
  echo envoy-operator >>"$world/clusters/shared-cluster/namespaces"
  add_member '{"id": "shared", "location": "global", "gke": {"project": "other-project", "location": "us-east1", "cluster": "shared-cluster"}}'
  run
  want_status 1 "a GKE fleet member in another project holding the namespace envoy-operator"
  want_line "FINDING cluster:other-project/us-east1/shared-cluster: namespace envoy-operator exists there, so its ServiceAccount envoy-operator/envoy-operator acts as $gsa through the workload pool $project.svc.id.goog" \
    "a GKE fleet member in another project holding the namespace envoy-operator"
  passed "(r) a GKE fleet member in another project holding the namespace envoy-operator exits 1 naming the cluster"

  # A member that is one of the project's own clusters, read once, by the project's step.
  base_world
  add_cluster other-cluster us-east1
  add_member "$(jq -nc --arg p "$project" '{id: "other", location: "global", gke: {project: $p, location: "us-east1", cluster: "other-cluster"}}')"
  run
  want_status 0 "a fleet member that is one of the project's clusters"
  want_line "PASS $fleet_step: 2 membership(s) listed, every location reached: 1 the operator's cluster, 1 read above as clusters of the project, 0 read here" \
    "a fleet member that is one of the project's clusters"
  passed "(r) a fleet member that is one of the project's clusters is read once, by the project's step"

  # A fleet the API answers with no membership.
  base_world
  edit "$world/fleet.json" '.memberships = []'
  run
  want_status 0 "an empty fleet"
  want_line "PASS $fleet_step: the GKE Hub API lists no membership in the fleet, and no location it could not reach" "an empty fleet"
  passed "(r) a fleet the API answers with no membership exits 0 and says so"

  # A listing that could not reach a location.
  base_world
  edit "$world/fleet.json" '.unreachable = ["us-west1"]'
  run
  want_status 2 "a fleet listing with an unreachable location"
  want_line "UNKNOWN $fleet_step: the listing could not reach the locations us-west1, so a membership registered there is not listed" \
    "a fleet listing with an unreachable location"
  passed "(r) a fleet listing that could not reach a location exits 2 with UNKNOWN"

  # A refused listing.
  base_world
  : >"$world/refuse-fleet"
  run
  want_status 2 "a refused fleet listing"
  want_text "UNKNOWN $fleet_step: the GKE Hub API refused the listing of projects/$project/locations/-/memberships (it needs gkehub.memberships.list there): 403 PERMISSION_DENIED Permission 'gkehub.memberships.list' denied" \
    "a refused fleet listing"
  passed "(r) a refused fleet listing exits 2 with UNKNOWN naming the permission"

  # No access token for the running identity.
  base_world
  : >"$world/refuse-token"
  run
  want_status 2 "no access token"
  want_text "UNKNOWN $fleet_step: gcloud auth print-access-token gave no access token for the running identity" "no access token"
  passed "(r) no access token for the running identity exits 2 with UNKNOWN"

  # A member whose credentials are refused.
  base_world
  make_cluster edge-cluster
  add_member '{"id": "edge", "location": "global", "cluster": "edge-cluster", "refuse": true}'
  run
  want_status 2 "a fleet member whose credentials are refused"
  want_text "UNKNOWN live: Workload Identity on fleet member edge: gcloud container fleet memberships get-credentials edge --location global --project $project was refused" \
    "a fleet member whose credentials are refused"
  passed "(r) a fleet member whose credentials are refused exits 2 with UNKNOWN"

  # A project's cluster list gcloud says is incomplete.
  base_world
  : >"$world/missing-zones"
  run
  want_status 2 "an incomplete cluster list"
  want_text "UNKNOWN live: clusters: gcloud container clusters list says its list is incomplete" "an incomplete cluster list"
  want_line "UNKNOWN live: Workload Identity: the project's cluster list is incomplete (live: clusters), so a cluster it leaves out is not read" \
    "an incomplete cluster list"
  passed "(r) a cluster list gcloud says is incomplete exits 2 with UNKNOWN"

  # --pool-project naming another pool: the analysis lists the real pool's member, not the one
  # named, and that pool's fleet cannot be listed, so the run cannot read clean.
  base_world
  run --pool-project other-pool
  [ "$status" -ne 0 ] || fail "--pool-project other-pool: exit 0"
  want_text "UNKNOWN live: minters of $gsa: the analysis does not list serviceAccount:other-pool.svc.id.goog[envoy-operator/envoy-operator]" \
    "--pool-project other-pool"
  want_text "UNKNOWN live: Workload Identity: the fleet of project other-pool: the GKE Hub API refused the listing" "--pool-project other-pool"
  passed "(r) --pool-project naming another pool than the operator's member names cannot read clean"
fi

# A GKE member of the fleet in another project, example-grade, whose cluster is grade-cluster.
grade_member() {
  make_cluster grade-cluster
  add_member '{"id": "grade", "location": "global", "gke": {"project": "example-grade", "location": "us-east1", "cluster": "grade-cluster"}}'
}
# cluster_admin CLUSTER SUBJECT: the built-in cluster-admin role bound to SUBJECT there.
cluster_admin() { subject_grant "$1" "$2" "*" "*" "*" "*" "" ""; }
grade_step="live: Workload Identity on fleet member grade (cluster example-grade/us-east1/grade-cluster)"

# --- (s) The binding subjects of a fleet member's cluster. ---
if row s; then
  base_world
  grade_member
  cluster_admin grade-cluster "$(user_subject "$grade_ci")"
  run
  want_status 1 "grade-ci bound to cluster-admin on a fleet member"
  want_line "FINDING serviceAccount:$grade_ci: create namespaces cluster-wide on cluster example-grade/us-east1/grade-cluster" \
    "grade-ci bound to cluster-admin on a fleet member"
  passed "(s) a Google service account bound to cluster-admin on a fleet member in another project exits 1 naming it and create namespaces"

  base_world
  grade_member
  cluster_admin grade-cluster "$(user_subject alice@example.com)"
  run
  want_status 0 "a person bound to cluster-admin on a fleet member"
  [ "$(count_prefix 'FINDING ')" -eq 0 ] || fail "a person bound to cluster-admin on a fleet member: a finding"
  want_text "PASS $grade_step: " "a person bound to cluster-admin on a fleet member"
  passed "(s) a person's email bound to cluster-admin on a fleet member stays allowed"

  base_world
  grade_member
  cluster_admin grade-cluster "$(group_subject grade-admins@example.com)"
  run
  want_status 0 "a Group bound to cluster-admin on a fleet member"
  [ "$(count_prefix 'FINDING ')" -eq 0 ] || fail "a Group bound to cluster-admin on a fleet member: a finding"
  passed "(s) a Group bound to cluster-admin on a fleet member is no finding"

  base_world
  grade_member
  : >"$world/clusters/grade-cluster/refuse-bindings"
  run
  want_status 2 "a fleet member whose bindings cannot be listed"
  want_text "UNKNOWN $grade_step: the API server refused the bindings' list (it needs list on clusterrolebindings and rolebindings" \
    "a fleet member whose bindings cannot be listed"
  passed "(s) a fleet member whose bindings cannot be listed exits 2 with UNKNOWN naming the permission"
fi

# --- (t) IAM on a fleet member's own project. ---
if row t; then
  base_world
  grade_member
  iam "$grade_res" roles/container.admin "serviceAccount:$grade_ci"
  run
  want_status 1 "grade-ci holding roles/container.admin on a fleet member's project"
  want_line "FINDING serviceAccount:$grade_ci: container.namespaces.create, container.serviceAccounts.create, container.pods.create on $grade_res through roles/container.admin, granted at $grade_res" \
    "grade-ci holding roles/container.admin on a fleet member's project"
  passed "(t) a Google service account holding roles/container.admin on a fleet member's project exits 1 naming it"
fi

# --- (u) The binding subjects of the operator's own cluster. ---
if row u; then
  base_world
  subject_grant operator-cluster "$(user_subject "$grade_ci")" create "" serviceaccounts token envoy-operator envoy-operator
  run
  want_status 1 "grade-ci bound to create the operator's token on its cluster"
  want_line "FINDING serviceAccount:$grade_ci: create serviceaccounts/token envoy-operator in envoy-operator" \
    "grade-ci bound to create the operator's token on its cluster"
  passed "(u) a Google service account bound on the operator's own cluster to create its token exits 1 naming it"
fi

# --- (v) The report's parts of each finding, for a reader matching principal and role. ---
if row v; then
  base_world
  iam "$project_res" roles/container.admin "serviceAccount:$ci_gsa"
  subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods exec envoy-operator ""
  run --report "$temporary_dir/report.json"
  want_status 1 "an IAM and an RBAC finding"
  report="$temporary_dir/report.json"
  # Every finding carries every field, each of its type.
  missing="$(jq -r '.findings[] as $f
    | ((["principal", "right", "role", "resource", "granted_at"][] as $k
        | select(($f[$k] | type) != "string") | "\($f.principal) has no \($k)"),
       (select(($f.permissions | type) != "array") | "\($f.principal) has no permissions list"))' "$report" | paste -sd ';' | sed 's/;/; /g')"
  [ -z "$missing" ] || fail "the report's findings lack fields: $missing"
  jq -e --arg p "serviceAccount:$ci_gsa" --arg res "$project_res" 'any(.findings[]; .principal == $p and .role == "roles/container.admin"
      and .resource == $res and .granted_at == $res and (.permissions | index("container.serviceAccounts.createToken") != null))' "$report" >/dev/null ||
    fail "the IAM finding's parts are not principal, roles/container.admin, $project_res and its permissions: $(jq -c --arg p "serviceAccount:$ci_gsa" '[.findings[] | select(.principal == $p)]' "$report")"
  jq -e 'any(.findings[]; .principal == "system:serviceaccount:legion:worker" and .role == "create pods/exec in envoy-operator"
      and .resource == "cluster:operator-cluster" and .permissions == ["create pods/exec in envoy-operator"]
      and (.granted_at | startswith("RBAC: allowed by")))' "$report" >/dev/null ||
    fail "the RBAC finding's parts are not its label, cluster:operator-cluster and the API server's reason: $(jq -c '[.findings[] | select(.principal == "system:serviceaccount:legion:worker")]' "$report")"
  passed "(v) the report's findings carry role, resource, permissions and granted_at, on an IAM finding and on an RBAC finding"
fi

# A fleet member that is no GKE cluster, edge, on the cluster edge-cluster.
edge_member() {
  make_cluster edge-cluster
  add_member '{"id": "edge", "location": "global", "cluster": "edge-cluster"}'
}

# --- (w) Two guarded pairs, both reported for grade-ci's cluster-admin on a fleet member. ---
if row w; then
  base_world
  grade_member
  cluster_admin grade-cluster "$(user_subject "$grade_ci")"
  run --guarded "$route_pair"
  want_status 1 "grade-ci bound to cluster-admin on a fleet member, two guarded pairs"
  for right in "create serviceaccounts in envoy-operator, guarding envoy-operator/envoy-operator" "create pods in envoy-operator, guarding envoy-operator/envoy-operator" \
    "create serviceaccounts in envoy-route, guarding $route_pair" "create pods in envoy-route, guarding $route_pair"; do
    want_line "FINDING serviceAccount:$grade_ci: $right on cluster example-grade/us-east1/grade-cluster" \
      "grade-ci bound to cluster-admin on a fleet member, two guarded pairs"
  done
  passed "(w) grade-ci bound to cluster-admin on a fleet member is reported for both guarded pairs"
fi

# --- (x) Pod create alone, in the operator's namespace, on a fleet member. ---
if row x; then
  base_world
  edge_member
  subject_grant edge-cluster "$(sa_subject apps deployer)" create "" pods "" envoy-operator ""
  run
  want_status 1 "a ServiceAccount that may create pods in envoy-operator on a fleet member"
  want_line "FINDING system:serviceaccount:apps:deployer: create pods in envoy-operator, guarding envoy-operator/envoy-operator on fleet member $project/global/edge" \
    "a ServiceAccount that may create pods in envoy-operator on a fleet member"
  [ "$(count_prefix 'FINDING ')" -eq 1 ] || fail "a ServiceAccount that may create pods in envoy-operator on a fleet member: want that one finding"
  passed "(x) a ServiceAccount that may create pods, and nothing else, in the operator's namespace on a fleet member exits 1 naming the pair"
fi

# --- (y) Pod create alone, in the route password reader's namespace, on a fleet member. ---
if row y; then
  base_world
  edge_member
  subject_grant edge-cluster "$(sa_subject apps deployer)" create "" pods "" envoy-route ""
  run --guarded "$route_pair"
  want_status 1 "a ServiceAccount that may create pods in envoy-route on a fleet member"
  want_line "FINDING system:serviceaccount:apps:deployer: create pods in envoy-route, guarding $route_pair on fleet member $project/global/edge" \
    "a ServiceAccount that may create pods in envoy-route on a fleet member"
  [ "$(count_prefix 'FINDING ')" -eq 1 ] || fail "a ServiceAccount that may create pods in envoy-route on a fleet member: want that one finding"
  passed "(y) a ServiceAccount that may create pods, and nothing else, in the route password reader's namespace on a fleet member exits 1 naming that pair"
fi

# --- (z) The report fixture: a fresh --report, run_at fixed, must equal the committed file. ---
if row z; then
  base_world
  iam "$project_res" roles/container.admin "serviceAccount:$ci_gsa"
  grade_member
  cluster_admin grade-cluster "$(user_subject "$grade_ci")"
  : >"$world/refuse-secret-show"
  run --guarded "$route_pair" --report "$temporary_dir/fixture-run.json"
  want_status 1 "the fixture's world"
  # run_at is the one field a run's clock sets; the fixture fixes it at the epoch, so no reader
  # takes it for a time something happened.
  jq '.run_at = "1970-01-01T00:00:00Z"' "$temporary_dir/fixture-run.json" >"$temporary_dir/fixture-fresh.json"
  jq -e 'any(.findings[]; .role | startswith("roles/")) and any(.findings[]; (.principal | startswith("serviceAccount:grade-ci@")) and (.granted_at | startswith("RBAC: ")))
      and (.unknown | length) > 0' "$temporary_dir/fixture-fresh.json" >/dev/null ||
    fail "the fixture's world gave no IAM finding, no RBAC finding of grade-ci, or no UNKNOWN: $(jq -c . "$temporary_dir/fixture-fresh.json")"
  # An RBAC finding's permissions are its one label, whole, though a guarded label holds a comma.
  split="$(jq -r '.findings[] | select(.granted_at | startswith("RBAC: ")) | select(.permissions != [.role]) | "\(.role): \(.permissions)"' "$temporary_dir/fixture-fresh.json")"
  [ -z "$split" ] || fail "an RBAC finding's permissions are not its label, whole: $split"
  if [ "${OPERATOR_MINTERS_WRITE_FIXTURE:-}" = 1 ]; then
    mkdir -p "$(dirname "$fixture")"
    cp "$temporary_dir/fixture-fresh.json" "$fixture"
    echo "wrote $fixture"
  fi
  [ -f "$fixture" ] || fail "no fixture at $fixture: run with OPERATOR_MINTERS_WRITE_FIXTURE=1 to write it"
  cmp -s "$fixture" "$temporary_dir/fixture-fresh.json" ||
    fail "the fixture $fixture differs from a fresh run (OPERATOR_MINTERS_WRITE_FIXTURE=1 rewrites it): $(diff "$fixture" "$temporary_dir/fixture-fresh.json" | sed -n '1,20p' || true)"
  passed "(z) the committed report fixture equals a fresh --report of its world"
fi

# add_sa CLUSTER NAMESPACE NAME: one more ServiceAccount, and its namespace, on a cluster's world.
add_sa() {
  edit "$world/clusters/$1/serviceaccounts.json" '. + [{namespace: $ns, name: $n}]' --arg ns "$2" --arg n "$3"
  grep -qxF -- "$2" "$world/clusters/$1/namespaces" || echo "$2" >>"$world/clusters/$1/namespaces"
}
# add_cluster_at NAME LOCATION: a second cluster of one name, in the project's list, whose world is
# clusters/<name>@<location>.
add_cluster_at() {
  edit "$world/clusters.json" '. + [{name: $n, location: $l, status: "RUNNING"}]' --arg n "$1" --arg l "$2"
  make_cluster "$1@$2"
}

# --- (aa) The operator's cluster by name and location. ---
if row aa; then
  base_world
  add_cluster_at operator-cluster us-east1
  echo envoy-operator >>"$world/clusters/operator-cluster@us-east1/namespaces"
  run --location us-central1
  want_status 1 "a second operator-cluster holding envoy-operator, --location us-central1"
  want_line "FINDING cluster:$project/us-east1/operator-cluster: namespace envoy-operator exists there, so its ServiceAccount envoy-operator/envoy-operator acts as $gsa through the workload pool $project.svc.id.goog" \
    "a second operator-cluster holding envoy-operator, --location us-central1"
  [ "$(count_prefix 'FINDING ')" -eq 1 ] || fail "a second operator-cluster holding envoy-operator: want that one finding, the operator's own cluster not read as another"
  passed "(aa) a second cluster named operator-cluster elsewhere, holding envoy-operator, is a finding with --location"

  base_world
  add_cluster_at operator-cluster us-east1
  echo envoy-operator >>"$world/clusters/operator-cluster@us-east1/namespaces"
  run
  want_status 2 "a second operator-cluster, no --location"
  want_line "UNKNOWN live: clusters: --cluster operator-cluster names 2 clusters of the project (in us-central1, us-east1), and no --location says which is the operator's, so none of them is read" \
    "a second operator-cluster, no --location"
  want_text "UNKNOWN live: Workload Identity on $project/us-east1/operator-cluster: it has --cluster's name" "a second operator-cluster, no --location"
  [ "$(count_prefix 'FINDING ')" -eq 0 ] || fail "a second operator-cluster, no --location: a finding"
  passed "(aa) two clusters named operator-cluster and no --location read UNKNOWN, and neither as the operator's"
fi

# --- (ab) The broker's pair, guarded as the operator's. ---
if row ab; then
  base_world
  subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods "" secrets-broker ""
  run
  want_status 1 "an agent ServiceAccount that may create pods in secrets-broker"
  want_line "FINDING system:serviceaccount:legion:worker: create pods in secrets-broker" "an agent ServiceAccount that may create pods in secrets-broker"
  passed "(ab) an agent ServiceAccount that may create pods in the broker's namespace exits 1"

  base_world
  add_cluster other-cluster us-east1
  echo secrets-broker >>"$world/clusters/other-cluster/namespaces"
  run
  want_status 1 "another cluster of the pool holding secrets-broker"
  want_line "FINDING cluster:$project/us-east1/other-cluster: namespace secrets-broker exists there, so its ServiceAccount secrets-broker/secrets-broker acts as the broker account $broker through the workload pool $project.svc.id.goog" \
    "another cluster of the pool holding secrets-broker"
  passed "(ab) another cluster of the pool holding the broker's namespace exits 1"

  base_world
  subject_grant operator-cluster "$(sa_subject legion worker)" create "" serviceaccounts token secrets-broker secrets-broker
  run
  want_status 1 "an agent ServiceAccount that may create the broker's token"
  want_line "FINDING system:serviceaccount:legion:worker: create serviceaccounts/token secrets-broker in secrets-broker" \
    "an agent ServiceAccount that may create the broker's token"
  passed "(ab) an agent ServiceAccount that may create the broker ServiceAccount's token exits 1"
fi

# --- (ac) Every ServiceAccount of the operator's cluster, not only the agents'. ---
if row ac; then
  base_world
  add_sa operator-cluster ci runner
  subject_grant operator-cluster "$(sa_subject ci runner)" create "" pods "" envoy-operator ""
  run
  want_status 1 "ci/runner with create pods in envoy-operator"
  want_line "FINDING system:serviceaccount:ci:runner: create pods in envoy-operator" "ci/runner with create pods in envoy-operator"
  passed "(ac) ci/runner, outside the agent namespaces, with create pods in envoy-operator exits 1"

  base_world
  add_sa operator-cluster ci runner
  cluster_admin operator-cluster "$(sa_subject ci runner)"
  run
  want_status 1 "ci/runner with cluster-admin"
  want_line "FINDING system:serviceaccount:ci:runner: create serviceaccounts/token envoy-operator in envoy-operator" "ci/runner with cluster-admin"
  passed "(ac) ci/runner, outside the agent namespaces, with cluster-admin exits 1"
fi

# --- (ad) The new container.* permissions, through IAM. ---
new_container_row() {
  base_world
  role "projects/$project/roles/only.$1" "$1"
  iam "$project_res" "projects/$project/roles/only.$1" "serviceAccount:$worker_gsa"
  run
  want_status 1 "$1 held by $worker_gsa"
  want_line "FINDING serviceAccount:$worker_gsa: $1 on $project_res through projects/$project/roles/only.$1, granted at $project_res" "$1 held by $worker_gsa"
}
if row ad; then
  each new_container_row container.replicationControllers.create container.replicationControllers.update container.nodes.proxy
  passed "(ad) container.replicationControllers.create and update and container.nodes.proxy, each held by an agent's Google service account, exit 1"
fi

# --- (ae) Who can register a cluster to the pool's fleet. ---
if row ae; then
  base_world
  iam "$project_res" roles/gkehub.admin "serviceAccount:$ci_gsa"
  run
  want_status 1 "a CI account holding roles/gkehub.admin"
  want_line "FINDING serviceAccount:$ci_gsa: gkehub.memberships.create, gkehub.memberships.update on $project_res through roles/gkehub.admin, granted at $project_res" \
    "a CI account holding roles/gkehub.admin"
  passed "(ae) a CI account holding roles/gkehub.admin on the pool project exits 1"
fi

# --- (af) One rule for Groups: a Group other than the system ones is no finding in the code step. ---
if row af; then
  base_world
  edit "$world/render.json" '.items += [{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding",
    metadata: {name: "admins", namespace: "envoy-operator"},
    roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "cluster-admin"},
    subjects: [{apiGroup: "rbac.authorization.k8s.io", kind: "Group", name: "cluster-admins@example.com"}]}]'
  run
  want_status 0 "a render binding the Group cluster-admins@example.com to cluster-admin"
  [ "$(count_prefix 'FINDING ')" -eq 0 ] || fail "a render binding the Group cluster-admins@example.com to cluster-admin: a finding"
  passed "(af) a render binding a Group other than the system ones to cluster-admin exits 0, as the live steps judge it"
fi

# --- (ag) IAM impersonation: who can become a holder holds what it holds. ---
if row ag; then
  base_world
  iam "$project_res" roles/container.admin "serviceAccount:$applier"
  iam "$applier_res" roles/iam.serviceAccountTokenCreator "serviceAccount:$worker_gsa"
  run
  want_status 1 "an agent's Google service account minting the tokens of a container.admin holder"
  want_text "FINDING serviceAccount:$worker_gsa: container.serviceAccounts.createToken," "an agent's account minting a holder's tokens"
  want_text " on $project_res: serviceAccount:$worker_gsa -> impersonates serviceAccount:$applier (roles/iam.serviceAccountTokenCreator) -> roles/container.admin, granted at $project_res" \
    "an agent's account minting a holder's tokens"
  passed "(ag) an agent's Google service account holding roles/iam.serviceAccountTokenCreator on a holder is a finding naming the chain"

  base_world
  iam "$project_res" roles/container.admin "serviceAccount:$applier"
  iam "$applier_res" roles/iam.serviceAccountUser "serviceAccount:$relay"
  iam "$relay_res" roles/iam.serviceAccountTokenCreator "serviceAccount:$worker_gsa"
  run
  want_status 1 "an agent's Google service account two links from a container.admin holder"
  want_text " on $project_res: serviceAccount:$worker_gsa -> impersonates serviceAccount:$relay (roles/iam.serviceAccountTokenCreator) -> impersonates serviceAccount:$applier (roles/iam.serviceAccountUser) -> roles/container.admin, granted at $project_res" \
    "an agent's account two links from a holder"
  passed "(ag) a chain through two service accounts is followed to its outermost principal"
fi

# --- (ah) Who can write IAM policy over a checked resource holds every permission there. ---
iam_writer_row() {
  local permission="${1%%=*}" resource="${1#*=}"
  base_world
  role "projects/$project/roles/only.$permission" "$permission"
  iam "$resource" "projects/$project/roles/only.$permission" "serviceAccount:$worker_gsa"
  run
  want_status 1 "$permission held by $worker_gsa on $resource"
  want_line "FINDING serviceAccount:$worker_gsa: $permission on $resource through projects/$project/roles/only.$permission, granted at $resource" \
    "$permission held by $worker_gsa on $resource"
}
if row ah; then
  base_world
  iam "$project_res" roles/resourcemanager.projectIamAdmin "serviceAccount:$ci_gsa"
  run
  want_status 1 "a CI account holding roles/resourcemanager.projectIamAdmin"
  want_line "FINDING serviceAccount:$ci_gsa: resourcemanager.projects.setIamPolicy on $project_res through roles/resourcemanager.projectIamAdmin, granted at $project_res" \
    "a CI account holding roles/resourcemanager.projectIamAdmin"
  passed "(ah) a holder of roles/resourcemanager.projectIamAdmin is a finding"

  each iam_writer_row "iam.serviceAccounts.setIamPolicy=$gsa_res" "secretmanager.secrets.setIamPolicy=$signing_res" \
    "resourcemanager.organizations.setIamPolicy=$org_res" "iam.roles.update=$project_res"
  passed "(ah) a holder of each IAM-writing permission over a checked resource is a finding"
fi

# --- (ai) Who can become a ServiceAccount with a finding holds what it holds. ---
become_row() {
  local verb resource sub name
  IFS='|' read -r verb resource sub name <<<"${1%%=*}"
  base_world
  add_sa operator-cluster ci runner
  add_sa operator-cluster deploy bot
  subject_grant operator-cluster "$(sa_subject ci runner)" create "" pods "" envoy-operator ""
  subject_grant operator-cluster "$(sa_subject deploy bot)" "$verb" "" "$resource" "$sub" ci "$name"
  run
  want_status 1 "deploy/bot with ${1#*=} in ci"
  want_line "FINDING system:serviceaccount:deploy:bot: ${1#*=} in ci, so it can become system:serviceaccount:ci:runner, which can create pods in envoy-operator" \
    "deploy/bot with ${1#*=} in ci"
}
if row ai; then
  each become_row "create|pods||=create pods" "create|serviceaccounts|token|runner=create serviceaccounts/token runner" \
    "impersonate|serviceaccounts||runner=impersonate serviceaccounts runner" "create|pods|exec|=create pods/exec" "get|secrets||=get secrets"
  passed "(ai) a ServiceAccount that can become one with a finding, by each of the five ways in its namespace, is a finding naming the chain"
fi

# --- (aj) Anyone unauthenticated, on every cluster. ---
if row aj; then
  base_world
  add_cluster other-cluster us-east1
  subject_grant other-cluster "$(group_subject system:unauthenticated)" create "" namespaces "" "" ""
  run
  want_status 1 "system:unauthenticated may create namespaces on another cluster"
  want_line "FINDING user:system:anonymous: create namespaces cluster-wide on cluster $project/us-east1/other-cluster" \
    "system:unauthenticated may create namespaces on another cluster"
  passed "(aj) the Group system:unauthenticated bound to create namespaces on another cluster is a finding"
fi

# --- (ak) Custom resources in a guarded namespace, from the API server's discovery. ---
if row ak; then
  base_world
  echo sandboxes.agents.x-k8s.io >"$world/clusters/operator-cluster/api-resources"
  subject_grant operator-cluster "$(sa_subject legion worker)" create agents.x-k8s.io sandboxes "" envoy-operator ""
  run
  want_status 1 "legion/worker with create on sandboxes in envoy-operator"
  want_line "FINDING system:serviceaccount:legion:worker: create sandboxes.agents.x-k8s.io in envoy-operator" \
    "legion/worker with create on sandboxes in envoy-operator"
  passed "(ak) create on a custom resource in a guarded namespace is a finding"

  base_world
  subject_grant operator-cluster "$(sa_subject legion worker)" create coordination.k8s.io leases "" envoy-operator ""
  run
  want_status 0 "legion/worker with create on Kubernetes' own leases in envoy-operator"
  passed "(ak) create on a resource of Kubernetes' own groups is no custom resource"

  base_world
  : >"$world/clusters/operator-cluster/refuse-api-resources"
  run
  want_status 2 "a discovery that cannot be read whole"
  want_text "UNKNOWN live: RBAC on operator-cluster: the API server's discovery of namespaced resources could not be read whole" \
    "a discovery that cannot be read whole"
  passed "(ak) a discovery that cannot be read whole reads UNKNOWN"
fi

# --- (al) A report is this run's or none. ---
if row al; then
  base_world
  report="$temporary_dir/old-report.json"
  echo '{"run_at": "2000-01-01T00:00:00Z", "findings": [], "unknown": []}' >"$report"
  : >"$world/clusters/operator-cluster/garbage-serviceaccounts"
  run --report "$report"
  want_status 2 "a run that stops mid-way"
  want_text "UNKNOWN run: the run stopped before its end" "a run that stops mid-way"
  [ -s "$report" ] || fail "a run that stops mid-way left no report"
  jq -e '.run_at != "2000-01-01T00:00:00Z" and any(.unknown[]; .step == "run")' "$report" >/dev/null ||
    fail "a run that stops mid-way left this report, not its own UNKNOWN one: $(jq -c . "$report")"
  passed "(al) a run that stops mid-way leaves its own UNKNOWN report, not the old one"

  echo '{"run_at": "2000-01-01T00:00:00Z", "findings": [], "unknown": []}' >"$report"
  run --report "$report" --no-such-flag
  want_status 2 "a usage error"
  [ ! -e "$report" ] || fail "a usage error left the old report: $(cat "$report")"
  passed "(al) a usage error leaves no report"
fi

# --- (am) Each --agent-namespace pattern matches something, or reads UNKNOWN. ---
if row am; then
  base_world
  mapfile -t args < <(args_without --agent-namespace)
  run_only "${args[@]}" --agent-namespace legion --agent-namespace agent-sessions --agent-namespace 'legion-*-session'
  want_status 2 "a mistyped --agent-namespace beside two that match"
  want_line "UNKNOWN live: RBAC on operator-cluster: --agent-namespace legion-*-session matches no namespace that holds a ServiceAccount, so the agents it was to name are not reviewed as agents" \
    "a mistyped --agent-namespace beside two that match"
  passed "(am) an --agent-namespace pattern that matches nothing is UNKNOWN, naming it"
fi

[ "$rows" -gt 0 ] || { echo "FAIL: OPERATOR_MINTERS_TEST_ROWS=${OPERATOR_MINTERS_TEST_ROWS:-} names no row" >&2; exit 1; }
echo "all $rows rows passed"
