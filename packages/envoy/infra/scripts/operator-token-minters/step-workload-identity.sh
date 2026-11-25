# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/step-workload-identity.sh: the step "live: Workload Identity", over every
# other cluster of the project and every member of the pool project's fleet. Sourced by
# operator-token-minters.sh, whose header says what each step checks.
# --- live: Workload Identity: every other cluster that uses the pool ---

pool_name="the workload pool's project"
[ -z "$POOL_PROJECT" ] || pool_name="project $POOL_PROJECT"

# read_other_cluster STEP PRINCIPAL WHERE GROUPS [SETTINGS]: on the cluster KUBE names, no guarded
# pair's namespace may exist (else a finding for PRINCIPAL, the cluster), and no ServiceAccount
# outside the cluster's system namespaces, none of Kubernetes' own groups, and no binding subject
# may hold a right of GUARD_RIGHTS, a custom or admission resource's right from the cluster's
# discovery, a write of a binding or a role in a namespace that holds a RoleBinding
# (binding_rights), or any of those limited by name (each a finding, its right followed by WHERE);
# every binding of system:masters but the bootstrap one is a finding, and its other Group subjects
# are judged by GROUPS, where the cluster takes its groups from (groups_verdict), and SETTINGS by
# settings_verdict. The reviews carry the witness. PASS when it recorded neither a finding nor an
# UNKNOWN.
read_other_cluster() {
  local cstep="$1" principal="$2" where="$3" groups_mode="$4" settings="${5:-}" ns name pair acts accounts=0
  local -A exists=()
  mark
  for pair in "${GUARDED[@]}"; do
    ns="${pair%%/*}"
    if [ -z "${exists[$ns]:-}" ]; then
      exists[$ns]=no
      if kubectl --kubeconfig "$KUBE" get namespace "$ns" -o name >/dev/null 2>"$WORK/namespace.err"; then
        exists[$ns]=yes
      elif ! grep -q '(NotFound)' "$WORK/namespace.err"; then
        exists[$ns]=unknown
        unknown "$cstep" "the API server refused the read of namespace $ns (it needs get on namespaces, container.namespaces.get on GKE): $(first_line "$WORK/namespace.err")"
      fi
    fi
    [ "${exists[$ns]}" = yes ] || continue
    acts="the Google service account the pool binds it to"
    [ "$pair" != "$NAMESPACE/$KSA" ] || acts="$operator_account"
    [ -z "$BROKER" ] || [ "$pair" != "$BROKER_WORKLOAD" ] || acts="the broker account $BROKER"
    finding "$principal" "namespace $ns exists there, so its ServiceAccount $pair acts as $acts through the workload pool $POOL_PROJECT.svc.id.goog" \
      "namespace $ns, guarding $pair" "$principal" "" ""
  done
  if ! kubectl --kubeconfig "$KUBE" get serviceaccounts --all-namespaces -o json >"$WORK/serviceaccounts.json" 2>"$WORK/serviceaccounts.err"; then
    unknown "$cstep" "the API server refused the ServiceAccounts' list (it needs list on serviceaccounts, container.serviceAccounts.list on GKE): $(first_line "$WORK/serviceaccounts.err")"
    return
  fi
  if ! is_list "$WORK/serviceaccounts.json" "$WORK/serviceaccounts.err"; then
    unknown "$cstep" "the API server's list of ServiceAccounts could not be read: $(first_line "$WORK/serviceaccounts.err")"
    return
  fi
  jq -r "$JQ_REC"'.items[] | [.metadata.namespace, .metadata.name] | rec' "$WORK/serviceaccounts.json" >"$WORK/serviceaccounts.rec"
  printf '%s\n%s\n' "$WITNESS_RECORD" "$ANONYMOUS_RECORD" >"$WORK/principals.rec"
  while IFS="$US" read -r -u 3 ns name; do
    if system_namespace "$ns"; then
      sa_record "$ns" "$name" seed: >>"$WORK/principals.rec"
      continue
    fi
    accounts=$((accounts + 1))
    sa_record "$ns" "$name" >>"$WORK/principals.rec"
  done 3<"$WORK/serviceaccounts.rec"
  bound_subjects "$KUBE" || unknown "$cstep" "$BOUND_ERROR"
  namespace_groups
  groups_verdict "$cstep" "$groups_mode"
  settings_verdict "$cstep" "$settings"
  masters_bindings "$cstep" "$where" "$principal"
  custom_rights "$KUBE" guard || unknown "$cstep" "$CUSTOM_ERROR"
  admission_rights "$KUBE" || unknown "$cstep" "$ADMISSION_ERROR"
  admission_power "$KUBE" "$cstep"
  binding_rights || unknown "$cstep" "$BINDING_ERROR"
  # The writes of a binding or a role in each namespace that holds a RoleBinding are reviewed
  # beside the guard rights, and live_named reads the rules that limit any of them by name from
  # this cluster's own roles; step-rbac.sh says why.
  guard_rights="$(with_rights "$(with_rights "$(with_rights "$(with_rights "$GUARD_RIGHTS" "$CUSTOM_RIGHTS")" "$ADMISSION_RIGHTS")" "$POWER_RIGHTS")" "$BINDING_RIGHTS")"
  # A cluster that is no GKE cluster runs kube-controller-manager's own approver of kubelet client
  # certificates (NODE_CLIENT_RIGHTS).
  [ "$groups_mode" != own ] || guard_rights="$(with_rights "$guard_rights" "$NODE_CLIENT_RIGHTS")"
  # Every cluster has default/default, so a list that holds no ServiceAccount outside the system
  # namespaces is not the cluster's whole list; its binding subjects are still reviewed.
  [ "$accounts" -gt 0 ] ||
    unknown "$cstep" "the cluster lists no ServiceAccount outside its system namespaces, though every cluster has default/default, so its list cannot be read as whole"
  if [ "$((accounts + BOUND))" -eq 0 ]; then
    unknown "$cstep" "the cluster lists no ServiceAccount outside its system namespaces and no binding subject to review, so there is no principal to review"
  elif ! review "$KUBE" "$(principals_json "$WORK/principals.rec")" "$guard_rights"; then
    unknown "$cstep" "$REVIEW_ERROR"
  elif judge_reviews "$cstep" "$where" "$principal"; then
    live_named "$KUBE" "$cstep" "$where" "$principal" "$guard_rights"
    csr_chains "$where" "$principal"
    follow_becomes "$KUBE" "$cstep" "$where" "$principal"
    remember_cluster "$KUBE" "$cstep" "$where" "$principal"
    ! unchanged ||
      pass "$cstep" "no guarded namespace ($GUARDED_TEXT), and none of its $accounts ServiceAccounts outside the system namespaces, $GROUPS_ADDED of Kubernetes' own groups, $BOUND binding subjects and anyone unauthenticated may make a namespace, a ServiceAccount, a pod or a custom resource in a guarded namespace, write a webhook or admission policy, or hold an escalation right"
  fi
}

