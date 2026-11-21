#!/usr/bin/env bash
# packages/envoy/infra/scripts/operator-token-minters.sh
#
# Who, other than a person, can come to hold the NATS auth service's operator identity.
#
# The broker's `operator` entry of BROKER_NATS_SERVICES names one Kubernetes ServiceAccount
# (envoy-operator/envoy-operator by default). NATS takes that account's token, and the account acts,
# through Workload Identity, as one Google service account, whose token reaches an agent only as the
# administrator-tagged broker secret ENVOY_OPERATOR, released when a person approves; the secrets
# broker mints that token, so the broker's own ServiceAccount (--broker-workload) is guarded as the
# operator's is. So holding the operator's power takes one of: minting a guarded ServiceAccount's
# token, running or entering a pod that runs as it, becoming a principal that can, minting the Google
# service account's token, making a namespace and ServiceAccount of the same names on another
# cluster of the project's workload pool, or registering a cluster to the pool's fleet. This check
# lists every principal that is not a person and holds such a right, and is the precondition checked
# before the operator entry is applied, and again before go-live.
#
# It runs with the gcloud, kubectl and agent-secrets sign-ins of the shell it runs in: an identity
# already held, never a person's administrator credential. It requests no credential, impersonates
# no one, and writes nothing but its own work directory and --report.
#
# The steps, in order; each prints PASS, a FINDING per principal and right, or UNKNOWN naming why:
#
#   permission names  Each permission name the IAM steps ask about is in the predefined role that
#                     grants it (`gcloud iam roles describe`): every container.* name in
#                     roles/container.admin's includedPermissions, the Google service account ones
#                     in roles/iam.serviceAccountTokenCreator, ...KeyAdmin and ...User, the hold
#                     writers' in roles/cloudsql.admin, roles/secretmanager.secretAccessor and
#                     roles/storage.objectViewer, and the fleet's in roles/gkehub.admin. A name the
#                     role does not hold would find no holder, which reads as clean, so it fails the
#                     run instead.
#   code: RBAC        What code declares, first: each --rbac-render file (the JSON render of the
#                     program that declares the cluster's Roles and bindings, a List, an array or a
#                     stream of objects) is read as the RBAC authorizer reads it, and each binding
#                     that gives a right below to a subject that is not a person, a system
#                     namespace's, a system component or the operator's own Google service account
#                     is a finding. A rule limited by resourceNames on impersonate or escalate is a
#                     finding naming the names (impersonating the group system:masters by name is
#                     cluster-admin). A binding to a role the render does not hold (other than the
#                     built-in cluster-admin, which gives every right, and view, which gives none)
#                     or to an aggregated ClusterRole is UNKNOWN: its rights are not in the render.
#   code: IAM         Each --iam-preview file (`pulumi preview --json --show-sames` of a program that
#                     declares IAM) is read for its organization, folder, project, service account,
#                     secret and bucket bindings, judged as the live IAM steps judge them, with each
#                     role's permissions from the preview's own custom roles or `gcloud iam roles
#                     describe`. A binding the preview does not know all of (a value computed at
#                     apply) and a resource type there that can grant on those resources but that
#                     this check does not read are UNKNOWN. A group the code names is judged by its
#                     members in the live steps, which expand it.
#   live: clusters    `gcloud container clusters list` of --project, which must hold --cluster. The
#                     operator's cluster is --cluster in --location, or, without --location, the
#                     list's one cluster of that name; a name the list holds more than once with no
#                     --location is UNKNOWN, and no cluster of that name is read as the operator's or
#                     as another. A list gcloud says is incomplete (a zone that did not respond) is
#                     UNKNOWN: a cluster there is neither read nor counted. With --pool-project
#                     naming another project, that project's own clusters are listed too, and read
#                     as other clusters of the pool; a refused or incomplete list is UNKNOWN.
#   live: RBAC        On the operator's cluster, one SubjectAccessReview per principal and right,
#                     the principals being every ServiceAccount outside the system namespaces
#                     (kube-system, gke-*, gmp-*), as on every other cluster of the pool, with the
#                     groups the API server gives it (system:serviceaccounts,
#                     system:serviceaccounts:<namespace>, system:authenticated), Kubernetes' own
#                     groups each alone (below), the Google service account each is bound to (its
#                     iam.gke.io/gcp-service-account annotation) with system:authenticated, the
#                     cluster's binding subjects (below), and anyone unauthenticated
#                     (system:anonymous with system:unauthenticated), always. The
#                     rights are the list below for each guarded pair, and create on each custom
#                     resource the API server's discovery (`kubectl api-resources --namespaced
#                     --verbs=create`) lists, in each guarded pair's namespace: a custom resource's
#                     controller may make a pod from it, as an agent sandbox does; a discovery that
#                     cannot be read whole is UNKNOWN. RBAC allows a request against a rule with
#                     resourceNames only when the request names one, so the rights also name each
#                     name: impersonate groups for system:masters and every Group subject the
#                     bindings name, impersonate users for every User subject, and escalate, bind,
#                     update and patch on each role bound to a reviewed principal
#                     (named-rights.jq); every other cluster of the pool is asked them too. A
#                     review without the groups would miss every grant made to a group. Its witness
#                     is a review for the group the built-in
#                     cluster-admin binding names, system:masters, which must be allowed: an API
#                     server whose answers cannot say yes proves nothing by saying no. Each
#                     --agent-namespace pattern that matches no namespace holding a ServiceAccount
#                     is UNKNOWN, so a mistyped one cannot drop its agents in silence. Then who can
#                     become a ServiceAccount with a finding is a finding naming the chain, followed
#                     in turn up to 5 steps: in its namespace, by any namespaced right of the list
#                     below (a pod or a workload controller runs as it; exec, attach, an ephemeral
#                     container or a pod patch enters one; a rolebinding, bind or escalate grants
#                     those; a token Secret is read) or create on a custom resource there, and by
#                     its name, create serviceaccounts/token or impersonate. A system namespace's
#                     ServiceAccount is reviewed too, never a finding itself, but when it is allowed
#                     a right asked, who can become it is followed the same way.
#   live: GKE IAM     GKE authorizes a Google identity's Kubernetes call through IAM beside RBAC, so
#                     Policy Analyzer's holders of the container.* permission of each right, on the
#                     project, and of resourcemanager.projects.setIamPolicy and iam.roles.update
#                     there, by which a holder grants itself any of them.
#   live: minters     Policy Analyzer's holders, on --gsa or above it, of
#                     iam.serviceAccounts.getAccessToken, getOpenIdToken, signJwt, signBlob, actAs,
#                     implicitDelegation, iam.serviceAccountKeys.create and setIamPolicy. They must
#                     be people and the Workload Identity member serviceAccount:<pool>.svc.id.goog[
#                     <namespace>/<service account>], which is the witness: the answer must list it.
#                     The broker needs roles/iam.serviceAccountTokenCreator on that account to mint
#                     the secret ENVOY_OPERATOR's token, so --broker-account allows that one binding,
#                     on that account and in that role alone, and then the broker account's own
#                     minters are checked the same way, its Workload Identity member
#                     (--broker-workload) the witness and the one non-person allowed.
#   impersonation     Every IAM step analyses with --analyze-service-account-impersonation: who can
#                     mint a holder's token, sign as it or act as it holds what the holder holds, so
#                     the outermost principal of each such chain, through any number of service
#                     accounts, is judged as a holder, its finding written as the chain
#                     (serviceAccount:a -> impersonates serviceAccount:b (<role>) -> <b's role>).
#                     The design's own links are no finding: a Workload Identity member acting as
#                     the account it is bound to, and the broker minting --gsa's token.
#   live: Workload    The Workload Identity member names the workload pool (<pool>.svc.id.goog,
#     Identity        --pool-project, by default --project), the namespace and the ServiceAccount,
#                     never the cluster, so every cluster that uses the pool can act as the Google
#                     service account a pair is bound to. The guarded pairs are the operator's
#                     (--namespace/--service-account), the broker's (--broker-workload) and each
#                     --guarded <namespace>/<service account>, such as a route password reader
#                     reached the same way. On every cluster of the pool (the operator's, every other
#                     cluster of the project, and every member cluster of the pool project's fleet)
#                     no principal the step reviews may create a namespace cluster-wide, or a
#                     ServiceAccount or a pod in a guarded pair's namespace, nor hold an escalation
#                     right by which it grants itself one: impersonate users, groups and
#                     serviceaccounts, escalate and bind clusterroles, and create and patch
#                     clusterrolebindings, cluster-wide; bind and escalate roles and create and
#                     patch rolebindings in a guarded pair's namespace. Each finding's right names
#                     the pairs it guards. On
#                     every cluster but the operator's, a guarded pair's namespace must not exist
#                     either, and the principals are that cluster's ServiceAccounts outside its
#                     system namespaces (kube-system, gke-*, gmp-*), with their groups, and its
#                     binding subjects (below); on the operator's cluster they are the RBAC step's.
#                     A project with the operator's cluster alone says so. The fleet's memberships
#                     are read from the GKE Hub API (projects/<pool>/locations/-/memberships, every
#                     page) with the running identity's access token, not through `gcloud container
#                     fleet memberships list`, which drops the answer's `unreachable` locations: a
#                     listing that could not reach a location reads complete there. A refused
#                     listing, and one with an unreachable location, is UNKNOWN; a fleet the API
#                     answers with no membership says so. A member that is a GKE cluster is read
#                     through `gcloud container clusters get-credentials` in its own project, any
#                     other through `gcloud container fleet memberships get-credentials` (the
#                     Connect gateway); the operator's cluster, and a cluster the project step read,
#                     are not read twice. GKE authorizes IAM beside RBAC, so for each GKE member in
#                     a project other than --project, Policy Analyzer's holders on that project of
#                     each guard right's container.* permission (container.namespaces.create,
#                     serviceAccounts.create and pods.create; clusters.impersonate, clusterRoles.bind
#                     and escalate, clusterRoleBindings.create and update, roles.bind and escalate,
#                     roleBindings.create and update, nodes.proxy) and of the IAM writers
#                     (setIamPolicy, iam.roles.update) are judged as the GKE IAM step judges its own
#                     (--project's are in that step).
#   live: fleet       Policy Analyzer's holders of gkehub.memberships.create and update on the pool
#     registration    project, each judged as in the GKE IAM step: a cluster registered to the
#                     pool's fleet shares the pool's Workload Identity, so its ServiceAccount
#                     envoy-operator/envoy-operator is the operator's member there.
#   live: IAM on      For each folder and organization above --project, the pool project and each
#     ancestors       fleet member's project (`gcloud projects get-ancestors`), Policy Analyzer's
#                     holders of its setIamPolicy, and of the organization's iam.roles.update: each
#                     can grant itself any role below it. An answer that does not hold the project
#                     itself, as a real one always does, is UNKNOWN.
#   live: hold        Who can write the writer hold's tables beside the broker's routes, each holder a
#     writers         finding: the holders of cloudsql.users.create or update on --sql-instance, of
#                     get on the Secret --db-secret (a SubjectAccessReview for each principal of the
#                     RBAC step, and container.secrets.get through IAM), of storage.objects.get on the
#                     bucket of --broker-state, and of secretmanager.versions.access or setIamPolicy
#                     on --signing-secret, which the operator's Google service account alone may read
#                     and must be listed on.
#   admin tag         `agent-secrets secret show <--secret> --json`, under the running session, must
#                     show "admin": "true" and "tier": "human": the operator's credential is an
#                     administrator's, released only when a person approves and never decided by a
#                     model judge. No such secret is a finding too.
#
# Binding subjects. The identities a cluster's RBAC names are principals of that cluster too: GKE
# authenticates a Google account as a User of its email, so `grade-ci@<p>.iam.gserviceaccount.com`
# bound to cluster-admin on a member of the pool's fleet can make the operator's namespace and
# ServiceAccount there. On the operator's cluster and on each other cluster of the pool, every User
# subject of its ClusterRoleBindings and RoleBindings (`kubectl get clusterrolebindings,rolebindings
# -A`) that is not a person is reviewed with system:authenticated (system:anonymous with
# system:unauthenticated). A person is a User that is an email outside gserviceaccount.com; a Google
# service account, and a User that is no email (a bare name such as deployer-bot, a principal://
# name), are reviewed and can be findings. A person's User subject is never reviewed, so never a
# finding, as iam_verdict never makes a `user:` member one and the accepted list names no person.
# A ServiceAccount a binding names is reviewed whether it exists or not: whoever can make it and a
# pod in its namespace takes the binding's rights. On every cluster a ServiceAccount list that holds
# none outside the system namespaces is UNKNOWN, since every cluster has default/default. A refused
# list of bindings is UNKNOWN, naming the permission.
#
# Group and User subjects. Kubernetes' own groups take their members from Kubernetes, whatever
# Google Groups for RBAC says: system:authenticated, system:serviceaccounts and
# system:serviceaccounts:<namespace> (for each namespace of a reviewed ServiceAccount, and each one
# a binding names) are reviewed alone on every cluster, and a right one holds is a finding, since it
# reaches every member. system:masters takes its members from client certificates, which no review
# can enumerate, and every review of it is allowed by the bootstrap cluster-admin ClusterRoleBinding
# (matched by its name, its roleRef ClusterRole cluster-admin and its label
# kubernetes.io/bootstrapping=rbac-defaults): every other binding that names it is a finding, and so
# is a render's. Any other Group is judged by where the cluster takes its groups from: on a GKE
# cluster with Google Groups for RBAC off (authenticatorGroupsConfig, read from the cluster list or
# `gcloud container clusters describe`) the API server gives no request such a group, so it is no
# finding; with it on, the cluster is UNKNOWN, a Group subject bound or not, since a Google group's
# members are neither listed nor judged here; on a fleet member that is no GKE cluster, read through
# the Connect gateway, its groups come from its own authenticator, and a Group subject bound there is
# UNKNOWN; where whether it is on cannot be read, a Group subject bound there is UNKNOWN. The code
# step reads no cluster's setting and judges another Group as off; the live step of the cluster says
# UNKNOWN when it is on. A system: User or Group other than these is a Kubernetes component and
# allowed.
#
# In every IAM step a `user:` member is a person and allowed; a group the analysis expands is judged
# by its members, and one it does not expand is a finding; Google's own service agents
# (service-<number>@gcp-sa-*.iam.gserviceaccount.com, ...@container-engine-robot and
# ...@compute-system) are allowed; every other member is a finding. A Policy Analyzer answer that
# lists no holder, or that was not fully explored, is UNKNOWN: a resource always has holders (its
# project's owners at least), so an answer listing none saw nothing. The analysis runs over the
# project's top ancestor (`gcloud projects get-ancestors`), or --analysis-scope: a scope at the
# project does not see a grant made on its folder or its organization.
#
# The rights, in each guarded pair's namespace unless cluster-wide: create serviceaccounts/token of
# the pair's ServiceAccount; impersonate that ServiceAccount, and users and groups cluster-wide;
# escalate and bind roles and clusterroles, create and patch rolebindings and clusterrolebindings;
# create, and get (which authorizes an exec, attach or port forward over WebSocket on API servers
# before 1.35), pods/exec, pods/attach and pods/portforward; create, patch and update pods, and patch
# and update pods/ephemeralcontainers; create, patch and update replicationcontrollers, deployments,
# replicasets, statefulsets, daemonsets, jobs and cronjobs, each of which runs a pod as the
# ServiceAccount; create and get nodes/proxy cluster-wide, whose kubelet API enters any pod on the
# node; get, list and watch secrets.
#
# Exit 0: no finding and no UNKNOWN. Exit 1: at least one finding, each line naming the principal
# and the right (UNKNOWN lines may stand beside them). Exit 2: no finding, and at least one UNKNOWN,
# each naming its step and why, such as the permission the running identity lacks; also a usage
# error, a copy of the script missing a part or a jq file of operator-token-minters/ (checked before
# any is sourced), and a run that stops before its end (a command failing under set -e), each of
# which prints the UNKNOWN `run` and writes it to --report (a usage error writes none). --report is
# deleted when the run starts, so a report is
# this run's or none. --report <file> writes {"run_at": "<RFC 3339>", "findings": [{"principal",
# "right", "role", "resource", "permissions", "granted_at"}], "unknown": [{"step", "reason"}]} beside
# the text, run_at being when the run began. `right` is the sentence the text prints; the rest are
# its parts, for a reader that matches a finding against an accepted entry by principal and role:
#   role         the IAM role (roles/container.admin), for an impersonation chain the role of its
#                outermost link; for an RBAC finding the right's label (create
#                serviceaccounts/token envoy-operator in envoy-operator); for the namespace and the
#                secret's tags, what is wrong (namespace envoy-operator, admin, tier, secret);
#   resource     what the right is on: the analysed resource's full name, `cluster:<name>` (the
#                operator's cluster; `<project>/<location>/<name>` for every other) or
#                `membership:<pool>/<location>/<id>`, or `secret:<name>`;
#   permissions  the IAM permissions the role grants among those asked, for a chain the holder's;
#                for an RBAC finding the right's label; empty for the namespace and the secret's tags;
#   granted_at   the resource the IAM binding is made on, for a chain its outermost link's; for an
#                RBAC finding the API server's reason (`RBAC: allowed by ClusterRoleBinding "x" of
#                ...`) or, from code, the binding; empty where there is none.
#
# The fixture testdata/operator-token-minters-report.json, beside this script, is this script's own
# --report from a world of operator-token-minters.test.sh's, with run_at fixed: an IAM finding, RBAC
# findings of a Google service account bound on a fleet member for each of three guarded pairs (the
# operator's, the broker's and a --guarded one), and an
# UNKNOWN. The writer hold's preflight, which reads --report, tests its reader against it, and the
# test fails when the file differs from a fresh run (OPERATOR_MINTERS_WRITE_FIXTURE=1 rewrites it).
#
#   bash packages/envoy/infra/scripts/operator-token-minters.sh --project <id> --cluster <name> \
#     --gsa <email> --agent-namespace <ns|glob>... --broker-account <email> \
#     --broker-workload <ns>/<sa> --sql-instance <name> --db-secret <ns>/<name> \
#     --broker-state gs://<bucket>[/<path>] --signing-secret <name> \
#     --rbac-render <file>... --iam-preview <file>... [--guarded <ns>/<sa>]... [--report <file>]
#
# The controls run against scratch targets: --namespace envoy-operator-control --gsa
# envoy-operator-control@<project>.iam.gserviceaccount.com --secret ENVOY_OPERATOR_CONTROL.
# operator-token-minters.test.sh runs every plant and control against a fake kubectl, gcloud, curl
# and agent-secrets. This file holds the run's flags, its exit and its report; each step is a part
# in operator-token-minters/ beside it, sourced in the order above, and their large jq programs
# are .jq files there. Needs bash 4.4, jq, and for the live steps gcloud, kubectl, curl and
# agent-secrets.
set -euo pipefail

