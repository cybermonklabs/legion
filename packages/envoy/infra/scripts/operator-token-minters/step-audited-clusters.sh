# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/step-audited-clusters.sh: the step "live: audited clusters". Sourced by
# operator-token-minters.sh, whose header says what each step checks.
# --- live: audited clusters ---
# Workload Identity maps a namespace and ServiceAccount to one identity across every cluster of the
# pool, so a GKE cluster of the pool that the auditor was not given (--audited-cluster) is one whose
# RBAC no run of the auditor reads. The pool's GKE clusters are the project's list and the pool
# project's (live: clusters) and each GKE member of the fleet (live: Workload Identity); a member
# that is no GKE cluster is that step's UNKNOWN already.
if [ "${#AUDITED_CLUSTERS[@]}" -eq 0 ]; then
  census_quiet "no --audited-cluster: the step does not run"
else
  step="live: audited clusters"
  mark
  if [ "$CLUSTERS_LISTED" != true ] || [ "$CLUSTERS_COMPLETE" != true ]; then
    unknown "$step" "the project's cluster list was not read whole (live: clusters), so a cluster of the pool it leaves out cannot be told from one the auditor was given"
  elif [ "$POOL_LISTED" != true ]; then
    unknown "$step" "the pool project's cluster list was not read whole (live: clusters of the pool project $POOL_PROJECT)"
  elif [ "$FLEET_LISTED" != true ]; then
    unknown "$step" "the fleet's memberships were not read whole (live: Workload Identity: the fleet), so a GKE cluster registered there cannot be told from one the auditor was given"
  else
    declare -A pool_cluster=() named_cluster=()
    for key in "${!PROJECT_CLUSTERS[@]}"; do pool_cluster[$key]="gcloud container clusters list --project ${key%%/*}"; done
    while IFS="$US" read -r -u 6 what id location gke_project gke_location gke_cluster; do
      [ "$what" = member ] && [ -n "$gke_cluster" ] || continue
      [ -n "${pool_cluster["$gke_project/$gke_location/$gke_cluster"]:-}" ] ||
        pool_cluster["$gke_project/$gke_location/$gke_cluster"]="the GKE Hub API's listing (membership $location/$id)"
    done 6<"$WORK/fleet.rec"
    for key in "${AUDITED_CLUSTERS[@]}"; do named_cluster[$key]=1; done
    while IFS= read -r key; do
      [ -n "${named_cluster[$key]:-}" ] ||
        finding "cluster:$key" "is a GKE cluster of the pool that no --audited-cluster names, so the auditor does not read it" \
          "" "cluster:$key" "" "${pool_cluster[$key]}"
    done < <(printf '%s\n' "${!pool_cluster[@]}" | sort)
    for key in "${AUDITED_CLUSTERS[@]}"; do
      [ -n "${pool_cluster[$key]:-}" ] ||
        unknown "$step" "--audited-cluster $key is not among the pool's GKE clusters ($(printf '%s\n' "${!pool_cluster[@]}" | sort | paste -sd, - | sed 's/,/, /g')), so the list the auditor was given is not the pool's"
    done
    if unchanged; then
      pass "$step" "the ${#AUDITED_CLUSTERS[@]} cluster(s) given are the pool's GKE clusters: $(printf '%s\n' "${AUDITED_CLUSTERS[@]}" | sort | paste -sd, - | sed 's/,/, /g')"
    fi
  fi
fi
