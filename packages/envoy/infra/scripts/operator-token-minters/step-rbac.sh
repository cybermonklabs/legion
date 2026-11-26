# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/step-rbac.sh: the step "live: RBAC" on the operator's cluster. Sourced by
# operator-token-minters.sh, whose header says what each step checks.
# --- live: RBAC on the operator's cluster ---
# Every ServiceAccount outside the system namespaces is a principal, as on every other cluster of
# the pool: a CI runner's ServiceAccount in a namespace no agent uses can still run a pod as the
# operator. The agent namespaces name the groups also reviewed alone, and must hold a ServiceAccount.
step="live: RBAC on $cluster_name"
RBAC_OK=false
if [ "$OPERATOR_FOUND" != true ]; then
  unknown "$step" "the operator's cluster could not be found (live: clusters)"
elif [ "${#AGENT_NAMESPACES[@]}" -eq 0 ]; then
  unknown "$step" "no --agent-namespace: the namespaces whose ServiceAccounts agents run as are not named"
elif ! kube_for "$CLUSTER" "$OPERATOR_LOCATION" "$PROJECT"; then
  unknown "$step" "$KUBE_ERROR"
elif ! kubectl --kubeconfig "$KUBE" get serviceaccounts --all-namespaces -o json >"$WORK/serviceaccounts.json" 2>"$WORK/serviceaccounts.err"; then
  unknown "$step" "the API server refused the ServiceAccounts' list (it needs list on serviceaccounts, container.serviceAccounts.list on GKE): $(first_line "$WORK/serviceaccounts.err")"
elif ! is_list "$WORK/serviceaccounts.json" "$WORK/serviceaccounts.err"; then
  unknown "$step" "the API server's list of ServiceAccounts could not be read: $(first_line "$WORK/serviceaccounts.err")"
