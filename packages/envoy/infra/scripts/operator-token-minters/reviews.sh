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
# The certificate signing chain's parts, by principal, which csr_part gathers from a cluster's
# reviews, its named rights and a render, and csr_chains judges once they are all read.
declare -A CSR_CREATE_OF=() CSR_APPROVAL_OF=() CSR_SIGNERS_OF=() CSR_WHY_OF=()
csr_reset() { CSR_CREATE_OF=() CSR_APPROVAL_OF=() CSR_SIGNERS_OF=() CSR_WHY_OF=(); }
# csr_part PRINCIPAL LABEL REASON: when LABEL is a part of the chain (CSR_RIGHTS: create, an approval
# write, a signer's approve, each by name or not), records it for PRINCIPAL and returns 0; else 1.
csr_part() {
  local label="$2" names
  case "$label" in
    "$CSR_CREATE") CSR_CREATE_OF[$1]=1 ;;
    *" certificatesigningrequests/approval.certificates.k8s.io "*)
      names="${label#* certificatesigningrequests/approval.certificates.k8s.io }"
      if [ "$names" = cluster-wide ]; then names=""; else names=" ${names% cluster-wide}"; fi
      CSR_APPROVAL_OF[$1]="${CSR_APPROVAL_OF[$1]:+${CSR_APPROVAL_OF[$1]}, }${label%% *} certificatesigningrequests/approval$names"
      CSR_WHY_OF[$1]="$3"
      ;;
    "$CSR_ANY_SIGNER") CSR_SIGNERS_OF[$1]="of any name" ;;
    "$CSR_SIGNER_PREFIX"*)
      # A rule naming a signer answers the review of that name and the unnamed right's named read
      # alike, so each name is kept once.
      names="${label#"$CSR_SIGNER_PREFIX"}" names="${names% cluster-wide}"
      case " and ${CSR_SIGNERS_OF[$1]:-} and " in
        " and of any name and " | *" and $names and "*) ;;
        *) CSR_SIGNERS_OF[$1]="${CSR_SIGNERS_OF[$1]:+${CSR_SIGNERS_OF[$1]} and }$names" ;;
      esac
      ;;
    *) return 1 ;;
  esac
}
# csr_chains SUFFIX RESOURCE: one finding on RESOURCE, its right followed by SUFFIX, for each
# principal csr_part saw allowed both an approval write and a signer, naming create too when it may
# make its own request; a seed's chain only seeds FOUND_SAS. Then forgets the parts.
csr_chains() {
  local principal chain who
  for principal in $(printf '%s\n' ${CSR_APPROVAL_OF[@]+"${!CSR_APPROVAL_OF[@]}"} | sort); do
    [ -n "${CSR_SIGNERS_OF[$principal]:-}" ] || continue
    chain="${CSR_CREATE_OF[$principal]:+create certificatesigningrequests, }${CSR_APPROVAL_OF[$principal]} and approve signers ${CSR_SIGNERS_OF[$principal]} cluster-wide, so it can have the cluster sign it a client certificate for a user, group or node it names"
    if [[ "$principal" == seed:* ]]; then
      who="${principal#seed:system:serviceaccount:}"
      [ -n "${FOUND_SAS[$who]:-}" ] || FOUND_SAS[$who]="$chain"
      continue
    fi
    finding "$principal" "$chain$1" "$chain" "$2" "$chain" "${CSR_WHY_OF[$principal]:-}"
    if [[ "$principal" =~ ^system:serviceaccount:([^:]+):([^:]+)$ ]] && [ -z "${FOUND_SAS[${BASH_REMATCH[1]}:${BASH_REMATCH[2]}]:-}" ]; then
      FOUND_SAS["${BASH_REMATCH[1]}:${BASH_REMATCH[2]}"]="$chain"
    fi
  done
  csr_reset
}
# judge_reviews STEP SUFFIX RESOURCE: each review of $WORK/review.rec. The witness's must all be
# allowed; every other allowed review is a finding on RESOURCE (the cluster), its right followed by
# SUFFIX, granted where the API server's reason says, except a seed's, which only seeds FOUND_SAS,
# and the certificate signing chain's parts, which csr_part gathers for csr_chains (the caller runs
# it after live_named, whose named approvals join the chain). Returns 1 when the witness was
# refused.
judge_reviews() {
  local principal label verdict error reason witness_refused=false who
  FOUND_SAS=()
  csr_reset
  while IFS="$US" read -r -u 4 principal label verdict error reason; do
    if [ "$principal" = witness ]; then
      [ "$verdict" = allowed ] || witness_refused=true
      continue
    fi
    case "$verdict" in
      allowed)
        ! csr_part "$principal" "$label" "$reason" || continue
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
# seed_named KIND NAMESPACE NAME DETAIL: a system namespace's ServiceAccount a live role allows a right
# by name (DETAIL) is never a finding itself, but who can become it holds that right, so it seeds
# FOUND_SAS, as judge_reviews seeds one allowed a right outright; returns 0 when it did. RBAC takes
# the account as a ServiceAccount subject or as the User system:serviceaccount:<namespace>:<name>
# (k8s_subject allows both), so both forms seed it.
seed_named() {
  local ns="$2" name="$3"
  case "$1:$name" in
    ServiceAccount:*) ;;
    User:system:serviceaccount:*:*)
      name="${name#system:serviceaccount:}"
      ns="${name%%:*}" name="${name#*:}"
      ;;
    *) return 1 ;;
  esac
  system_namespace "$ns" || return 1
  [ -n "${FOUND_SAS[$ns:$name]:-}" ] || FOUND_SAS[$ns:$name]="$4"
}
# follow_becomes KUBECONFIG STEP SUFFIX RESOURCE: who can become a ServiceAccount of FOUND_SAS holds
# what it holds. In the ServiceAccount's namespace a principal becomes it by any namespaced right of
# RIGHTS_TABLE (a pod or a workload controller runs as any ServiceAccount of its namespace; exec,
# attach, an ephemeral container or a pod patch enters one; a rolebinding, bind or escalate grants
# itself those; a token Secret is read) or by create, update or patch of a custom resource there,
# and, by its name, by create serviceaccounts/token or impersonate (become-rights.jq). Each
# principal of $WORK/principals.rec allowed one of these is a finding naming the chain, but a seed,
# which is only followed; and so is a binding subject whose live role limits one of them by name (a
# patch of one Deployment sets its serviceAccountName), read as live_named reads the others when it
# read the cluster's roles. Each ServiceAccount so found is followed in turn, up to 5 steps; one
# found in the fifth and not followed is UNKNOWN. A ServiceAccount that becomes only itself is no
# finding.
follow_becomes() {
  local kube="$1" step="$2" suffix="$3" resource="$4" round sa rights label targets principal verdict error reason who why
  local what id detail kind ns name
  local -A followed=() targets_of=() targets_why=()
  for ((round = 0; round < 5; round++)); do
    : >"$WORK/become.tsv"
    while IFS= read -r sa; do
      [ -z "${followed[$sa]:-}" ] || continue
      followed[$sa]=1
      printf '%s\t%s\t%s\n' "${sa%%:*}" "${sa#*:}" "${FOUND_SAS[$sa]}" >>"$WORK/become.tsv"
    done < <(printf '%s\n' ${FOUND_SAS[@]+"${!FOUND_SAS[@]}"} | sed '/^$/d' | sort)
    [ -s "$WORK/become.tsv" ] || return 0
    rights="$(jq -R -s -c -L "$LIB" --argjson table "$TABLE_JSON" --rawfile names "$WORK/api-resources" -f "$LIB/become-rights.jq" "$WORK/become.tsv")"
    targets_of=() targets_why=()
    while IFS=$'\t' read -r label targets; do targets_of[$label]="$targets"; done < <(jq -r '.[] | [.label, (.targets | join(" "))] | @tsv' <<<"$rights")
    while IFS=$'\t' read -r why targets; do targets_why[$why]="$targets"; done < <(jq -r '.[] | [.why, (.targets | join(" "))] | @tsv' <<<"$rights")
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
    # The same rights limited by name in a live role, which a review naming nothing never finds.
    [ "${ROLES_OK:-false}" = true ] || continue
    if ! jq -r -n --argjson rights '[]' --argjson named_rights "$rights" --argjson live true -L "$LIB" -f "$LIB/code-rbac.jq" \
      "$WORK/bindings.json" "$WORK/roles.json" >"$WORK/become-named.rec" 2>"$WORK/become-named.err"; then
      unknown "$step" "the live roles could not be read for who can become a found ServiceAccount by a right limited by name: $(first_line "$WORK/become-named.err")"
      continue
    fi
    while IFS="$US" read -r -u 6 what id detail kind ns name; do
      [ "$what" = grant ] || continue
      k8s_subject "$kind" "$ns" "$name"
      ! seed_named "$kind" "$ns" "$name" "$detail" || continue
      [ "$SUBJECT_OK" != true ] || continue
      targets=""
      for why in "${!targets_why[@]}"; do
        [[ "$detail" != *", $why" ]] || { targets="${targets_why[$why]}"; break; }
      done
      [ "$targets" != "${SUBJECT#system:serviceaccount:}" ] || continue
      finding "$SUBJECT" "$detail$suffix" "$detail" "$resource" "$detail" "RBAC: $id limits it by name"
      if [[ "$SUBJECT" =~ ^system:serviceaccount:([^:]+):([^:]+)$ ]] && [ -z "${FOUND_SAS[${BASH_REMATCH[1]}:${BASH_REMATCH[2]}]:-}" ]; then
        FOUND_SAS["${BASH_REMATCH[1]}:${BASH_REMATCH[2]}"]="$detail"
      fi
    done 6<"$WORK/become-named.rec"
  done
  for sa in ${FOUND_SAS[@]+"${!FOUND_SAS[@]}"}; do
    [ -n "${followed[$sa]:-}" ] ||
      unknown "$step" "who can become a ServiceAccount with a finding is followed 5 steps; system:serviceaccount:$sa, found in the fifth, is not followed further"
  done
}
# remember_cluster KUBECONFIG STEP SUFFIX RESOURCE: keeps what follow_becomes needs of the cluster just
# read (its principals, its discovery's custom resources, and its bindings and roles with whether
# live_named read them), for follow_members, which runs after the IAM steps find the Workload
# Identity members of an RBAC finding's Google account.
REMEMBERED=()
remember_cluster() {
  local n="${#REMEMBERED[@]}" file
  cp "$WORK/principals.rec" "$WORK/remembered-$n.principals"
  for file in api-resources bindings.json roles.json; do
    if [ -e "$WORK/$file" ]; then cp "$WORK/$file" "$WORK/remembered-$n.$file"; else : >"$WORK/remembered-$n.$file"; fi
  done
  REMEMBERED+=("$1$US$2$US$3$US$4$US${ROLES_OK:-false}")
}
# follow_members MEMBERS: on every cluster remember_cluster kept, who can become a ServiceAccount of
# MEMBERS (a JSON object of namespace:name to the Google account it acts as through Workload
# Identity) holds what that account holds, so it is followed as follow_becomes follows a found one.
follow_members() {
  local n kube step suffix resource member file
  for ((n = 0; n < ${#REMEMBERED[@]}; n++)); do
    IFS="$US" read -r kube step suffix resource ROLES_OK <<<"${REMEMBERED[$n]}"
    cp "$WORK/remembered-$n.principals" "$WORK/principals.rec"
    for file in api-resources bindings.json roles.json; do cp "$WORK/remembered-$n.$file" "$WORK/$file"; done
    FOUND_SAS=()
    while IFS=$'\t' read -r member account; do
      FOUND_SAS[$member]="acts as $account through Workload Identity"
    done < <(jq -r 'to_entries[] | [.key, .value] | @tsv' <<<"$1")
    follow_becomes "$kube" "$step" "$suffix" "$resource"
  done
}
# custom_rights KUBECONFIG STYLE: sets CUSTOM_RIGHTS, the JSON array, as rights_json writes rights,
# of create, update and patch on each custom resource the API server's discovery lists as namespaced
# and creatable, in each guarded pair's namespace: a custom resource's controller may make a pod
# from it (an agent sandbox does), so create on one there is the same path as create pods, and a
# patch that finds no object creates it while an update changes what its controller makes. Custom
# is any API group outside Kubernetes' own: not the core group, no dotless group (apps, batch), and
# none of Kubernetes' built-in *.k8s.io groups (custom.jq names them); any other *.k8s.io group a CRD
# uses (snapshot.storage.k8s.io, *.x-k8s.io) is custom. STYLE `guard` labels each right as
# GUARD_RIGHTS does. Returns 1 with CUSTOM_ERROR when the discovery could not be read whole;
# CUSTOM_RIGHTS holds what it listed.
custom_rights() {
  CUSTOM_ERROR=""
  if ! kubectl --kubeconfig "$1" api-resources --namespaced=true --verbs=create -o name >"$WORK/api-resources" 2>"$WORK/api-resources.err"; then
    CUSTOM_ERROR="the API server's discovery of namespaced resources could not be read whole, so create, update and patch of a custom resource it leaves out are not reviewed: $(first_line "$WORK/api-resources.err")"
  fi
  CUSTOM_RIGHTS="$(printf '%s\n' "${GUARDED[@]}" | jq -R -s -c -L "$LIB" --rawfile names "$WORK/api-resources" --arg style "$2" -f "$LIB/custom-rights.jq")"
  [ -z "$CUSTOM_ERROR" ]
}
# admission_rights KUBECONFIG: sets ADMISSION_RIGHTS, the JSON array, as rights_json writes rights,
# of create, update and patch, cluster-wide, of each resource of admissionregistration.k8s.io the
# API server's discovery lists, and of ADMISSION_ALWAYS whatever it lists (rights.sh says why).
# Returns 1 with ADMISSION_ERROR when the discovery could not be read, or lists either of
# ADMISSION_ALWAYS short.
admission_rights() {
  local missing="" resource
  ADMISSION_ERROR=""
  if ! kubectl --kubeconfig "$1" api-resources --api-group=admissionregistration.k8s.io -o name >"$WORK/admission-resources" 2>"$WORK/admission-resources.err"; then
    ADMISSION_ERROR="the API server's discovery of admissionregistration.k8s.io could not be read, so a webhook or admission policy it serves beside the two webhook configurations is not reviewed: $(first_line "$WORK/admission-resources.err")"
  else
    for resource in "${ADMISSION_ALWAYS[@]}"; do
      grep -qxF -- "$resource.admissionregistration.k8s.io" "$WORK/admission-resources" || missing="${missing:+$missing, }$resource.admissionregistration.k8s.io"
    done
    [ -z "$missing" ] ||
      ADMISSION_ERROR="the API server's discovery of admissionregistration.k8s.io lists no $missing, which every API server serves, so its list is not whole"
  fi
  ADMISSION_RIGHTS="$({ cat "$WORK/admission-resources"; printf '%s.admissionregistration.k8s.io\n' "${ADMISSION_ALWAYS[@]}"; } | jq -R -s -c '
    [split("\n")[] | select(endswith(".admissionregistration.k8s.io")) | rtrimstr(".admissionregistration.k8s.io")] | unique
    | [.[] as $res | ("create", "update", "patch")
       | {verb: ., group: "admissionregistration.k8s.io", resource: $res, sub: "", ns: "", name: "",
          label: "\(.) \($res).admissionregistration.k8s.io cluster-wide"}
       | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}]')"
  [ -z "$ADMISSION_ERROR" ]
}
# live_named KUBECONFIG STEP SUFFIX RESOURCE RIGHTS: the rights a live role limits by name. RBAC
# applies a rule with resourceNames only to a request that names one of them, so a review that names
# nothing is refused for an update of one webhook configuration, a patch of one Deployment or a
# pods/exec of one pod by name. The cluster's Roles and ClusterRoles (`kubectl get
# roles,clusterroles -A`) are read against $WORK/bindings.json as the code step reads a render
# (code-rbac.jq, by name alone): each rule whose resourceNames limit a right of RIGHTS (the JSON
# array the cluster's reviews asked) is a finding for a binding subject k8s_subject does not allow,
# its right followed by SUFFIX, on RESOURCE; a ServiceAccount so found is followed as judge_reviews'
# are (FOUND_SAS). A part of the certificate chain so limited (an approval by name, whose name the
# requester picks) goes to csr_part, so it joins the signers judge_reviews' reviews allowed; the
# caller runs csr_chains after this. Runs after judge_reviews; a refused list is UNKNOWN naming the
# permission.
live_named() {
  local what id detail kind ns name
  # ROLES_OK: whether $WORK/roles.json holds this cluster's roles, for follow_becomes's named read.
  ROLES_OK=false
  [ "$BINDINGS_OK" = true ] || return 0
  if ! kubectl --kubeconfig "$1" get roles,clusterroles --all-namespaces -o json >"$WORK/roles.json" 2>"$WORK/roles.err"; then
    unknown "$2" "the API server refused the roles' list (it needs list on roles and clusterroles, container.roles.list and container.clusterRoles.list on GKE), so a right a role limits by name is not read: $(first_line "$WORK/roles.err")"
    return 0
  fi
  if ! jq -r -n --argjson rights '[]' --argjson named_rights "$5" --argjson live true -L "$LIB" -f "$LIB/code-rbac.jq" \
    "$WORK/bindings.json" "$WORK/roles.json" >"$WORK/named.rec" 2>"$WORK/named.err"; then
    unknown "$2" "the API server's list of roles is not JSON: $(first_line "$WORK/named.err")"
    return 0
  fi
  ROLES_OK=true
  while IFS="$US" read -r -u 6 what id detail kind ns name; do
    [ "$what" = grant ] || continue
    k8s_subject "$kind" "$ns" "$name"
    ! seed_named "$kind" "$ns" "$name" "$detail" || continue
    [ "$SUBJECT_OK" != true ] || continue
    ! csr_part "$SUBJECT" "$detail" "RBAC: $id limits it by name" || continue
    finding "$SUBJECT" "$detail$3" "$detail" "$4" "$detail" "RBAC: $id limits it by name"
    if [[ "$SUBJECT" =~ ^system:serviceaccount:([^:]+):([^:]+)$ ]] && [ -z "${FOUND_SAS[${BASH_REMATCH[1]}:${BASH_REMATCH[2]}]:-}" ]; then
      FOUND_SAS["${BASH_REMATCH[1]}:${BASH_REMATCH[2]}"]="$detail"
    fi
  done 6<"$WORK/named.rec"
}
# bound_subjects KUBECONFIG: the cluster's binding subjects (the header's "Binding subjects"), added
# to $WORK/principals.rec when the file does not hold them already: each User subject k8s_subject
# does not allow, with system:authenticated (system:anonymous with system:unauthenticated), each
# Group subject that is one of Kubernetes' own (own_group), alone, and each ServiceAccount subject,
# whether it exists or not, with its groups (a system namespace's as a seed). A person's User
# subject, a Group subject that is not Kubernetes' own (groups_verdict judges those) and
# system:masters (masters_bindings) are left out. Sets BOUND to how many it added, and BINDINGS_OK to
# whether $WORK/bindings.json holds this cluster's bindings; returns 1 with BOUND_ERROR when the API
# server refused the list or answered no JSON, or when the list lacks the bootstrap cluster-admin
# ClusterRoleBinding, its witness: every cluster has it, so a list without it is not whole (its
# subjects are still added).
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
  if ! jq -e 'any(.items[]?; .kind == "ClusterRoleBinding" and .metadata.name == "cluster-admin" and .roleRef.kind == "ClusterRole"
      and .roleRef.name == "cluster-admin" and (.metadata.labels // {})["kubernetes.io/bootstrapping"] == "rbac-defaults")' \
    "$WORK/bindings.json" >/dev/null; then
    BOUND_ERROR="the bindings' list holds no bootstrap cluster-admin ClusterRoleBinding, which every cluster has, so it is not the cluster's whole list"
    return 1
  fi
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
# authenticator: UNKNOWN when a Group is bound, and when a User whose name looks like an email is,
# since that authenticator, not Google, says who holds the name. MODE `unknown:<why>`: UNKNOWN when
# a Group is bound.
groups_verdict() {
  local step="$1" mode="$2" groups="" users=""
  if [ "$BINDINGS_OK" = true ]; then
    groups="$(jq -r '[.items[]?.subjects[]? | select(.kind == "Group" and ((.name | startswith("system:")) | not)) | .name] | unique | join(", ")' \
      "$WORK/bindings.json")"
    users="$(jq -r '[.items[]?.subjects[]? | select(.kind == "User" and (.name | test("^[^@/:\\s]+@[^@/:\\s]+\\.[^@/:\\s]+$"))
      and ((.name | endswith(".gserviceaccount.com")) | not)) | .name] | unique | join(", ")' "$WORK/bindings.json")"
  fi
  case "$mode" in
    off) ;;
    on:*)
      unknown "$step" "Google Groups for RBAC is on (security group ${mode#on:}), so a Group subject gives its Google group's members its rights, which this check neither lists nor judges (Group subjects bound now: ${groups:-none})"
      ;;
    own)
      [ -z "$groups" ] ||
        unknown "$step" "it takes its users and groups from its own authenticator, not Google, so the Group subjects bound here ($groups) name members this check cannot list"
      [ -z "$users" ] ||
        unknown "$step" "it takes its users from its own authenticator, not Google, so the User subjects bound here that look like emails ($users) are not known to be people"
      ;;
    unknown:*)
      [ -z "$groups" ] ||
        unknown "$step" "whether Google Groups for RBAC is on is unknown (${mode#unknown:}), so the Group subjects bound here ($groups) cannot be judged"
      ;;
  esac
}
# settings_verdict STEP SETTINGS: a GKE cluster's settings that authorize or authenticate outside
# RBAC and Google (CLUSTER_SETTINGS), each UNKNOWN: legacy ABAC authorizes requests by its own
# policies beside RBAC, and the identity service for GKE takes users and groups from an outside
# provider, so no review sees either. `unread:<why>` is UNKNOWN for both.
settings_verdict() {
  case "$2" in
    unread:*)
      unknown "$1" "whether legacy ABAC or the identity service for GKE is on is unknown (${2#unread:}), so who the cluster authorizes outside RBAC cannot be read"
      return
      ;;
  esac
  case ",$2," in *,abac,*)
    unknown "$1" "legacy ABAC is on, so the cluster authorizes requests by its ABAC policies beside RBAC, which no review here sees" ;;
  esac
  case ",$2," in *,identity,*)
    unknown "$1" "the identity service for GKE is on, so the cluster takes users and groups from an outside identity provider, whose members this check cannot list" ;;
  esac
}
# named_rights [PROJECT]: sets NAMED_RIGHTS, the name-limited rights (named-rights.jq) of the cluster
# whose bindings bound_subjects read last, for the principals of $WORK/principals.rec: impersonate
# groups and users by name, the users being the bindings' User subjects and each user or Google
# service account the GKE IAM step listed as a holder, and, for a cluster in PROJECT, a project other
# than --project, each one its member project's IAM step listed (GKE's IAM authorizer judges an
# impersonated user by its own IAM there); bind and escalate on every binding's role, and patch and
# update on every binding, by name; and update and patch by name on each role bound to a reviewed
# principal. When the bindings could not be read, it holds impersonate groups system:masters and
# the IAM holders alone.
named_rights() {
  local reviewed bindings='{"items": []}' iam_users='[]' file files=()
  reviewed="$(cut -d"$US" -f1 "$WORK/principals.rec" | jq -R -s -c 'split("\n") | map(select(length > 0))')"
  [ "$BINDINGS_OK" != true ] || bindings="$(<"$WORK/bindings.json")"
  for file in "$WORK/gke-analysis.rec" ${1:+"$WORK/gke-analysis-$1.rec"}; do
    [ ! -s "$file" ] || files+=("$file")
  done
  if [ "${#files[@]}" -gt 0 ]; then
    iam_users="$(awk -F"$US" '$1 == "row" || $1 == "chain" { print $2 }' "${files[@]}" |
      sed -n 's/^\(user\|serviceAccount\):\([^[]*\)$/\2/p' | sort -u | jq -R -s -c 'split("\n") | map(select(length > 0))')"
  fi
  NAMED_RIGHTS="$(jq -c --argjson reviewed "$reviewed" --argjson iam_users "$iam_users" -f "$LIB/named-rights.jq" <<<"$bindings")"
}
# with_rights RIGHTS MORE: RIGHTS and MORE, two JSON arrays of rights, each right once by its key;
# where both hold one, RIGHTS's label stays.
with_rights() { jq -c --argjson more "$2" '. + $more | reduce .[] as $r ([]; if any(.[]; .key == $r.key) then . else . + [$r] end)' <<<"$1"; }
