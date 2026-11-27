# shellcheck shell=bash
# shellcheck disable=SC2004,SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order; CHECK_* are associative (checks.sh declares them), which this file alone cannot show
# operator-token-minters/iam-analysis.sh: Policy Analyzer's scope, its analysis, and how a live IAM
# check judges its holders and their impersonators. Sourced by operator-token-minters.sh, whose
# header says what each step checks.
# --- The live IAM checks. ---

# scope_of PROJECT: the analysis scope for a resource of PROJECT, --analysis-scope, else the
# project's top ancestor, so a grant made on its folder or its organization is seen. Sets
# SCOPE_OF ("organizations/N", ...), SCOPE_OF_FLAG (the flag analyze passes) and SCOPE_OF_ERROR.
scope_of() {
  SCOPE_OF="$ANALYSIS_SCOPE" SCOPE_OF_ERROR=""
  if [ -z "$SCOPE_OF" ] && [ -n "$1" ]; then
    if ! have gcloud; then
      SCOPE_OF_ERROR="gcloud is not on PATH"
    elif ! gcloud projects get-ancestors "$1" --format=json --quiet >"$WORK/ancestors.json" 2>"$WORK/ancestors.err"; then
      SCOPE_OF_ERROR="gcloud projects get-ancestors $1 was refused (it needs resourcemanager.projects.get), so the analysis scope is unknown: $(first_line "$WORK/ancestors.err")"
    else
      SCOPE_OF="$(jq -r 'last | if .type == "organization" then "organizations/\(.id)" elif .type == "folder" then "folders/\(.id)" elif .type == "project" then "projects/\(.id)" else "" end' \
        "$WORK/ancestors.json" 2>/dev/null || true)"
      [ -n "$SCOPE_OF" ] || SCOPE_OF_ERROR="gcloud projects get-ancestors $1 answered no ancestor"
    fi
  fi
  case "$SCOPE_OF" in
    organizations/*) SCOPE_OF_FLAG="--organization=${SCOPE_OF#organizations/}" ;;
    folders/*) SCOPE_OF_FLAG="--folder=${SCOPE_OF#folders/}" ;;
    *) SCOPE_OF_FLAG="--project=${SCOPE_OF#projects/}" ;;
  esac
}
scope_of "$PROJECT"
SCOPE="$SCOPE_OF" SCOPE_FLAG="$SCOPE_OF_FLAG" SCOPE_ERROR="$SCOPE_OF_ERROR"
# A check's own scope, for a resource outside --project: CHECK_SCOPE, CHECK_SCOPE_FLAG and
# CHECK_SCOPE_ERROR, keyed by the check; a check with none uses the project's.
declare -A CHECK_SCOPE=() CHECK_SCOPE_FLAG=() CHECK_SCOPE_ERROR=()
if [ -n "$POOL_PROJECT" ] && [ "$POOL_PROJECT" != "$PROJECT" ]; then
  scope_of "$POOL_PROJECT"
  CHECK_SCOPE[fleet]="$SCOPE_OF" CHECK_SCOPE_FLAG[fleet]="$SCOPE_OF_FLAG" CHECK_SCOPE_ERROR[fleet]="$SCOPE_OF_ERROR"
fi

# analyze RESOURCE PERMISSIONS_CSV SCOPE_FLAG SCOPE: Policy Analyzer's holders of any of the
# permissions on the resource, with service account impersonation: a principal that can mint a
# holder's token, sign as it, or act as it holds what the holder holds. Written as records to
# $WORK/analysis.rec: a `row` per binding and identity (identity, role, the resource the binding is
# made on, the permissions it grants, comma-separated, and `expanded` for a group whose members the
# analysis lists), and a `chain` per impersonator of a service account holder, through any number
# of service accounts (the outermost impersonator, its role and where that is granted, the holder's
# permissions, `expanded`, `designed` for a link the design makes, the holder's role and where it
# is granted, the holder, and the chain written out). Returns 1 with ANALYZE_ERROR, also for an
# impersonation analysis not fully explored or naming no service account: an impersonator it did
# not list would read as none.
#
# The designed links are a Workload Identity member acting as the account it is bound to (the
# operator's member as --gsa, the broker's as --broker-account) and the broker minting --gsa's
# token: its outermost principal is the account it acts as, already judged, so it is no finding.
analyze() {
  ANALYZE_ERROR=""
  if ! gcloud asset analyze-iam-policy "$3" --full-resource-name="$1" --permissions="$2" \
    --expand-groups --output-group-edges --expand-roles --analyze-service-account-impersonation \
    --show-response --format=json --quiet >"$WORK/analysis.json" 2>"$WORK/analysis.err"; then
    ANALYZE_ERROR="Policy Analyzer refused the analysis (it needs cloudasset.assets.analyzeIamPolicy on $4): $(first_line "$WORK/analysis.err")"
    return 1
  fi
  if ! jq -r --arg wi "$WI_MEMBER" --arg gsa "$GSA" --arg bwi "$BROKER_WI_MEMBER" --arg broker "$BROKER" -L "$LIB" -f "$LIB/analysis.jq" \
    "$WORK/analysis.json" >"$WORK/analysis.rec" 2>"$WORK/analysis.err"; then
    ANALYZE_ERROR="the analysis is not the JSON Policy Analyzer answers: $(first_line "$WORK/analysis.err")"
    return 1
  fi
  # The first record, read by the shell: an error record is `error` and its reason.
  local first reason
  IFS="$US" read -r first reason <"$WORK/analysis.rec" || true
  if [ "$first" = error ]; then
    ANALYZE_ERROR="$reason"
    return 1
  fi
}

# live_iam CHECK [PASS_NOTE]: the check's analysis, each holder judged (iam_verdict): a finding, or,
# for a check of CHECK_RESIDUAL, an ALLOWANCE naming the holder's chain to the account and the
# fence's residual (iam_holder).
live_iam() {
  local check="$1" step="${CHECK_STEP[$1]}" resource="${CHECK_RESOURCE[$1]}" witness_member="${CHECK_WITNESS[$1]:-}"
  local holders what identity role attached perms how designed hattached text chains=0 allowances
  if [ -n "${CHECK_MISSING[$check]:-}" ]; then
    unknown "$step" "${CHECK_MISSING[$check]}"
    return
  fi
  local scope="${CHECK_SCOPE[$check]-$SCOPE}" scope_flag="${CHECK_SCOPE_FLAG[$check]-$SCOPE_FLAG}" scope_error="${CHECK_SCOPE_ERROR[$check]-$SCOPE_ERROR}"
  if [ -n "$scope_error" ]; then
    unknown "$step" "$scope_error"
    return
  fi
  if ! have gcloud; then
    unknown "$step" "gcloud is not on PATH"
    return
  fi
  if [ "$check" = signing ] && [ -z "$PROJECT_NUMBER" ]; then
    unknown "$step" "the project number, which the key's resource name holds, is unknown: $PROJECT_NUMBER_ERROR"
    return
  fi
  if ! analyze "$resource" "${CHECK_PERMS[$check]// /,}" "$scope_flag" "$scope"; then
    unknown "$step" "$ANALYZE_ERROR"
    return
  fi
  if [ ! -s "$WORK/analysis.rec" ]; then
    unknown "$step" "the analysis lists no holder of ${CHECK_PERMS[$check]// /, } on $resource, though its project's owners hold them, so it saw nothing"
    return
  fi
  mark
  if [ "$check" = dbsecret ] && [ "$RBAC_OK" != true ]; then
    unknown "$step" "the RBAC step's principals' reviews for get on it could not be made (live: RBAC)"
  fi
  # The holders, each once, and whether the witness is among them, in one read of the records (an
  # awk that reads them whole: never `cut | grep -q` under pipefail, where grep stops at its first
  # match and a long answer's next write fails the pipeline with SIGPIPE, reading a witness that is
  # there as one that is not).
  local witness_listed
  read -r holders witness_listed < <(WITNESS="$witness_member" awk -F"$US" '$1 == "row" { if (!seen[$2]++) n++; if ($2 == ENVIRON["WITNESS"]) listed = 1 }
    END { print n + 0, listed + 0 }' "$WORK/analysis.rec")
  if [ -n "$witness_member" ] && [ "$witness_listed" != 1 ]; then
    unknown "$step" "the analysis does not list $witness_member, which holds a permission on it by design, so it cannot be read as complete"
  fi
  # A chain record's holder role and holder are in its written-out chain, the sentence's part.
  while IFS="$US" read -r -u 3 what identity role attached perms how designed _ hattached _ text; do
    if [ "$what" = chain ]; then
      # The outermost impersonator, judged as a holder of what the holder holds; a designed link's
      # principal is the account it acts as, already judged.
      chains=$((chains + 1))
      [ "$designed" != designed ] || continue
      iam_verdict "$check" "$identity" "$role" "$attached" "$how"
      [ "$VERDICT" != finding ] ||
        iam_holder "$check" "$identity" "$(held_text "$check" "$perms") on $resource: $text, granted at $hattached$NOTE" \
          "$role" "$resource" "$(held_list "$check" "$perms")" "$attached"
      continue
    fi
    iam_verdict "$check" "$identity" "$role" "$attached" "$how"
    [ "$VERDICT" != finding ] ||
      iam_holder "$check" "$identity" "$(held_text "$check" "$perms") on $resource through $role, granted at $attached$NOTE" \
        "$role" "$resource" "$(held_list "$check" "$perms")" "$attached"
  done 3<"$WORK/analysis.rec"
  if unchanged; then
    allowances="$(allowed)"
    pass "$step" "$holders holders listed, each a person, a group judged by its members, Google's service agent or the one allowed by name, and $chains impersonation chains, each from such a principal or by design${2:+; $2}$([ "$allowances" -eq 0 ] || echo ", beside $allowances that reach the broker's pod fence's residual through the account, each an ALLOWANCE")"
  fi
}
# iam_holder CHECK IDENTITY RIGHT ROLE RESOURCE PERMISSIONS ATTACHED: a holder iam_verdict judged a
# finding: for a check of CHECK_RESIDUAL, whose account's grant reaches the broker's pod fence's
# residual and no more, an ALLOWANCE naming RIGHT, the chain to the account and the residual
# (residual_allowance, broker-fence.sh); else a finding.
iam_holder() {
  if [ -n "${CHECK_RESIDUAL[$1]:-}" ]; then
    residual_allowance "$2" "$3, so it can act as ${CHECK_RESIDUAL[$1]}" "$5" "$7"
  else
    finding "$2" "$3" "$4" "$5" "$6" "$7"
  fi
}

# member_project_iam PROJECT: the step "live: GKE IAM on project PROJECT, of a fleet member", once per
# project. GKE authorizes IAM beside RBAC on a member in a project other than --project, so a holder
# there of a guard right's container.* permission (a guarded namespace, ServiceAccount or pod, or an
# escalation right by which it grants itself one) has it on that member as its RBAC would, and a
# holder of the IAM writers there grants itself any of them (MEMBER_PERMISSIONS). The Workload
# Identity step runs it before reading that project's clusters.
declare -A MEMBER_IAM_DONE=()
member_project_iam() {
  local check="gke:$1"
  [ -z "${MEMBER_IAM_DONE[$1]:-}" ] || return 0
  MEMBER_IAM_DONE[$1]=1
  know_project "$1"
  CHECK_STEP[$check]="live: GKE IAM on project $1, of a fleet member"
  CHECK_RESOURCE[$check]="//cloudresourcemanager.googleapis.com/projects/$1"
  CHECK_PERMS[$check]="${MEMBER_PERMISSIONS[*]}"
  scope_of "$1"
  CHECK_SCOPE[$check]="$SCOPE_OF" CHECK_SCOPE_FLAG[$check]="$SCOPE_OF_FLAG" CHECK_SCOPE_ERROR[$check]="$SCOPE_OF_ERROR"
  live_iam "$check"
}
