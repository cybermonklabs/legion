# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/principals.sh: who a principal is, and whether it is allowed. Sourced by
# operator-token-minters.sh, whose header says what each step checks.
# --- Principals. ---

system_namespace() { case "$1" in kube-system | gke-* | gmp-*) return 0 ;; *) return 1 ;; esac; }
# KNOWN_NUMBERS: the project numbers of the projects this check reads (--project, the pool project
# and each fleet member's project), each read from its project by know_project, to the project.
declare -A KNOWN_NUMBERS=() KNOWN_PROJECTS=()
# know_project PROJECT: adds PROJECT's number (`gcloud projects describe`) to KNOWN_NUMBERS, once. A
# describe that is refused adds none, so that project's service agents are judged as any account is.
know_project() {
  local number
  [ -n "$1" ] && [ -z "${KNOWN_PROJECTS[$1]:-}" ] || return 0
  KNOWN_PROJECTS[$1]=1
  number="$(gcloud projects describe "$1" --format=json --quiet 2>/dev/null | jq -r '.projectNumber // ""' 2>/dev/null)" || true
  [ -z "$number" ] || KNOWN_NUMBERS[$number]="$1"
}
# service_agent MEMBER: a Google-managed service agent the deployment needs, which no principal of
# the project can act as: GKE's (service-<number>@container-engine-robot), GKE Hub's
# (service-<number>@gcp-sa-gkehub) and Compute Engine's (service-<number>@compute-system) of a
# project in KNOWN_NUMBERS, and each --service-agent. Such an agent of any other project acts for
# whoever owns that project (its GKE robot granted rights here, as in a Shared VPC), and any other
# service-<number>@gcp-sa-* account acts for whoever uses that product: each is judged as any
# account is. Sets AGENT_NOTE, for a finding, when MEMBER is one of the three kinds of an unknown
# project.
service_agent() {
  local named
  AGENT_NOTE=""
  if [[ "$1" =~ ^serviceAccount:service-([0-9]+)@(container-engine-robot|gcp-sa-gkehub|compute-system)\.iam\.gserviceaccount\.com$ ]]; then
    [ -z "${KNOWN_NUMBERS[${BASH_REMATCH[1]}]:-}" ] || return 0
    AGENT_NOTE=" (the service agent of project number ${BASH_REMATCH[1]}, none of the projects this check reads)"
  fi
  for named in ${SERVICE_AGENTS[@]+"${SERVICE_AGENTS[@]}"}; do
    [ "serviceAccount:$named" != "$1" ] || return 0
  done
  return 1
}
# account_project EMAIL sets ACCOUNT_PROJECT to the project a Google service account's email
# names, or --project.
account_project() {
  if [[ "$1" =~ @([a-z][a-z0-9-]*)\.iam\.gserviceaccount\.com$ ]]; then ACCOUNT_PROJECT="${BASH_REMATCH[1]}"; else ACCOUNT_PROJECT="$PROJECT"; fi
}
account_project "$GSA"
GSA_PROJECT="$ACCOUNT_PROJECT"
account_project "$BROKER"
BROKER_PROJECT="$ACCOUNT_PROJECT"
# The members a workload pool gives a Kubernetes ServiceAccount: <pool>.svc.id.goog[<ns>/<name>],
# whatever cluster of the pool the ServiceAccount is on.
WI_MEMBER="serviceAccount:$POOL_PROJECT.svc.id.goog[$NAMESPACE/$KSA]"
BROKER_WI_MEMBER=""
[ -z "$BROKER_WORKLOAD" ] || BROKER_WI_MEMBER="serviceAccount:$POOL_PROJECT.svc.id.goog[$BROKER_WORKLOAD]"
# The pool also gives a ServiceAccount a member by its UID, its metadata.uid, which names that one
# object and not its namespace and name on every cluster: UID_MEMBER_FORM, with the project number,
# the pool and the UID captured.
UID_MEMBER_FORM='^principal://iam\.googleapis\.com/projects/([^/]+)/locations/global/workloadIdentityPools/([^/]+)/kubernetes\.serviceaccount\.uid/([^/]+)$'
# broker_uid MEMBER: whether MEMBER names the --broker-workload ServiceAccount by its UID, against
# what broker-uid.sh read before any verdict asks (BROKER_UID, on the operator's cluster, and
# POOL_NUMBER, the pool project's number). Sets BROKER_UID_MATCH to none (MEMBER is no member by
# UID), no (no --broker-workload, or a part the run read differs: the pool is not
# $POOL_PROJECT.svc.id.goog, or the UID or the number is another, so MEMBER names another object), yes
# (the UID and the number read, and every part equal) or unread (every part read equal, and the UID
# or the number not read, as BROKER_UID_UNREAD says). The broker check's verdict (iam_verdict) and a
# chain's link from a member by UID to --broker-account (live_iam) both ask it, through
# broker_uid_verdict (checks.sh), so the rule is written here alone.
broker_uid() {
  BROKER_UID_MATCH=none
  [[ "$1" =~ $UID_MEMBER_FORM ]] || return 0
  BROKER_UID_MATCH=no
  [ -n "$BROKER_WORKLOAD" ] && [ "${BASH_REMATCH[2]}" = "$POOL_PROJECT.svc.id.goog" ] || return 0
  [ -z "$BROKER_UID" ] || [ "${BASH_REMATCH[3]}" = "$BROKER_UID" ] || return 0
  [ -z "$POOL_NUMBER" ] || [ "${BASH_REMATCH[1]}" = "$POOL_NUMBER" ] || return 0
  if [ -n "$BROKER_UID" ] && [ -n "$POOL_NUMBER" ]; then
    BROKER_UID_MATCH=yes
  else
    BROKER_UID_MATCH=unread
  fi
}