# The project's other clusters. A cluster is the operator's by name and location: one of the same
# name elsewhere is another cluster of the pool, and while which one is the operator's is unknown,
# none of that name is read.
step="live: Workload Identity"
if [ "$CLUSTERS_LISTED" != true ]; then
  unknown "$step" "the project's clusters could not be listed (live: clusters)"
else
  [ "$CLUSTERS_COMPLETE" = true ] ||
    unknown "$step" "the project's cluster list is incomplete (live: clusters), so a cluster it leaves out is not read"
  if [ "$CLUSTERS_COMPLETE" = true ] && [ "$OPERATOR_FOUND" = true ] && [ "$(wc -l <"$WORK/clusters.tsv")" -eq 1 ]; then
    pass "$step" "the project runs the operator's cluster $CLUSTER alone"
  fi
  while IFS=$'\t' read -r -u 5 cluster location; do
    label="$PROJECT/$location/$cluster"
    if [ "$cluster" = "$CLUSTER" ]; then
      if [ "$OPERATOR_FOUND" != true ]; then
        unknown "live: Workload Identity on $label" "it has --cluster's name, and which cluster of that name is the operator's is unknown (live: clusters), so it is not read"
        continue
      fi
      [ "$location" != "$OPERATOR_LOCATION" ] || continue
    fi
    cstep="live: Workload Identity on $label"
    if ! kube_for "$cluster" "$location" "$PROJECT"; then
      unknown "$cstep" "$KUBE_ERROR"
      continue
    fi
    read_other_cluster "$cstep" "cluster:$label" " on cluster $label" "${GROUPS_CONFIG["$label"]:-unknown:the cluster list gave no authenticatorGroupsConfig}" \
      "${CLUSTER_SETTINGS["$label"]:-}"
  done 5<"$WORK/clusters.tsv"
