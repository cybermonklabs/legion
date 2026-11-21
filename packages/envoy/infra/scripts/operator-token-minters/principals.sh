# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/principals.sh: who a principal is, and whether it is allowed. Sourced by
# operator-token-minters.sh, whose header says what each step checks.
# --- Principals. ---

system_namespace() { case "$1" in kube-system | gke-* | gmp-*) return 0 ;; *) return 1 ;; esac; }
# service_agent: a Google-managed service agent, which no principal of the project can act as.
service_agent() {
  [[ "$1" =~ ^serviceAccount:service-[0-9]+@(gcp-sa-[a-z0-9-]+|container-engine-robot|compute-system)\.iam\.gserviceaccount\.com$ ]]
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

# k8s_subject KIND NAMESPACE NAME: sets SUBJECT, the principal's name in a finding, and SUBJECT_OK,
# whether it is allowed: a person, a system component, a system namespace's ServiceAccount, a Group
# other than the system groups that hold ServiceAccounts or anyone (the header's "Group subjects"),
# Google's service agent, or the operator's own Google service account.
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
        system:serviceaccounts | system:serviceaccounts:* | system:authenticated | system:unauthenticated) ;;
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
        *) SUBJECT="user:$name" SUBJECT_OK=true ;;
      esac
      ;;
    *) SUBJECT="$kind:$name" ;;
  esac
}
