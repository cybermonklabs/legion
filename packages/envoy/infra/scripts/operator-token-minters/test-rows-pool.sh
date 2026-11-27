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
#        namespaces, and so does a person's email bound the same way, naming the person; a Group
#        bound the same way while the member has Google Groups for RBAC off exits 0; a refused list
#        of bindings exits 2 with UNKNOWN;
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
#        ServiceAccount a right);
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
#   (hd) a structured SERVICE_DISABLED answer for GKE Hub in the pool project reads UNKNOWN naming
#        it, with --fleet none too, and the audited clusters step with it; PERMISSION_DENIED and
#        SERVICE_DISABLED for another service or project stay UNKNOWN; control: a copy that reads
#        the disabled API as a listing of none passes, exit 0;
#   (bz) the operator's cluster on GKE Standard reads UNKNOWN naming the node escape;
#   (cw) --audited-cluster: the pool's GKE clusters all named pass; a cluster of the project, and a
#        GKE member of the fleet in another project, that none names are each a finding naming it,
#        which a copy of the check without the step misses; a named cluster the pool lacks, a refused
#        or incomplete cluster list and a refused fleet listing read UNKNOWN; a cluster named twice
#        is a usage error; without the flag the step does not run.
#   (ne) another cluster of the pool on GKE Standard: a system ServiceAccount with a running pod on
#        a node is reviewed for every right the cluster's review asks: allowed create namespaces,
#        get nodes/proxy (outright, or by a rule a live role limits to one node's name) or
#        impersonate users, it is a finding naming it; the certificate chain's approval alone is
#        none; the same account on Autopilot, with no pod, or with a pod bound to no node, is no
#        finding; its review ending in an evaluation error, a pods' list that holds no pod (or a
#        List of no item), a refused describe, and a refused pods' list there, read UNKNOWN;
#        controls: a copy that asks a node's account only the namespace, ServiceAccount and pod
#        rights misses nodes/proxy, one that skips an evaluation error and one without the census
#        witness each exit 0;
#   (lb) the operator's cluster's findings carry cluster:<project>/<location>/<name>, apart from a
#        cluster of its name in another location; a copy using the bare name fails the row;
#   (gn) a guarded namespace on another cluster: the cluster is reviewed with the operator's
#        cluster's rights, a holder named with the cluster's label (a right the guard rights lack,
#        the operator's token); with no guarded namespace the guard rights' review stands; a copy
#        without the operator's rights misses the holder.

