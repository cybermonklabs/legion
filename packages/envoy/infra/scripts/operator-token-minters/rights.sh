# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/rights.sh: the rights and the permissions every step asks about. Sourced by
# operator-token-minters.sh, whose header says what each step checks.
# --- The rights and the permissions. ---

# Each right: verb, API group, resource, subresource, and scope: `ns` for a guarded pair's
# namespace, `cluster` for cluster-wide, `named` for the pair's namespace with its ServiceAccount as
# the resource name ("-" is empty). Every guarded pair (the operator's, the broker's, each
# --guarded) gets each right on the operator's cluster. A namespaced right is asked in the
# namespace, so a ClusterRoleBinding that grants it is found too; escalate on clusterroles, the
# bindings of clusterroles, impersonating users and groups and a node's proxy are cluster-scoped,
# which only a ClusterRoleBinding grants. A node's proxy reaches the kubelet's API, whose exec and
# attach enter any pod on the node, the guarded ones included.
RIGHTS_TABLE='
create - serviceaccounts token named
impersonate - serviceaccounts - named
impersonate - users - cluster
impersonate - groups - cluster
escalate rbac.authorization.k8s.io roles - ns
bind rbac.authorization.k8s.io roles - ns
escalate rbac.authorization.k8s.io clusterroles - cluster
bind rbac.authorization.k8s.io clusterroles - ns
create rbac.authorization.k8s.io rolebindings - ns
patch rbac.authorization.k8s.io rolebindings - ns
create rbac.authorization.k8s.io clusterrolebindings - cluster
patch rbac.authorization.k8s.io clusterrolebindings - cluster
create - pods exec ns
create - pods attach ns
create - pods portforward ns
get - pods exec ns
get - pods attach ns
get - pods portforward ns
create - pods - ns
patch - pods - ns
update - pods - ns
create - replicationcontrollers - ns
patch - replicationcontrollers - ns
update - replicationcontrollers - ns
create - nodes proxy cluster
get - nodes proxy cluster
patch - pods ephemeralcontainers ns
update - pods ephemeralcontainers ns
create apps deployments - ns
patch apps deployments - ns
update apps deployments - ns
create apps replicasets - ns
patch apps replicasets - ns
update apps replicasets - ns
create apps statefulsets - ns
patch apps statefulsets - ns
update apps statefulsets - ns
create apps daemonsets - ns
patch apps daemonsets - ns
update apps daemonsets - ns
create batch jobs - ns
patch batch jobs - ns
update batch jobs - ns
create batch cronjobs - ns
patch batch cronjobs - ns
update batch cronjobs - ns
get - secrets - ns
list - secrets - ns
watch - secrets - ns
'
# The container.* permission of each right: what GKE's IAM authorizer checks for the same call,
# with container.namespaces.create and container.serviceAccounts.create for the Workload Identity
# step's other clusters. watch secrets is container.secrets.list's, as list is.
CONTAINER_PERMISSIONS=(
  container.serviceAccounts.createToken container.clusters.impersonate
  container.roles.escalate container.roles.bind container.clusterRoles.escalate container.clusterRoles.bind
  container.roleBindings.create container.roleBindings.update container.clusterRoleBindings.create
  container.clusterRoleBindings.update container.pods.exec container.pods.attach container.pods.portForward
  container.pods.create container.pods.update container.replicationControllers.create
  container.replicationControllers.update container.nodes.proxy container.deployments.create container.deployments.update
  container.replicaSets.create container.replicaSets.update container.statefulSets.create
  container.statefulSets.update container.daemonSets.create container.daemonSets.update container.jobs.create
  container.jobs.update container.cronJobs.create container.cronJobs.update container.secrets.get
  container.secrets.list container.namespaces.create container.serviceAccounts.create
)
GSA_PERMISSIONS=(
  iam.serviceAccounts.getAccessToken iam.serviceAccounts.getOpenIdToken iam.serviceAccounts.signJwt
  iam.serviceAccounts.signBlob iam.serviceAccounts.actAs iam.serviceAccounts.implicitDelegation
  iam.serviceAccountKeys.create
)
# Who can write IAM policy over the checked resources, or change a custom role a principal holds:
# each can grant itself any permission below, so each is a holder of every one.
IAM_WRITE_PERMISSIONS=(resourcemanager.projects.setIamPolicy resourcemanager.folders.setIamPolicy
  resourcemanager.organizations.setIamPolicy iam.serviceAccounts.setIamPolicy secretmanager.secrets.setIamPolicy
  iam.roles.update)
