# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/reviews.sh: SubjectAccessReviews, how their answers are judged, who can
# become a ServiceAccount with a finding, and the principals and rights a cluster adds. Sourced by
# operator-token-minters.sh, whose header says what each step checks.
# review KUBECONFIG PRINCIPALS RIGHTS: one SubjectAccessReview for each principal of the JSON array
# PRINCIPALS ({principal, user, groups}) and each right of the JSON array RIGHTS, all sent as one
# List, so a cluster is asked in one kubectl call. A review's answer is matched to its question by
# the subject and the attributes the API server echoes in its spec. Writes a record of principal,
# right label, allowed|refused|error and the evaluation error per review to $WORK/review.rec;
# returns 1 with REVIEW_ERROR when the API server refused the call or did not answer every review.
review() {
  REVIEW_ERROR=""
  if ! jq -n --argjson principals "$2" --argjson rights "$3" -f "$LIB/review-request.jq" >"$WORK/review-in.json" 2>"$WORK/review.err"; then
    REVIEW_ERROR="this script could not build the reviews: $(first_line "$WORK/review.err")"
    return 1
  fi
  if ! kubectl --kubeconfig "$1" create -f - -o json <"$WORK/review-in.json" >"$WORK/review.json" 2>"$WORK/review.err"; then
    REVIEW_ERROR="the API server refused the review (it needs create on subjectaccessreviews, container.subjectAccessReviews.create on GKE): $(first_line "$WORK/review.err")"
    return 1
  fi
  if ! jq -r --argjson principals "$2" --argjson rights "$3" -L "$LIB" -f "$LIB/review-answer.jq" \
    "$WORK/review.json" >"$WORK/review.rec" 2>"$WORK/review.err"; then
    REVIEW_ERROR="the API server's answer to the reviews is not JSON: $(first_line "$WORK/review.err")"
    return 1
  fi
  local what answered asked
  IFS="$US" read -r what answered asked <"$WORK/review.rec" || true
  if [ "$what" = short ]; then
    REVIEW_ERROR="the API server answered $answered of the $asked reviews asked"
    return 1
  fi
}
# principals_json FILE: the principals of FILE's records (principal, user, groups joined by
# commas) as review's JSON array.
principals_json() {
  jq -R -s -c '[split("\n")[] | select(length > 0) | split("\u001f")
    | {principal: .[0], user: .[1], groups: (.[2] | split(",") | map(select(length > 0)))}]' "$1"
}
# The witness, the first principal of every cluster's reviews: the group the built-in cluster-admin
# binding names, which every review must allow. An API server whose answers cannot say yes proves
# nothing by saying no.
WITNESS_RECORD=$'witness\x1f\x1fsystem:masters'
# Anyone the API server cannot name: every cluster's reviews hold it, so a grant to the group
# system:unauthenticated is a finding whether or not a binding names system:anonymous.
ANONYMOUS_RECORD=$'user:system:anonymous\x1fsystem:anonymous\x1fsystem:unauthenticated'
# FOUND_SAS: each ServiceAccount (<namespace>:<name>) with a finding on the cluster read last, and
# the right of its first finding, for follow_becomes.
declare -A FOUND_SAS=()
# judge_reviews STEP SUFFIX RESOURCE: each review of $WORK/review.rec. The witness's must all be
# allowed; every other allowed review is a finding on RESOURCE (the cluster), its right followed by
# SUFFIX, granted where the API server's reason says. Returns 1 when the witness was refused.
judge_reviews() {
  local principal label verdict error reason witness_refused=false
  FOUND_SAS=()
  while IFS="$US" read -r -u 4 principal label verdict error reason; do
    if [ "$principal" = witness ]; then
      [ "$verdict" = allowed ] || witness_refused=true
      continue
    fi
    case "$verdict" in
      allowed)
        finding "$principal" "$label$2" "$label" "$3" "$label" "$reason"
        if [[ "$principal" =~ ^system:serviceaccount:([^:]+):([^:]+)$ ]] && [ -z "${FOUND_SAS[${BASH_REMATCH[1]}:${BASH_REMATCH[2]}]:-}" ]; then
          FOUND_SAS["${BASH_REMATCH[1]}:${BASH_REMATCH[2]}"]="$label"
        fi
        ;;
      error) unknown "$1" "the review of $principal for $label ended in an evaluation error, so its refusal proves nothing: $error" ;;
    esac
  done 4<"$WORK/review.rec"
  if [ "$witness_refused" = true ]; then
    unknown "$1" "the witness review, the cluster-admin binding's group system:masters, was not allowed, so a refusal from this API server proves nothing"
    return 1
  fi
}
# follow_becomes KUBECONFIG STEP SUFFIX RESOURCE: who can become a ServiceAccount with a finding
# holds what it holds. In the ServiceAccount's namespace a principal becomes it by create pods (a
# pod runs as any ServiceAccount of its namespace), create or get pods/exec (entering a pod that
# runs as it), get, list or watch secrets (a token Secret of it), and, by its name, create
# serviceaccounts/token or impersonate. Each principal of $WORK/principals.rec allowed one of these
# is a finding naming the chain, and each ServiceAccount so found is followed in turn, up to 5
# steps; one found in the fifth and not followed is UNKNOWN. A ServiceAccount that becomes only
# itself is no finding.
follow_becomes() {
  local kube="$1" step="$2" suffix="$3" resource="$4" round sa rights label targets principal verdict error reason
  local -A followed=() targets_of=()
  for ((round = 0; round < 5; round++)); do
    : >"$WORK/become.tsv"
    while IFS= read -r sa; do
      [ -z "${followed[$sa]:-}" ] || continue
      followed[$sa]=1
      printf '%s\t%s\t%s\n' "${sa%%:*}" "${sa#*:}" "${FOUND_SAS[$sa]}" >>"$WORK/become.tsv"
    done < <(printf '%s\n' ${FOUND_SAS[@]+"${!FOUND_SAS[@]}"} | sed '/^$/d' | sort)
    [ -s "$WORK/become.tsv" ] || return 0
    rights="$(jq -R -s -c -f "$LIB/become-rights.jq" "$WORK/become.tsv")"
    targets_of=()
    while IFS=$'\t' read -r label targets; do targets_of[$label]="$targets"; done < <(jq -r '.[] | [.label, (.targets | join(" "))] | @tsv' <<<"$rights")
    if ! review "$kube" "$(principals_json "$WORK/principals.rec")" "$rights"; then
      unknown "$step" "$REVIEW_ERROR"
      return 0
    fi
    while IFS="$US" read -r -u 4 principal label verdict error reason; do
      [ "$principal" != witness ] || continue
      case "$verdict" in
        allowed)
          [ "${targets_of[$label]:-}" != "${principal#system:serviceaccount:}" ] || continue
          finding "$principal" "$label$suffix" "$label" "$resource" "$label" "$reason"
          if [[ "$principal" =~ ^system:serviceaccount:([^:]+):([^:]+)$ ]] && [ -z "${FOUND_SAS[${BASH_REMATCH[1]}:${BASH_REMATCH[2]}]:-}" ]; then
            FOUND_SAS["${BASH_REMATCH[1]}:${BASH_REMATCH[2]}"]="$label"
          fi
          ;;
        error) unknown "$step" "the review of $principal for $label ended in an evaluation error, so its refusal proves nothing: $error" ;;
      esac
    done 4<"$WORK/review.rec"
  done
  for sa in ${FOUND_SAS[@]+"${!FOUND_SAS[@]}"}; do
    [ -n "${followed[$sa]:-}" ] ||
      unknown "$step" "who can become a ServiceAccount with a finding is followed 5 steps; system:serviceaccount:$sa, found in the fifth, is not followed further"
  done
}
# custom_rights KUBECONFIG STYLE: sets CUSTOM_RIGHTS, the JSON array, as rights_json writes rights,
# of create on each custom resource the API server's discovery lists as namespaced and creatable,
# in each guarded pair's namespace: a custom resource's controller may make a pod from it (an agent
# sandbox does), so create on one there is the same path as create pods. Custom is any API group
# outside Kubernetes' own: not the core group, no dotless group (apps, batch), and no *.k8s.io
# group other than a *.x-k8s.io one. STYLE `guard` labels each right as GUARD_RIGHTS does. Returns
# 1 with CUSTOM_ERROR when the discovery could not be read whole; CUSTOM_RIGHTS holds what it listed.
custom_rights() {
  CUSTOM_ERROR=""
  if ! kubectl --kubeconfig "$1" api-resources --namespaced=true --verbs=create -o name >"$WORK/api-resources" 2>"$WORK/api-resources.err"; then
    CUSTOM_ERROR="the API server's discovery of namespaced resources could not be read whole, so create on a custom resource it leaves out is not reviewed: $(first_line "$WORK/api-resources.err")"
  fi
  CUSTOM_RIGHTS="$(printf '%s\n' "${GUARDED[@]}" | jq -R -s -c --rawfile names "$WORK/api-resources" --arg style "$2" -f "$LIB/custom-rights.jq")"
  [ -z "$CUSTOM_ERROR" ]
}
# bound_subjects KUBECONFIG: the cluster's binding subjects (the header's "Binding subjects"), added
# to $WORK/principals.rec: each User subject of its ClusterRoleBindings and RoleBindings that
# k8s_subject does not allow and that the file does not hold already, with system:authenticated
# (system:anonymous with system:unauthenticated). A person's User subject, every Group subject and
# every ServiceAccount subject are left out. Sets BOUND to how many it added; returns 1 with
# BOUND_ERROR when the API server refused the list or answered no JSON.
bound_subjects() {
  local kind name label
  local -A held=()
  BOUND=0 BOUND_ERROR=""
  if ! kubectl --kubeconfig "$1" get clusterrolebindings,rolebindings --all-namespaces -o json >"$WORK/bindings.json" 2>"$WORK/bindings.err"; then
    BOUND_ERROR="the API server refused the bindings' list (it needs list on clusterrolebindings and rolebindings, container.clusterRoleBindings.list and container.roleBindings.list on GKE): $(first_line "$WORK/bindings.err")"
    return 1
  fi
  if ! jq -r "$JQ_REC"'[.items[]?.subjects[]? | select(.kind == "User") | [.kind, .name]] | unique[] | rec' \
    "$WORK/bindings.json" >"$WORK/bindings.rec" 2>"$WORK/bindings.err"; then
    BOUND_ERROR="the API server's list of bindings is not JSON: $(first_line "$WORK/bindings.err")"
    return 1
  fi
  while IFS="$US" read -r label _; do held[$label]=1; done <"$WORK/principals.rec"
  while IFS="$US" read -r -u 7 kind name; do
    k8s_subject "$kind" "" "$name"
    [ "$SUBJECT_OK" != true ] || continue
    case "$SUBJECT" in system:serviceaccount:*) continue ;; esac
    [ -z "${held[$SUBJECT]:-}" ] || continue
    held[$SUBJECT]=1
    if [ "$name" = system:anonymous ]; then
      printf '%s\x1f%s\x1fsystem:unauthenticated\n' "$SUBJECT" "$name" >>"$WORK/principals.rec"
    else
      printf '%s\x1f%s\x1fsystem:authenticated\n' "$SUBJECT" "$name" >>"$WORK/principals.rec"
    fi
    BOUND=$((BOUND + 1))
  done 7<"$WORK/bindings.rec"
}
