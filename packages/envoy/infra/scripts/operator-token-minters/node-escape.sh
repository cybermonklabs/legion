# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/node-escape.sh: GKE Standard's node escape on a cluster that is not the
# operator's. Sourced by operator-token-minters.sh, whose header says what each step checks.
#
# A system namespace's ServiceAccount is no finding on its own (principals.sh, system_namespace): its
# pods are the cluster's own. But GKE Standard admits a privileged or hostPath pod, which Autopilot
# refuses, and such a pod reads every token on its node. So on a Standard cluster a system
# ServiceAccount with a pod on a node, allowed a right over the guarded namespaces or
# ServiceAccounts, is a right someone on that node can take: each is a finding naming it.

# NODE_RIGHTS: the rights over the guarded namespaces and ServiceAccounts such a token gives: create
# or patch namespaces cluster-wide, create or patch serviceaccounts and pods in each guarded
# namespace (GUARD_RIGHTS' own entries), and create serviceaccounts/token for each guarded pair.
NODE_RIGHTS="$(jq -c --args '
  [.[] | select((.resource == "namespaces" and .ns == "")
                or (.ns != "" and .sub == "" and (.resource == "serviceaccounts" or .resource == "pods")
                    and (.verb == "create" or .verb == "patch")))]
  + [$ARGS.positional[] | split("/") | {verb: "create", group: "", resource: "serviceaccounts", sub: "token", ns: .[0], name: .[1],
                                        label: "create serviceaccounts/token \(.[1]) in \(.[0])"}
     | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}]' "${GUARDED[@]}" <<<"$GUARD_RIGHTS")"

# autopilot_of NAME LOCATION PROJECT: sets AUTOPILOT_MODE to the GKE cluster's autopilot.enabled,
# `true` or `false`, from `gcloud container clusters describe`, or `unknown:<why>` when the describe
# was refused or answered no cluster.
autopilot_of() {
  AUTOPILOT_MODE=""
  if ! gcloud container clusters describe "$1" --location="$2" --project="$3" --format=json --quiet >"$WORK/autopilot.json" 2>"$WORK/autopilot.err"; then
    AUTOPILOT_MODE="unknown:gcloud container clusters describe $1 --location $2 --project $3 was refused (it needs container.clusters.get): $(first_line "$WORK/autopilot.err")"
  elif ! AUTOPILOT_MODE="$(jq -er 'if type == "object" and (.name | type == "string") then (.autopilot.enabled == true | tostring) else error("no cluster") end' \
    "$WORK/autopilot.json" 2>"$WORK/autopilot.err")"; then
    AUTOPILOT_MODE="unknown:gcloud container clusters describe $1 answered no cluster: $(first_line "$WORK/autopilot.err")"
  fi
}

# node_escape STEP SUFFIX RESOURCE MODE: on the cluster KUBE names, with MODE its AUTOPILOT_MODE: an
# UNKNOWN for an unknown mode; for Standard (`false`), the ServiceAccount of each system namespace
# pod bound to a node is reviewed for NODE_RIGHTS, and each right allowed is a finding, its label
# followed by SUFFIX; Autopilot (`true`) and "" (no GKE cluster) read nothing.
node_escape() {
  local cstep="$1" suffix="$2" resource="$3" mode="$4" principal label verdict _
  case "$mode" in
    "" | true) return 0 ;;
    unknown:*) unknown "$cstep" "whether the cluster is GKE Autopilot is unknown, so whether a privileged pod on its nodes can read a system ServiceAccount's token is unknown: ${mode#unknown:}"; return 0 ;;
  esac
  if ! kubectl --kubeconfig "$KUBE" get pods --all-namespaces -o json >"$WORK/node-pods.json" 2>"$WORK/node-pods.err"; then
    unknown "$cstep" "the cluster is GKE Standard, and the API server refused the pods' list, so which system ServiceAccounts run on its nodes is unknown (it needs list on pods, container.pods.list on GKE): $(first_line "$WORK/node-pods.err")"
    return 0
  fi
  if ! is_list "$WORK/node-pods.json" "$WORK/node-pods.err"; then
    unknown "$cstep" "the API server's list of pods could not be read: $(first_line "$WORK/node-pods.err")"
    return 0
  fi
  jq -r "$JQ_REC"'[.items[] | select((.spec.nodeName // "") != "" and .status.phase == "Running")
      | [.metadata.namespace, (.spec.serviceAccountName // "default")]] | unique | .[] | rec' \
    "$WORK/node-pods.json" >"$WORK/node-pods.rec"
  printf '%s\n' "$WITNESS_RECORD" >"$WORK/node-principals.rec"
  local ns name accounts=0
  while IFS="$US" read -r -u 3 ns name; do
    system_namespace "$ns" || continue
    accounts=$((accounts + 1))
    sa_record "$ns" "$name" >>"$WORK/node-principals.rec"
  done 3<"$WORK/node-pods.rec"
  [ "$accounts" -gt 0 ] || return 0
  if ! review "$KUBE" "$(principals_json "$WORK/node-principals.rec")" "$NODE_RIGHTS"; then
    unknown "$cstep" "$REVIEW_ERROR"
    return 0
  fi
  witness_held || { unknown "$cstep" "$WITNESS_ERROR"; return 0; }
  while IFS="$US" read -r -u 4 principal label verdict _; do
    [ "$principal" != witness ] && [ "$verdict" = allowed ] || continue
    finding "$principal" "$label$suffix, and it runs a pod on a node of this GKE Standard cluster, where a privileged or hostPath pod reads every token on its node" \
      "$label" "$resource" "$label" "RBAC: a system ServiceAccount with a pod on a node of a GKE Standard cluster"
  done 4<"$WORK/review.rec"
}
