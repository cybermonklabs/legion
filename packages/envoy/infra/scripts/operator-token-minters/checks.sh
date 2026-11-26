# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/checks.sh: the IAM checks, how a holder is judged, and the roles' permissions.
# Sourced by operator-token-minters.sh, whose header says what each step checks.
# --- The IAM checks: each a step, a resource, the permissions asked and a witness. ---

# CHECK_RESIDUAL, for a check of the minters of a Google service account whose credited grant reaches
# the broker's pod fence's residual (step-live-iam.sh): the account and that grant, which each
# holder's ALLOWANCE names in place of a finding (live_iam).
declare -A CHECK_STEP=() CHECK_RESOURCE=() CHECK_PERMS=() CHECK_WITNESS=() CHECK_MISSING=() CHECK_RESIDUAL=()
CHECKS=(gke dm gsa broker sql dbsecret state signing fleet)
PROJECT_RES="//cloudresourcemanager.googleapis.com/projects/$PROJECT"
BUCKET=""
if [ -n "$BROKER_STATE" ]; then
  BUCKET="${BROKER_STATE#gs://}"
  BUCKET="${BUCKET%%/*}"
fi
project_name="the operator's project"
[ -z "$PROJECT" ] || project_name="project $PROJECT"
CHECK_STEP[gke]="live: GKE IAM on $project_name"
CHECK_RESOURCE[gke]="$PROJECT_RES"
CHECK_PERMS[gke]="${CONTAINER_PERMISSIONS[*]} resourcemanager.projects.setIamPolicy iam.roles.update"
# Deployment Manager acts as the project's Google APIs service agent, which holds roles/editor there,
# whoever asks it to (DEPLOYMENT_MANAGER_PERMISSIONS, rights.sh).
CHECK_STEP[dm]="live: Deployment Manager on $project_name"
CHECK_RESOURCE[dm]="$PROJECT_RES"
CHECK_PERMS[dm]="${DEPLOYMENT_MANAGER_PERMISSIONS[*]}"
# The names steps print: a flag's value, or what it would name. A default holding an apostrophe
# cannot be written as ${NAME:-...} inside double quotes, where bash reads the apostrophe as a quote.
operator_account="the operator's Google service account" sql_name="the broker's instance"
db_name="of the broker's database login" cluster_name="the operator's cluster"
[ -z "$GSA" ] || operator_account="$GSA"
[ -z "$SQL_INSTANCE" ] || sql_name="$SQL_INSTANCE"
[ -z "$DB_SECRET" ] || db_name="$DB_SECRET"
[ -z "$CLUSTER" ] || cluster_name="$CLUSTER"
CHECK_STEP[gsa]="live: minters of $operator_account"
CHECK_RESOURCE[gsa]="//iam.googleapis.com/projects/$GSA_PROJECT/serviceAccounts/$GSA"
CHECK_PERMS[gsa]="${GSA_PERMISSIONS[*]} iam.serviceAccounts.setIamPolicy"
CHECK_WITNESS[gsa]="$WI_MEMBER"
[ -n "$GSA" ] || CHECK_MISSING[gsa]="no --gsa: the operator's Google service account is not named"
CHECK_STEP[broker]="live: minters of the broker account $BROKER"
CHECK_RESOURCE[broker]="//iam.googleapis.com/projects/$BROKER_PROJECT/serviceAccounts/$BROKER"
CHECK_PERMS[broker]="${GSA_PERMISSIONS[*]} iam.serviceAccounts.setIamPolicy"
CHECK_WITNESS[broker]="$BROKER_WI_MEMBER"
[ -n "$BROKER_WORKLOAD" ] || CHECK_MISSING[broker]="no --broker-workload: the broker account's Workload Identity member cannot be told from another's"
CHECK_STEP[sql]="live: hold writers: Cloud SQL users of $sql_name"
CHECK_RESOURCE[sql]="//sqladmin.googleapis.com/projects/$PROJECT/instances/$SQL_INSTANCE"
CHECK_PERMS[sql]="${SQL_PERMISSIONS[*]}"
[ -n "$SQL_INSTANCE" ] || CHECK_MISSING[sql]="no --sql-instance: the broker database's instance is not named"
CHECK_STEP[dbsecret]="live: hold writers: Secret $db_name"
CHECK_RESOURCE[dbsecret]="$PROJECT_RES"
CHECK_PERMS[dbsecret]="container.secrets.get"
[ -n "$DB_SECRET" ] || CHECK_MISSING[dbsecret]="no --db-secret: the Secret of the broker's database login is not named"
state_name="the broker program's state"
[ -z "$BUCKET" ] || state_name="the broker program's state in gs://$BUCKET"
CHECK_STEP[state]="live: hold writers: $state_name"
CHECK_RESOURCE[state]="//storage.googleapis.com/projects/_/buckets/$BUCKET"
CHECK_PERMS[state]="storage.objects.get"
[ -n "$BUCKET" ] || CHECK_MISSING[state]="no --broker-state: where the broker program's state is kept is not named"
CHECK_STEP[signing]="live: hold writers: the signing key ${SIGNING_SECRET:-of the writer hold}"
CHECK_PERMS[signing]="secretmanager.versions.access secretmanager.secrets.setIamPolicy"
CHECK_WITNESS[signing]="serviceAccount:$GSA"
if [ -z "$SIGNING_SECRET" ]; then
  CHECK_MISSING[signing]="no --signing-secret: the writer hold's signing key is not named"
