# shellcheck shell=bash
# shellcheck disable=SC2004,SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order; CHECK_* are associative (checks.sh declares them), which this file alone cannot show
# operator-token-minters/step-live-iam.sh: the live IAM steps after Workload Identity: the fleet
# members' projects, the minters of each Google service account an RBAC finding names and of each
# whose credited grant reaches the broker's pod fence's residual, the projects' ancestors, fleet
# registration and the hold writers. Sourced by operator-token-minters.sh, whose header says what
# each step checks.
# --- live: GKE IAM on the projects of the fleet's other GKE members, each once
# (member_project_iam): the Workload Identity step ran it for each project whose clusters it read,
# and this runs it for any it has not. --project's holders are the GKE IAM step's. ---
for other in $(printf '%s\n' ${OTHER_PROJECTS[@]+"${!OTHER_PROJECTS[@]}"} | sort); do
  member_project_iam "$other"
done

# --- live: minters of the Google service accounts the RBAC reads name, to a fixed point. ---
# account_minters CHECK EMAIL STEP [RESIDUAL]: the minters check CHECK of the Google service account
# EMAIL, its step STEP: Policy Analyzer's holders on it of the minters step's permissions, judged as
# that step judges --gsa's, each a finding; with RESIDUAL (the account and the grant of it that
# reaches the broker's pod fence's residual), each an ALLOWANCE naming RESIDUAL (CHECK_RESIDUAL,
# live_iam).
account_minters() {
  account_project "$2"
  CHECK_STEP[$1]="$3"
  CHECK_RESOURCE[$1]="//iam.googleapis.com/projects/$ACCOUNT_PROJECT/serviceAccounts/$2"
  CHECK_PERMS[$1]="${GSA_PERMISSIONS[*]} iam.serviceAccounts.setIamPolicy"
  [ -z "${4:-}" ] || CHECK_RESIDUAL[$1]="$4"
  scope_of "$ACCOUNT_PROJECT"
  CHECK_SCOPE[$1]="$SCOPE_OF" CHECK_SCOPE_FLAG[$1]="$SCOPE_OF_FLAG" CHECK_SCOPE_ERROR[$1]="$SCOPE_OF_ERROR"
  live_iam "$1"
}
# pool_member PRINCIPAL RESOURCE: whether PRINCIPAL is a Workload Identity member of the operator's
# pool and RESOURCE a Google service account; sets POOL_ACCOUNT to the account's email, POOL_MEMBERS
# to the member keys follow_members (reviews.sh) follows it by, and POOL_UNREAD to why there are none.
# A name member, serviceAccount:<pool>.svc.id.goog[<ns>/<name>], names that pair on every cluster of
# the pool: the key <ns>:<name>. A member by UID (UID_MEMBER_FORM, principals.sh) of the pool and of
# the pool project's number (POOL_NUMBER) names the one ServiceAccount whose metadata.uid it holds:
# for each line of the UID map (uid_map) that holds its UID, the key <resource>|<ns>:<name>, scoped
# to that line's cluster. The broker's member by UID takes this rule too, its UID being in the
# operator's cluster's list. POOL_UNREAD names a member by UID of the pool that no ServiceAccount the
# run listed holds, or whose number cannot be compared since the pool project's is not read
# (POOL_NUMBER_ERROR). A member of another pool, or by UID of another number, returns 1.
pool_member() {
  local uid
  POOL_MEMBERS=() POOL_UNREAD=""
  [[ "$2" == //iam.googleapis.com/projects/*/serviceAccounts/* ]] || return 1
  POOL_ACCOUNT="${2##*/}"
  if [[ "$1" =~ ^serviceAccount:([^[]+)\.svc\.id\.goog\[([^/]+)/([^]]+)\]$ ]]; then
    [ "${BASH_REMATCH[1]}" = "$POOL_PROJECT" ] || return 1
    POOL_MEMBERS=("${BASH_REMATCH[2]}:${BASH_REMATCH[3]}")
    return 0
  fi
  [[ "$1" =~ $UID_MEMBER_FORM ]] && [ "${BASH_REMATCH[2]}" = "$POOL_PROJECT.svc.id.goog" ] || return 1
  if [ -z "$POOL_NUMBER" ]; then
    POOL_UNREAD="$POOL_NUMBER_ERROR"
    return 0
  fi
  [ "${BASH_REMATCH[1]}" = "$POOL_NUMBER" ] || return 1
  uid="${BASH_REMATCH[3]}"
  mapfile -t POOL_MEMBERS < <(POOL_UID="$uid" awk -F'\t' '$1 == ENVIRON["POOL_UID"] { print $2 "|" $3 ":" $4 }' "$WORK/uids.tsv")
  [ "${#POOL_MEMBERS[@]}" -gt 0 ] || POOL_UNREAD="no ServiceAccount that the run listed on the clusters of the pool holds UID $uid"
}
# pool_unknown CHECK PRINCIPAL: the UNKNOWN on CHECK's step that PRINCIPAL, a member by UID whose
# ServiceAccount pool_member could not tell (POOL_UNREAD), can act as POOL_ACCOUNT, and that who can
# become that ServiceAccount is not followed. unknown prints it once, though each pass reads it.
pool_unknown() {
  unknown "${CHECK_STEP[$1]}" "$2 can act as $POOL_ACCOUNT through Workload Identity, and who can become the ServiceAccount it names is not followed: $POOL_UNREAD"
}
# cluster_account PRINCIPAL RESOURCE: whether a record's PRINCIPAL is a Google service account and
# its RESOURCE a cluster or a fleet member (the renders' too); sets CLUSTER_ACCOUNT to its email.
cluster_account() {
  case "$1:$2" in serviceAccount:*:cluster:* | serviceAccount:*:membership:*) ;; *) return 1 ;; esac
  CLUSTER_ACCOUNT="${1#serviceAccount:}"
}
# Each Google service account an RBAC finding names (on a cluster, a fleet member, or in the renders)
# has its minters judged: who can mint its token, sign as it or act as it holds what the finding
# gives it, so once the finding is accepted, an agent that can become the account must still be
# reported. Each Google service account whose credited grant reaches the broker's pod fence's
# residual (an ALLOWANCE, broker-fence.sh) and which no finding names has its minters read the same
# way, each an ALLOWANCE naming the chain and the residual. Each Workload Identity member of the
# operator's pool such a check lists acts as the account, so who can run as the ServiceAccount it
# names (a pod in its namespace, a token of it) is followed again from it (follow_members): a name
# member's on every cluster of the pool, each of which can hold that namespace and name, and a member
# by UID's on the one cluster whose listed ServiceAccount holds that UID (pool_member), since a UID
# names one object. A member by UID whose ServiceAccount cannot be told (no listed ServiceAccount
# holds its UID, or the pool project's number is not read) is UNKNOWN on the account's step, and
# nothing is followed from it. A chain to a found account's member is a finding, and one to a
# residual account's member an ALLOWANCE. A follow can name further Google accounts, by a finding or
# by an ALLOWANCE, so the checks run until a pass adds none: each pass reads FINDINGS and RESIDUALS
# afresh, judges each account a finding names that is not yet judged as found, follows each new
# member of a found account, and only once no found account is new judges each new residual account
# and follows its new members. An account found after it was read for the residual is judged again
# as found, and a member followed as found is never followed for the residual; a member here is its
# key (MEMBER_FOLLOWED), so a member by UID's scoped key and a name member's are each followed once.
# A residual check's holders and its members' chains are ALLOWANCEs, never findings, so the found
# accounts settle first.
declare -A FOUND_CHECKED=() RESIDUAL_CHECKED=() MEMBER_FOLLOWED=() accounts=()
while :; do
  new=false accounts=()
  for entry in ${FINDINGS[@]+"${FINDINGS[@]}"}; do
    IFS=$'\t' read -r principal _ _ resource _ <<<"$entry"
    cluster_account "$principal" "$resource" || continue
    [ -n "${FOUND_CHECKED[$CLUSTER_ACCOUNT]:-}" ] || accounts[$CLUSTER_ACCOUNT]=1
  done
  for email in $(printf '%s\n' ${accounts[@]+"${!accounts[@]}"} | sort); do
    FOUND_CHECKED[$email]=1 new=true
    account_minters "rbac-account:$email" "$email" "live: minters of $email, which an RBAC finding names"
  done
  members='{}'
  for entry in ${FINDINGS[@]+"${FINDINGS[@]}"}; do
    IFS=$'\t' read -r principal _ _ resource _ <<<"$entry"
    pool_member "$principal" "$resource" || continue
    [ -n "${FOUND_CHECKED[$POOL_ACCOUNT]:-}" ] || continue
    [ -z "$POOL_UNREAD" ] || pool_unknown "rbac-account:$POOL_ACCOUNT" "$principal"
    for key in ${POOL_MEMBERS[@]+"${POOL_MEMBERS[@]}"}; do
      [ "${MEMBER_FOLLOWED[$key]:-}" != found ] || continue
      MEMBER_FOLLOWED[$key]=found
      members="$(jq -c --arg k "$key" --arg v "act as $POOL_ACCOUNT through Workload Identity" '. + {($k): $v}' <<<"$members")"
    done
  done
  if [ "$members" != '{}' ]; then
    follow_members "$members" '{}'
    new=true
  fi
  # The found accounts settle before any residual one is read.
  [ "$new" = false ] || continue
  accounts=()
  for entry in ${RESIDUALS[@]+"${RESIDUALS[@]}"}; do
    IFS=$'\t' read -r principal right resource _ <<<"$entry"
    cluster_account "$principal" "$resource" || continue
    [ -z "${FOUND_CHECKED[$CLUSTER_ACCOUNT]:-}" ] && [ -z "${RESIDUAL_CHECKED[$CLUSTER_ACCOUNT]:-}" ] || continue
    keep_least accounts "$CLUSTER_ACCOUNT" "$right"
  done
  for email in $(printf '%s\n' ${accounts[@]+"${!accounts[@]}"} | sort); do
    RESIDUAL_CHECKED[$email]="${accounts[$email]}" new=true
    account_minters "residual-account:$email" "$email" "live: minters of $email, whose credited grant reaches the broker's pod fence's residual" \
      "serviceAccount:$email, which can ${accounts[$email]}"
  done
  members='{}'
  for entry in ${RESIDUALS[@]+"${RESIDUALS[@]}"}; do
    IFS=$'\t' read -r principal _ resource _ <<<"$entry"
    pool_member "$principal" "$resource" || continue
    [ -n "${RESIDUAL_CHECKED[$POOL_ACCOUNT]:-}" ] && [ -z "${FOUND_CHECKED[$POOL_ACCOUNT]:-}" ] || continue
    [ -z "$POOL_UNREAD" ] || pool_unknown "residual-account:$POOL_ACCOUNT" "$principal"
    for key in ${POOL_MEMBERS[@]+"${POOL_MEMBERS[@]}"}; do
      [ -z "${MEMBER_FOLLOWED[$key]:-}" ] || continue
      MEMBER_FOLLOWED[$key]=residual
      members="$(jq -c --arg k "$key" --arg v "act as $POOL_ACCOUNT (which can ${RESIDUAL_CHECKED[$POOL_ACCOUNT]}) through Workload Identity" \
        '. + {($k): $v}' <<<"$members")"
    done
  done
  if [ "$members" != '{}' ]; then
    follow_members '{}' "$members"
    new=true
  fi
  [ "$new" = true ] || break
