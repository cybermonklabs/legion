# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-pool.sh: the rows of operator-token-minters.test.sh on the
# pool's other clusters: the project's other clusters, the pool project's own clusters, and the
# members of the pool project's fleet, GKE or not. Sourced by the test after its world and
# helpers; its header says what the fakes model. The rows:
#
#   (g)  on a second cluster of the project, the namespace envoy-operator, the scratch namespace
#        envoy-operator-control, or a ServiceAccount outside the system namespaces allowed to make a
#        namespace each exit 1 naming that cluster; the system namespaces' own grants exit 0;
#   (r)  the pool project's fleet: a member that is no GKE cluster, listed on the fleet's second
#        page, whose cluster lets a ServiceAccount make a namespace, exits 1 naming the member (the
#        control is the script before this step, which misses it); a GKE member in another project
#        holding the namespace envoy-operator exits 1 naming the cluster; a fleet the API answers
#        empty, a listing with an unreachable location, a refused listing, a member whose
#        credentials are refused, and a project's cluster list gcloud says is incomplete each exit 2
#        with UNKNOWN; --pool-project naming another pool cannot read clean;
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
#   (an) each escalation right alone, given to ci/runner on another cluster of the pool (impersonate
#        users, groups and serviceaccounts, escalate and bind clusterroles, create, patch and update
#        clusterrolebindings cluster-wide; bind and escalate roles and create, patch and update
#        rolebindings in envoy-operator), exits 1 naming it;
#   (as) on a GKE fleet member in another project, apps/deployer given alone impersonate groups,
#        create clusterrolebindings, bind clusterroles, create or get nodes/proxy, or create pods in
#        kube-system (where namespace-controller may create namespaces) exits 1 naming it;
#   (az) with --pool-project example-pool, that project's own cluster holding the namespace
#        envoy-operator exits 1 naming it, and a pool project whose clusters cannot be listed reads
#        UNKNOWN;
#   (bm) a fleet scope, on the API listing's first page or its second, whose namespaces and RBAC role
#        bindings reach its bound members, and a refused scope listing each read UNKNOWN; a fleet
#        with no scope says so;
#   (bp) legacy ABAC on the operator's cluster, the identity service on another cluster of the
#        project, and legacy ABAC on a GKE fleet member in another project (from its describe) each
#        read UNKNOWN on that cluster's step;
#   (bt) on a GKE fleet member in another project, apps/deployer that may impersonate, by name,
#        dev@example.com, who holds roles/container.admin on that member's project alone, exits 1
#        naming the user and the member's cluster;
#   (bx) with --fleet none, an empty membership listing passes and a listed membership is a finding
#        naming it; --fleet takes none alone;
#   (by) a fleet member that is no GKE cluster reads UNKNOWN unless --rbac-only-member names it;
#   (bz) the operator's cluster on GKE Standard reads UNKNOWN naming the node escape.

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
  want_line "PASS $fleet_step: 2 membership(s) listed, every location reached: 1 the operator's cluster, 1 read above as clusters of the project, 0 read here, and the fleet holds no scope" \
    "a fleet member that is one of the project's clusters"
  passed "(r) a fleet member that is one of the project's clusters is read once, by the project's step"

  # A fleet the API answers with no membership.
  base_world
  edit "$world/fleet.json" '.memberships = []'
  run
  want_status 2 "an empty fleet"
  want_line "UNKNOWN $fleet_step: the GKE Hub API lists no membership in the fleet, so an answer that saw nothing cannot be told from a fleet that holds none (--fleet none says it has none)" \
    "an empty fleet"
  passed "(r) a fleet the API answers with no membership reads UNKNOWN, naming --fleet none"

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