elif [ -z "$GSA" ]; then
  CHECK_MISSING[signing]="no --gsa: the signing key's one reader, the operator's Google service account, is not named"
fi
CHECK_STEP[fleet]="live: who can register a cluster to, or write namespaces and RBAC on, the fleet of project ${POOL_PROJECT:-?}"
CHECK_RESOURCE[fleet]="//cloudresourcemanager.googleapis.com/projects/$POOL_PROJECT"
CHECK_PERMS[fleet]="${FLEET_PERMISSIONS[*]}"
# The pool project's own IAM writers, when it is not --project's, whose are the GKE IAM step's.
[ -z "$POOL_PROJECT" ] || [ "$POOL_PROJECT" = "$PROJECT" ] || CHECK_PERMS[fleet]+=" resourcemanager.projects.setIamPolicy"
if [ -z "$PROJECT" ]; then
  for check in "${CHECKS[@]}"; do CHECK_MISSING[$check]="no --project: the project that runs the operator's cluster is not named"; done
fi
[ -n "$BROKER" ] || CHECK_MISSING[broker]=skip

# iam_verdict CHECK MEMBER ROLE ATTACHED HOW: sets VERDICT to ok (allowed), skip (a group judged by
# its members) or finding, with NOTE beside a finding. ATTACHED is the resource the binding is
# made on; HOW is `expanded` for a group the analysis lists the members of, `code` for a member a
# preview declares (its group, if any, is the live steps' to expand) or empty.
iam_verdict() {
  local check="$1" member="$2" role="$3" attached="$4" how="$5"
  VERDICT=finding NOTE=""
  case "$member" in
    user:*) VERDICT=ok; return ;;
    group:*)
      if [ "$how" = expanded ] || [ "$how" = code ]; then VERDICT=skip; else NOTE=" (a group whose members the analysis does not list)"; fi
      return
      ;;
  esac
  if service_agent "$member"; then VERDICT=ok; return; fi
  NOTE="$AGENT_NOTE"
  case "$check" in
    gsa)
      if [ "$member" = "$WI_MEMBER" ]; then VERDICT=ok; return; fi
      if [ -n "$BROKER" ] && [ "$member" = "serviceAccount:$BROKER" ] && [ "$role" = roles/iam.serviceAccountTokenCreator ] &&
        [[ "$attached" == */serviceAccounts/"$GSA" ]]; then
        VERDICT=ok
      fi
      ;;
    broker) if [ -n "$BROKER_WI_MEMBER" ] && [ "$member" = "$BROKER_WI_MEMBER" ]; then VERDICT=ok; fi ;;
    signing) if [ "$member" = "serviceAccount:$GSA" ]; then VERDICT=ok; fi ;;
    dm) NOTE="$NOTE, by which Deployment Manager creates a deployment's resources as the project's Google APIs service agent, which holds roles/editor there" ;;
  esac
}
# held_text CHECK PERMISSIONS_CSV: the check's permissions among PERMISSIONS, in the check's order.
held_text() {
  local permission out=""
  for permission in ${CHECK_PERMS[$1]}; do
    case ",$2," in *",$permission,"*) out="${out:+$out, }$permission" ;; esac
  done
  printf '%s' "${out:-a permission of ${CHECK_PERMS[$1]// /, }}"
}
# held_list CHECK PERMISSIONS_CSV: the same, separated by $US, for the report: every permission the
# check asks about where the answer names none of them.
held_list() {
  local permission out=""
  for permission in ${CHECK_PERMS[$1]}; do
    case ",$2," in *",$permission,"*) out="${out:+$out$US}$permission" ;; esac
  done
  [ -n "$out" ] || out="${CHECK_PERMS[$1]// /$US}"
  printf '%s' "$out"
}