fi

# The projects of the clusters read here other than --project (the pool project's, a fleet GKE
# member's), whose own IAM the step after this one reads.
declare -A OTHER_PROJECTS=()

# The pool project's own clusters, when --pool-project names another project (live: clusters of the
# pool project): each uses the pool, as the project's other clusters do. The pool project's own IAM
# is read first, so each cluster asks who may impersonate its holders by name.
while IFS=$'\t' read -r -u 5 cluster location _; do
  OTHER_PROJECTS[$POOL_PROJECT]=1
  label="$POOL_PROJECT/$location/$cluster"
  cstep="live: Workload Identity on $label"
  if ! kube_for "$cluster" "$location" "$POOL_PROJECT"; then
    unknown "$cstep" "$KUBE_ERROR"
    continue
  fi
  member_project_iam "$POOL_PROJECT"
  read_other_cluster "$cstep" "cluster:$label" " on cluster $label" "${GROUPS_CONFIG["$label"]:-unknown:the cluster list gave no authenticatorGroupsConfig}" \
    "${CLUSTER_SETTINGS["$label"]:-}"
done 5<"$WORK/pool-clusters.tsv"

# fleet_memberships: every membership of the pool project's fleet, from the GKE Hub API, every
# page, as records in $WORK/fleet.rec: `unreachable` and the locations the listing could not reach,
# when there are any, then one `member` per membership, with its id and location and, for a GKE
# cluster, the cluster's project, location and name. Returns 1 with FLEET_ERROR when the listing
# was refused or its answer cannot be read. The call is the API's own, not `gcloud container fleet
# memberships list`: that command's list drops the answer's `unreachable` field, so a listing
# that missed a location would read as complete.
fleet_memberships() {
  FLEET_ERROR=""
  hub_list memberships gkehub.memberships.list "$WORK/fleet-pages.json" || { FLEET_ERROR="$HUB_ERROR"; return 1; }
  if ! jq -r -s -L "$LIB" -f "$LIB/fleet.jq" "$WORK/fleet-pages.json" >"$WORK/fleet.rec" 2>"$WORK/fleet.err"; then
    FLEET_ERROR="the GKE Hub API's answer holds no list of memberships: $(first_line "$WORK/fleet.err")"
    return 1
  fi
}