# --- (an) The escalation rights on another cluster of the pool, each alone. Each entry is the grant
# (verb, API group, resource, the binding's namespace or none for cluster-wide) and the right the
# run must name. ---
escalation_rights=(
  "impersonate||users|=impersonate users cluster-wide"
  "impersonate||groups|=impersonate groups cluster-wide"
  "impersonate||serviceaccounts|=impersonate serviceaccounts cluster-wide"
  "escalate|rbac.authorization.k8s.io|clusterroles|=escalate clusterroles.rbac.authorization.k8s.io cluster-wide"
  "bind|rbac.authorization.k8s.io|clusterroles|=bind clusterroles.rbac.authorization.k8s.io cluster-wide"
  "create|rbac.authorization.k8s.io|clusterrolebindings|=create clusterrolebindings.rbac.authorization.k8s.io cluster-wide"
  "patch|rbac.authorization.k8s.io|clusterrolebindings|=patch clusterrolebindings.rbac.authorization.k8s.io cluster-wide"
  "update|rbac.authorization.k8s.io|clusterrolebindings|=update clusterrolebindings.rbac.authorization.k8s.io cluster-wide"
  "bind|rbac.authorization.k8s.io|roles|envoy-operator=bind roles.rbac.authorization.k8s.io in envoy-operator, guarding envoy-operator/envoy-operator"
  "escalate|rbac.authorization.k8s.io|roles|envoy-operator=escalate roles.rbac.authorization.k8s.io in envoy-operator, guarding envoy-operator/envoy-operator"
  "create|rbac.authorization.k8s.io|rolebindings|envoy-operator=create rolebindings.rbac.authorization.k8s.io in envoy-operator, guarding envoy-operator/envoy-operator"
  "patch|rbac.authorization.k8s.io|rolebindings|envoy-operator=patch rolebindings.rbac.authorization.k8s.io in envoy-operator, guarding envoy-operator/envoy-operator"
  "update|rbac.authorization.k8s.io|rolebindings|envoy-operator=update rolebindings.rbac.authorization.k8s.io in envoy-operator, guarding envoy-operator/envoy-operator"
  "create||nodes/proxy|=create nodes/proxy cluster-wide"
  "get||nodes/proxy|=get nodes/proxy cluster-wide"
)
escalation_row() {
  local right="${1#*=}" verb group resource sub="" namespace
  IFS='|' read -r verb group resource namespace <<<"${1%%=*}"
  case "$resource" in */*) sub="${resource#*/}" resource="${resource%%/*}" ;; esac
  base_world
  add_cluster other-cluster us-east1
  add_sa other-cluster ci runner
  subject_grant other-cluster "$(sa_subject ci runner)" "$verb" "$group" "$resource" "$sub" "$namespace" ""
  run
  want_status 1 "ci/runner with $right on other-cluster"
  want_line "FINDING system:serviceaccount:ci:runner: $right on cluster $project/us-east1/other-cluster" "ci/runner with $right on other-cluster"
  [ "$(count_prefix 'FINDING system:serviceaccount:ci:runner: ')" -eq "$(count_prefix 'FINDING ')" ] ||
    fail "ci/runner with $right on other-cluster: a finding names another principal"
}
if row an; then
  each escalation_row "${escalation_rights[@]}"
  passed "(an) each of the ${#escalation_rights[@]} escalation rights, given alone to ci/runner on another cluster of the pool, exits 1 naming it"
fi

# --- (as) The escalation rights, nodes/proxy and the system namespaces on a GKE fleet member in
# another project, each alone given to apps/deployer there. Each entry is the grant (verb, API group,
# resource[/subresource], the binding's namespace or none for cluster-wide) and the right the run
# must name. ---
member_rights=(
  "impersonate||groups|=impersonate groups cluster-wide"
  "create|rbac.authorization.k8s.io|clusterrolebindings|=create clusterrolebindings.rbac.authorization.k8s.io cluster-wide"
  "bind|rbac.authorization.k8s.io|clusterroles|=bind clusterroles.rbac.authorization.k8s.io cluster-wide"
  "create||nodes/proxy|=create nodes/proxy cluster-wide"
  "get||nodes/proxy|=get nodes/proxy cluster-wide"
  "create||pods|kube-system=create pods in kube-system, so it can become system:serviceaccount:kube-system:namespace-controller, which can create namespaces cluster-wide"
)
member_right_row() {
  local right="${1#*=}" verb group resource sub="" namespace
  IFS='|' read -r verb group resource namespace <<<"${1%%=*}"
  case "$resource" in */*) sub="${resource#*/}" resource="${resource%%/*}" ;; esac
  base_world
  grade_member
  subject_grant grade-cluster "$(sa_subject apps deployer)" "$verb" "$group" "$resource" "$sub" "$namespace" ""
  run
  want_status 1 "apps/deployer with $right on the fleet member grade"
  want_line "FINDING system:serviceaccount:apps:deployer: $right on cluster example-grade/us-east1/grade-cluster" \
    "apps/deployer with $right on the fleet member grade"
}
if row as; then
  each member_right_row "${member_rights[@]}"
  passed "(as) each of the ${#member_rights[@]} escalation and system-namespace rights, given alone to a ServiceAccount on a GKE fleet member in another project, exits 1 naming it"
fi

