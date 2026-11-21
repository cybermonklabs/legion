# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-code.sh: the rows of operator-token-minters.test.sh on the code
# read: the RBAC render's grants and the IAM preview's bindings, each run with every live step
# refused (refuse_live), so each finding is the code's. Sourced by the test after its world and
# helpers; its header says what the fakes model. The rows:
#
#   (p)  the code read: a grant the RBAC render or an IAM preview declares is a finding with every
#        live step refused; a binding to a role the render does not hold, a member a preview does
#        not know, an IAM resource type the check does not read, and a missing render or preview,
#        each read UNKNOWN;
#   (af) a render binding the Group cluster-admins@example.com to cluster-admin exits 0 while the
#        operator's cluster has Google Groups for RBAC off: a Group other than Kubernetes' own is
#        judged by where the cluster takes its groups from, as in the live steps;
#   (ap) a render's rule limited by resourceNames on impersonate or escalate is a finding naming the
#        names, with every live step refused.

# --- (p) The code read. Every live step is refused, so each finding is the code's. ---
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
  passed "(af) a render binding a Group other than Kubernetes' own to cluster-admin exits 0 while Google Groups for RBAC is off, as the live steps judge it"
fi

# --- (ap) The code read of a rule limited by resource name. ---
if row ap; then
  base_world
  refuse_live
  edit "$world/render.json" '.items += [
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole", metadata: {name: "masters-and-view"},
     rules: [{apiGroups: [""], resources: ["groups"], verbs: ["impersonate"], resourceNames: ["system:masters"]},
             {apiGroups: ["rbac.authorization.k8s.io"], resources: ["clusterroles"], verbs: ["escalate"], resourceNames: ["view"]}]},
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", metadata: {name: "worker-masters"},
     roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "masters-and-view"},
     subjects: [{kind: "ServiceAccount", name: "worker", namespace: "legion"}]}]'
  run
  want_status 1 "a render's name-limited impersonate and escalate rules"
  want_line "FINDING system:serviceaccount:legion:worker: impersonate groups system:masters cluster-wide, declared by ClusterRoleBinding worker-masters" \
    "a render's name-limited impersonate rule"
  want_line "FINDING system:serviceaccount:legion:worker: escalate clusterroles.rbac.authorization.k8s.io view cluster-wide, declared by ClusterRoleBinding worker-masters" \
    "a render's name-limited escalate rule"
  passed "(ap) a render's rule limited by resourceNames on impersonate or escalate is a finding naming the names"
fi