# --- (g) A second cluster of the project. ---
if row g; then
  base_world
  add_cluster other-cluster us-east1
  run
  want_status 0 "a second cluster whose system namespaces alone may make namespaces"
  want_text "PASS live: Workload Identity on example-project/us-east1/other-cluster: " "a clean second cluster"
  passed "(g) a second cluster whose system namespaces alone may make namespaces and ServiceAccounts exits 0"

  # A guarded namespace there names no principal: the cluster is reviewed with the operator's
  # cluster's rights, and the namespace reported as reviewed, never as a finding of its own.
  base_world
  add_cluster other-cluster us-east1
  edit "$world/clusters.json" 'map(if .name == "other-cluster" then . + {autopilot: {enabled: true}} else . end)'
  echo envoy-operator >>"$world/clusters/other-cluster/namespaces"
  run
  want_status 0 "the namespace envoy-operator on a second Autopilot cluster with no holder"
  want_line "REVIEWED cluster:example-project/us-east1/other-cluster: namespace envoy-operator exists there, so its ServiceAccount envoy-operator/envoy-operator acts as $gsa through the workload pool $project.svc.id.goog; the cluster is reviewed with the operator's cluster's rights" \
    "the namespace envoy-operator on a second cluster"
  ! has_prefix "FINDING cluster:example-project/us-east1/other-cluster: namespace envoy-operator exists there" ||
    fail "the namespace envoy-operator on a second cluster is still a finding that names no principal"
  passed "(g) the namespace envoy-operator on a second cluster makes it reviewed with the operator's rights, reported as reviewed, no finding of its own"

  base_world
  add_cluster other-cluster us-east1
  echo envoy-operator-control >>"$world/clusters/other-cluster/namespaces"
  run --namespace envoy-operator-control --gsa "$control_gsa"
  # Not 2: with --gsa naming the scratch account, the world's signing-key grants to the real
  # operator account are findings of their own, so the run exits 1 beside the UNKNOWN.
  [ "$status" -ne 0 ] || fail "the scratch namespace on a second Standard cluster: exit 0"
  want_text "REVIEWED cluster:example-project/us-east1/other-cluster: namespace envoy-operator-control exists there" "the scratch namespace on a second cluster"
  want_text "UNKNOWN live: Workload Identity on example-project/us-east1/other-cluster: the cluster is not GKE Autopilot and holds the guarded namespace envoy-operator-control" \
    "the scratch namespace on a second Standard cluster"
  passed "(g) the scratch namespace envoy-operator-control on a second Standard cluster is reviewed, and its node escape reads UNKNOWN (the plan's control)"

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
  want_status 2 "a GKE fleet member in another project holding the namespace envoy-operator"
  want_line "REVIEWED cluster:other-project/us-east1/shared-cluster: namespace envoy-operator exists there, so its ServiceAccount envoy-operator/envoy-operator acts as $gsa through the workload pool $project.svc.id.goog; the cluster is reviewed with the operator's cluster's rights" \
    "a GKE fleet member in another project holding the namespace envoy-operator"
  passed "(r) a GKE fleet member in another project holding the namespace envoy-operator is reviewed with the operator's rights"

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
  want_status 1 "a person bound to cluster-admin on a fleet member"
  want_line "FINDING user:alice@example.com: create namespaces cluster-wide on cluster example-grade/us-east1/grade-cluster" \
    "a person bound to cluster-admin on a fleet member"
  ! has_prefix "PASS $grade_step: " || fail "a person bound to cluster-admin on a fleet member: the member's step passed"
  passed "(s) a person's email bound to cluster-admin on a fleet member is reviewed and exits 1 naming the person"

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
  want_line "REVIEWED cluster:$project/us-east1/operator-cluster: namespace envoy-operator exists there, so its ServiceAccount envoy-operator/envoy-operator acts as $gsa through the workload pool $project.svc.id.goog; the cluster is reviewed with the operator's cluster's rights" \
    "a second operator-cluster holding envoy-operator, --location us-central1"
  ! has_prefix "REVIEWED cluster:$project/us-central1/operator-cluster" || fail "the operator's own cluster was read as another"
  [ "$(count_prefix 'FINDING ')" -eq 0 ] || fail "a second operator-cluster holding envoy-operator, with no holder: want no finding"
  passed "(aa) a second cluster named operator-cluster elsewhere, holding envoy-operator, is reviewed as another cluster with --location"

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

# --- (lb) The operator's cluster's label is <project>/<location>/<name>, as every other cluster's:
# with two clusters named operator-cluster in two locations, each finding names its own, and a copy
# labelling the operator's cluster by its bare name is caught. ---
label_row() {
  local script
  base_world
  add_cluster_at operator-cluster us-east1
  echo envoy-operator >>"$world/clusters/operator-cluster@us-east1/namespaces"
  subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods exec envoy-operator ""
  subject_grant operator-cluster@us-east1 "$(sa_subject apps deployer)" create "" pods exec envoy-operator ""
  case "$1" in
    full) script="$check" ;;
    bare)
      script="$(script_copy bare-label)"
      plant "$(dirname "$script")/operator-token-minters/step-rbac.sh" '  op_res="$(operator_res)"' '  op_res="cluster:$CLUSTER"'
      ;;
  esac
  run_script="$script" run --location us-central1 --report "$temporary_dir/label.json"
  jq -e --arg op "cluster:$project/us-central1/operator-cluster" --arg other "cluster:$project/us-east1/operator-cluster" '
    any(.findings[]; .principal == "system:serviceaccount:legion:worker" and .resource == $op)
    and any(.findings[]; .principal == "system:serviceaccount:apps:deployer" and .resource == $other)' "$temporary_dir/label.json" >/dev/null
}
if row lb; then
  label_row full || fail "the operator's cluster's finding is not labelled cluster:$project/us-central1/operator-cluster beside the other cluster's cluster:$project/us-east1/operator-cluster"
  ! label_row bare || fail "a copy labelling the operator's cluster by its bare name still passed the label check"
  passed "(lb) the operator's cluster is labelled cluster:<project>/<location>/<name>, apart from a cluster of its name elsewhere; a bare-name copy is caught"