# --- (az) The pool project's own clusters, with --pool-project naming another project. ---
pool_row() {
  base_world
  case "$1" in
    holds)
      mkdir -p "$world/pool-clusters"
      jq -n '[{name: "pool-cluster", location: "us-west1", status: "RUNNING"}]' >"$world/pool-clusters/example-pool.json"
      make_cluster pool-cluster
      echo envoy-operator >>"$world/clusters/pool-cluster/namespaces"
      run --pool-project example-pool
      want_status 1 "the pool project's cluster holding envoy-operator"
      want_line "FINDING cluster:example-pool/us-west1/pool-cluster: namespace envoy-operator exists there, so its ServiceAccount envoy-operator/envoy-operator acts as $gsa through the workload pool example-pool.svc.id.goog" \
        "the pool project's cluster holding envoy-operator"
      ;;
    refused)
      run --pool-project other-pool
      [ "$status" -ne 0 ] || fail "a pool project whose clusters cannot be listed: exit 0"
      want_text "UNKNOWN live: clusters of the pool project other-pool: gcloud container clusters list --project other-pool was refused (it needs container.clusters.list)" \
        "a pool project whose clusters cannot be listed"
      ;;
  esac
}
if row az; then
  each pool_row holds refused
  passed "(az) the pool project's own cluster holding envoy-operator exits 1, and a pool project whose clusters cannot be listed reads UNKNOWN"
fi

# --- (bm) The fleet's scopes, from the GKE Hub API's projects/<pool>/locations/-/scopes, every page.
# A scope's namespaces and RBAC role bindings reach every member cluster bound to it, and this check
# does not analyse them, so a scope reads UNKNOWN. ---
scope_row() {
  base_world
  case "$1" in
    present)
      jq -n --arg p "$project" '[{name: "projects/\($p)/locations/global/scopes/team-a", uid: "uid-team-a"}]' >"$world/scopes.json"
      run
      want_status 2 "a fleet scope"
      want_line "UNKNOWN $fleet_step: the fleet holds the scope projects/$project/locations/global/scopes/team-a, whose namespaces and RBAC role bindings reach every member cluster bound to it, which this check does not analyse" \
        "a fleet scope"
      ;;
    second-page)
      jq -n --arg p "$project" '[{name: "projects/\($p)/locations/global/scopes/team-a"}, {name: "projects/\($p)/locations/us-east1/scopes/team-b"}]' \
        >"$world/scopes.json"
      run
      want_status 2 "a fleet scope on the listing's second page"
      want_text "UNKNOWN $fleet_step: the fleet holds the scope projects/$project/locations/us-east1/scopes/team-b," "a fleet scope on the listing's second page"
      ;;
    refused)
      : >"$world/refuse-scopes"
      run
      want_status 2 "a refused scope listing"
      want_text "UNKNOWN $fleet_step: the GKE Hub API refused the listing of projects/$project/locations/-/scopes (it needs gkehub.scopes.list there)" "a refused scope listing"
      ;;
    none)
      run
      want_status 0 "a fleet with no scope"
      want_text "PASS $fleet_step: 1 membership(s) listed, every location reached" "a fleet with no scope"
      want_text ", and the fleet holds no scope" "a fleet with no scope"
      ;;
  esac
}
if row bm; then
  each scope_row present second-page refused none
  passed "(bm) a fleet scope, on the listing's first page or its second, or a refused scope listing reads UNKNOWN; a fleet with none says so"
fi

# --- (bx) --fleet none: a deployment whose clusters are in no fleet says so, and an empty membership
# listing then passes; a membership listed beside it is a finding naming it. ---
fleet_none_row() {
  base_world
  case "$1" in
    empty)
      edit "$world/fleet.json" '.memberships = []'
      run --fleet none
      want_status 0 "--fleet none and an empty fleet"
      want_line "PASS $fleet_step: the GKE Hub API lists no membership in the fleet, as --fleet none says, and the fleet holds no scope" \
        "--fleet none and an empty fleet"
      ;;
    member)
      run --fleet none
      want_status 1 "--fleet none beside a membership"
      want_line "FINDING membership:$project/global/operator-cluster: is registered to the fleet of project $project, though --fleet none says it has no member" \
        "--fleet none beside a membership"
      ;;
    bad)
      run --fleet some
      want_status 2 "--fleet some"
      want_text "operator-token-minters: --fleet takes none and nothing else" "--fleet some"
      ! has_prefix "some" || fail "the usage error printed the value given to --fleet"
      ;;
  esac
}
if row bx; then
  each fleet_none_row empty member bad
  passed "(bx) --fleet none passes an empty membership listing and finds a listed membership; --fleet takes none alone"
