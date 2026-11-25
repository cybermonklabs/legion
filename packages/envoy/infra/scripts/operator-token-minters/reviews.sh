# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/reviews.sh: SubjectAccessReviews, how their answers are judged, who can
# become a ServiceAccount with a finding, and the principals and rights a cluster adds. Sourced by
# operator-token-minters.sh, whose header says what each step checks.
# REVIEW_WORKERS: the most kubectl calls review runs at once on one cluster, each with its own share
# of the principals; 10 unless a run sets it. The calls on a cluster are REVIEW_WORKERS or the
# nominal seats that cluster's API server gives the priority level global-default, whichever is
# fewer (review_workers). kubectl POSTs one review at a time, so a call holds one seat at a time.
# Measured on a mock API server over loopback, so kubectl's own cost (a live API server adds its
# latency to each POST): 2,000 reviews took 86.4 s in one call (23.2 a second), 8.9 s in ten (225
# a second) and 4.6 s in twenty (433 a second). The answer's order does not depend on the number
# of calls, since the chunks are read back in the order they were asked.
REVIEW_WORKERS="${REVIEW_WORKERS:-10}"
# review_workers KUBECONFIG: sets WORKERS, how many calls review makes at once on that cluster:
# REVIEW_WORKERS, capped at the nominal seats of the priority level global-default, which serves a
# user no earlier FlowSchema matches, as the one running this check is. The seats are the API
# server's apiserver_flowcontrol_nominal_limit_seats in its /metrics, from the replica that
# answers. More calls than seats would only wait behind each other at the API server, and take
# seats from global-default's other users. A /metrics that cannot be read, or that holds no
# whole number of seats for global-default, gives one call, which loads the API server least: the
# review is slower, never wrong. Returns 1 with REVIEW_ERROR when REVIEW_WORKERS is not a whole
# number above 0.
review_workers() {
  local seats
  WORKERS=1
  [[ "$REVIEW_WORKERS" =~ ^[1-9][0-9]{0,3}$ ]] || {
    REVIEW_ERROR="REVIEW_WORKERS is not a whole number from 1 to 9999, so the reviews were not asked"
    return 1
  }
  seats="$(kubectl --kubeconfig "$1" get --raw /metrics 2>/dev/null |
    awk '$1 == "apiserver_flowcontrol_nominal_limit_seats{priority_level=\"global-default\"}" { print $2; exit }')" || true
  [[ "$seats" =~ ^[1-9][0-9]{0,5}$ ]] || return 0
  WORKERS=$((seats < REVIEW_WORKERS ? seats : REVIEW_WORKERS))
}
# review KUBECONFIG PRINCIPALS RIGHTS [FENCE]: one SubjectAccessReview for each principal of the
# JSON array PRINCIPALS ({principal, user, groups}) and each right of the JSON array RIGHTS. The
# principals are split into WORKERS chunks (review_workers: REVIEW_WORKERS, capped at the cluster's
# seats), each asked by its own kubectl call as one List, so each call POSTs its share one at a time
# and the calls run at once; the first chunk always holds the witness, whose reviews every
# judgement ends on. kubectl create answers with one JSON document per review it made, never a List
# (review-answer.jq), read with -s. A review's answer is matched to its question by the subject and
# the attributes the API server echoes in its spec. Writes a record of principal, right label,
# allowed|refused|error, the evaluation error, the API server's reason and the fence's verdict on
# the right (review-answer.jq) per review to $WORK/review.rec, in the chunks' order; FENCE is the
# broker's pod fence credited on this cluster (broker-fence.sh's FENCE_CREDITED), none on any other.
# Returns 1 with REVIEW_ERROR when the API server refused any call, when a document of an answer is
# not a SubjectAccessReview with a status, or when the answers do not hold each review asked once
# (an empty body sent with exit 0 holds none). PRINCIPALS and RIGHTS reach jq through files
# (--slurpfile), never its command line: a cluster's principals and rights are more than one
# argument may hold. Each must be one JSON array (one_array), so an empty argument fails the
# reviews' build.
review() {
  local chunks chunk refused="" pids=()
  REVIEW_ERROR=""
  printf '%s' "$2" >"$WORK/review-principals.json"
  printf '%s' "$3" >"$WORK/review-rights.json"
  review_workers "$1" || return 1
  # The chunks: at most WORKERS, each a share of the principals, the witness's in the first.
  if ! chunks="$(jq -r --argjson workers "$WORKERS" '
      (if length < $workers then length else $workers end) as $n
      | if $n < 1 then 1 else $n end' "$WORK/review-principals.json" 2>"$WORK/review.err")"; then
    REVIEW_ERROR="this script could not count the principals: $(first_line "$WORK/review.err")"
    return 1
  fi
  rm -f "$WORK"/review-in-*.json "$WORK"/review-out-*.json "$WORK"/review-err-*
  for ((chunk = 0; chunk < chunks; chunk++)); do
    if ! jq -n --slurpfile principals "$WORK/review-principals.json" --slurpfile rights "$WORK/review-rights.json" \
      --argjson chunk "$chunk" --argjson chunks "$chunks" -L "$LIB" \
      -f "$LIB/review-request.jq" >"$WORK/review-in-$chunk.json" 2>"$WORK/review.err"; then
      REVIEW_ERROR="this script could not build the reviews: $(first_line "$WORK/review.err")"
      return 1
    fi
  done
  for ((chunk = 0; chunk < chunks; chunk++)); do
    kubectl --kubeconfig "$1" create -f - -o json <"$WORK/review-in-$chunk.json" \
      >"$WORK/review-out-$chunk.json" 2>"$WORK/review-err-$chunk" &
    pids[chunk]=$!
  done
  # Each call is waited for by its own pid, so no other background job of the shell is; every
  # call is waited for, and the first the API server refused, in the order asked, names the
  # refusal.
  for ((chunk = 0; chunk < chunks; chunk++)); do
    wait "${pids[chunk]}" || [ -n "$refused" ] || refused=$chunk
  done
  if [ -n "$refused" ]; then
    REVIEW_ERROR="the API server refused the review (it needs create on subjectaccessreviews, container.subjectAccessReviews.create on GKE): $(first_line "$WORK/review-err-$refused")"
    return 1
  fi
  # The chunks are read in the order they were asked, so the records' order does not depend on which
  # worker answered first.
  : >"$WORK/review.json"
  for ((chunk = 0; chunk < chunks; chunk++)); do cat "$WORK/review-out-$chunk.json" >>"$WORK/review.json"; done
  if ! jq -r -s --slurpfile principals "$WORK/review-principals.json" --slurpfile rights "$WORK/review-rights.json" \
    --argjson fence "${4:-null}" -L "$LIB" -f "$LIB/review-answer.jq" "$WORK/review.json" >"$WORK/review.rec" 2>"$WORK/review.err"; then
    REVIEW_ERROR="the API server's answer to the reviews could not be read: $(first_line "$WORK/review.err")"
    return 1
  fi
  local what answered asked documents
  IFS="$US" read -r what answered asked documents <"$WORK/review.rec" || true
  if [ "$what" = short ]; then
    REVIEW_ERROR="the API server answered $answered of the $asked reviews asked, in $documents documents"
    return 1
  fi
}
# witness_held: whether the API server allowed every one of the witness's reviews in
# $WORK/review.rec (WITNESS_RECORD, the first principal of every review); returns 1 with
# WITNESS_ERROR. review's answer holds each principal and right asked once, the witness's among them,
# so none is missing here. An API server that does not say yes to the witness proves nothing by
# saying no to anyone else, so judge_reviews and each follow_becomes round end on it.
witness_held() {
  WITNESS_ERROR=""
  [ "$(awk -F"$US" '$1 == "witness" { print $3 }' "$WORK/review.rec" | sort -u)" = allowed ] && return 0
  WITNESS_ERROR="the witness review, the cluster-admin binding's group system:masters, was not allowed, so a refusal from this API server proves nothing"
  return 1
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
# the right follow_becomes names it by (found_sa); and each seed, a system namespace's
# ServiceAccount (its principal labelled seed:<its name>) allowed a right asked: no finding itself,
# but who can become it holds what it holds.
declare -A FOUND_SAS=()
# keep_least NAME KEY VALUE: sets the associative array NAME's KEY to VALUE unless it already holds
# KEY with a value that sorts first in the C locale, an empty one included. The reviews' answers come
# in the API server's order, so keeping the first one read would make the text built from it hang on
# that order.
keep_least() {
  local -n least_of="$1"
  local LC_ALL=C
  [ -n "${least_of[$2]+set}" ] && [[ ! "$3" < "${least_of[$2]}" ]] || least_of[$2]="$3"
}
# sa_key PRINCIPAL: sets SA_KEY to PRINCIPAL's <namespace>:<name> when it is a ServiceAccount
# (system:serviceaccount:<ns>:<name>, or a seed, seed: before it); any other principal is no
# ServiceAccount (SA_KEY "", returns 1).
sa_key() {
  SA_KEY=""
  [[ "${1#seed:}" =~ ^system:serviceaccount:([^:]+):([^:]+)$ ]] || return 1
  SA_KEY="${BASH_REMATCH[1]}:${BASH_REMATCH[2]}"
}
# RESIDUAL_SAS: each ServiceAccount (<namespace>:<name>, a seed's too) on the cluster read last whose
# grant reaches the broker's pod fence's residual and no more: a credited grant that carries it
# (judge_grant), or a chain to such an account (judge_chain); and the right follow_becomes names it
# by. Who can become it reaches the residual too, so follow_becomes follows it and names each such
# chain by an ALLOWANCE (broker-fence.sh's residual_allowance), never silence.
declare -A RESIDUAL_SAS=()
# keep_sa KIND PRINCIPAL LABEL: when PRINCIPAL is a ServiceAccount (sa_key), records LABEL as one of
# its rights in FOUND_SAS (KIND found) or RESIDUAL_SAS (KIND residual), keeping the one whose label
# sorts first (keep_least). The two sets never share an account, and a found one wins, here alone:
# a found account leaves RESIDUAL_SAS, and one in FOUND_SAS is never kept for the residual. Any
# other principal is left.
keep_sa() {
  sa_key "$2" || return 0
  if [ "$1" = found ]; then
    unset "RESIDUAL_SAS[$SA_KEY]"
    keep_least FOUND_SAS "$SA_KEY" "$3"
  elif [ -z "${FOUND_SAS[$SA_KEY]:-}" ]; then
    keep_least RESIDUAL_SAS "$SA_KEY" "$3"
  fi
}
found_sa() { keep_sa found "$1" "$2"; }
residual_sa() { keep_sa residual "$1" "$2"; }
# The certificate signing chain's parts, by principal, which csr_part gathers from a cluster's
# reviews, its named rights and a render, and csr_chains judges once they are all read. Each list of
# parts is a line a part, which csr_chains sorts, and CSR_WHY_OF keeps the reason that sorts first
# (keep_least), so a chain's text does not hang on the order the parts were read in.
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
      CSR_APPROVAL_OF[$1]="${CSR_APPROVAL_OF[$1]:+${CSR_APPROVAL_OF[$1]}$'\n'}${label%% *} certificatesigningrequests/approval$names"
      keep_least CSR_WHY_OF "$1" "$3"
      ;;
    "$CSR_ANY_SIGNER") CSR_SIGNERS_OF[$1]="of any name" ;;
    "$CSR_SIGNER_PREFIX"*)
      names="${label#"$CSR_SIGNER_PREFIX"}" names="${names% cluster-wide}"
      [ "${CSR_SIGNERS_OF[$1]:-}" = "of any name" ] || CSR_SIGNERS_OF[$1]="${CSR_SIGNERS_OF[$1]:+${CSR_SIGNERS_OF[$1]}$'\n'}$names"
      ;;
    *) return 1 ;;
  esac
}
# sorted_join SEPARATOR LINES: LINES, a line each, sorted in the C locale, each once, and joined by
# SEPARATOR. A rule naming a signer answers the review of that name and the unnamed right's named
# read alike, and two reads can give one approval, so each part is joined once.
sorted_join() {
  local joined="" line
  while IFS= read -r line; do joined="${joined:+$joined$1}$line"; done < <(printf '%s\n' "$2" | LC_ALL=C sort -u)
  printf '%s' "$joined"
}
# csr_chains SUFFIX RESOURCE: one finding on RESOURCE, its right followed by SUFFIX, for each
# principal csr_part saw allowed both an approval write and a signer, naming create too when it may
# make its own request; a seed's chain only seeds FOUND_SAS. Then forgets the parts.
csr_chains() {
  local principal chain
  for principal in $(printf '%s\n' ${CSR_APPROVAL_OF[@]+"${!CSR_APPROVAL_OF[@]}"} | sort); do
    [ -n "${CSR_SIGNERS_OF[$principal]:-}" ] || continue
    chain="${CSR_CREATE_OF[$principal]:+create certificatesigningrequests, }$(sorted_join ', ' "${CSR_APPROVAL_OF[$principal]}") and approve signers $(sorted_join ' and ' "${CSR_SIGNERS_OF[$principal]}") cluster-wide, so it can have the cluster sign it a client certificate for a user, group or node it names"
    found_sa "$principal" "$chain"
    [[ "$principal" == seed:* ]] || finding "$principal" "$chain$1" "$chain" "$2" "$chain" "${CSR_WHY_OF[$principal]:-}"
  done
  csr_reset
}
# judge_grant PRINCIPAL LABEL SUFFIX RESOURCE REASON POLICY RESIDUAL: the verdict on one grant a
# review allowed, or a live role allows by name. POLICY is the broker's pod fence's policy that
# refuses the right's use on the broker's pods (fence_fields, broker-fence.jq), or "": the grant is
# a credit (broker-fence.sh's credit), which finds no ServiceAccount; RESIDUAL `residual` says its
# use on the fence's namespace's other pods carries the fence's residual, which credit names by an
# ALLOWANCE and which keeps a ServiceAccount it is in RESIDUAL_SAS, so who can become it is named
# too. Any other grant is a finding on RESOURCE, its right followed by SUFFIX, granted where REASON
# says, and records a ServiceAccount it is (found_sa). A seed (seed: before its name) is never a
# credit, an allowance or a finding: it only seeds FOUND_SAS or RESIDUAL_SAS.
judge_grant() {
  if [ -n "$6" ]; then
    [ "$7" != residual ] || residual_sa "$1" "$2"
    [[ "$1" == seed:* ]] || credit "$1" "$2$3" "$4" "$5" "$6" "$7"
    return 0
  fi
  found_sa "$1" "$2"
  [[ "$1" == seed:* ]] || finding "$1" "$2$3" "$2" "$4" "$2" "$5"
}
# chain_residual POLICY TARGETS FOLLOWED: whether a grant follow_becomes asks, by which its holder
# becomes TARGETS (namespace:name, space-separated), reaches the fence's residual and no more: the
# fence refuses no use of it (POLICY is "") and each target, of one at least, is followed for the
# residual alone (FOLLOWED, follow_becomes's associative array of each account by its kind). A
# target's right comes from the round's list, which sets its kind in FOLLOWED, so the verdict and
# the label the line prints read one state.
chain_residual() {
  local target
  local -n followed_as="$3"
  [ -z "$1" ] && [ -n "$2" ] || return 1
  for target in $2; do
    [ "${followed_as[$target]:-}" = residual ] || return 1
  done
}
# judge_chain PRINCIPAL LABEL SUFFIX RESOURCE REASON POLICY RESIDUAL TARGETS FOLLOWED: the verdict
# on a grant follow_becomes asks. One that reaches the residual alone (chain_residual) is an
# ALLOWANCE naming the chain and the residual (residual_allowance, broker-fence.sh), and keeps its
# holder, a ServiceAccount, in RESIDUAL_SAS, to be followed in turn; a seed only seeds it. Any other
# is judged as judge_grant judges a grant.
judge_chain() {
  if chain_residual "$6" "$8" "$9"; then
    residual_sa "$1" "$2"
    [[ "$1" == seed:* ]] || residual_allowance "$1" "$2$3" "$4" "$5"
  else
    judge_grant "$1" "$2" "$3" "$4" "$5" "$6" "$7"
  fi
}
# judge_reviews STEP SUFFIX RESOURCE: each review of $WORK/review.rec. Every allowed review but the
# witness's is judged (judge_grant), except the certificate signing chain's parts, which csr_part
# gathers for csr_chains (the caller runs it after live_named, whose named approvals join the
# chain). Returns 1 with an UNKNOWN when the witness's reviews are missing or one was refused
# (witness_held).
judge_reviews() {
  local principal label verdict error reason policy residual
  FOUND_SAS=() RESIDUAL_SAS=()
  csr_reset
  while IFS="$US" read -r -u 4 principal label verdict error reason policy residual; do
    [ "$principal" != witness ] || continue
    case "$verdict" in
      allowed)
        ! csr_part "$principal" "$label" "$reason" || continue
        judge_grant "$principal" "$label" "$2" "$3" "$reason" "$policy" "$residual"
        ;;
      error) unknown "$1" "the review of $principal for $label ended in an evaluation error, so its refusal proves nothing: $error" ;;
    esac
  done 4<"$WORK/review.rec"
  witness_held || { unknown "$1" "$WITNESS_ERROR"; return 1; }
}
# named_principal KIND NAMESPACE NAME: sets NAMED_PRINCIPAL to the principal a binding subject a live
# role names is judged as. A system namespace's ServiceAccount, as a ServiceAccount subject or as
# the User system:serviceaccount:<namespace>:<name> (k8s_subject allows both), is the seed
# seed:system:serviceaccount:<namespace>:<name>: never a finding itself, but who can become it holds
# what the role allows it, so judge_grant and judge_chain seed FOUND_SAS or RESIDUAL_SAS from it, as
# judge_reviews seeds one allowed a right outright. A subject k8s_subject does not allow is itself;
# any other (a person, a component, the operator's own account) is "", which is not judged.
named_principal() {
  local ns="$2" name="$3"
  NAMED_PRINCIPAL=""
  case "$1:$name" in
    ServiceAccount:*) ;;
    User:system:serviceaccount:*:*)
      name="${name#system:serviceaccount:}"
      ns="${name%%:*}" name="${name#*:}"
      ;;
    *) ns="" ;;
  esac
  if [ -n "$ns" ] && system_namespace "$ns"; then
    NAMED_PRINCIPAL="seed:system:serviceaccount:$ns:$name"
    return 0
  fi
  k8s_subject "$1" "$2" "$3"
  [ "$SUBJECT_OK" = true ] || NAMED_PRINCIPAL="$SUBJECT"
}
# follow_becomes KUBECONFIG STEP SUFFIX RESOURCE [FENCE]: who can become a ServiceAccount of
# FOUND_SAS holds what it holds, and who can become one of RESIDUAL_SAS reaches the fence's residual
# as it does. In the ServiceAccount's namespace a principal becomes it by any namespaced right of
# RIGHTS_TABLE (a pod or a workload controller runs as any ServiceAccount of its namespace; exec,
# attach, an ephemeral container or a pod patch enters one; a rolebinding, bind or escalate grants
# itself those; a token Secret is read) or by create, update or patch of a custom resource there,
# and, by its name, by create serviceaccounts/token or impersonate (become-rights.jq). Each
# principal of $WORK/principals.rec allowed one of these is judged (judge_chain: a finding, or an
# ALLOWANCE when each account it becomes reaches the residual alone), and so is a binding subject
# whose live role limits one of them by name (a patch of one Deployment sets its
# serviceAccountName), read as live_named reads the others when it read the cluster's roles; FENCE
# is the cluster's credited fence, as review takes it. Each ServiceAccount so found is followed in
# turn, up to 5 steps, and one followed for the residual alone is followed again once it has a
# finding; one found in the fifth and not followed is UNKNOWN. A ServiceAccount that becomes only
# itself is no finding. A chain's verdict reads `followed` (chain_residual). Each round's reviews
# must hold the witness's, all allowed (witness_held), as judge_reviews' must: a round whose API
# server cannot say yes proves nothing by saying no.
follow_becomes() {
  local kube="$1" step="$2" suffix="$3" resource="$4" fence="${5:-}" round sa rights label targets principal verdict error reason who why
  local what id detail kind ns name policy residual
  local -A followed=() targets_of=() targets_why=()
  for ((round = 0; round < 5; round++)); do
    : >"$WORK/become.tsv"
    while IFS= read -r sa; do
      [ "${followed[$sa]:-}" != found ] || continue
      followed[$sa]=found
      printf '%s\t%s\t%s\n' "${sa%%:*}" "${sa#*:}" "${FOUND_SAS[$sa]}" >>"$WORK/become.tsv"
    done < <(printf '%s\n' ${FOUND_SAS[@]+"${!FOUND_SAS[@]}"} | sed '/^$/d' | sort)
    # keep_sa keeps a found account out of RESIDUAL_SAS, so each account here reaches the residual alone.
    while IFS= read -r sa; do
      [ -z "${followed[$sa]:-}" ] || continue
      followed[$sa]=residual
      printf '%s\t%s\t%s\n' "${sa%%:*}" "${sa#*:}" "${RESIDUAL_SAS[$sa]}" >>"$WORK/become.tsv"
    done < <(printf '%s\n' ${RESIDUAL_SAS[@]+"${!RESIDUAL_SAS[@]}"} | sed '/^$/d' | sort)
    [ -s "$WORK/become.tsv" ] || return 0
    rights="$(jq -R -s -c -L "$LIB" --argjson table "$TABLE_JSON" --rawfile names "$WORK/api-resources" -f "$LIB/become-rights.jq" "$WORK/become.tsv")"
    targets_of=() targets_why=()
    while IFS=$'\t' read -r label targets; do targets_of[$label]="$targets"; done < <(jq -r '.[] | [.label, (.targets | join(" "))] | @tsv' <<<"$rights")
    while IFS=$'\t' read -r why targets; do targets_why[$why]="$targets"; done < <(jq -r '.[] | [.why, (.targets | join(" "))] | @tsv' <<<"$rights")
    if ! review "$kube" "$(principals_json "$WORK/principals.rec")" "$rights" "$fence"; then
      unknown "$step" "$REVIEW_ERROR"
      return 0
    fi
    while IFS="$US" read -r -u 4 principal label verdict error reason policy residual; do
      [ "$principal" != witness ] || continue
      case "$verdict" in
        allowed)
          who="${principal#seed:}"
          who="${who#system:serviceaccount:}"
          [ "${targets_of[$label]:-}" != "$who" ] || continue
          judge_chain "$principal" "$label" "$suffix" "$resource" "$reason" "$policy" "$residual" "${targets_of[$label]:-}" followed
          ;;
        error) unknown "$step" "the review of $principal for $label ended in an evaluation error, so its refusal proves nothing: $error" ;;
      esac
    done 4<"$WORK/review.rec"
    witness_held || { unknown "$step" "$WITNESS_ERROR"; return 0; }
    # The same rights limited by name in a live role, which a review naming nothing never finds.
    [ "${ROLES_OK:-false}" = true ] || continue
    if ! named_grants "$rights" "$WORK/become-named.rec" "$fence"; then
      unknown "$step" "the live roles could not be read for who can become a found ServiceAccount by a right limited by name: $(first_line "$WORK/become-named.rec.err")"
      continue
    fi
    while IFS="$US" read -r -u 6 what id detail kind ns name policy residual; do
      [ "$what" = grant ] || continue
      named_principal "$kind" "$ns" "$name"
      [ -n "$NAMED_PRINCIPAL" ] || continue
      targets=""
      for why in "${!targets_why[@]}"; do
        [[ "$detail" != *", $why" ]] || { targets="${targets_why[$why]}"; break; }
      done
      who="${NAMED_PRINCIPAL#seed:}"
      [ "$targets" != "${who#system:serviceaccount:}" ] || continue
      judge_chain "$NAMED_PRINCIPAL" "$detail" "$suffix" "$resource" "RBAC: $id limits it by name" "$policy" "$residual" "$targets" followed
    done 6<"$WORK/become-named.rec"
  done
  for sa in ${FOUND_SAS[@]+"${!FOUND_SAS[@]}"}; do
    [ "${followed[$sa]:-}" = found ] ||
      unknown "$step" "who can become a ServiceAccount with a finding is followed 5 steps; system:serviceaccount:$sa, found in the fifth, is not followed further"
  done
  for sa in ${RESIDUAL_SAS[@]+"${!RESIDUAL_SAS[@]}"}; do
    [ -n "${followed[$sa]:-}" ] ||
      unknown "$step" "who can become a ServiceAccount that reaches the broker's pod fence's residual is followed 5 steps; system:serviceaccount:$sa, found in the fifth, is not followed further"
  done
}
# remember_cluster KUBECONFIG STEP SUFFIX RESOURCE [FENCE]: keeps what follow_becomes needs of the
# cluster just read (its principals, its discovery's custom resources, its bindings and roles with
# whether live_named read them, and its credited fence), for follow_members, which the IAM steps run
# on each pass of their fixed point (step-live-iam.sh), for the Workload Identity members of each
# Google account a finding names and of each whose credited grant reaches the residual.
REMEMBERED=()
remember_cluster() {
  local n="${#REMEMBERED[@]}" file
  cp "$WORK/principals.rec" "$WORK/remembered-$n.principals"
  for file in api-resources bindings.json roles.json; do
    if [ -e "$WORK/$file" ]; then cp "$WORK/$file" "$WORK/remembered-$n.$file"; else : >"$WORK/remembered-$n.$file"; fi
  done
  REMEMBERED+=("$1$US$2$US$3$US$4$US${ROLES_OK:-false}$US${5:-}")
}
# follow_members FOUND RESIDUAL: on every cluster remember_cluster kept, who can become a
# ServiceAccount of FOUND or of RESIDUAL (each a JSON object of namespace:name to the right its
# caller names it by, as follow_becomes writes a label after "which can": `act as <email> through
# Workload Identity`) holds what that right gives: an account FOUND names has a finding, and who can
# become it is followed as a found one; one RESIDUAL names reaches the broker's pod fence's residual
# alone, and is followed as RESIDUAL_SAS are.
follow_members() {
  local n kube step suffix resource fence member label file
  for ((n = 0; n < ${#REMEMBERED[@]}; n++)); do
    IFS="$US" read -r kube step suffix resource ROLES_OK fence <<<"${REMEMBERED[$n]}"
    cp "$WORK/remembered-$n.principals" "$WORK/principals.rec"
    for file in api-resources bindings.json roles.json; do cp "$WORK/remembered-$n.$file" "$WORK/$file"; done
    FOUND_SAS=() RESIDUAL_SAS=()
    while IFS=$'\t' read -r member label; do found_sa "system:serviceaccount:$member" "$label"; done < <(jq -r 'to_entries[] | [.key, .value] | @tsv' <<<"$1")
    while IFS=$'\t' read -r member label; do residual_sa "system:serviceaccount:$member" "$label"; done < <(jq -r 'to_entries[] | [.key, .value] | @tsv' <<<"$2")
    follow_becomes "$kube" "$step" "$suffix" "$resource" "$fence"
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
# GUARD_RIGHTS does. Returns 1 with CUSTOM_ERROR when the discovery could not be read whole, and when
# custom-rights.jq built no JSON array of rights, which sets CUSTOM_RIGHTS to none ([]) rather than
# to what it printed; CUSTOM_RIGHTS holds what it listed.
custom_rights() {
  local built
  CUSTOM_ERROR=""
  if ! kubectl --kubeconfig "$1" api-resources --namespaced=true --verbs=create -o name >"$WORK/api-resources" 2>"$WORK/api-resources.err"; then
    CUSTOM_ERROR="the API server's discovery of namespaced resources could not be read whole, so create, update and patch of a custom resource it leaves out are not reviewed: $(first_line "$WORK/api-resources.err")"
  fi
  CUSTOM_RIGHTS='[]'
  if built="$(printf '%s\n' "${GUARDED[@]}" | jq -R -s -c -L "$LIB" --rawfile names "$WORK/api-resources" --arg style "$2" \
    -f "$LIB/custom-rights.jq" 2>"$WORK/custom-rights.err")" && is_rights "$built"; then
    CUSTOM_RIGHTS="$built"
  else
    CUSTOM_ERROR="${CUSTOM_ERROR:+$CUSTOM_ERROR; and }the rights on the custom resources could not be built, so none is reviewed: $(first_line "$WORK/custom-rights.err")"
  fi
  [ -z "$CUSTOM_ERROR" ]
}
# admission_rights KUBECONFIG: sets ADMISSION_RIGHTS, the JSON array, as rights_json writes rights,
# of create, update and patch, cluster-wide, of each resource of admissionregistration.k8s.io the
# API server's discovery lists, and of ADMISSION_ALWAYS whatever it lists (rights.sh says why).
# Returns 1 with ADMISSION_ERROR when the discovery could not be read, or lists either of
# ADMISSION_ALWAYS short, and when no JSON array of rights was built, which sets ADMISSION_RIGHTS to
# none ([]) rather than to what jq printed.
admission_rights() {
  local missing="" resource built
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
  ADMISSION_RIGHTS='[]'
  if built="$({ cat "$WORK/admission-resources"; printf '%s.admissionregistration.k8s.io\n' "${ADMISSION_ALWAYS[@]}"; } | jq -R -s -c '
    [split("\n")[] | select(endswith(".admissionregistration.k8s.io")) | rtrimstr(".admissionregistration.k8s.io")] | unique
    | [.[] as $res | ("create", "update", "patch")
       | {verb: ., group: "admissionregistration.k8s.io", resource: $res, sub: "", ns: "", name: "",
          label: "\(.) \($res).admissionregistration.k8s.io cluster-wide"}
       | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}]' 2>"$WORK/admission-rights.err")" && is_rights "$built"; then
    ADMISSION_RIGHTS="$built"
  else
    ADMISSION_ERROR="${ADMISSION_ERROR:+$ADMISSION_ERROR; and }the rights on the admission resources could not be built, so none is reviewed: $(first_line "$WORK/admission-rights.err")"
  fi
  [ -z "$ADMISSION_ERROR" ]
}
# named_grants RIGHTS OUT [FENCE]: code-rbac.jq's read of the cluster's live bindings and roles
# ($WORK/bindings.json and $WORK/roles.json) for the rules that limit a right of RIGHTS (a JSON array,
# as rights_json writes rights) by name alone, its records to OUT and its errors to OUT.err, each
# grant with the fence's verdict on its right under FENCE (the cluster's credited fence, as review
# takes it). RIGHTS reaches jq through a file, never its command line, as review's do.
named_grants() {
  printf '[]' >"$WORK/no-rights.json"
  printf '%s' "$1" >"$2.rights.json"
  jq -r -n --slurpfile rights "$WORK/no-rights.json" --slurpfile named_rights "$2.rights.json" --argjson live true \
    --argjson fence "${3:-null}" -L "$LIB" -f "$LIB/code-rbac.jq" "$WORK/bindings.json" "$WORK/roles.json" >"$2" 2>"$2.err"
}
# live_named KUBECONFIG STEP SUFFIX RESOURCE RIGHTS [FENCE]: the rights a live role limits by name.
# RBAC applies a rule with resourceNames only to a request that names one of them, so a review that
# names nothing is refused for an update of one webhook configuration, a patch of one Deployment or
# a pods/exec of one pod by name. The cluster's Roles and ClusterRoles (`kubectl get
# roles,clusterroles -A`) are read against $WORK/bindings.json as the code step reads a render
# (code-rbac.jq, by name alone): each rule whose resourceNames limit a right of RIGHTS (the JSON
# array the cluster's reviews asked) is judged for a binding subject k8s_subject does not allow, and
# seeded for a system namespace's ServiceAccount (named_principal, judge_grant), its right followed
# by SUFFIX, on RESOURCE, under FENCE (the cluster's credited fence, as review takes it). A part of
# the certificate chain so limited (an approval by name, whose name the requester picks) goes to
# csr_part, so it joins the signers judge_reviews' reviews allowed; the caller runs csr_chains after
# this. Runs after judge_reviews; a refused list is UNKNOWN naming the permission, and an answer that
# is not a list (is_list), that holds no bootstrap ClusterRole cluster-admin (labelled
# kubernetes.io/bootstrapping=rbac-defaults, which every cluster has, so a list without it is not
# whole: code-rbac.jq passes over a binding whose role the list lacks), or that code-rbac.jq cannot
# read is UNKNOWN naming the roles' list.
live_named() {
  local what id detail kind ns name policy residual
  # ROLES_OK: whether $WORK/roles.json holds this cluster's roles, for follow_becomes's named read.
  ROLES_OK=false
  [ "$BINDINGS_OK" = true ] || return 0
  if ! kubectl --kubeconfig "$1" get roles,clusterroles --all-namespaces -o json >"$WORK/roles.json" 2>"$WORK/roles.err"; then
    unknown "$2" "the API server refused the roles' list (it needs list on roles and clusterroles, container.roles.list and container.clusterRoles.list on GKE), so a right a role limits by name is not read: $(first_line "$WORK/roles.err")"
    return 0
  fi
  if ! is_list "$WORK/roles.json" "$WORK/named.rec.err"; then
    unknown "$2" "the API server's list of roles could not be read, so a right a role limits by name is not read: $(first_line "$WORK/named.rec.err")"
    return 0
  fi
  if ! jq -e 'any(.items[]; .kind == "ClusterRole" and .metadata.name == "cluster-admin"
      and .metadata.labels["kubernetes.io/bootstrapping"] == "rbac-defaults")' "$WORK/roles.json" >/dev/null 2>&1; then
    unknown "$2" "the API server's list of roles holds no bootstrap ClusterRole cluster-admin (labelled kubernetes.io/bootstrapping=rbac-defaults), which every cluster has, so it is not whole and a right a role limits by name is not read"
    return 0
  fi
  if ! named_grants "$5" "$WORK/named.rec" "${6:-}"; then
    unknown "$2" "the API server's list of roles could not be read, so a right a role limits by name is not read: $(first_line "$WORK/named.rec.err")"
    return 0
  fi
  ROLES_OK=true
  while IFS="$US" read -r -u 6 what id detail kind ns name policy residual; do
    [ "$what" = grant ] || continue
    named_principal "$kind" "$ns" "$name"
    [ -n "$NAMED_PRINCIPAL" ] || continue
    [[ "$NAMED_PRINCIPAL" == seed:* ]] || ! csr_part "$NAMED_PRINCIPAL" "$detail" "RBAC: $id limits it by name" || continue
    judge_grant "$NAMED_PRINCIPAL" "$detail" "$3" "$4" "RBAC: $id limits it by name" "$policy" "$residual"
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
# server refused the list or answered no list (is_list), or when the list lacks the bootstrap
# cluster-admin ClusterRoleBinding, its witness: every cluster has it, so a list without it is not
# whole (its subjects are still added).
bound_subjects() {
  local kind ns name label
  local -A held=()
  BOUND=0 BOUND_ERROR="" BINDINGS_OK=false
  if ! kubectl --kubeconfig "$1" get clusterrolebindings,rolebindings --all-namespaces -o json >"$WORK/bindings.json" 2>"$WORK/bindings.err"; then
    BOUND_ERROR="the API server refused the bindings' list (it needs list on clusterrolebindings and rolebindings, container.clusterRoleBindings.list and container.roleBindings.list on GKE): $(first_line "$WORK/bindings.err")"
    return 1
  fi
  if ! is_list "$WORK/bindings.json" "$WORK/bindings.err" || ! jq -r "$JQ_REC"'[.items[] | (.metadata.namespace // "") as $bns | .subjects[]?
      | select(.kind == "User" or .kind == "Group" or .kind == "ServiceAccount")
      | [.kind, (if .kind == "ServiceAccount" then (.namespace // $bns) else "" end), .name]] | unique[] | rec' \
    "$WORK/bindings.json" >"$WORK/bindings.rec" 2>"$WORK/bindings.err"; then
    BOUND_ERROR="the API server's list of bindings could not be read: $(first_line "$WORK/bindings.err")"
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
# binding_rights: sets BINDING_RIGHTS, the rights binding-rights.jq builds from the bindings
# bound_subjects read last ($WORK/bindings.json): for each namespace that holds a RoleBinding, bind
# and escalate on roles, bind on clusterroles, and patch and update on roles and rolebindings,
# naming no resource. The step reviews them for every principal and live_named reads the rules that
# limit them by name, which is how a right in a namespace outside the guarded ones is found. When
# bound_subjects could not read the bindings (its own UNKNOWN), it holds none. Returns 1 with
# BINDING_ERROR when binding-rights.jq built no JSON array of rights, which leaves BINDING_RIGHTS
# none ([]) rather than what it printed.
binding_rights() {
  local built
  BINDING_RIGHTS='[]' BINDING_ERROR=""
  [ "$BINDINGS_OK" = true ] || return 0
  if built="$(jq -c -L "$LIB" -f "$LIB/binding-rights.jq" "$WORK/bindings.json" 2>"$WORK/binding-rights.err")" && is_rights "$built"; then
    BINDING_RIGHTS="$built"
  else
    BINDING_ERROR="the rights of the namespaces that hold a RoleBinding could not be built, so none is reviewed or read from the cluster's roles: $(first_line "$WORK/binding-rights.err")"
    return 1
  fi
}
# is_rights VALUE: whether VALUE is exactly one JSON array, as rights_json writes rights, by rec.jq's
# one_array, the one place that rule is written. An empty VALUE, which a jq program that failed or
# printed nothing leaves, is none, so a step never reads it as no rights.
is_rights() { printf '%s' "$1" | jq -s -L "$LIB" 'include "rec"; one_array(.; "rights") | empty' >/dev/null 2>&1; }
# is_list FILE ERR: whether FILE holds one JSON object whose items is an array, as kubectl get -o json
# writes a list. jq reads an empty answer, which a proxy that drops a response's body sends with exit
# 0, as no value: a program over it prints nothing and exits 0, so a read that checks only jq's exit
# status takes it for an empty list. Every list the check reads with kubectl get -o json (the
# ServiceAccounts, the bindings, the roles, the webhook configurations, the CRDs, the workload
# allowlists, the admission policies and their bindings) is held to this before any read of it; a
# webhook's Service is held to is_object. kubectl create's answer to the reviews is one document per
# review, never a List, and review-answer.jq holds it to that. On failure ERR says why (jq's own
# error for an answer that is not JSON), for the caller's UNKNOWN.
is_list() {
  jq -e -s 'length == 1 and (.[0] | type) == "object" and (.[0].items | type) == "array"' "$1" >/dev/null 2>"$2" && return 0
  shape_error "$1" "$2" "one object with a list of items"
}
# is_object FILE ERR: whether FILE holds one JSON object, as kubectl get -o json writes one object;
# is_list says why an empty answer is not.
is_object() {
  jq -e -s 'length == 1 and (.[0] | type) == "object"' "$1" >/dev/null 2>"$2" && return 0
  shape_error "$1" "$2" "one object"
}
# shape_error FILE ERR WANT: writes to ERR why FILE is not WANT, unless jq's error is there already;
# returns 1.
shape_error() {
  [ -s "$2" ] && return 1
  if grep -q '[^[:space:]]' "$1" 2>/dev/null; then echo "the answer is JSON, but not $3" >"$2"; else echo "the answer was empty" >"$2"; fi
  return 1
}
# with_rights RIGHTS MORE: RIGHTS and MORE, two JSON arrays of rights, each right once by its key;
# where both hold one, RIGHTS's label stays. Both reach jq on its stdin, never its command line. It
# refuses anything but two JSON arrays (is_rights), an empty argument included, printing why and
# returning 1: a step assigns its result, so under errexit the run stops there, UNKNOWN, rather
# than go on without the rights the argument owed. Every rights value a step joins goes through it.
# custom_rights, admission_rights and binding_rights check what they build with is_rights too, so a
# failed build reads as their own UNKNOWN, naming those rights, rather than stopping the run.
with_rights() {
  if ! is_rights "$1" || ! is_rights "$2"; then
    echo "with_rights takes two JSON arrays, and was given: $(printf '%.60s' "$1"), $(printf '%.60s' "$2")" >&2
    return 1
  fi
  printf '%s\n%s\n' "$1" "$2" | jq -c -s '.[0] + .[1] | reduce .[] as $r ([]; if any(.[]; .key == $r.key) then . else . + [$r] end)'
}
