# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/step-workload-identity.sh: the step "live: Workload Identity", over every
# other cluster of the project and every member of the pool project's fleet. Sourced by
# operator-token-minters.sh, whose header says what each step checks.
# --- live: Workload Identity: every other cluster that uses the pool ---

pool_name="the workload pool's project"
[ -z "$POOL_PROJECT" ] || pool_name="project $POOL_PROJECT"

# read_other_cluster STEP PRINCIPAL WHERE GROUPS: on the cluster KUBE names, no guarded pair's
# namespace may exist (else a finding for PRINCIPAL, the cluster), and no ServiceAccount outside the
# cluster's system namespaces, none of Kubernetes' own groups, and no binding subject may hold a
# right of GUARD_RIGHTS (each a finding, its right followed by WHERE); every binding of
# system:masters but the bootstrap one is a finding, and its other Group subjects are judged by
# GROUPS, where the cluster takes its groups from (groups_verdict). The reviews carry the witness.
# PASS when it recorded neither a finding nor an UNKNOWN.
read_other_cluster() {
  local cstep="$1" principal="$2" where="$3" groups_mode="$4" ns name pair acts accounts=0
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
  jq -r "$JQ_REC"'.items[]? | [.metadata.namespace, .metadata.name] | rec' "$WORK/serviceaccounts.json" >"$WORK/serviceaccounts.rec"
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
  masters_bindings "$cstep" "$where" "$principal"
  custom_rights "$KUBE" guard || unknown "$cstep" "$CUSTOM_ERROR"
  named_rights
  guard_rights="$(with_rights "$(with_rights "$GUARD_RIGHTS" "$CUSTOM_RIGHTS")" "$NAMED_RIGHTS")"
  # Every cluster has default/default, so a list that holds no ServiceAccount outside the system
  # namespaces is not the cluster's whole list; its binding subjects are still reviewed.
  [ "$accounts" -gt 0 ] ||
    unknown "$cstep" "the cluster lists no ServiceAccount outside its system namespaces, though every cluster has default/default, so its list cannot be read as whole"
  if [ "$((accounts + BOUND))" -eq 0 ]; then
    unknown "$cstep" "the cluster lists no ServiceAccount outside its system namespaces and no binding subject to review, so there is no principal to review"
  elif ! review "$KUBE" "$(principals_json "$WORK/principals.rec")" "$guard_rights"; then
    unknown "$cstep" "$REVIEW_ERROR"
  elif judge_reviews "$cstep" "$where" "$principal"; then
    follow_becomes "$KUBE" "$cstep" "$where" "$principal"
    ! unchanged ||
      pass "$cstep" "no guarded namespace ($GUARDED_TEXT), and none of its $accounts ServiceAccounts outside the system namespaces, $GROUPS_ADDED of Kubernetes' own groups, $BOUND binding subjects and anyone unauthenticated may make a namespace, a ServiceAccount, a pod or a custom resource in a guarded namespace, or hold an escalation right"
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
    read_other_cluster "$cstep" "cluster:$label" " on cluster $label" "${GROUPS_CONFIG["$label"]:-unknown:the cluster list gave no authenticatorGroupsConfig}"
  done 5<"$WORK/clusters.tsv"
fi

# The projects of the clusters read here other than --project (the pool project's, a fleet GKE
# member's), whose own IAM the step after this one reads.
declare -A OTHER_PROJECTS=()

# The pool project's own clusters, when --pool-project names another project (live: clusters of the
# pool project): each uses the pool, as the project's other clusters do.
while IFS=$'\t' read -r -u 5 cluster location _; do
  OTHER_PROJECTS[$POOL_PROJECT]=1
  label="$POOL_PROJECT/$location/$cluster"
  cstep="live: Workload Identity on $label"
  if ! kube_for "$cluster" "$location" "$POOL_PROJECT"; then
    unknown "$cstep" "$KUBE_ERROR"
    continue
  fi
  read_other_cluster "$cstep" "cluster:$label" " on cluster $label" "${GROUPS_CONFIG["$label"]:-unknown:the cluster list gave no authenticatorGroupsConfig}"
done 5<"$WORK/pool-clusters.tsv"

# fleet_memberships: every membership of the pool project's fleet, from the GKE Hub API, every
# page, as records in $WORK/fleet.rec: `unreachable` and the locations the listing could not reach,
# when there are any, then one `member` per membership, with its id and location and, for a GKE
# cluster, the cluster's project, location and name. Returns 1 with FLEET_ERROR when the listing
# was refused or its answer cannot be read. The call is the API's own, not `gcloud container fleet
# memberships list`: that command's list drops the answer's `unreachable` field, so a listing
# that missed a location would read as complete.
fleet_memberships() {
  local token page="" pages=0 status why
  local -a query
  FLEET_ERROR=""
  if ! have curl; then
    FLEET_ERROR="curl is not on PATH"
    return 1
  fi
  if ! token="$(gcloud auth print-access-token --quiet 2>"$WORK/token.err")" || [ -z "$token" ]; then
    FLEET_ERROR="gcloud auth print-access-token gave no access token for the running identity: $(last_line "$WORK/token.err")"
    return 1
  fi
  : >"$WORK/fleet-pages.json"
  while :; do
    pages=$((pages + 1))
    if [ "$pages" -gt 1000 ]; then
      FLEET_ERROR="the listing did not end within 1000 pages"
      return 1
    fi
    query=(--data-urlencode pageSize=100)
    [ -z "$page" ] || query+=(--data-urlencode "pageToken=$page")
    status=0
    # The token goes in through curl's configuration on stdin, never on a command line.
    printf 'header = "Authorization: Bearer %s"\n' "$token" |
      curl -sS --fail-with-body --max-time 60 -G -K - "${query[@]}" \
        "https://gkehub.googleapis.com/v1/projects/$POOL_PROJECT/locations/-/memberships" >"$WORK/fleet-page.json" 2>"$WORK/fleet.err" || status=$?
    if [ "$status" -ne 0 ]; then
      why="$(jq -r '[.error.code, .error.status, .error.message] | map(select(. != null) | tostring) | join(" ")' "$WORK/fleet-page.json" 2>/dev/null |
        sed -n '1{s/[[:cntrl:]]/ /g;p;}')" || true
      [ -n "$why" ] || why="$(last_line "$WORK/fleet.err")"
      FLEET_ERROR="the GKE Hub API refused the listing of projects/$POOL_PROJECT/locations/-/memberships (it needs gkehub.memberships.list there): $why"
      return 1
    fi
    if ! jq -c . "$WORK/fleet-page.json" >>"$WORK/fleet-pages.json" 2>"$WORK/fleet.err"; then
      FLEET_ERROR="the GKE Hub API's answer is not JSON: $(first_line "$WORK/fleet.err")"
      return 1
    fi
    page="$(jq -r '.nextPageToken // ""' "$WORK/fleet-page.json")"
    [ -n "$page" ] || break
  done
  if ! jq -r -s -L "$LIB" -f "$LIB/fleet.jq" "$WORK/fleet-pages.json" >"$WORK/fleet.rec" 2>"$WORK/fleet.err"; then
    FLEET_ERROR="the GKE Hub API's answer holds no list of memberships: $(first_line "$WORK/fleet.err")"
    return 1
  fi
}