fi

# --- (by) A fleet member that is no GKE cluster: its cloud's authorizers and its own token issuer can
# grant beside RBAC, which this check does not read, so it reads UNKNOWN unless --rbac-only-member
# names it. ---
rbac_only_row() {
  base_world
  edge_member
  case "$1" in
    unnamed)
      run
      want_status 2 "a non-GKE member not named RBAC-only"
      want_line "UNKNOWN live: Workload Identity on fleet member edge: it is no GKE cluster, so its own cloud's authorizers and token issuer may grant beside the RBAC read here; --rbac-only-member edge says it has none" \
        "a non-GKE member not named RBAC-only"
      ;;
    named)
      run --rbac-only-member edge
      want_status 0 "a non-GKE member named RBAC-only"
      want_text "PASS live: Workload Identity on fleet member edge: " "a non-GKE member named RBAC-only"
      ;;
  esac
}
if row by; then
  each rbac_only_row unnamed named
  passed "(by) a fleet member that is no GKE cluster reads UNKNOWN unless --rbac-only-member names it"
fi

# --- (bp) Cluster settings that authorize or authenticate outside RBAC and Google: legacy ABAC
# authorizes requests by its own policies, and the identity service for GKE takes users and groups
# from an outside provider, so neither is seen by a review. ---
settings_row() {
  local want
  base_world
  case "$1" in
    operator-abac)
      edit "$world/clusters.json" 'map(if .name == "operator-cluster" then . + {legacyAbac: {enabled: true}} else . end)'
      want="UNKNOWN live: RBAC on operator-cluster: legacy ABAC is on, so the cluster authorizes requests by its ABAC policies beside RBAC, which no review here sees"
      ;;
    other-identity)
      add_cluster other-cluster us-east1
      edit "$world/clusters.json" 'map(if .name == "other-cluster" then . + {identityServiceConfig: {enabled: true}} else . end)'
      want="UNKNOWN live: Workload Identity on $project/us-east1/other-cluster: the identity service for GKE is on, so the cluster takes users and groups from an outside identity provider, whose members this check cannot list"
      ;;
    member-abac)
      grade_member
      edit "$world/fleet.json" '.memberships |= map(if .id == "grade" then . + {settings: {legacyAbac: {enabled: true}}} else . end)'
      want="UNKNOWN $grade_step: legacy ABAC is on, so the cluster authorizes requests by its ABAC policies beside RBAC, which no review here sees"
      ;;
  esac
  run
  want_status 2 "a cluster setting outside RBAC ($1)"
  want_line "$want" "a cluster setting outside RBAC ($1)"
}
if row bp; then
  each settings_row operator-abac other-identity member-abac
  passed "(bp) legacy ABAC or the identity service on the operator's cluster, another of the project, or a GKE fleet member reads UNKNOWN"
fi

# --- (bt) Impersonating, by name, a user whose power is the member project's IAM: GKE's IAM authorizer
# on the member judges an impersonated user by that project's IAM, so the member's own project's
# holders are asked by name on its cluster, as --project's are on every cluster. ---
if row bt; then
  base_world
  grade_member
  iam "$grade_res" roles/container.admin user:dev@example.com
  subject_grant grade-cluster "$(sa_subject apps deployer)" impersonate "" users "" "" dev@example.com
  run
  want_status 1 "apps/deployer may impersonate dev@example.com by name on the fleet member"
  want_line "FINDING system:serviceaccount:apps:deployer: impersonate users dev@example.com cluster-wide on cluster example-grade/us-east1/grade-cluster" \
    "apps/deployer may impersonate dev@example.com by name on the fleet member"
  passed "(bt) who may impersonate, by name, a holder of a fleet member project's GKE IAM permissions on that member exits 1 naming the user"
fi

# --- (bz) The operator's cluster on GKE Standard: a privileged or hostPath pod on its node reads the
# operator's token by a path no review sees, which Autopilot refuses. ---
if row bz; then
  base_world
  edit "$world/clusters.json" 'map(if .name == "operator-cluster" then del(.autopilot) else . end)'
  run
  want_status 2 "the operator's cluster on GKE Standard"
  want_line "UNKNOWN live: RBAC on operator-cluster: the cluster is GKE Standard, not Autopilot, so a privileged or hostPath pod on the operator's node can read the kubelet's credentials and the operator's ServiceAccount token there, a node escape no RBAC review here sees" \
    "the operator's cluster on GKE Standard"
  passed "(bz) the operator's cluster on GKE Standard reads UNKNOWN naming the node escape"
fi