fi

# --- (zc) One report, one label form: a grant the render declares and a live grant on the
# operator's cluster, and the fence's declared and live credits and residuals, each name the
# cluster cluster:<project>/<location>/<name>, with --location and without it (the project's list
# gives the location the code step could not know yet). No resource in the report is a bare
# cluster name. Controls: a copy whose code step labels its findings by the bare name, and one
# whose fence credits a declared grant by it, each fail the row. ---
cluster_label_row() {
  local script="$check" args=(--location us-central1) copy
  case "$1" in
    fence | fence-no-location | fence-bare)
      fence_world
      script="$fence_check"
      ;;
    *) base_world ;;
  esac
  case "$1" in
    code-live | code-live-no-location | code-bare)
      edit "$world/render.json" '.items += [{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding",
        metadata: {name: "bot", namespace: "envoy-operator"},
        roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "cluster-admin"},
        subjects: [{apiGroup: "rbac.authorization.k8s.io", kind: "User", name: "deployer-bot"}]}]'
      subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods exec envoy-operator ""
      ;;
  esac
  case "$1" in
    *-no-location) args=() ;;
    code-bare)
      copy="$(script_copy code-bare-label)"
      plant "$(dirname "$copy")/operator-token-minters/step-code-rbac.sh" '"$2" "$(operator_res)" "$2" "$3"; }' '"$2" "cluster:$cluster_name" "$2" "$3"; }'
      script="$copy"
      ;;
    fence-bare)
      copy="$(script_copy fence-bare-label)"
      cp "$(dirname "$fence_check")/operator-token-minters/broker-fence.json" "$(dirname "$copy")/operator-token-minters/broker-fence.json"
      plant "$(dirname "$copy")/operator-token-minters/broker-fence.sh" '"$detail, declared by $id" "$(operator_res)"' '"$detail, declared by $id" "cluster:$cluster_name"'
      script="$copy"
      ;;
  esac
  case "$1" in
    fence*) run_script="$script" fence_run ${args[@]+"${args[@]}"} --report "$temporary_dir/labels-$1.json" ;;
    *) run_script="$script" run ${args[@]+"${args[@]}"} --report "$temporary_dir/labels-$1.json" ;;
  esac
  local report="$temporary_dir/labels-$1.json" why=""
  case "$1" in
    code*)
      want_status 1 "a declared and a live finding ($1)"
      jq -e --arg op "cluster:$project/us-central1/operator-cluster" '
        any(.findings[]; .principal == "user:deployer-bot" and .resource == $op)
        and any(.findings[]; .principal == "system:serviceaccount:legion:worker" and .resource == $op)' "$report" >/dev/null ||
        why="the declared or the live finding does not name the operator's cluster in full: $(jq -c '[.findings[] | {principal, resource}]' "$report")"
      ;;
    fence*)
      want_status 0 "the fence's credits ($1)"
      jq -e --arg op "cluster:$project/us-central1/operator-cluster" '
        (.credited | length) > 0 and (.residuals | length) > 0 and all(.credited[], .residuals[]; .resource == $op)
        and any(.credited[]; .right | contains(", declared by "))' "$report" >/dev/null ||
        why="a credit or residual does not name the operator's cluster in full: $(jq -c '[.credited[], .residuals[] | {principal, right, resource}]' "$report")"
      ;;
  esac
  [ -n "$why" ] || [ -z "$(full_labels "$report")" ] || why="the report holds a cluster label not in full: $(full_labels "$report")"
  case "$1" in
    *-bare) [ -n "$why" ] || fail "a copy labelling by the bare name ($1) passed the label check" ;;
    *) [ -z "$why" ] || fail "$why ($1)" ;;
  esac
}
if row zc; then
  each cluster_label_row code-live code-live-no-location fence fence-no-location
  passed "(zc) a declared and a live finding, and the fence's declared and live credits and residuals, name the operator's cluster cluster:$project/us-central1/operator-cluster, with --location and without it, and the report holds no bare cluster name"
  each cluster_label_row code-bare fence-bare
  passed "(zc) control: a copy whose code step, or whose fence's declared credit, labels by the bare name fails the label check"