usage() {
  cat <<'USAGE'
usage: operator-token-minters.sh [flags]
  --project ID               the project that runs the operator's cluster
  --cluster NAME             the operator's cluster, one of the project's
  --location LOCATION        the operator's cluster's location (a region or a zone); needed when the
                             project holds more than one cluster named --cluster
  --pool-project ID          the project whose workload pool (ID.svc.id.goog) the operator's and
                             the broker's ServiceAccounts are in, and whose fleet's member clusters
                             share it (default --project)
  --namespace NS             the operator's namespace (default envoy-operator)
  --service-account NAME     the operator's Kubernetes ServiceAccount (default envoy-operator)
  --guarded NS/NAME          one more namespace and ServiceAccount the pool's Workload Identity binds
                             to a Google service account, guarded as the operator's pair is, which is
                             always guarded, as the broker's (--broker-workload) is when it is named;
                             repeatable
  --gsa EMAIL                the Google service account the operator's ServiceAccount acts as
  --agent-namespace NS|GLOB  a namespace whose ServiceAccounts agents run as; repeatable. Every
                             ServiceAccount outside the system namespaces is reviewed; these name
                             the groups reviewed alone, and one must hold a ServiceAccount
  --broker-account EMAIL     the secrets broker's Google service account, which mints the
                             operator's token for the secret ENVOY_OPERATOR
  --broker-workload NS/NAME  the broker's Kubernetes ServiceAccount, bound to --broker-account
  --sql-instance NAME        the Cloud SQL instance of the broker's database, in --project
  --db-secret NS/NAME        the Secret holding the broker's database login
  --broker-state gs://BUCKET[/PATH]  where the broker program's Pulumi state is kept
  --signing-secret NAME      the Secret Manager secret of the writer hold's signing key
  --secret NAME              the broker secret of the operator's credential (default ENVOY_OPERATOR)
  --rbac-render FILE         a JSON render of the cluster's RBAC program; repeatable
  --iam-preview FILE         `pulumi preview --json --show-sames` of an IAM program; repeatable
  --analysis-scope organizations/N|folders/N|projects/ID
                             Policy Analyzer's scope (default: the project's top ancestor)
  --credentials-arg ARG      passed to each `gcloud container clusters get-credentials`; repeatable
  --report FILE              also write the result as JSON
