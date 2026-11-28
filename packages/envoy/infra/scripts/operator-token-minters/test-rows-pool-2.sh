# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-pool-2.sh: the rows of operator-token-minters.test.sh on the
# pool's other clusters: a fleet member's binding subjects and the guarded pairs there, a second
# cluster of the operator's cluster's name, and the escalation rights on another cluster of the pool
# and on a GKE fleet member in another project. Sourced by the test after its world and helpers; its
# header says what the fakes model. The rows:
#
#   (s)  a binding subject on a fleet member: grade-ci, a Google service account, bound to
#        cluster-admin on a GKE member in another project exits 1 naming grade-ci and create
#        namespaces; a person's email bound the same way, and a Group bound the same way while the
#        member has Google Groups for RBAC off, exit 0; a refused list of bindings exits 2 with
#        UNKNOWN;
#   (w)  two guarded pairs (--guarded envoy-route/envoy-route-password-reader beside the
#        operator's): grade-ci bound to cluster-admin on a fleet member is reported for both;
#   (x)  a ServiceAccount on a fleet member that can create pods, and nothing else, in the
#        operator's namespace is reported, naming the operator's pair;
#   (y)  the same in the route password reader's namespace, naming that pair;
#   (aa) a second cluster of the project named operator-cluster, in us-east1, holding the namespace
#        envoy-operator, is a finding with --location us-central1, and UNKNOWN without it;
#   (an) each escalation right (impersonate users, groups and serviceaccounts, escalate and bind
#        clusterroles, create, patch and update clusterrolebindings cluster-wide; bind and escalate
#        roles and create, patch and update rolebindings in envoy-operator; create and get
#        nodes/proxy), given on another cluster of the pool to a ServiceAccount that holds nothing
#        else, is a finding naming it; no finding of that ServiceAccount names another planted
#        account's right or a right of another verb or resource, and no other principal has a
#        finding (one world, one ServiceAccount a right);
#   (as) on a GKE fleet member in another project, impersonate groups, create clusterrolebindings,
#        bind clusterroles, create or get nodes/proxy, or create pods in kube-system (where
#        namespace-controller may create namespaces), each given to a ServiceAccount that holds
#        nothing else, is a finding naming it, and no finding of that ServiceAccount names another
#        planted account's right or a right of another verb or resource (one world, one
#        ServiceAccount a right).

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
  passed "(s) a Group bound to cluster-admin on a fleet member with Google Groups for RBAC off is no finding"

  base_world
  grade_member
  : >"$world/clusters/grade-cluster/refuse-bindings"
  run
  want_status 2 "a fleet member whose bindings cannot be listed"
  want_text "UNKNOWN $grade_step: the API server refused the bindings' list (it needs list on clusterrolebindings and rolebindings" \
    "a fleet member whose bindings cannot be listed"
  passed "(s) a fleet member whose bindings cannot be listed exits 2 with UNKNOWN naming the permission"
fi

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

# --- (an) The escalation rights on another cluster of the pool, each given to a ServiceAccount of
# its own there that holds nothing else, ci/right-<its place in the list>, all in one world and one
# run (plant_rights, want_planted). Each entry is the grant (verb, API group, resource, subresource,
# the binding's namespace or none for cluster-wide, the resource name or none) and the right the
# run must name. ---
escalation_rights=(
  "impersonate||users||||=impersonate users cluster-wide"
  "impersonate||groups||||=impersonate groups cluster-wide"
  "impersonate||serviceaccounts||||=impersonate serviceaccounts cluster-wide"
  "escalate|rbac.authorization.k8s.io|clusterroles||||=escalate clusterroles.rbac.authorization.k8s.io cluster-wide"
  "bind|rbac.authorization.k8s.io|clusterroles||||=bind clusterroles.rbac.authorization.k8s.io cluster-wide"
  "create|rbac.authorization.k8s.io|clusterrolebindings||||=create clusterrolebindings.rbac.authorization.k8s.io cluster-wide"
  "patch|rbac.authorization.k8s.io|clusterrolebindings||||=patch clusterrolebindings.rbac.authorization.k8s.io cluster-wide"
  "update|rbac.authorization.k8s.io|clusterrolebindings||||=update clusterrolebindings.rbac.authorization.k8s.io cluster-wide"
  "bind|rbac.authorization.k8s.io|roles||envoy-operator|=bind roles.rbac.authorization.k8s.io in envoy-operator, guarding envoy-operator/envoy-operator"
  "escalate|rbac.authorization.k8s.io|roles||envoy-operator|=escalate roles.rbac.authorization.k8s.io in envoy-operator, guarding envoy-operator/envoy-operator"
  "create|rbac.authorization.k8s.io|rolebindings||envoy-operator|=create rolebindings.rbac.authorization.k8s.io in envoy-operator, guarding envoy-operator/envoy-operator"
  "patch|rbac.authorization.k8s.io|rolebindings||envoy-operator|=patch rolebindings.rbac.authorization.k8s.io in envoy-operator, guarding envoy-operator/envoy-operator"
  "update|rbac.authorization.k8s.io|rolebindings||envoy-operator|=update rolebindings.rbac.authorization.k8s.io in envoy-operator, guarding envoy-operator/envoy-operator"
  "create||nodes|proxy|||=create nodes/proxy cluster-wide"
  "get||nodes|proxy|||=get nodes/proxy cluster-wide"
)
if row an; then
  base_world
  add_cluster other-cluster us-east1
  plant_rights other-cluster ci " on cluster $project/us-east1/other-cluster" "${escalation_rights[@]}"
  run
  want_planted "each escalation right on other-cluster given to a ServiceAccount of its own" only
  passed "(an) each of the ${#escalation_rights[@]} escalation rights, given on another cluster of the pool to a ServiceAccount that holds nothing else, is a finding naming it; no finding of that ServiceAccount names another planted account's right or a right of another verb or resource, and no other principal has a finding"
fi

# --- (as) The escalation rights, nodes/proxy and the system namespaces on a GKE fleet member in
# another project, each given to a ServiceAccount of its own there that holds nothing else,
# apps/right-<its place in the list>, all in one world and one run. Each entry is the grant, as
# (an)'s are, and the right the run must name. ---
member_rights=(
  "impersonate||groups||||=impersonate groups cluster-wide"
  "create|rbac.authorization.k8s.io|clusterrolebindings||||=create clusterrolebindings.rbac.authorization.k8s.io cluster-wide"
  "bind|rbac.authorization.k8s.io|clusterroles||||=bind clusterroles.rbac.authorization.k8s.io cluster-wide"
  "create||nodes|proxy|||=create nodes/proxy cluster-wide"
  "get||nodes|proxy|||=get nodes/proxy cluster-wide"
  "create||pods||kube-system|=create pods in kube-system, so it can become system:serviceaccount:kube-system:namespace-controller, which can create namespaces cluster-wide"
)
if row as; then
  base_world
  grade_member
  plant_rights grade-cluster apps " on cluster example-grade/us-east1/grade-cluster" "${member_rights[@]}"
  run
  want_planted "each right on the fleet member grade given to a ServiceAccount of its own" own
  passed "(as) each of the ${#member_rights[@]} escalation and system-namespace rights, given on a GKE fleet member in another project to a ServiceAccount that holds nothing else, is a finding naming it, and no finding of that ServiceAccount names another planted account's right or a right of another verb or resource"
fi
