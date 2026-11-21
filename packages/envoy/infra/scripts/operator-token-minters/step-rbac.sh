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
else
  mark
  # Every guarded pair's rights and the other clusters' guard rights, each right once: where both
  # list one, the full list's label stays.
  rights="$(jq -c --argjson guard "$GUARD_RIGHTS" '. + $guard | reduce .[] as $r ([]; if any(.[]; .key == $r.key) then . else . + [$r] end)' <<<"$(rights_json db)")"
  custom_rights "$KUBE" plain || unknown "$step" "$CUSTOM_ERROR"
  rights="$(jq -c --argjson custom "$CUSTOM_RIGHTS" '. + $custom | reduce .[] as $r ([]; if any(.[]; .key == $r.key) then . else . + [$r] end)' <<<"$rights")"
  jq -r "$JQ_REC"'.items[]? | [.metadata.namespace, .metadata.name, (.metadata.annotations["iam.gke.io/gcp-service-account"] // "")] | rec' \
    "$WORK/serviceaccounts.json" >"$WORK/serviceaccounts.rec"
  printf '%s\n%s\n' "$WITNESS_RECORD" "$ANONYMOUS_RECORD" >"$WORK/principals.rec"
  declare -A seen_groups=() seen_accounts=() pattern_matched=()
  accounts=0 agents=0
  while IFS="$US" read -r -u 3 ns name bound; do
    ! system_namespace "$ns" || continue
    accounts=$((accounts + 1))
    printf 'system:serviceaccount:%s:%s\x1fsystem:serviceaccount:%s:%s\x1fsystem:serviceaccounts,system:serviceaccounts:%s,system:authenticated\n' \
      "$ns" "$name" "$ns" "$name" "$ns" >>"$WORK/principals.rec"
    is_agent=false
    for pattern in "${AGENT_NAMESPACES[@]}"; do
      # shellcheck disable=SC2053 # the pattern is a glob on purpose: legion-*-sessions
      if [[ "$ns" == $pattern ]]; then
        is_agent=true
        pattern_matched[$pattern]=1
      fi
    done
    if [ "$is_agent" = true ]; then
      agents=$((agents + 1))
      seen_groups[system:serviceaccounts]=1 seen_groups[system:serviceaccounts:$ns]=1 seen_groups[system:authenticated]=1
    fi
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
  while IFS= read -r group; do
    printf 'group:%s\x1f\x1f%s\n' "$group" "$group" >>"$WORK/principals.rec"
  done < <(printf '%s\n' ${seen_groups[@]+"${!seen_groups[@]}"} | sed '/^$/d' | sort)
  bound_subjects "$KUBE" || unknown "$step" "$BOUND_ERROR"
  if [ "$((accounts + ${#seen_accounts[@]} + BOUND))" -eq 0 ]; then
    unknown "$step" "the cluster lists no ServiceAccount outside its system namespaces and no binding subject, so there is no principal to review"
  elif ! review "$KUBE" "$(principals_json "$WORK/principals.rec")" "$rights"; then
    unknown "$step" "$REVIEW_ERROR"
  elif judge_reviews "$step" "" "cluster:$CLUSTER"; then
    RBAC_OK=true
    follow_becomes "$KUBE" "$step" "" "cluster:$CLUSTER"
    if unchanged; then
      pass "$step" "$((accounts + ${#seen_accounts[@]} + ${#seen_groups[@]} + BOUND + 1)) principals ($accounts ServiceAccounts outside the system namespaces, $agents of them in the agent namespaces, ${#seen_accounts[@]} Google service accounts, ${#seen_groups[@]} groups, $BOUND binding subjects, and anyone unauthenticated), $(jq length <<<"$rights") rights each: none allowed"
    fi
  fi
fi