SQL_PERMISSIONS=(cloudsql.users.create cloudsql.users.update)
# Who can register a cluster to the pool project's fleet: a registered cluster shares the pool's
# Workload Identity, so its ServiceAccount envoy-operator/envoy-operator is the operator's member.
FLEET_PERMISSIONS=(gkehub.memberships.create gkehub.memberships.update)
# granting_role sets GRANTING_ROLE to the predefined role whose includedPermissions must hold a
# checked permission.
granting_role() {
  case "$1" in
    container.*) GRANTING_ROLE=roles/container.admin ;;
    resourcemanager.projects.setIamPolicy) GRANTING_ROLE=roles/resourcemanager.projectIamAdmin ;;
    resourcemanager.folders.setIamPolicy) GRANTING_ROLE=roles/resourcemanager.folderIamAdmin ;;
    resourcemanager.organizations.setIamPolicy) GRANTING_ROLE=roles/resourcemanager.organizationAdmin ;;
    iam.serviceAccounts.setIamPolicy) GRANTING_ROLE=roles/iam.serviceAccountAdmin ;;
    secretmanager.secrets.setIamPolicy) GRANTING_ROLE=roles/secretmanager.admin ;;
    iam.roles.update) GRANTING_ROLE=roles/iam.roleAdmin ;;
    iam.serviceAccounts.actAs) GRANTING_ROLE=roles/iam.serviceAccountUser ;;
    iam.serviceAccountKeys.*) GRANTING_ROLE=roles/iam.serviceAccountKeyAdmin ;;
    iam.serviceAccounts.*) GRANTING_ROLE=roles/iam.serviceAccountTokenCreator ;;
    cloudsql.*) GRANTING_ROLE=roles/cloudsql.admin ;;
    secretmanager.*) GRANTING_ROLE=roles/secretmanager.secretAccessor ;;
    storage.*) GRANTING_ROLE=roles/storage.objectViewer ;;
    gkehub.*) GRANTING_ROLE=roles/gkehub.admin ;;
  esac
}

# rights_json [db]: the rights as a JSON array of {verb, group, resource, sub, ns, name, key,
# label}, each right of the table for each guarded pair, each once, with `db` the hold writers' get
# on --db-secret too. key is how a review's answer, which echoes its attributes, finds its right.
rights_json() {
  local verb group resource sub scope ns name pair
  {
    for pair in "${GUARDED[@]}"; do
      while read -r verb group resource sub scope; do
        [ -n "$verb" ] || continue
        [ "$group" != - ] || group=""
        [ "$sub" != - ] || sub=""
        ns="${pair%%/*}" name=""
        case "$scope" in cluster) ns="" ;; named) name="${pair#*/}" ;; esac
        printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$verb" "$group" "$resource" "$sub" "$ns" "$name"
      done <<<"$RIGHTS_TABLE"
    done
    if [ "${1:-}" = db ] && [ -n "$DB_SECRET" ]; then
      printf 'get\t\tsecrets\t\t%s\t%s\n' "${DB_SECRET%%/*}" "${DB_SECRET#*/}"
    fi
  } | jq -R -s -c '[split("\n")[] | select(length > 0) | split("\t")
      | {verb: .[0], group: .[1], resource: .[2], sub: .[3], ns: .[4], name: .[5]}
      | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|")),
             label: ("\(.verb) \(.resource)\(if .sub != "" then "/" + .sub else "" end)\(if .group != "" then "." + .group else "" end)"
                     + (if .name != "" then " " + .name else "" end) + (if .ns != "" then " in " + .ns else " cluster-wide" end))}]
    | reduce .[] as $r ([]; if any(.[]; .key == $r.key) then . else . + [$r] end)'
}
# GUARD_RIGHTS: the rights no principal the Workload Identity step reviews may hold on a cluster of
# the pool, as rights_json writes rights: create namespaces cluster-wide, and for each guarded
# pair's namespace, create serviceaccounts and create pods there. A pod in the namespace runs as
# any ServiceAccount of it, and the ServiceAccount is the pair the pool binds, so either lets its
# maker act as the pair's Google service account. Each label names the pairs the namespace holds.
GUARD_RIGHTS="$(printf '%s\n' "${GUARDED[@]}" | jq -R -s -c -f "$LIB/guard-rights.jq")"
GUARDED_TEXT="$(printf '%s, ' "${GUARDED[@]}")"
GUARDED_TEXT="${GUARDED_TEXT%, }"
