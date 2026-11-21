# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/step-clusters.sh: the step "live: clusters", and how a cluster is reached.
# Sourced by operator-token-minters.sh, whose header says what each step checks.
# --- live: clusters ---
# The operator's cluster is one name in one location. Two clusters of the project can share a name
# in two locations, and the one that is not the operator's, holding the namespace envoy-operator, is
# another cluster of the pool. --location says which; without it, the list's one cluster of that
# name is the operator's, and with two or more none is: the run says so, and reads none of them as
# the operator's or as another cluster.
step="live: clusters"
CLUSTERS_LISTED=false CLUSTERS_COMPLETE=false OPERATOR_FOUND=false OPERATOR_LOCATION="$LOCATION"
# PROJECT_CLUSTERS: <project>/<location>/<name> of each cluster the project's list holds, which the
# Workload Identity step reads, so its fleet part does not read one twice.
declare -A PROJECT_CLUSTERS=()
mark
if [ -z "$PROJECT" ]; then
  unknown "$step" "no --project: the project that runs the operator's cluster is not named"
elif [ -z "$CLUSTER" ]; then
  unknown "$step" "no --cluster: the operator's cluster is not named"
elif ! have gcloud; then
  unknown "$step" "gcloud is not on PATH"
elif ! gcloud container clusters list --project="$PROJECT" --format=json --quiet >"$WORK/clusters.json" 2>"$WORK/clusters.err"; then
  unknown "$step" "gcloud container clusters list was refused (it needs container.clusters.list): $(first_line "$WORK/clusters.err")"
elif ! jq -r '.[] | [.name, .location] | @tsv' "$WORK/clusters.json" >"$WORK/clusters.tsv" 2>"$WORK/clusters.err.jq"; then
  unknown "$step" "gcloud container clusters list answered no JSON list: $(first_line "$WORK/clusters.err.jq")"
elif [ ! -s "$WORK/clusters.tsv" ]; then
  unknown "$step" "the project lists no cluster, so the operator's cluster cannot be read"
else
  CLUSTERS_LISTED=true
  while IFS=$'\t' read -r name location; do
    PROJECT_CLUSTERS["$PROJECT/$location/$name"]=1
  done <"$WORK/clusters.tsv"
  clusters_text="$(awk -F'\t' '{ printf "%s%s (%s)", (NR > 1 ? ", " : ""), $1, $2 }' "$WORK/clusters.tsv")"
  same_name="$(awk -F'\t' -v c="$CLUSTER" '$1 == c { n++; l = l (n > 1 ? ", " : "") $2 } END { print n + 0 "\t" l }' "$WORK/clusters.tsv")"
  if [ -n "$LOCATION" ]; then
    if [ -n "${PROJECT_CLUSTERS["$PROJECT/$LOCATION/$CLUSTER"]:-}" ]; then
      OPERATOR_FOUND=true
    else
      unknown "$step" "--cluster $CLUSTER --location $LOCATION is not among the project's clusters ($clusters_text)"
    fi
  elif [ "${same_name%%$'\t'*}" -eq 1 ]; then
    OPERATOR_FOUND=true OPERATOR_LOCATION="${same_name#*$'\t'}"
  elif [ "${same_name%%$'\t'*}" -eq 0 ]; then
    unknown "$step" "--cluster $CLUSTER is not among the project's clusters ($clusters_text)"
  else
    unknown "$step" "--cluster $CLUSTER names ${same_name%%$'\t'*} clusters of the project (in ${same_name#*$'\t'}), and no --location says which is the operator's, so none of them is read"
  fi
  # gcloud lists what the zones that answered hold, and says on stderr which did not ("The following
  # zones did not respond: ... List results may be incomplete."), exiting 0.
  if grep -q 'did not respond' "$WORK/clusters.err"; then
    unknown "$step" "gcloud container clusters list says its list is incomplete, so a cluster in a zone that did not answer is neither read nor counted: $(grep -m 1 'did not respond' "$WORK/clusters.err" | sed 's/[[:cntrl:]]/ /g')"
  else
    CLUSTERS_COMPLETE=true
  fi
  ! unchanged || pass "$step" "the project runs $clusters_text; the operator's is $CLUSTER in $OPERATOR_LOCATION"
fi

# kube_for NAME LOCATION PROJECT: sets KUBE to a kubeconfig of this run's own for the GKE cluster;
# returns 1 with KUBE_ERROR. The caller's own kubeconfig is never read or written.
kube_for() {
  KUBE="$WORK/kube/$(printf 'gke_%s_%s_%s' "$3" "$2" "$1" | tr -c 'A-Za-z0-9._-' '_')" KUBE_ERROR=""
  if ! have kubectl; then
    KUBE_ERROR="kubectl is not on PATH"
    return 1
  fi
  if ! KUBECONFIG="$KUBE" gcloud container clusters get-credentials "$1" --location="$2" --project="$3" --quiet \
    ${CREDENTIALS_ARGS[@]+"${CREDENTIALS_ARGS[@]}"} >/dev/null 2>"$WORK/credentials.err"; then
    KUBE_ERROR="gcloud container clusters get-credentials $1 --location $2 --project $3 was refused (it needs container.clusters.get): $(first_line "$WORK/credentials.err")"
    return 1
  fi
}
# kube_for_membership ID LOCATION: the same for a member of the pool project's fleet that is no GKE
# cluster, through the Connect gateway.
kube_for_membership() {
  KUBE="$WORK/kube/$(printf 'membership_%s_%s' "$2" "$1" | tr -c 'A-Za-z0-9._-' '_')" KUBE_ERROR=""
  if ! have kubectl; then
    KUBE_ERROR="kubectl is not on PATH"
    return 1
  fi
  if ! KUBECONFIG="$KUBE" gcloud container fleet memberships get-credentials "$1" --location="$2" --project="$POOL_PROJECT" --quiet \
    >/dev/null 2>"$WORK/credentials.err"; then
    KUBE_ERROR="gcloud container fleet memberships get-credentials $1 --location $2 --project $POOL_PROJECT was refused (it needs gkehub.gateway.get and gkehub.gateway.generateCredentials): $(first_line "$WORK/credentials.err")"
    return 1
  fi
}