USAGE
}
usage_error() {
  echo "operator-token-minters: $1" >&2
  usage >&2
  exit 2
}

# A run's report is this run's or none: an older one would be read as this run's if the run stopped
# before writing its own, so it goes before anything, a usage error included, can stop the run.
for ((arg = 1; arg <= $#; arg++)); do
  case "${!arg}" in
    --report)
      next=$((arg + 1))
      [ "$next" -gt "$#" ] || rm -f -- "${!next}"
      ;;
    --report=*) report_arg="${!arg}" && rm -f -- "${report_arg#--report=}" ;;
  esac
done
PROJECT="" CLUSTER="" LOCATION="" POOL_PROJECT="" NAMESPACE=envoy-operator KSA=envoy-operator GSA="" BROKER="" BROKER_WORKLOAD=""
SQL_INSTANCE="" DB_SECRET="" BROKER_STATE="" SIGNING_SECRET="" SECRET=ENVOY_OPERATOR ANALYSIS_SCOPE="" REPORT=""
AGENT_NAMESPACES=() RBAC_RENDERS=() IAM_PREVIEWS=() CREDENTIALS_ARGS=() GUARDED_FLAGS=()
while [ "$#" -gt 0 ]; do
  flag="$1"
  shift
  case "$flag" in
    -h | --help) usage; exit 0 ;;
    --*=*) set -- "${flag#*=}" "$@"; flag="${flag%%=*}" ;;
  esac
  case "$flag" in
    --project | --cluster | --location | --pool-project | --namespace | --service-account | --gsa | --agent-namespace | --broker-account | \
      --broker-workload | --sql-instance | --db-secret | --broker-state | --signing-secret | --secret | \
      --rbac-render | --iam-preview | --analysis-scope | --credentials-arg | --report | --guarded)
      [ "$#" -gt 0 ] || usage_error "$flag needs a value"
      value="$1"
      shift
      ;;
    *) usage_error "unknown argument $flag" ;;
  esac
  case "$flag" in
    --project) PROJECT="$value" ;;
    --cluster) CLUSTER="$value" ;;
    --location) LOCATION="$value" ;;
    --pool-project) POOL_PROJECT="$value" ;;
    --namespace) NAMESPACE="$value" ;;
    --service-account) KSA="$value" ;;
    --gsa) GSA="$value" ;;
    --agent-namespace) AGENT_NAMESPACES+=("$value") ;;
    --broker-account) BROKER="$value" ;;
    --broker-workload) BROKER_WORKLOAD="$value" ;;
    --sql-instance) SQL_INSTANCE="$value" ;;
    --db-secret) DB_SECRET="$value" ;;
    --broker-state) BROKER_STATE="$value" ;;
    --signing-secret) SIGNING_SECRET="$value" ;;
    --secret) SECRET="$value" ;;
    --rbac-render) RBAC_RENDERS+=("$value") ;;
    --iam-preview) IAM_PREVIEWS+=("$value") ;;
    --analysis-scope) ANALYSIS_SCOPE="$value" ;;
    --credentials-arg) CREDENTIALS_ARGS+=("$value") ;;
    --report) REPORT="$value" ;;
    --guarded) GUARDED_FLAGS+=("$value") ;;
  esac
