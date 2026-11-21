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
# sa_record NAMESPACE NAME [PREFIX]: a ServiceAccount's principal record, with the groups the API
# server gives it; PREFIX `seed:` labels a system namespace's ServiceAccount, which is reviewed as a
# seed for follow_becomes and never a finding itself.
sa_record() {
  printf '%ssystem:serviceaccount:%s:%s\x1fsystem:serviceaccount:%s:%s\x1fsystem:serviceaccounts,system:serviceaccounts:%s,system:authenticated\n' \
    "${3:-}" "$1" "$2" "$1" "$2" "$1"
}
# FOUND_SAS: each ServiceAccount (<namespace>:<name>) with a finding on the cluster read last, and
# the right of its first finding, for follow_becomes; and each seed, a system namespace's
# ServiceAccount (its principal labelled seed:<its name>) allowed a right asked: no finding itself,
# but who can become it holds what it holds.
declare -A FOUND_SAS=()
# judge_reviews STEP SUFFIX RESOURCE: each review of $WORK/review.rec. The witness's must all be
# allowed; every other allowed review is a finding on RESOURCE (the cluster), its right followed by
# SUFFIX, granted where the API server's reason says, except a seed's, which only seeds FOUND_SAS.
# Returns 1 when the witness was refused.
judge_reviews() {
  local principal label verdict error reason witness_refused=false who
  FOUND_SAS=()
  while IFS="$US" read -r -u 4 principal label verdict error reason; do
    if [ "$principal" = witness ]; then
      [ "$verdict" = allowed ] || witness_refused=true
      continue
    fi
    case "$verdict" in
      allowed)
        if [[ "$principal" == seed:* ]]; then
          who="${principal#seed:system:serviceaccount:}"
          [ -n "${FOUND_SAS[$who]:-}" ] || FOUND_SAS[$who]="$label"
          continue
        fi
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
# follow_becomes KUBECONFIG STEP SUFFIX RESOURCE: who can become a ServiceAccount of FOUND_SAS holds
# what it holds. In the ServiceAccount's namespace a principal becomes it by any namespaced right of
# RIGHTS_TABLE (a pod or a workload controller runs as any ServiceAccount of its namespace; exec,
# attach, an ephemeral container or a pod patch enters one; a rolebinding, bind or escalate grants
# itself those; a token Secret is read) or by create on a custom resource there, and, by its name,
# by create serviceaccounts/token or impersonate (become-rights.jq). Each principal of
# $WORK/principals.rec allowed one of these is a finding naming the chain, but a seed, which is only
# followed; each ServiceAccount so found is followed in turn, up to 5 steps; one found in the fifth
# and not followed is UNKNOWN. A ServiceAccount that becomes only itself is no finding.
follow_becomes() {
  local kube="$1" step="$2" suffix="$3" resource="$4" round sa rights label targets principal verdict error reason who
  local -A followed=() targets_of=()
  for ((round = 0; round < 5; round++)); do
    : >"$WORK/become.tsv"
    while IFS= read -r sa; do
      [ -z "${followed[$sa]:-}" ] || continue
      followed[$sa]=1
      printf '%s\t%s\t%s\n' "${sa%%:*}" "${sa#*:}" "${FOUND_SAS[$sa]}" >>"$WORK/become.tsv"
    done < <(printf '%s\n' ${FOUND_SAS[@]+"${!FOUND_SAS[@]}"} | sed '/^$/d' | sort)
    [ -s "$WORK/become.tsv" ] || return 0
    rights="$(jq -R -s -c -L "$LIB" --argjson table "$TABLE_JSON" --rawfile names "$WORK/api-resources" -f "$LIB/become-rights.jq" "$WORK/become.tsv")"
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
          who="${principal#seed:}"
          who="${who#system:serviceaccount:}"
          [ "${targets_of[$label]:-}" != "$who" ] || continue
          if [[ "$principal" == seed:* ]]; then
            [ -n "${FOUND_SAS[$who]:-}" ] || FOUND_SAS[$who]="$label"
            continue
          fi
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
  CUSTOM_RIGHTS="$(printf '%s\n' "${GUARDED[@]}" | jq -R -s -c -L "$LIB" --rawfile names "$WORK/api-resources" --arg style "$2" -f "$LIB/custom-rights.jq")"
  [ -z "$CUSTOM_ERROR" ]
}
# bound_subjects KUBECONFIG: the cluster's binding subjects (the header's "Binding subjects"), added
# to $WORK/principals.rec when the file does not hold them already: each User subject k8s_subject
# does not allow, with system:authenticated (system:anonymous with system:unauthenticated), each
# Group subject that is one of Kubernetes' own (own_group), alone, and each ServiceAccount subject,
# whether it exists or not, with its groups (a system namespace's as a seed). A person's User
# subject, a Group subject that is not Kubernetes' own (groups_verdict judges those) and
# system:masters (masters_bindings) are left out. Sets BOUND to how many it added, and BINDINGS_OK to
# whether $WORK/bindings.json holds this cluster's bindings; returns 1 with BOUND_ERROR when the API
# server refused the list or answered no JSON.
bound_subjects() {
  local kind ns name label
  local -A held=()
  BOUND=0 BOUND_ERROR="" BINDINGS_OK=false
  if ! kubectl --kubeconfig "$1" get clusterrolebindings,rolebindings --all-namespaces -o json >"$WORK/bindings.json" 2>"$WORK/bindings.err"; then
    BOUND_ERROR="the API server refused the bindings' list (it needs list on clusterrolebindings and rolebindings, container.clusterRoleBindings.list and container.roleBindings.list on GKE): $(first_line "$WORK/bindings.err")"
    return 1
  fi
  if ! jq -r "$JQ_REC"'[.items[]? | (.metadata.namespace // "") as $bns | .subjects[]?
      | select(.kind == "User" or .kind == "Group" or .kind == "ServiceAccount")
      | [.kind, (if .kind == "ServiceAccount" then (.namespace // $bns) else "" end), .name]] | unique[] | rec' \
    "$WORK/bindings.json" >"$WORK/bindings.rec" 2>"$WORK/bindings.err"; then
    BOUND_ERROR="the API server's list of bindings is not JSON: $(first_line "$WORK/bindings.err")"
    return 1
  fi
  BINDINGS_OK=true
  while IFS="$US" read -r label _; do held[$label]=1; done <"$WORK/principals.rec"
  while IFS="$US" read -r -u 7 kind ns name; do
    if [ "$kind" = Group ]; then
      own_group "$name" || continue
      [ -z "${held[group:$name]:-}" ] || continue
      held[group:$name]=1
      printf 'group:%s\x1f\x1f%s\n' "$name" "$name" >>"$WORK/principals.rec"
      BOUND=$((BOUND + 1))
      continue
    fi
    # A ServiceAccount a binding names is reviewed whether or not it exists yet: whoever can make it
    # and a pod in its namespace takes the binding's rights. A system namespace's is a seed.
    if [ "$kind" = ServiceAccount ]; then
      [ -z "${held[system:serviceaccount:$ns:$name]:-}" ] && [ -z "${held[seed:system:serviceaccount:$ns:$name]:-}" ] || continue
      held[system:serviceaccount:$ns:$name]=1
      if system_namespace "$ns"; then sa_record "$ns" "$name" seed:; else sa_record "$ns" "$name"; fi >>"$WORK/principals.rec"
      BOUND=$((BOUND + 1))
      continue
    fi
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
# namespace_groups: Kubernetes' own groups, each reviewed alone and added to $WORK/principals.rec
# when the file does not hold it: system:authenticated, system:serviceaccounts, and
# system:serviceaccounts:<namespace> for the namespace of each reviewed ServiceAccount outside the
# system namespaces, a bound one too (a system namespace's ServiceAccounts are seeds, reviewed with
# that group). A binding to one reaches every member Kubernetes gives it, whatever Google Groups for
# RBAC says. Sets GROUPS_ADDED.
namespace_groups() {
  local group
  GROUPS_ADDED=0
  while IFS= read -r group; do
    grep -qxF -- "group:$group" < <(cut -d"$US" -f1 "$WORK/principals.rec") && continue
    printf 'group:%s\x1f\x1f%s\n' "$group" "$group" >>"$WORK/principals.rec"
    GROUPS_ADDED=$((GROUPS_ADDED + 1))
  done < <({
    printf '%s\n' system:authenticated system:serviceaccounts
    cut -d"$US" -f1 "$WORK/principals.rec" | sed -n 's/^system:serviceaccount:\([^:]*\):.*/system:serviceaccounts:\1/p'
  } | sort -u)
}
# masters_bindings STEP SUFFIX RESOURCE: every binding of $WORK/bindings.json that names the group
# system:masters is a finding, but the bootstrap cluster-admin ClusterRoleBinding (matched by its
# name, its roleRef ClusterRole cluster-admin and its label kubernetes.io/bootstrapping=rbac-defaults):
# its members come from client certificates, which no review can enumerate, so another binding hands
# its role to every holder of one. The bootstrap binding's own grant is why reviews of system:masters
# are always allowed (the witness), so this group is read from the bindings, not reviewed.
masters_bindings() {
  local id role
  [ "$BINDINGS_OK" = true ] || return 0
  while IFS="$US" read -r -u 7 id role; do
    finding "group:system:masters" "bound by $id to $role$2, so every holder of a system:masters certificate takes it" \
      "$role" "$3" "" "$id"
  done 7< <(jq -r "$JQ_REC"'.items[]?
      | select(any(.subjects[]?; .kind == "Group" and .name == "system:masters"))
      | select((.kind == "ClusterRoleBinding" and .metadata.name == "cluster-admin" and .roleRef.kind == "ClusterRole"
                and .roleRef.name == "cluster-admin" and (.metadata.labels // {})["kubernetes.io/bootstrapping"] == "rbac-defaults") | not)
      | [(if .kind == "RoleBinding" then "RoleBinding \(.metadata.namespace)/\(.metadata.name)" else "ClusterRoleBinding \(.metadata.name)" end),
         "\(.roleRef.kind) \(.roleRef.name)"] | rec' "$WORK/bindings.json")
}
# groups_verdict STEP MODE: the Group subjects of $WORK/bindings.json that are not Kubernetes' own
# (none starts system:), judged by where the cluster takes its groups from. MODE `off`: Google Groups
# for RBAC is off, so the API server gives no request such a group, and they are no finding. MODE
# `on:<security group>`: Google Groups for RBAC is on, so each gives its Google group's members its
# rights, which this check neither lists nor judges: UNKNOWN for the cluster, whether one is bound
# now or not. MODE `own`: a cluster that is no GKE cluster takes its users and groups from its own
# authenticator: UNKNOWN when one is bound. MODE `unknown:<why>`: UNKNOWN when one is bound.
groups_verdict() {
  local step="$1" mode="$2" groups=""
  if [ "$BINDINGS_OK" = true ]; then
    groups="$(jq -r '[.items[]?.subjects[]? | select(.kind == "Group" and ((.name | startswith("system:")) | not)) | .name] | unique | join(", ")' \
      "$WORK/bindings.json")"
  fi
  case "$mode" in
    off) ;;
    on:*)
      unknown "$step" "Google Groups for RBAC is on (security group ${mode#on:}), so a Group subject gives its Google group's members its rights, which this check neither lists nor judges (Group subjects bound now: ${groups:-none})"
      ;;
    own)
      [ -z "$groups" ] ||
        unknown "$step" "it takes its users and groups from its own authenticator, not Google, so the Group subjects bound here ($groups) name members this check cannot list"
      ;;
    unknown:*)
      [ -z "$groups" ] ||
        unknown "$step" "whether Google Groups for RBAC is on is unknown (${mode#unknown:}), so the Group subjects bound here ($groups) cannot be judged"
      ;;
  esac
}
# named_rights: sets NAMED_RIGHTS, the name-limited rights (named-rights.jq) of the cluster whose
# bindings bound_subjects read last, for the principals of $WORK/principals.rec: impersonate groups
# and users by name, and escalate, bind, update and patch by name on each role bound to a reviewed
# principal. When the bindings could not be read, it holds impersonate groups system:masters alone.
named_rights() {
  local reviewed bindings='{"items": []}'
  reviewed="$(cut -d"$US" -f1 "$WORK/principals.rec" | jq -R -s -c 'split("\n") | map(select(length > 0))')"
  [ "$BINDINGS_OK" != true ] || bindings="$(<"$WORK/bindings.json")"
  NAMED_RIGHTS="$(jq -c --argjson reviewed "$reviewed" -f "$LIB/named-rights.jq" <<<"$bindings")"
}
# with_rights RIGHTS MORE: RIGHTS and MORE, two JSON arrays of rights, each right once by its key;
# where both hold one, RIGHTS's label stays.
with_rights() { jq -c --argjson more "$2" '. + $more | reduce .[] as $r ([]; if any(.[]; .key == $r.key) then . else . + [$r] end)' <<<"$1"; }