# k8s_subject KIND NAMESPACE NAME: sets SUBJECT, the principal's name in a finding, and SUBJECT_OK,
# whether it is allowed (the header's "Group and User subjects"). Allowed: a person (a User that is
# an email outside gserviceaccount.com), a Kubernetes component (a system: User or Group other than
# those below), a system namespace's ServiceAccount, Google's service agent, the operator's own
# Google service account, and a Group other than Kubernetes' own, which the live steps judge by
# where its members come from. Never allowed: Kubernetes' own groups (system:authenticated,
# system:serviceaccounts and system:serviceaccounts:<namespace>, system:unauthenticated,
# system:masters), whose members Kubernetes decides whatever Google Groups for RBAC says, anyone
# unauthenticated, and a User that is no email (a bare name, a principal:// name).
k8s_subject() {
  local kind="$1" ns="$2" name="$3"
  SUBJECT_OK=false
  case "$kind" in
    ServiceAccount)
      SUBJECT="system:serviceaccount:$ns:$name"
      if system_namespace "$ns"; then SUBJECT_OK=true; fi
      ;;
    Group)
      SUBJECT="group:$name"
      case "$name" in
        system:serviceaccounts | system:serviceaccounts:* | system:authenticated | system:unauthenticated | system:masters) ;;
        *) SUBJECT_OK=true ;;
      esac
      ;;
    User)
      case "$name" in
        system:serviceaccount:*:*)
          SUBJECT="$name"
          ns="${name#system:serviceaccount:}"
          if system_namespace "${ns%%:*}"; then SUBJECT_OK=true; fi
          ;;
        system:anonymous) SUBJECT="user:$name" ;;
        system:*) SUBJECT="user:$name" SUBJECT_OK=true ;;
        *.gserviceaccount.com)
          SUBJECT="serviceAccount:$name"
          if { [ -n "$GSA" ] && [ "$name" = "$GSA" ]; } || service_agent "$SUBJECT"; then SUBJECT_OK=true; fi
          ;;
        *)
          SUBJECT="user:$name"
          # A person is an email: one @, a dotted domain, no slash, colon or space.
          if [[ "$name" =~ ^[^@/:[:space:]]+@[^@/:[:space:]]+\.[^@/:[:space:]]+$ ]]; then SUBJECT_OK=true; fi
          ;;
      esac
      ;;
    *) SUBJECT="$kind:$name" ;;
  esac
}
# own_group NAME: whether a Group is one of Kubernetes' own that is reviewed alone (system:masters is
# not: every review of it is allowed by the bootstrap cluster-admin binding, so its other bindings
# are read instead).
own_group() { case "$1" in system:serviceaccounts | system:serviceaccounts:* | system:authenticated | system:unauthenticated) return 0 ;; *) return 1 ;; esac; }
# --project's and the pool project's numbers, before any step judges a principal; each fleet
# member's project is added as the Workload Identity step reaches it (member_project_iam).
know_project "$PROJECT"
know_project "$POOL_PROJECT"
