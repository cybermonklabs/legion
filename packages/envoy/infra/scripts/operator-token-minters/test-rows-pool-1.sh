# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-pool-1.sh: the rows of operator-token-minters.test.sh on the
# pool's other clusters: a second cluster of the project, and the members of the pool project's
# fleet, GKE or not. Sourced by the test after its world and helpers; its header says what the fakes
# model. The rows:
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
#        with UNKNOWN; --pool-project naming another pool cannot read clean.

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