done

# --- live: who can set IAM on the projects' ancestors: --project's, the pool project's and each
# fleet member's. A holder of a folder's or the organization's setIamPolicy can grant itself any role
# below it, container.admin on the project included, and a holder of the organization's
# iam.roles.update can add a permission to a custom role it holds. ---
ancestor_projects=("$PROJECT")
[ -z "$POOL_PROJECT" ] || [ "$POOL_PROJECT" = "$PROJECT" ] || ancestor_projects+=("$POOL_PROJECT")
for other in $(printf '%s\n' ${OTHER_PROJECTS[@]+"${!OTHER_PROJECTS[@]}"} | sort); do
  [ "$other" = "$POOL_PROJECT" ] || ancestor_projects+=("$other")
done
declare -A ANCESTORS_SEEN=()
for anc_project in "${ancestor_projects[@]}"; do
  step="live: who can set IAM on the ancestors of project ${anc_project:-?}"
  if [ -z "$anc_project" ]; then
    unknown "$step" "no --project: the project that runs the operator's cluster is not named"
    continue
  elif ! have gcloud; then
    unknown "$step" "gcloud is not on PATH"
    continue
  elif ! gcloud projects get-ancestors "$anc_project" --format=json --quiet >"$WORK/ancestors.json" 2>"$WORK/ancestors.err"; then
    unknown "$step" "gcloud projects get-ancestors $anc_project was refused (it needs resourcemanager.projects.get): $(first_line "$WORK/ancestors.err")"
    continue
  elif ! jq -r '.[] | select(.type == "folder" or .type == "organization") | "\(.type)s/\(.id)"' "$WORK/ancestors.json" \
    >"$WORK/ancestors.list" 2>"$WORK/ancestors.err"; then
    unknown "$step" "gcloud projects get-ancestors $anc_project answered no JSON list: $(first_line "$WORK/ancestors.err")"
    continue
  elif ! jq -e --arg p "$anc_project" 'any(.[]; .type == "project" and .id == $p)' "$WORK/ancestors.json" >/dev/null 2>&1; then
    unknown "$step" "gcloud projects get-ancestors $anc_project answered no ancestor holding the project itself, though a real answer always does, so it saw nothing"
    continue
  elif [ ! -s "$WORK/ancestors.list" ]; then
    pass "$step" "the project has no folder or organization above it"
    continue
  fi
  while IFS= read -r -u 8 anc; do
    [ -z "${ANCESTORS_SEEN[$anc]:-}" ] || continue
    ANCESTORS_SEEN[$anc]=1
    check="ancestor:$anc"
    CHECK_STEP[$check]="live: who can set IAM on $anc"
    CHECK_RESOURCE[$check]="//cloudresourcemanager.googleapis.com/$anc"
    case "$anc" in
      folders/*) CHECK_PERMS[$check]="resourcemanager.folders.setIamPolicy" ;;
      *) CHECK_PERMS[$check]="resourcemanager.organizations.setIamPolicy iam.roles.update" ;;
    esac
    scope_of "$anc_project"
    CHECK_SCOPE[$check]="$SCOPE_OF" CHECK_SCOPE_FLAG[$check]="$SCOPE_OF_FLAG" CHECK_SCOPE_ERROR[$check]="$SCOPE_OF_ERROR"
    live_iam "$check"
  done 8<"$WORK/ancestors.list"
done

# --- live: who can register a cluster to the pool project's fleet, or write the namespaces and RBAC
# its scopes put on the fleet's members. A registered cluster shares the pool's Workload Identity, so
# whoever registers one makes the operator's member on it. ---
live_iam fleet

# --- live: hold writers ---
live_iam sql
live_iam dbsecret "the RBAC step's principals' get on it is in that step"
live_iam state
live_iam signing
