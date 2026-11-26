# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/node-escape.sh: GKE Standard's node escape on a cluster that is not the
# operator's. Sourced by operator-token-minters.sh, whose header says what each step checks.
#
# A system namespace's ServiceAccount is no finding on its own (principals.sh, system_namespace): its
# pods are the cluster's own. But GKE Standard admits a privileged or hostPath pod, which Autopilot
# refuses, and such a pod reads every token on its node. So on a Standard cluster a system
# ServiceAccount with a pod on a node holds, for whoever can reach that node, every right the
# cluster's own review asks: each it is allowed, outright or limited by name in a live role, is a
# finding naming it, judged as the cluster's other principals are (the certificate signing chain's
# parts only as a whole chain, and a review that ended in an evaluation error UNKNOWN).

# NODE_TOKEN_RIGHTS: create serviceaccounts/token for each guarded pair, asked beside the cluster's
# rights: a token of the node can ask it whether or not the pair's namespace is there yet.
NODE_TOKEN_RIGHTS="$(jq -n -c --args '
  [$ARGS.positional[] | split("/") | {verb: "create", group: "", resource: "serviceaccounts", sub: "token", ns: .[0], name: .[1],
                                      label: "create serviceaccounts/token \(.[1]) in \(.[0])"}
   | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}]' "${GUARDED[@]}")"

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

# node_escape STEP SUFFIX RESOURCE MODE RIGHTS: on the cluster KUBE names, with MODE its
# AUTOPILOT_MODE: an UNKNOWN for an unknown mode; for Standard (`false`), the ServiceAccount of each
# system namespace pod bound to a node and running is reviewed for RIGHTS, the rights the cluster's
# own review asked (read_other_cluster), and NODE_TOKEN_RIGHTS; each right allowed, and each of the
# rules of the cluster's roles that live_named read ($WORK/named.rec) that gives one by name, is a
# finding, its label followed by SUFFIX. Autopilot (`true`) and "" (no GKE cluster) read nothing.
# Every GKE cluster runs kube-system's pods, so a list of the cluster's pods that holds none cannot
# be told from one that saw nothing, and is UNKNOWN; a list that holds pods, none of them a system
# ServiceAccount's on a node, is the clean case.
node_escape() {
  local cstep="$1" suffix="$2" resource="$3" mode="$4" rights principal label verdict error reason _
  local what id detail kind ns name accounts=0
  local why=", and it runs a pod on a node of this GKE Standard cluster, where a privileged or hostPath pod reads every token on its node"
  local -A on_node=()
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
  if [ "$(jq '.items | length' "$WORK/node-pods.json")" -eq 0 ]; then
    unknown "$cstep" "the cluster is GKE Standard, and the API server's list of pods holds no pod, though every GKE cluster runs kube-system's, so it cannot be told from a list that saw nothing and which system ServiceAccounts run on its nodes is unknown"
    return 0
  fi
  jq -r "$JQ_REC"'[.items[] | select((.spec.nodeName // "") != "" and .status.phase == "Running")
      | [.metadata.namespace, (.spec.serviceAccountName // "default")]] | unique | .[] | rec' \
    "$WORK/node-pods.json" >"$WORK/node-pods.rec"
  printf '%s\n' "$WITNESS_RECORD" >"$WORK/node-principals.rec"
  while IFS="$US" read -r -u 3 ns name; do
    system_namespace "$ns" || continue
    accounts=$((accounts + 1))
    on_node["system:serviceaccount:$ns:$name"]=1
    sa_record "$ns" "$name" >>"$WORK/node-principals.rec"
  done 3<"$WORK/node-pods.rec"
  [ "$accounts" -gt 0 ] || return 0
  rights="$(with_rights "$5" "$NODE_TOKEN_RIGHTS")"
  if ! review "$KUBE" "$(principals_json "$WORK/node-principals.rec")" "$rights"; then
    unknown "$cstep" "$REVIEW_ERROR"
    return 0
  fi
  witness_held || { unknown "$cstep" "$WITNESS_ERROR"; return 0; }
  csr_reset
  while IFS="$US" read -r -u 4 principal label verdict error reason _; do
    [ "$principal" != witness ] || continue
    case "$verdict" in
      allowed)
        ! csr_part "$principal" "$label" "$reason" || continue
        finding "$principal" "$label$suffix$why" "$label" "$resource" "$label" "$reason"
        ;;
      error) unknown "$cstep" "the review of $principal, which runs a pod on a node of this GKE Standard cluster, for $label ended in an evaluation error, so its refusal proves nothing: $error" ;;
    esac
  done 4<"$WORK/review.rec"
  # The same rights limited by name: live_named's read of this cluster's roles, whose subjects it
  # judged as seeds, names each node account's own.
  if [ "${ROLES_OK:-false}" = true ]; then
    while IFS="$US" read -r -u 6 what id detail kind ns name _; do
      [ "$what" = grant ] || continue
      named_principal "$kind" "$ns" "$name"
      [ -n "${on_node[${NAMED_PRINCIPAL#seed:}]:-}" ] || continue
      ! csr_part "${NAMED_PRINCIPAL#seed:}" "$detail" "RBAC: $id limits it by name" || continue
      finding "${NAMED_PRINCIPAL#seed:}" "$detail$suffix$why" "$detail" "$resource" "$detail" "RBAC: $id limits it by name"
    done 6<"$WORK/named.rec"
  fi
  csr_chains "$suffix$why" "$resource"
}