# hub_list COLLECTION PERMISSION PAGES: every page of the GKE Hub API's
# projects/$POOL_PROJECT/locations/-/COLLECTION, each page's JSON a line of PAGES. Returns 1 with
# HUB_ERROR, naming PERMISSION, when the listing was refused or a page is not JSON.
hub_list() {
  local token page="" quoted_page pages=0 status why
  HUB_ERROR=""
  if ! have curl; then
    HUB_ERROR="curl is not on PATH"
    return 1
  fi
  if ! token="$(gcloud auth print-access-token --quiet 2>"$WORK/token.err")" || [ -z "$token" ]; then
    HUB_ERROR="gcloud auth print-access-token gave no access token for the running identity: $(last_line "$WORK/token.err")"
    return 1
  fi
  : >"$3"
  while :; do
    pages=$((pages + 1))
    if [ "$pages" -gt 1000 ]; then
      HUB_ERROR="the listing of projects/$POOL_PROJECT/locations/-/$1 did not end within 1000 pages"
      return 1
    fi
    if [[ "$page" == *[[:cntrl:]]* ]]; then
      HUB_ERROR="the GKE Hub API's listing of projects/$POOL_PROJECT/locations/-/$1 gave a next page token holding a control character, which this check does not send back"
      return 1
    fi
    status=0
    # The access token, and the page token beside it, go in through curl's configuration on
    # stdin, never on a command line. The page token is quoted for that configuration: each
    # backslash and double quote in it is escaped with a backslash.
    quoted_page="${page//\\/\\\\}"
    quoted_page="${quoted_page//\"/\\\"}"
    {
      printf 'header = "Authorization: Bearer %s"\n' "$token"
      [ -z "$page" ] || printf 'data-urlencode = "pageToken=%s"\n' "$quoted_page"
    } | curl -sS --fail-with-body --max-time 60 -G -K - --data-urlencode pageSize=100 \
      "https://gkehub.googleapis.com/v1/projects/$POOL_PROJECT/locations/-/$1" >"$WORK/hub-page.json" 2>"$WORK/hub.err" || status=$?
    if [ "$status" -ne 0 ]; then
      why="$(jq -r '[.error.code, .error.status, .error.message] | map(select(. != null) | tostring) | join(" ")' "$WORK/hub-page.json" 2>/dev/null |
        sed -n '1{s/[[:cntrl:]]/ /g;p;}')" || true
      [ -n "$why" ] || why="$(last_line "$WORK/hub.err")"
      HUB_ERROR="the GKE Hub API refused the listing of projects/$POOL_PROJECT/locations/-/$1 (it needs $2 there): $why"
      return 1
    fi
    if ! jq -c . "$WORK/hub-page.json" >>"$3" 2>"$WORK/hub.err"; then
      HUB_ERROR="the GKE Hub API's answer is not JSON: $(first_line "$WORK/hub.err")"
      return 1
    fi
    page="$(jq -r '.nextPageToken // ""' "$WORK/hub-page.json")"
    [ -n "$page" ] || break
  done
}

# fleet_scopes: the pool project's fleet scopes, from the API's scopes.list on locations/-, every
# page (`gcloud container fleet scopes list` lists only the permitted scopes at locations/global, so
# a scope elsewhere would read as none). A scope's namespaces and RBAC role bindings reach every
# member cluster bound to it, the operator's namespace and Workload Identity member included, and
# this check does not analyse them: each scope is UNKNOWN, and so is a refused listing. Sets SCOPES
# to how many it listed.
fleet_scopes() {
  local scope
  SCOPES=0
  if ! hub_list scopes gkehub.scopes.list "$WORK/scope-pages.json"; then
    unknown "$step" "$HUB_ERROR, so a scope whose namespaces and RBAC role bindings reach the members is not seen"
    return
  fi
  if ! jq -r '.scopes[]?.name' "$WORK/scope-pages.json" >"$WORK/scopes.list" 2>"$WORK/scopes.err"; then
    unknown "$step" "the GKE Hub API's scopes answer holds no list of scopes: $(first_line "$WORK/scopes.err")"
    return
  fi
  while IFS= read -r scope; do
    SCOPES=$((SCOPES + 1))
    unknown "$step" "the fleet holds the scope $scope, whose namespaces and RBAC role bindings reach every member cluster bound to it, which this check does not analyse"
  done <"$WORK/scopes.list"
}

# rbac_only ID: whether --rbac-only-member names the membership ID.
rbac_only() {
  local named
  for named in ${RBAC_ONLY_MEMBERS[@]+"${RBAC_ONLY_MEMBERS[@]}"}; do
    [ "$named" != "$1" ] || return 0
  done
  return 1
}

# The pool project's fleet: every member cluster shares the workload pool, so a ServiceAccount
# envoy-operator/envoy-operator there is the operator's member too. The operator's own cluster,
# and a cluster the project's list held, are read above. A listing that holds no membership cannot
# be told from one that saw nothing, so it is UNKNOWN, unless --fleet none says the pool has no
# fleet; with it, each membership listed is a finding. The fleet's scopes are read beside it.
step="live: Workload Identity: the fleet of $pool_name"
if [ -z "$POOL_PROJECT" ]; then
  unknown "$step" "no --project or --pool-project: the workload pool's project is not named"
elif ! have gcloud; then
  unknown "$step" "gcloud is not on PATH"