done
[ -n "$POOL_PROJECT" ] || POOL_PROJECT="$PROJECT"
# The guarded pairs: the operator's first, then the broker's (--broker-workload), then each
# --guarded, each once.
GUARDED=("$NAMESPACE/$KSA")
pair_name='^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-.a-z0-9]*[a-z0-9])?$'
if [ -n "$BROKER_WORKLOAD" ]; then
  [[ "$BROKER_WORKLOAD" =~ $pair_name ]] || usage_error "--broker-workload is <namespace>/<name>, each a Kubernetes name, not $BROKER_WORKLOAD"
  [ "$BROKER_WORKLOAD" = "${GUARDED[0]}" ] || GUARDED+=("$BROKER_WORKLOAD")
fi
for pair in ${GUARDED_FLAGS[@]+"${GUARDED_FLAGS[@]}"}; do
  [[ "$pair" =~ $pair_name ]] || usage_error "--guarded is <namespace>/<serviceaccount>, each a Kubernetes name, not $pair"
  case " ${GUARDED[*]} " in *" $pair "*) ;; *) GUARDED+=("$pair") ;; esac
done
case "$DB_SECRET" in "" | */*) ;; *) usage_error "--db-secret is <namespace>/<name>, not $DB_SECRET" ;; esac
case "$BROKER_STATE" in "" | gs://?*) ;; *) usage_error "--broker-state is gs://<bucket>[/<path>], not $BROKER_STATE" ;; esac
case "$ANALYSIS_SCOPE" in "" | organizations/?* | folders/?* | projects/?*) ;; *) usage_error "--analysis-scope is organizations/N, folders/N or projects/ID" ;; esac
command -v jq >/dev/null || usage_error "jq is not on PATH"

RUN_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/operator-token-minters.XXXXXX")"
# on_exit, the EXIT trap: a run that stops before its end (a command that failed under set -e)
# reads as UNKNOWN, never as clean or as a finding. crash_report, below, prints the UNKNOWN naming
# that, writes --report with it, and the run exits 2. The work directory goes either way.
FINISHED=false
on_exit() {
  local status=$?
  set +e
  if [ "$FINISHED" != true ] && declare -F crash_report >/dev/null; then
    crash_report "$status"
    status=2
  fi
  rm -rf "$WORK"
  exit "$status"
}
trap on_exit EXIT
mkdir -p "$WORK/kube" "$WORK/roles"

# The steps run in the header's order, each part in its own file of operator-token-minters/ beside
# this script, sourced here so that all of them share this run's state; a part's large jq programs
# are .jq files there, read with jq -f.
LIB="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/operator-token-minters"
# Every part and jq file must be readable before any is sourced: a missing one would stop the run
# before output.sh defines its report, or leave a step that needs it unread.
missing=""
for part in output.sh rights.sh principals.sh checks.sh step-permission-names.sh step-code-rbac.sh step-code-iam.sh \
  step-clusters.sh reviews.sh step-rbac.sh iam-analysis.sh step-gke-iam.sh step-workload-identity.sh step-live-iam.sh \
  step-admin-tag.sh analysis.jq become-rights.jq code-iam.jq code-rbac.jq custom.jq custom-rights.jq fleet.jq \
  guard-rights.jq named-rights.jq rec.jq review-answer.jq review-request.jq; do
  [ -r "$LIB/$part" ] || missing="${missing:+$missing, }$part"
done
if [ -n "$missing" ]; then
  reason="the parts $missing of $LIB cannot be read, so no step runs"
  echo "UNKNOWN run: $reason"
  [ -z "$REPORT" ] ||
    jq -n --arg run_at "$RUN_AT" --arg reason "$reason" '{run_at: $run_at, findings: [], unknown: [{step: "run", reason: $reason}]}' >"$REPORT"
  echo "operator-token-minters: no step ran, 1 unknown: exit 2"
  FINISHED=true
  exit 2
fi
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/output.sh
. "$LIB/output.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/rights.sh
. "$LIB/rights.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/principals.sh
. "$LIB/principals.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/checks.sh
. "$LIB/checks.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-permission-names.sh
. "$LIB/step-permission-names.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-code-rbac.sh
. "$LIB/step-code-rbac.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-code-iam.sh
. "$LIB/step-code-iam.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-clusters.sh
. "$LIB/step-clusters.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/reviews.sh
. "$LIB/reviews.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-rbac.sh
. "$LIB/step-rbac.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/iam-analysis.sh
. "$LIB/iam-analysis.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-gke-iam.sh
. "$LIB/step-gke-iam.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-workload-identity.sh
. "$LIB/step-workload-identity.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-live-iam.sh
. "$LIB/step-live-iam.sh"
# shellcheck source=packages/envoy/infra/scripts/operator-token-minters/step-admin-tag.sh
. "$LIB/step-admin-tag.sh"

# --- The result. ---
findings="${#FINDINGS[@]}"
unknowns="${#UNKNOWNS[@]}"
write_report
FINISHED=true
if [ "$findings" -gt 0 ]; then
  echo "operator-token-minters: $findings finding(s), $unknowns unknown: exit 1"
  exit 1
fi
if [ "$unknowns" -gt 0 ]; then
  echo "operator-token-minters: no finding, $unknowns unknown: exit 2"
  exit 2
fi
echo "operator-token-minters: no finding and nothing unknown: exit 0"