else
  mark
  # Every guarded pair's rights and the other clusters' guard rights, each right once: where both
  # list one, the full list's label stays; and the custom resources' and the admission resources'
  # rights, from this cluster's discovery.
  rights="$(with_rights "$(rights_json db)" "$GUARD_RIGHTS")"
  custom_rights "$KUBE" plain || unknown "$step" "$CUSTOM_ERROR"
  rights="$(with_rights "$rights" "$CUSTOM_RIGHTS")"
  admission_rights "$KUBE" || unknown "$step" "$ADMISSION_ERROR"
  rights="$(with_rights "$rights" "$ADMISSION_RIGHTS")"
  # Admission power outside the admission objects: webhook backends, policy engines' resources and
  # admission policies' parameters (admission-backends.sh).
  admission_power "$KUBE" "$step" operator
  rights="$(with_rights "$rights" "$POWER_RIGHTS")"
  # Each token Secret of a guarded namespace, by name: a review that names no Secret is refused for
  # a get limited to the name, and a Secret of type kubernetes.io/service-account-token holds its
  # ServiceAccount's token. Those a guarded pair's ServiceAccount lists (its `secrets`), and every
  # one the namespace's list of that type holds, whichever ServiceAccount it names; a refused list
  # is UNKNOWN. The list asks for the Table form first, whose rows carry no Secret's data: kubectl
  # asks the API server for the server-side Table form for its default output alone (Accept:
  # application/json;as=Table;v=v1;g=meta.k8s.io, then the same for v1beta1, then plain
  # application/json), whose rows carry each Secret's name,
  # type, number of data keys and age, and beside them the Secret's metadata alone: kubectl sends no
  # includeObject, and the API server then puts each object's PartialObjectMetadata in its row
  # (k8s.io/apiserver's endpoints/handlers/response.go, asTable). With -o name, -o jsonpath or -o
  # json, kubectl asks for plain JSON and gets the Secrets whole, data included, and picks the names
  # out itself (kubectl's get.go, transformRequests), and get --raw sends no Accept header that
  # could ask for a PartialObjectMetadataList; both read with kubectl 1.36.4 against a server that
  # records each request's Accept header. So the list is that default output, and the names are its
  # first column under its header. Two cases still bring a token into kubectl's memory, though
  # kubectl prints and writes none of it:
  #   - Each row's metadata is the Secret's whole metadata. A token Secret made by `kubectl apply`
  #     from a manifest that held its token carries that token in its
  #     kubectl.kubernetes.io/last-applied-configuration annotation.
  #   - The Accept header ends with plain application/json, so a server or proxy that cannot make
  #     the Table answers with the Secrets whole, data included; kube-apiserver makes the Table for
  #     core Secrets. kubectl then prints its own columns, NAME and AGE alone (read with kubectl
  #     1.36.4 against a server that answers plain JSON whatever the Accept header asks), so a list
  #     whose header is not NAME TYPE DATA AGE is UNKNOWN, and the run never passes on it.
  # An empty list passes the header check: kubectl prints "No resources found in <namespace>
  # namespace." to stderr alone (kubectl's get.go), so the list's file is empty and holds no name.
  : >"$WORK/token-secrets.tsv"
  jq -r '.items[] | select(("\(.metadata.namespace)/\(.metadata.name)") as $p | any($ARGS.positional[]; . == $p))
    | .metadata.namespace as $ns | (.secrets // [])[] | [$ns, .name] | @tsv' "$WORK/serviceaccounts.json" --args "${GUARDED[@]}" >>"$WORK/token-secrets.tsv"
  for guarded_ns in $(printf '%s\n' "${GUARDED[@]%%/*}" | sort -u); do
    if ! kubectl --kubeconfig "$KUBE" get secrets -n "$guarded_ns" --field-selector type=kubernetes.io/service-account-token \
      --server-print=true >"$WORK/token-secret-rows.txt" 2>"$WORK/token-secrets.err"; then
      unknown "$step" "the API server refused the list of token Secrets in $guarded_ns (it needs list on secrets there, container.secrets.list on GKE), so a token Secret no ServiceAccount lists is not asked by name: $(first_line "$WORK/token-secrets.err")"
    elif [ -s "$WORK/token-secret-rows.txt" ] &&
      ! awk 'NR == 1 { exit !(NF == 4 && $1 == "NAME" && $2 == "TYPE" && $3 == "DATA" && $4 == "AGE") }' "$WORK/token-secret-rows.txt"; then
      unknown "$step" "the API server's list of token Secrets in $guarded_ns came without the Table's columns NAME TYPE DATA AGE, so it was not the Table form: a server that cannot make the Table sends the Secrets whole, data included, which kubectl read though it printed none"
    elif ! awk 'NR > 1 { print $1 }' "$WORK/token-secret-rows.txt" >"$WORK/token-secret-names.txt" ||
      grep -qvE '^([a-z0-9]([-.a-z0-9]*[a-z0-9])?)?$' "$WORK/token-secret-names.txt"; then
      unknown "$step" "the API server's list of token Secrets in $guarded_ns holds a row whose first column is no Kubernetes name"
    else
      while IFS= read -r secret_name; do
        [ -z "$secret_name" ] || printf '%s\t%s\n' "$guarded_ns" "$secret_name" >>"$WORK/token-secrets.tsv"
      done <"$WORK/token-secret-names.txt"
    fi
  done
  rights="$(with_rights "$rights" "$(jq -R -s -c '[split("\n")[] | select(length > 0) | split("\t") | {ns: .[0], n: .[1]}] | unique[]
      | {verb: "get", group: "", resource: "secrets", sub: "", ns: .ns, name: .n, label: "get secrets \(.n) in \(.ns)"}
      | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}' "$WORK/token-secrets.tsv" | jq -s -c .)")"
  # The broker's pod fence (broker-fence.sh), read once the admission discovery, the webhooks'
  # backends and the guarded namespaces' token Secrets are: while it is credited (FENCE_CREDITED),
  # this cluster's reviews judge what it refuses as credits, and who can delete it is asked beside
  # who can write it.
  fence_live "$KUBE" "$step"
  [ -z "$FENCE_CREDITED" ] || rights="$(with_rights "$rights" "$FENCE_WRITE_RIGHTS")"
  jq -r "$JQ_REC"'.items[] | [.metadata.namespace, .metadata.name, (.metadata.annotations["iam.gke.io/gcp-service-account"] // "")] | rec' \
    "$WORK/serviceaccounts.json" >"$WORK/serviceaccounts.rec"
  printf '%s\n%s\n' "$WITNESS_RECORD" "$ANONYMOUS_RECORD" >"$WORK/principals.rec"
  declare -A seen_accounts=() pattern_matched=()
  accounts=0 agents=0
  while IFS="$US" read -r -u 3 ns name bound; do
    if system_namespace "$ns"; then
      sa_record "$ns" "$name" seed: >>"$WORK/principals.rec"
      continue
    fi
    accounts=$((accounts + 1))
    sa_record "$ns" "$name" >>"$WORK/principals.rec"
    is_agent=false
    for pattern in "${AGENT_NAMESPACES[@]}"; do
      # shellcheck disable=SC2053 # the pattern is a glob on purpose: legion-*-sessions
      if [[ "$ns" == $pattern ]]; then
        is_agent=true
        pattern_matched[$pattern]=1
      fi
    done
    [ "$is_agent" != true ] || agents=$((agents + 1))
    # The Google service account a ServiceAccount is bound to acts as a User of its email; the
    # operator's own is allowed, as k8s_subject says.
    if [ -n "$bound" ]; then
      k8s_subject User "" "$bound"
      [ "$SUBJECT_OK" = true ] || seen_accounts[$bound]=1
    fi
  done 3<"$WORK/serviceaccounts.rec"
  [ "$agents" -gt 0 ] ||
    unknown "$step" "the cluster lists no ServiceAccount in the agent namespaces (${AGENT_NAMESPACES[*]}), so no agent's principal is among those reviewed"
  # Each pattern alone: a mistyped one (legion-*-session) drops its namespaces while the others match.
  for pattern in "${AGENT_NAMESPACES[@]}"; do
    [ -n "${pattern_matched[$pattern]:-}" ] ||
      unknown "$step" "--agent-namespace $pattern matches no namespace that holds a ServiceAccount, so the agents it was to name are not reviewed as agents"
  done
  while IFS= read -r bound; do
    printf 'serviceAccount:%s\x1f%s\x1fsystem:authenticated\n' "$bound" "$bound" >>"$WORK/principals.rec"
  done < <(printf '%s\n' ${seen_accounts[@]+"${!seen_accounts[@]}"} | sed '/^$/d' | sort)
  bound_subjects "$KUBE" || unknown "$step" "$BOUND_ERROR"
  namespace_groups
  # Groups that are not Kubernetes' own, judged by where this cluster takes its groups from, and
  # every binding of system:masters but the bootstrap one.
  groups_verdict "$step" "${GROUPS_CONFIG["$PROJECT/$OPERATOR_LOCATION/$CLUSTER"]:-unknown:the cluster list gave no authenticatorGroupsConfig}"
  settings_verdict "$step" "${CLUSTER_SETTINGS["$PROJECT/$OPERATOR_LOCATION/$CLUSTER"]:-}"
  # GKE Standard runs a privileged or hostPath pod, which Autopilot refuses: such a pod on the
  # operator's node reads the kubelet's credentials and every ServiceAccount token on the node, the
  # operator's included, by a path no review here sees.
  [ "${AUTOPILOT["$PROJECT/$OPERATOR_LOCATION/$CLUSTER"]:-}" = true ] ||
    unknown "$step" "the cluster is GKE Standard, not Autopilot, so a privileged or hostPath pod on the operator's node can read the kubelet's credentials and the operator's ServiceAccount token there, a node escape no RBAC review here sees"
  masters_bindings "$step" "" "cluster:$CLUSTER"
  # A right a role limits to a resource by name never answers a review that names nothing, so
  # those are read rather than reviewed: live_named below reads the cluster's own Roles and
  # ClusterRoles for each rule whose resourceNames limit one of the rights reviewed, and names the
  # subject the binding gives it. So each right is asked without a name, in each namespace where
  # it matters: the guarded namespaces and cluster-wide (RIGHTS_TABLE and the rights joined above),
  # and, for the writes of a binding or a role, each namespace that holds a RoleBinding
  # (binding_rights), where binding a role grants what it holds there, that namespace's Secrets
  # among them. The review finds a grant of any authorizer that names no resource, and GKE's IAM
  # authorizer grants a verb on a resource, never on one resource's name.
  binding_rights || unknown "$step" "$BINDING_ERROR"
  rights="$(with_rights "$rights" "$BINDING_RIGHTS")"
  # Every cluster has default/default, so a list that holds no ServiceAccount outside the system
  # namespaces is not the cluster's whole list.
  [ "$accounts" -gt 0 ] ||
    unknown "$step" "the cluster lists no ServiceAccount outside its system namespaces, though every cluster has default/default, so its list cannot be read as whole"
  if [ "$((accounts + ${#seen_accounts[@]} + BOUND))" -eq 0 ]; then
    unknown "$step" "the cluster lists no ServiceAccount outside its system namespaces and no binding subject, so there is no principal to review"
  elif ! review "$KUBE" "$(principals_json "$WORK/principals.rec")" "$rights" "$FENCE_CREDITED"; then
    unknown "$step" "$REVIEW_ERROR"
  elif judge_reviews "$step" "" "cluster:$CLUSTER"; then
    RBAC_OK=true
    live_named "$KUBE" "$step" "" "cluster:$CLUSTER" "$rights" "$FENCE_CREDITED"
    csr_chains "" "cluster:$CLUSTER"
    follow_becomes "$KUBE" "$step" "" "cluster:$CLUSTER" "$FENCE_CREDITED"
    remember_cluster "$KUBE" "$step" "" "cluster:$CLUSTER" "$FENCE_CREDITED"
    if unchanged; then
      credits="$(credited)"
      pass "$step" "$((accounts + ${#seen_accounts[@]} + GROUPS_ADDED + BOUND + 1)) principals ($accounts ServiceAccounts outside the system namespaces, $agents of them in the agent namespaces, ${#seen_accounts[@]} Google service accounts, $GROUPS_ADDED of Kubernetes' own groups, $BOUND binding subjects, and anyone unauthenticated), $(jq length <<<"$rights") rights each reviewed, and every rule of the cluster's roles that limits one of them by name read: none allowed$([ "$credits" -eq 0 ] || echo " but $credits grants whose use on the broker's pods its pod fence refuses")"
    fi
  fi
fi
# The code step's grants the declared fence refuses, now that the cluster's fence was read or not.
fence_settle