fi

# --- (gn) A guarded namespace on another cluster of the pool makes that cluster reviewed with the
# operator's cluster's rights, each holder named with the cluster's label; without one, the cluster
# keeps the guard rights' review; a copy that leaves the operator's rights out misses the holder,
# and a copy that still reports the namespace as a finding naming no principal is caught. ---
guarded_ns_row() {
  local script="$check" want="FINDING system:serviceaccount:apps:deployer: create serviceaccounts/token envoy-operator in envoy-operator on cluster $project/us-east1/other-cluster"
  local bare="FINDING cluster:$project/us-east1/other-cluster: namespace envoy-operator exists there"
  base_world
  add_cluster other-cluster us-east1
  edit "$world/clusters.json" 'map(if .name == "other-cluster" then . + {autopilot: {enabled: true}} else . end)'
  subject_grant other-cluster "$(sa_subject apps deployer)" create "" serviceaccounts token envoy-operator envoy-operator
  case "$1" in
    present | copy | old-finding) echo envoy-operator >>"$world/clusters/other-cluster/namespaces" ;;
  esac
  case "$1" in
    copy)
      script="$(script_copy no-operator-rights)"
      sed -i '/^  \[ "\${#present\[@\]}" -eq 0 \] || guard_rights=/d' "$(dirname "$script")/operator-token-minters/step-workload-identity.sh"
      ! grep -qF '[ "${#present[@]}" -eq 0 ] || guard_rights=' "$(dirname "$script")/operator-token-minters/step-workload-identity.sh" ||
        fail "the test could not take the operator's rights out of its copy"
      ;;
    old-finding)
      # The rule before this change: the namespace itself a finding, its principal the cluster.
      script="$(script_copy namespace-finding)"
      sed -i '/^    echo "REVIEWED \$principal: namespace /a\    finding "$principal" "namespace ${pair%%/*} exists there" namespace "$principal" "" ""' \
        "$(dirname "$script")/operator-token-minters/step-workload-identity.sh"
      grep -qF 'finding "$principal" "namespace ${pair%%/*} exists there"' "$(dirname "$script")/operator-token-minters/step-workload-identity.sh" ||
        fail "the test could not put the old finding into its copy"
      ;;
  esac
  # Each item's report is its own world's: the items run at once (each), and one shared path let
  # another item's report be read as this one's (a present item failed so under load, 2026-11-27).
  run_script="$script" run --report "$world/guarded-ns.json"
  case "$1" in
    present)
      want_status 1 "a holder of the operator's token in envoy-operator on another cluster"
      want_line "$want" "a holder of the operator's token in envoy-operator on another cluster"
      ! has_prefix "$bare" || fail "the namespace on another cluster is still a finding that names no principal"
      jq -e --arg res "cluster:$project/us-east1/other-cluster" \
        'any(.findings[]; .principal == "system:serviceaccount:apps:deployer" and .resource == $res)' "$world/guarded-ns.json" >/dev/null ||
        fail "the holder's finding does not carry the cluster's label cluster:$project/us-east1/other-cluster"
      ;;
    absent)
      want_status 0 "the same grant on a cluster with no guarded namespace"
      ! has_prefix "$want" || fail "the operator's rights were asked on a cluster with no guarded namespace"
      want_text "PASS live: Workload Identity on $project/us-east1/other-cluster: no guarded namespace" "the same grant on a cluster with no guarded namespace"
      ;;
    copy) ! has_prefix "$want" || fail "a copy without the operator's rights still named the holder" ;;
    old-finding) has_prefix "$bare" || fail "a copy reporting the namespace as a finding was not caught by the principal-less check" ;;
  esac
}
if row gn; then
  each guarded_ns_row present absent copy old-finding
  passed "(gn) a guarded namespace on another cluster brings the operator's rights there, naming each holder with the cluster's label and no principal-less finding; without one the guard rights' review stands; copies without the operator's rights, or with the old namespace finding, are caught"
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
      [ "$status" -ne 0 ] || fail "the pool project's cluster holding envoy-operator: exit 0"
      want_line "REVIEWED cluster:example-pool/us-west1/pool-cluster: namespace envoy-operator exists there, so its ServiceAccount envoy-operator/envoy-operator acts as $gsa through the workload pool example-pool.svc.id.goog; the cluster is reviewed with the operator's cluster's rights" \
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

