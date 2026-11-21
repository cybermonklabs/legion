# shellcheck shell=bash
# shellcheck disable=SC2004,SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order; CHECK_* are associative (checks.sh declares them), which this file alone cannot show
# operator-token-minters/step-live-iam.sh: the live IAM steps after Workload Identity: the fleet
# members' projects, the projects' ancestors, fleet registration and the hold writers. Sourced by
# operator-token-minters.sh, whose header says what each step checks.
# --- live: GKE IAM on the projects of the fleet's other GKE members. GKE authorizes IAM beside
# RBAC, so a holder of container.namespaces.create, container.serviceAccounts.create or
# container.pods.create there makes a guarded pair's namespace, ServiceAccount or pod on that member
# as its RBAC would; IAM grants them in every namespace. --project's holders are the GKE IAM step's.
# ---
for other in $(printf '%s\n' ${OTHER_PROJECTS[@]+"${!OTHER_PROJECTS[@]}"} | sort); do
  check="gke:$other"
  CHECK_STEP[$check]="live: GKE IAM on project $other, of a fleet member"
  CHECK_RESOURCE[$check]="//cloudresourcemanager.googleapis.com/projects/$other"
  CHECK_PERMS[$check]="container.namespaces.create container.serviceAccounts.create container.pods.create"
  scope_of "$other"
  CHECK_SCOPE[$check]="$SCOPE_OF" CHECK_SCOPE_FLAG[$check]="$SCOPE_OF_FLAG" CHECK_SCOPE_ERROR[$check]="$SCOPE_OF_ERROR"
  live_iam "$check"
done

# --- live: who can set IAM on the projects' ancestors. A holder of a folder's or the organization's
# setIamPolicy can grant itself any role below it, container.admin on the project included, and a
# holder of the organization's iam.roles.update can add a permission to a custom role it holds. ---
ancestor_projects=("$PROJECT")
[ -z "$POOL_PROJECT" ] || [ "$POOL_PROJECT" = "$PROJECT" ] || ancestor_projects+=("$POOL_PROJECT")
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

# --- live: who can register a cluster to the pool project's fleet. A registered cluster shares the
# pool's Workload Identity, so whoever registers one makes the operator's member on it. ---
live_iam fleet

# --- live: hold writers ---
live_iam sql
live_iam dbsecret "the RBAC step's principals' get on it is in that step"
live_iam state
live_iam signing
