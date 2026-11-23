# shellcheck shell=bash
# shellcheck disable=SC2004,SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order; CHECK_* are associative (checks.sh declares them), which this file alone cannot show
# operator-token-minters/step-live-iam.sh: the live IAM steps after Workload Identity: the fleet
# members' projects, the minters of each Google service account an RBAC finding names, the
# projects' ancestors, fleet registration and the hold writers. Sourced by
# operator-token-minters.sh, whose header says what each step checks.
# --- live: GKE IAM on the projects of the fleet's other GKE members, each once
# (member_project_iam): the Workload Identity step ran it for each project whose clusters it read,
# and this runs it for any it has not. --project's holders are the GKE IAM step's. ---
for other in $(printf '%s\n' ${OTHER_PROJECTS[@]+"${!OTHER_PROJECTS[@]}"} | sort); do
  member_project_iam "$other"
done

# --- live: minters of each Google service account an RBAC finding names (on a cluster, a fleet
# member, or in the renders). Who can mint its token, sign as it or act as it holds what the finding
# gives it, so its holders of the Google service account permissions are judged as the operator's
# own account's are: once the finding is accepted, an agent that can become the account must still
# be reported. ---
declare -A RBAC_ACCOUNTS=()
for entry in ${FINDINGS[@]+"${FINDINGS[@]}"}; do
  IFS=$'\t' read -r principal _ _ resource _ <<<"$entry"
  case "$principal:$resource" in serviceAccount:*:cluster:* | serviceAccount:*:membership:*) RBAC_ACCOUNTS[${principal#serviceAccount:}]=1 ;; esac
done
for email in $(printf '%s\n' ${RBAC_ACCOUNTS[@]+"${!RBAC_ACCOUNTS[@]}"} | sort); do
  account_project "$email"
  check="rbac-account:$email"
  CHECK_STEP[$check]="live: minters of $email, which an RBAC finding names"
  CHECK_RESOURCE[$check]="//iam.googleapis.com/projects/$ACCOUNT_PROJECT/serviceAccounts/$email"
  CHECK_PERMS[$check]="${GSA_PERMISSIONS[*]} iam.serviceAccounts.setIamPolicy"
  scope_of "$ACCOUNT_PROJECT"
  CHECK_SCOPE[$check]="$SCOPE_OF" CHECK_SCOPE_FLAG[$check]="$SCOPE_OF_FLAG" CHECK_SCOPE_ERROR[$check]="$SCOPE_OF_ERROR"
  live_iam "$check"
done

# --- Who can become a Workload Identity member of such an account: a ServiceAccount ns/name the
# account's analysis lists as a member of the operator's pool acts as the account on every cluster
# of the pool, so who can run as it there (a pod in ns, a token of it) holds what the RBAC finding
# gives the account. Each cluster's read is followed again from those members (follow_members). ---
members_json='{}'
for entry in ${FINDINGS[@]+"${FINDINGS[@]}"}; do
  IFS=$'\t' read -r principal _ _ resource _ <<<"$entry"
  [[ "$principal" =~ ^serviceAccount:([^[]+)\.svc\.id\.goog\[([^/]+)/([^]]+)\]$ ]] || continue
  [ "${BASH_REMATCH[1]}" = "$POOL_PROJECT" ] || continue
  [[ "$resource" == //iam.googleapis.com/projects/*/serviceAccounts/* ]] || continue
  [ -n "${RBAC_ACCOUNTS[${resource##*/}]:-}" ] || continue
  members_json="$(jq -c --arg k "${BASH_REMATCH[2]}:${BASH_REMATCH[3]}" --arg v "${resource##*/}" '. + {($k): $v}' <<<"$members_json")"
done
[ "$members_json" = '{}' ] || follow_members "$members_json"

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