# --- (hd) A disabled GKE Hub API is no empty fleet: Google keeps a disabled service's memberships,
# and whoever holds serviceusage.services.disable on the pool project, which no step reads, can turn
# it off, so it reads UNKNOWN, with --fleet none too. ---
hub_disabled_row() {
  local copy script="$check" disabled="UNKNOWN $fleet_step: the GKE Hub API is disabled in project $project (SERVICE_DISABLED), and refused the listing of projects/$project/locations/-/memberships"
  base_world
  case "$1" in
    disabled | disabled-fleet-none | disabled-audited | disabled-control)
      : >"$world/fleet-service-disabled"
      ;;
  esac
  case "$1" in
    disabled | disabled-fleet-none)
      if [ "$1" = disabled ]; then run --report "$temporary_dir/hub-disabled-$1.json"; else run --fleet none --report "$temporary_dir/hub-disabled-$1.json"; fi
      want_status 2 "a disabled GKE Hub API ($1)"
      want_text "$disabled" "a disabled GKE Hub API ($1)"
      ! has_prefix "PASS $fleet_step: " || fail "a disabled GKE Hub API ($1): the fleet step passed"
      jq -e 'all(.allowances[]; .flag != "gkehub-service-disabled")' "$temporary_dir/hub-disabled-$1.json" >/dev/null ||
        fail "a disabled GKE Hub API ($1) is still an allowance in --report"
      ;;
    disabled-audited)
      run --fleet none --audited-cluster "$project/us-central1/operator-cluster"
      want_status 2 "a disabled GKE Hub API beside --audited-cluster"
      want_text "UNKNOWN live: audited clusters: the fleet's memberships were not read whole" "a disabled GKE Hub API beside --audited-cluster"
      ;;
    disabled-control)
      copy="$(script_copy hub-disabled-as-none)"
      plant "$(dirname "$copy")/operator-token-minters/step-workload-identity.sh" \
        'HUB_ERROR="the GKE Hub API is disabled in project $POOL_PROJECT (SERVICE_DISABLED)' \
        'printf '"'"'{"resources":[]}\n'"'"' >"$3"; return 0; HUB_ERROR="the GKE Hub API is disabled in project $POOL_PROJECT (SERVICE_DISABLED)'
      run_script="$copy" run --fleet none
      want_status 0 "a copy reading a disabled GKE Hub API as none"
      want_text "PASS $fleet_step: the GKE Hub API lists no membership in the fleet, as --fleet none says" "a copy reading a disabled GKE Hub API as none"
      ;;
    denied)
      : >"$world/refuse-fleet"
      run
      want_status 2 "a GKE Hub permission denial"
      want_text "UNKNOWN $fleet_step: the GKE Hub API refused the listing" "a GKE Hub permission denial"
      ;;
    other-service)
      : >"$world/fleet-service-disabled-other-service"
      run
      want_status 2 "another disabled service"
      want_text "UNKNOWN $fleet_step: the GKE Hub API refused the listing" "another disabled service"
      ;;
    other-project)
      : >"$world/fleet-service-disabled-other-project"
      run
      want_status 2 "GKE Hub disabled in another project"
      want_text "UNKNOWN $fleet_step: the GKE Hub API refused the listing" "GKE Hub disabled in another project"
      ;;
  esac
}
if row hd; then
  each hub_disabled_row disabled disabled-fleet-none disabled-audited denied other-service other-project disabled-control
  passed "(hd) GKE Hub SERVICE_DISABLED in the pool project reads UNKNOWN naming it, with --fleet none and beside --audited-cluster too, as does any other refusal; a copy reading it as none passes, exit 0"
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