else
  fleet_f="${#FINDINGS[@]}" fleet_u="${#UNKNOWNS[@]}" members=0 own=0 above=0 here=0
  if ! fleet_memberships; then
    unknown "$step" "$FLEET_ERROR"
  else
    # A GKE member read here in a project other than --project adds to OTHER_PROJECTS.
    while IFS="$US" read -r -u 6 what id location gke_project gke_location gke_cluster; do
      if [ "$what" = unreachable ]; then
        unknown "$step" "the listing could not reach the locations $id, so a membership registered there is not listed"
        continue
      fi
      members=$((members + 1))
      [ "$FLEET" != none ] ||
        finding "membership:$POOL_PROJECT/$location/$id" "is registered to the fleet of project $POOL_PROJECT, though --fleet none says it has no member" \
          "" "membership:$POOL_PROJECT/$location/$id" "" "the GKE Hub API's listing"
      if [ -n "$gke_cluster" ]; then
        # The operator's cluster is --cluster in the operator's location, never by name alone; a
        # member of that name in the project, while the operator's location is unknown, cannot be
        # told from it.
        if [ "$gke_project" = "$PROJECT" ] && [ "$gke_cluster" = "$CLUSTER" ]; then
          if [ -z "$OPERATOR_LOCATION" ]; then
            unknown "live: Workload Identity on fleet member $id (cluster $gke_project/$gke_location/$gke_cluster)" \
              "it has --cluster's name, and the operator's cluster's location is unknown (live: clusters), so it is not read"
            continue
          fi
          if [ "$gke_location" = "$OPERATOR_LOCATION" ]; then
            own=$((own + 1))
            continue
          fi
        fi
        if [ -n "${PROJECT_CLUSTERS["$gke_project/$gke_location/$gke_cluster"]:-}" ]; then
          above=$((above + 1))
          continue
        fi
        here=$((here + 1))
        member_project=""
        if [ "$gke_project" != "$PROJECT" ]; then
          OTHER_PROJECTS[$gke_project]=1 member_project="$gke_project"
        fi
        label="$gke_project/$gke_location/$gke_cluster"
        cstep="live: Workload Identity on fleet member $id (cluster $label)"
        if ! kube_for "$gke_cluster" "$gke_location" "$gke_project"; then
          unknown "$cstep" "$KUBE_ERROR"
          continue
        fi
        groups_config_of "$gke_cluster" "$gke_location" "$gke_project"
        # The member's own project's IAM first (its own step), then its cluster.
        [ -z "$member_project" ] || member_project_iam "$member_project"
        read_other_cluster "$cstep" "cluster:$label" " on cluster $label" "$GROUPS_MODE" "${CLUSTER_SETTINGS["$label"]:-}"
      else
        here=$((here + 1))
        label="$POOL_PROJECT/$location/$id"
        cstep="live: Workload Identity on fleet member $id"
        # Its own cloud's authorizers and the membership's token issuer can grant beside RBAC, and
        # this check reads its RBAC alone.
        rbac_only "$id" ||
          unknown "$cstep" "it is no GKE cluster, so its own cloud's authorizers and token issuer may grant beside the RBAC read here; --rbac-only-member $id says it has none"
        if ! kube_for_membership "$id" "$location"; then
          unknown "$cstep" "$KUBE_ERROR"
          continue
        fi
        read_other_cluster "$cstep" "membership:$label" " on fleet member $label" own
      fi
    done 6<"$WORK/fleet.rec"
    [ "$members" -gt 0 ] || [ "$FLEET" = none ] ||
      unknown "$step" "the GKE Hub API lists no membership in the fleet, so an answer that saw nothing cannot be told from a fleet that holds none (--fleet none says it has none)"
  fi
  fleet_scopes
  if [ "${#FINDINGS[@]}" -eq "$fleet_f" ] && [ "${#UNKNOWNS[@]}" -eq "$fleet_u" ]; then
    if [ "$members" -eq 0 ]; then
      pass "$step" "the GKE Hub API lists no membership in the fleet, as --fleet none says, and the fleet holds no scope"
    else
      pass "$step" "$members membership(s) listed, every location reached: $own the operator's cluster, $above read above as clusters of the project, $here read here, and the fleet holds no scope"
    fi
  fi
fi