# The pool project's fleet: every member cluster shares the workload pool, so a ServiceAccount
# envoy-operator/envoy-operator there is the operator's member too. The operator's own cluster,
# and a cluster the project's list held, are read above.
step="live: Workload Identity: the fleet of $pool_name"
if [ -z "$POOL_PROJECT" ]; then
  unknown "$step" "no --project or --pool-project: the workload pool's project is not named"
elif ! have gcloud; then
  unknown "$step" "gcloud is not on PATH"
elif ! fleet_memberships; then
  unknown "$step" "$FLEET_ERROR"
else
  fleet_f="${#FINDINGS[@]}" fleet_u="${#UNKNOWNS[@]}" members=0 own=0 above=0 here=0
  # A GKE member read here in a project other than --project adds to OTHER_PROJECTS.
  while IFS="$US" read -r -u 6 what id location gke_project gke_location gke_cluster; do
    if [ "$what" = unreachable ]; then
      unknown "$step" "the listing could not reach the locations $id, so a membership registered there is not listed"
      continue
    fi
    members=$((members + 1))
    if [ -n "$gke_cluster" ]; then
      # The operator's cluster is --cluster in the operator's location, never by name alone; a member
      # of that name in the project, while the operator's location is unknown, cannot be told from it.
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
      [ "$gke_project" = "$PROJECT" ] || OTHER_PROJECTS[$gke_project]=1
      label="$gke_project/$gke_location/$gke_cluster"
      cstep="live: Workload Identity on fleet member $id (cluster $label)"
      if ! kube_for "$gke_cluster" "$gke_location" "$gke_project"; then
        unknown "$cstep" "$KUBE_ERROR"
        continue
      fi
      groups_config_of "$gke_cluster" "$gke_location" "$gke_project"
      read_other_cluster "$cstep" "cluster:$label" " on cluster $label" "$GROUPS_MODE"
    else
      here=$((here + 1))
      label="$POOL_PROJECT/$location/$id"
      cstep="live: Workload Identity on fleet member $id"
      if ! kube_for_membership "$id" "$location"; then
        unknown "$cstep" "$KUBE_ERROR"
        continue
      fi
      read_other_cluster "$cstep" "membership:$label" " on fleet member $label" own
    fi
  done 6<"$WORK/fleet.rec"
  if [ "${#FINDINGS[@]}" -eq "$fleet_f" ] && [ "${#UNKNOWNS[@]}" -eq "$fleet_u" ]; then
    if [ "$members" -eq 0 ]; then
      pass "$step" "the GKE Hub API lists no membership in the fleet, and no location it could not reach"
    else
      pass "$step" "$members membership(s) listed, every location reached: $own the operator's cluster, $above read above as clusters of the project, $here read here"
    fi
  fi
fi