# --- (cw) The clusters the auditor was given are the pool's GKE clusters. ---
readonly own_cluster="$project/us-central1/operator-cluster"
audited_row() {
  base_world
  local copy
  case "$1" in
    match)
      run --audited-cluster "$own_cluster"
      want_status 0 "the pool's one cluster, named"
      want_line "PASS live: audited clusters: the 1 cluster(s) given are the pool's GKE clusters: $own_cluster" "the pool's one cluster, named"
      ;;
    missing)
      add_cluster other-cluster us-east1
      run --audited-cluster "$own_cluster"
      want_text "FINDING cluster:$project/us-east1/other-cluster: is a GKE cluster of the pool that no --audited-cluster names" \
        "a cluster of the project no --audited-cluster names"
      run --audited-cluster "$own_cluster" --audited-cluster "$project/us-east1/other-cluster"
      ! has_prefix "FINDING cluster:" || fail "a cluster of the project named by --audited-cluster is still a finding"
      want_text "PASS live: audited clusters: the 2 cluster(s) given" "both clusters named"
      ;;
    fleet)
      grade_member
      run --audited-cluster "$own_cluster"
      want_text "FINDING cluster:example-grade/us-east1/grade-cluster: is a GKE cluster of the pool that no --audited-cluster names" \
        "a GKE member of the fleet no --audited-cluster names"
      run --audited-cluster "$own_cluster" --audited-cluster example-grade/us-east1/grade-cluster
      ! has_prefix "FINDING cluster:" || fail "a fleet member named by --audited-cluster is still a finding"
      ;;
    control)
      add_cluster other-cluster us-east1
      copy="$(script_copy audited-clusters-control)"
      sed -i '\#^\. "\$LIB/step-audited-clusters.sh"$#d' "$copy"
      ! grep -qF 'step-audited-clusters.sh"' "$copy" || fail "the test could not take the step out of its copy"
      run_script="$copy" run --audited-cluster "$own_cluster"
      ! has_prefix "FINDING cluster:" || fail "a copy without the step still named the cluster"
      ;;
    extra)
      run --audited-cluster "$own_cluster" --audited-cluster "$project/us-west1/gone-cluster"
      want_status 2 "a named cluster the pool lacks"
      want_text "UNKNOWN live: audited clusters: --audited-cluster $project/us-west1/gone-cluster is not among the pool's GKE clusters" \
        "a named cluster the pool lacks"
      ;;
    refused)
      : >"$world/refuse-clusters"
      run --audited-cluster "$own_cluster"
      want_status 2 "a refused cluster list"
      want_text "UNKNOWN live: audited clusters: the project's cluster list was not read whole" "a refused cluster list"
      ;;
    incomplete)
      : >"$world/missing-zones"
      run --audited-cluster "$own_cluster"
      want_status 2 "an incomplete cluster list"
      want_text "UNKNOWN live: audited clusters: the project's cluster list was not read whole" "an incomplete cluster list"
      ;;
    fleet-refused)
      : >"$world/refuse-fleet"
      run --audited-cluster "$own_cluster"
      want_status 2 "a refused fleet listing"
      want_text "UNKNOWN live: audited clusters: the fleet's memberships were not read whole" "a refused fleet listing"
      ;;
    twice)
      run --audited-cluster "$own_cluster" --audited-cluster "$own_cluster"
      want_status 2 "a cluster named twice"
      want_text "--audited-cluster names each cluster once" "a cluster named twice"
      ;;
    absent)
      run
      ! has_prefix "live: audited clusters" || fail "the step ran with no --audited-cluster"
      ;;
  esac
}
if row cw; then
  each audited_row match missing fleet control extra refused incomplete fleet-refused twice absent
  passed "(cw) each GKE cluster of the pool must be an --audited-cluster: one left out is a finding, which a copy without the step misses; a named cluster the pool lacks, and a pool list not read whole, read UNKNOWN"
fi