# --- Roles. ---

# role_file ROLE: sets ROLE_FILE to the file under $WORK/roles that lists the role's permissions,
# named by the role with each character outside A-Za-z0-9._- made _. role_perms and the previews'
# custom roles (step-code-iam.sh) name it so. The shell makes the name: a run asks for a role's
# file about a hundred times, and a process for each would cost more than the describes do.
role_file() { ROLE_FILE="$WORK/roles/${1//[^A-Za-z0-9._-]/_}"; }
# role_perms ROLE: sets ROLE_FILE (role_file) to a file listing the role's permissions, from the
# previews' custom roles or `gcloud iam roles describe`, once a run; returns 1 with ROLE_ERROR.
role_perms() {
  role_file "$1"
  ROLE_ERROR=""
  if [ -e "$ROLE_FILE.error" ]; then
    ROLE_ERROR="$(<"$ROLE_FILE.error")"
    return 1
  fi
  [ ! -e "$ROLE_FILE" ] || return 0
  if ! have gcloud; then
    ROLE_ERROR="gcloud is not on PATH, so the role $1 cannot be described"
  elif ! gcloud iam roles describe "$1" --format=json --quiet >"$WORK/role.json" 2>"$WORK/role.err"; then
    ROLE_ERROR="gcloud iam roles describe $1 was refused (it needs iam.roles.get for a custom role): $(first_line "$WORK/role.err")"
  elif ! jq -r '[.includedPermissions[]?] | unique[]' "$WORK/role.json" >"$ROLE_FILE" 2>"$WORK/role.err"; then
    ROLE_ERROR="gcloud iam roles describe $1 answered no JSON role: $(first_line "$WORK/role.err")"
    rm -f "$ROLE_FILE"
  fi
  if [ -n "$ROLE_ERROR" ]; then
    printf '%s' "$ROLE_ERROR" >"$ROLE_FILE.error"
    return 1
  fi
}
# role_listed PERMISSION: whether the role file ROLE_FILE (role_file, role_perms) lists PERMISSION.
# Each role file is read into ROLE_LISTED once a run, every line of it, a last one no line break
# ends too, so a preview's many bindings of one role start no process.
declare -A ROLE_READ=() ROLE_LISTED=()
role_listed() {
  local permission
  if [ -z "${ROLE_READ[$ROLE_FILE]:-}" ]; then
    while IFS= read -r permission || [ -n "$permission" ]; do ROLE_LISTED["$ROLE_FILE $permission"]=1; done <"$ROLE_FILE"
    ROLE_READ[$ROLE_FILE]=1
  fi
  [ -n "${ROLE_LISTED["$ROLE_FILE $1"]:-}" ]
}
# role_granted CHECK: sets GRANTED to those of CHECK's permissions (CHECK_PERMS, in that order) that
# the role file ROLE_FILE lists (role_listed), comma-separated, or "" for none.
role_granted() {
  local permission
  GRANTED=""
  for permission in ${CHECK_PERMS[$1]}; do
    ! role_listed "$permission" || GRANTED="${GRANTED:+$GRANTED,}$permission"
  done
}
