# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/broker-uid.sh: the pool project's number and the UID of the broker's
# ServiceAccount (--broker-workload) on the operator's cluster, read once, after the step "live:
# clusters" and before any verdict that compares a member by UID (broker_uid, principals.sh). It
# prints no line of its own: each check that must compare a member by UID it could not read says so,
# and the broker check's PASS names what it read (BROKER_NOTE). Every command here is a test of an
# if or elif, so none stops the run under set -e. Sourced by operator-token-minters.sh, whose header
# says what each step checks.
# --- The broker's member by UID. ---

# POOL_NUMBER: the pool project's number, which a member by UID names, from KNOWN_NUMBERS (which
# know_project, principals.sh, read), or POOL_NUMBER_ERROR, why it is not read. --project's
# (PROJECT_NUMBER, step-code-iam.sh) can be another.
POOL_NUMBER="" POOL_NUMBER_ERROR=""
if [ -z "$POOL_PROJECT" ]; then
  POOL_NUMBER_ERROR="the number of the pool project is not read: no --project names it"
else
  for known in ${KNOWN_NUMBERS[@]+"${!KNOWN_NUMBERS[@]}"}; do
    if [ "${KNOWN_NUMBERS[$known]}" = "$POOL_PROJECT" ]; then POOL_NUMBER="$known"; fi
  done
  if [ -z "$POOL_NUMBER" ]; then
    POOL_NUMBER_ERROR="the number of the pool project $POOL_PROJECT is not read: gcloud projects describe $POOL_PROJECT gave no number (it needs resourcemanager.projects.get)"
  fi
fi

# BROKER_UID: the metadata.uid of the --broker-workload ServiceAccount on the operator's cluster,
# read through this run's own kubeconfig (kube_for), when the answer is exactly that one
# ServiceAccount and its uid is a lower-case UUID, as Kubernetes makes every UID: a value of another
# form is no UID, so no member is allowed against it. BROKER_UID_ERROR says why it is not read. The
# read lists the namespace's ServiceAccounts of that name, which takes list on ServiceAccounts, as
# the RBAC step's list does; a missing ServiceAccount is an empty list.
BROKER_UID="" BROKER_UID_ERROR=""
if [ -n "$BROKER_WORKLOAD" ]; then
  broker_namespace="${BROKER_WORKLOAD%%/*}" broker_name="${BROKER_WORKLOAD#*/}"
  broker_list="the list of ServiceAccounts named $broker_name in $broker_namespace"
  if [ "$OPERATOR_FOUND" != true ]; then
    BROKER_UID_ERROR="the operator's cluster could not be found (live: clusters)"
  elif ! kube_for "$CLUSTER" "$OPERATOR_LOCATION" "$PROJECT"; then
    BROKER_UID_ERROR="$KUBE_ERROR"
  elif ! kubectl --kubeconfig "$KUBE" get serviceaccounts -n "$broker_namespace" --field-selector "metadata.name=$broker_name" -o json \
    >"$WORK/broker-sa.json" 2>"$WORK/broker-sa.err"; then
    BROKER_UID_ERROR="the API server refused $broker_list (it needs list on serviceaccounts, container.serviceAccounts.list on GKE): $(first_line "$WORK/broker-sa.err")"
  elif ! is_list "$WORK/broker-sa.json" "$WORK/broker-sa.err" ||
    ! broker_answer="$(jq -r --arg ns "$broker_namespace" --arg n "$broker_name" '
      [.items[] | select(.kind == "ServiceAccount" and .metadata.namespace == $ns and .metadata.name == $n)] as $match
      | if (.items | length) == 0 then "none"
        elif (.items | length) != 1 or ($match | length) != 1 then "other \(.items | length)"
        elif ($match[0].metadata.uid | type) == "string"
          and ($match[0].metadata.uid | test("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")) then "uid \($match[0].metadata.uid)"
        else "nouuid" end' "$WORK/broker-sa.json" 2>"$WORK/broker-sa.err")"; then
    BROKER_UID_ERROR="the API server's answer to $broker_list could not be read: $(first_line "$WORK/broker-sa.err")"
  else
    case "$broker_answer" in
      "uid "*) BROKER_UID="${broker_answer#uid }" ;;
      none) BROKER_UID_ERROR="cluster $CLUSTER holds no ServiceAccount $BROKER_WORKLOAD" ;;
      nouuid) BROKER_UID_ERROR="its metadata.uid on cluster $CLUSTER is no UUID, as every Kubernetes UID is" ;;
      *) BROKER_UID_ERROR="$broker_list answered ${broker_answer#other } items, not that one ServiceAccount" ;;
    esac
  fi
fi

# BROKER_UID_UNREAD: why a member by UID equal to the broker's in every part read cannot be told to
# be it (uid_note's reason): the UID, the number or both not read. BROKER_UID_MEMBER: the broker's
# member by UID, once both are read, which then satisfies the broker check's witness as its name
# member does (CHECK_WITNESS[broker], the two joined by $US). BROKER_NOTE: the broker check's PASS
# note, naming the members it allows, or why the one by UID is not known.
BROKER_UID_UNREAD="" BROKER_UID_MEMBER="" BROKER_NOTE=""
if [ -n "$BROKER_WORKLOAD" ]; then
  if [ -z "$BROKER_UID" ]; then BROKER_UID_UNREAD="the UID of $BROKER_WORKLOAD is not read: $BROKER_UID_ERROR"; fi
  if [ -z "$POOL_NUMBER" ]; then BROKER_UID_UNREAD="${BROKER_UID_UNREAD:+$BROKER_UID_UNREAD; }$POOL_NUMBER_ERROR"; fi
  if [ -z "$BROKER_UID_UNREAD" ]; then
    BROKER_UID_MEMBER="principal://iam.googleapis.com/projects/$POOL_NUMBER/locations/global/workloadIdentityPools/$POOL_PROJECT.svc.id.goog/kubernetes.serviceaccount.uid/$BROKER_UID"
    CHECK_WITNESS[broker]+="$US$BROKER_UID_MEMBER"
    BROKER_NOTE="the broker's ServiceAccount $BROKER_WORKLOAD is allowed as $BROKER_WI_MEMBER and, by its UID on cluster $CLUSTER, as $BROKER_UID_MEMBER"
  else
    BROKER_NOTE="the broker's ServiceAccount $BROKER_WORKLOAD is allowed as $BROKER_WI_MEMBER; its member by UID is not known ($BROKER_UID_UNREAD), so a member by UID that may be it is UNKNOWN"
  fi
fi