# --- (ne) GKE Standard's node escape on another cluster of the pool. ---
readonly ne_cluster="$project/us-east1/other-cluster"
node_pod() {
  jq -n --arg ns "$1" --arg sa "$2" --arg node "$3" --arg phase "$4" \
    '{metadata: {namespace: $ns, name: "\($sa)-pod"}, spec: ({serviceAccountName: $sa} + (if $node != "" then {nodeName: $node} else {} end)), status: {phase: $phase}}'
}
node_escape_row() {
  base_world
  add_cluster other-cluster us-east1
  local pods="$world/clusters/other-cluster/pods.json" copy script="$check" sa
  local node_why=" on cluster $ne_cluster, and it runs a pod on a node of this GKE Standard cluster, where a privileged or hostPath pod reads every token on its node"
  local want="FINDING system:serviceaccount:kube-system:namespace-controller: create namespaces cluster-wide$node_why"
  local proxy="FINDING system:serviceaccount:kube-system:metrics-agent: get nodes/proxy cluster-wide$node_why"
  case "$1" in
    nodes-proxy | nodes-proxy-control | impersonate | named-proxy | approval-alone | eval-error | eval-error-control)
      sa=metrics-agent
      node_pod kube-system "$sa" node-1 Running | jq -s . >"$pods"
      ;;
  esac
  case "$1" in
    standard)
      node_pod kube-system namespace-controller node-1 Running | jq -s . >"$pods"
      run
      want_status 1 "a system ServiceAccount with a pod on a Standard cluster's node"
      want_text "$want" "a system ServiceAccount with a pod on a Standard cluster's node"
      return
      ;;
    nodes-proxy | nodes-proxy-control) subject_grant other-cluster "$(sa_subject kube-system "$sa")" get "" nodes proxy "" "" ;;
    impersonate) subject_grant other-cluster "$(sa_subject kube-system "$sa")" impersonate "" users "" "" "" ;;
    named-proxy) subject_grant other-cluster "$(sa_subject kube-system "$sa")" get "" nodes proxy "" node-1 ;;
    approval-alone) subject_grant other-cluster "$(sa_subject kube-system "$sa")" update certificates.k8s.io certificatesigningrequests approval "" "" ;;
    eval-error | eval-error-control)
      # On the token of a guarded pair by name, which the node review asks and the cluster's own
      # review of its other principals does not, so the error is the node review's alone.
      grant other-cluster "$(jq -nc --arg sa "$sa" '{subject: {kind: "ServiceAccount", namespace: "kube-system", name: $sa},
        verb: "create", group: "", resource: "serviceaccounts", subresource: "token", namespace: "envoy-operator", name: "envoy-operator",
        error: "authorizer timed out"}')"
      ;;
    autopilot)
      node_pod kube-system namespace-controller node-1 Running | jq -s . >"$pods"
      edit "$world/clusters.json" 'map(if .name == "other-cluster" then . + {autopilot: {enabled: true}} else . end)'
      run
      ! has_prefix "$want" || fail "an Autopilot cluster's system ServiceAccount was a node escape finding"
      return
      ;;
    no-pod)
      run
      want_status 0 "a system ServiceAccount with no pod, beside a census of kube-dns"
      ! has_prefix "$want" || fail "a system ServiceAccount with no pod was a node escape finding"
      return
      ;;
    unscheduled)
      node_pod kube-system namespace-controller "" Pending | jq -s . >"$pods"
      run
      ! has_prefix "$want" || fail "a system ServiceAccount whose pod is on no node was a node escape finding"
      return
      ;;
    empty-census | empty-census-control) echo '[]' >"$pods" ;;
    emptylist-census) : >"$world/clusters/other-cluster/emptylist-pods" ;;
    describe-refused)
      node_pod kube-system namespace-controller node-1 Running | jq -s . >"$pods"
      : >"$world/refuse-describe-other-cluster"
      run
      want_status 2 "a refused describe"
      want_text "UNKNOWN live: Workload Identity on $ne_cluster: whether the cluster is GKE Autopilot is unknown" "a refused describe"
      return
      ;;
    pods-refused)
      : >"$world/clusters/other-cluster/refuse-pods"
      run
      want_status 2 "a refused pods' list on a Standard cluster"
      want_text "UNKNOWN live: Workload Identity on $ne_cluster: the cluster is GKE Standard, and the API server refused the pods' list" "a refused pods' list"
      return
      ;;
  esac
  case "$1" in
    nodes-proxy-control)
      copy="$(script_copy node-rights-subset)"
      plant "$(dirname "$copy")/operator-token-minters/node-escape.sh" 'rights="$(with_rights "$5" "$NODE_TOKEN_RIGHTS")"' \
        'rights="$(with_rights "$(jq -c '"'"'[.[] | select(.sub == "" and (.resource == "namespaces" or .resource == "serviceaccounts" or .resource == "pods"))]'"'"' <<<"$5")" "$NODE_TOKEN_RIGHTS")"'
      script="$copy"
      ;;
    eval-error-control)
      copy="$(script_copy node-error-skipped)"
      plant "$(dirname "$copy")/operator-token-minters/node-escape.sh" '      error) unknown "$cstep" "the review of $principal, which runs a pod' '      error) : "the review of $principal, which runs a pod'
      script="$copy"
      ;;
    empty-census-control)
      copy="$(script_copy no-census-witness)"
      plant "$(dirname "$copy")/operator-token-minters/node-escape.sh" 'if [ "$(jq '"'"'.items | length'"'"' "$WORK/node-pods.json")" -eq 0 ]; then' 'if false; then'
      script="$copy"
      ;;
  esac
  run_script="$script" run
  case "$1" in
    *-control)
      want_status 0 "a copy blind to the node escape ($1)"
      [ "$(count_prefix 'UNKNOWN ')" -eq 0 ] || fail "a copy blind to the node escape ($1) read UNKNOWN"
      ;;
    nodes-proxy)
      want_status 1 "a system ServiceAccount on a node with get nodes/proxy"
      want_line "$proxy" "a system ServiceAccount on a node with get nodes/proxy"
      ;;
    impersonate)
      want_status 1 "a system ServiceAccount on a node that may impersonate users"
      want_line "FINDING system:serviceaccount:kube-system:metrics-agent: impersonate users cluster-wide$node_why" "a system ServiceAccount on a node that may impersonate users"
      ;;
    named-proxy)
      want_status 1 "a system ServiceAccount on a node with get nodes/proxy on one node by name"
      want_text "FINDING system:serviceaccount:kube-system:metrics-agent: get nodes/proxy node-1" "a system ServiceAccount on a node with get nodes/proxy on one node by name"
      want_text "$node_why" "a system ServiceAccount on a node with get nodes/proxy on one node by name"
      ;;
    approval-alone)
      want_status 0 "a system ServiceAccount on a node with the certificate approval alone"
      ! has_prefix "FINDING system:serviceaccount:kube-system:metrics-agent" || fail "the certificate approval alone, on a node's account, was a finding"
      ;;
    eval-error)
      want_status 2 "a node account's review that ended in an evaluation error"
      want_text "UNKNOWN live: Workload Identity on $ne_cluster: the review of system:serviceaccount:kube-system:metrics-agent, which runs a pod on a node of this GKE Standard cluster, for create serviceaccounts/token envoy-operator in envoy-operator ended in an evaluation error, so its refusal proves nothing: authorizer timed out" \
        "a node account's review that ended in an evaluation error"
      ;;
    empty-census | emptylist-census)
      want_status 2 "a Standard cluster's pods' list that holds no pod ($1)"
      want_text "UNKNOWN live: Workload Identity on $ne_cluster: the cluster is GKE Standard, and the API server's list of pods holds no pod, though every GKE cluster runs kube-system's" \
        "a Standard cluster's pods' list that holds no pod ($1)"
      ;;
  esac
}
if row ne; then
  each node_escape_row standard nodes-proxy impersonate named-proxy approval-alone eval-error autopilot no-pod unscheduled \
    empty-census emptylist-census describe-refused pods-refused nodes-proxy-control eval-error-control empty-census-control
  passed "(ne) on another Standard cluster of the pool, a system ServiceAccount with a running pod on a node and any right the cluster's review asks (create namespaces, get nodes/proxy outright or by name, impersonate users) is a finding; the certificate approval alone is none; on Autopilot, with no pod or a pod on no node it is none; an evaluation error, a pods' list of no pod, a refused describe or pods' list reads UNKNOWN; each control copy exits 0"
fi
