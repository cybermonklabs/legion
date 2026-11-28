# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-pool-3.sh: the rows of operator-token-minters.test.sh on the
# pool's other clusters: the pool project's own clusters, the fleet's scopes, --fleet none and
# --rbac-only-member, the clusters' settings (legacy ABAC, the identity service, GKE Standard),
# and a fleet member's IAM user. Sourced by the test after its world and helpers; its header says
# what the fakes model. The rows:
#
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
