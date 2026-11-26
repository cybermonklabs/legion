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
# attach enter any pod on the node, the guarded ones included. A binding is written by create,
# patch or update, and RBAC lets one who may bind its role, by name, update it, so each is asked;
# a Role or ClusterRole is written by patch or update, which adds a rule to a role someone already
# holds, so those are asked too (binding_rights asks the writes of a binding or a role in each
# other namespace that holds a RoleBinding, and live_named reads each rule that limits one of
# them by name from the cluster's roles). A Secret of type
# kubernetes.io/service-account-token naming a ServiceAccount is filled with its token, so create
# secrets is asked beside get, list and watch.
RIGHTS_TABLE='
create - serviceaccounts token named
impersonate - serviceaccounts - named
impersonate - users - cluster
impersonate - groups - cluster
escalate rbac.authorization.k8s.io roles - ns
bind rbac.authorization.k8s.io roles - ns
escalate rbac.authorization.k8s.io clusterroles - cluster
bind rbac.authorization.k8s.io clusterroles - ns
patch rbac.authorization.k8s.io roles - ns
update rbac.authorization.k8s.io roles - ns
patch rbac.authorization.k8s.io clusterroles - cluster
update rbac.authorization.k8s.io clusterroles - cluster
create rbac.authorization.k8s.io rolebindings - ns
patch rbac.authorization.k8s.io rolebindings - ns
update rbac.authorization.k8s.io rolebindings - ns
create rbac.authorization.k8s.io clusterrolebindings - cluster
patch rbac.authorization.k8s.io clusterrolebindings - cluster
update rbac.authorization.k8s.io clusterrolebindings - cluster
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
create - secrets - ns
patch - secrets - ns
'
# The admission rights: create, update and patch of each resource of admissionregistration.k8s.io
# a cluster's discovery lists, cluster-wide, on every cluster of the pool (admission_rights): a
# mutating webhook or admission policy can add a container to the operator's next pod, and with it
# the operator's identity, and a validating one can block or reshape what the operator applies.
# Taking the resources from discovery asks the admission policies where the server serves them, and
# never a name it does not; every API server serves the two webhook configurations, so a discovery
# that lists either one short is not whole.
ADMISSION_ALWAYS=(mutatingwebhookconfigurations validatingwebhookconfigurations)
# ADMISSION_CODE_RIGHTS: the admission rights the code step reads, with no cluster's discovery to
# read: create, update and patch of the two webhook configurations and of the admission policies
# and their bindings, mutating and validating.
ADMISSION_CODE_RIGHTS="$(jq -n -c '[("mutatingwebhookconfigurations", "validatingwebhookconfigurations", "mutatingadmissionpolicies",
    "mutatingadmissionpolicybindings", "validatingadmissionpolicies", "validatingadmissionpolicybindings") as $res
  | ("create", "update", "patch")
  | {verb: ., group: "admissionregistration.k8s.io", resource: $res, sub: "", ns: "", name: "",
     label: "\(.) \($res).admissionregistration.k8s.io cluster-wide"}
  | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}]')"
# The certificate signing chain, asked cluster-wide on every cluster of the pool: a certificate
# signed for kubernetes.io/kube-apiserver-client authenticates as any user and groups its request
# names but system:masters, one for kubernetes.io/kube-apiserver-client-kubelet as a node, whose
# Node authorizer lets it mint the token of a pod's ServiceAccount on it, the operator's included,
# and kubernetes.io/legacy-unknown signs what it is asked; GKE cannot turn the certificates API
# off. Whoever may write a request's approval (update or patch, either one) and approve one of those
# signers can become anyone, so the approval and the signers are asked by name (and approve unnamed,
# which a rule with no names allows), and csr_chains reports a principal allowed both as one finding,
# naming create too when it may make its own request; create alone signs nothing.
CSR_CREATE="create certificatesigningrequests.certificates.k8s.io cluster-wide"
CSR_SIGNER_PREFIX="approve signers.certificates.k8s.io "
CSR_ANY_SIGNER="${CSR_SIGNER_PREFIX}of any name cluster-wide"
CSR_RIGHTS="$(jq -n -c --arg create "$CSR_CREATE" --arg signer "$CSR_SIGNER_PREFIX" --arg any "$CSR_ANY_SIGNER" '
  [{verb: "create", group: "certificates.k8s.io", resource: "certificatesigningrequests", sub: "", ns: "", name: "", label: $create},
   (("update", "patch") | {verb: ., group: "certificates.k8s.io", resource: "certificatesigningrequests", sub: "approval", ns: "", name: "",
     label: "\(.) certificatesigningrequests/approval.certificates.k8s.io cluster-wide"}),
   ("kubernetes.io/kube-apiserver-client", "kubernetes.io/kube-apiserver-client-kubelet", "kubernetes.io/legacy-unknown", "kubernetes.io/*"
    | {verb: "approve", group: "certificates.k8s.io", resource: "signers", sub: "", ns: "", name: ., label: "\($signer)\(.) cluster-wide"}),
   {verb: "approve", group: "certificates.k8s.io", resource: "signers", sub: "", ns: "", name: "", label: $any}]
  | map(. + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))})')"
# NODE_CLIENT_RIGHTS: on a fleet member that is no GKE cluster, whose kube-controller-manager
# approves by itself a kubelet's client certificate request from a holder of create on
# certificatesigningrequests/nodeclient or selfnodeclient, each is asked too.
NODE_CLIENT_RIGHTS="$(jq -n -c '[("nodeclient", "selfnodeclient")
  | {verb: "create", group: "certificates.k8s.io", resource: "certificatesigningrequests", sub: ., ns: "", name: "",
     label: "create certificatesigningrequests/\(.).certificates.k8s.io cluster-wide, which kube-controller-manager approves by itself"}
  | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}]')"
# The container.* permission of each right: what GKE's IAM authorizer checks for the same call,
# with container.namespaces.create and container.serviceAccounts.create for the Workload Identity
# step's other clusters. watch secrets is container.secrets.list's, as list is, and patch is
# update's. roles/container.admin names no permission for the admission policies or their
# bindings (read with `gcloud iam roles describe` on 2026-11-21), so only RBAC is asked those.
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
  container.mutatingWebhookConfigurations.create container.mutatingWebhookConfigurations.update
  container.validatingWebhookConfigurations.create container.validatingWebhookConfigurations.update
  container.certificateSigningRequests.approve container.clusters.update container.secrets.create
  container.secrets.update container.namespaces.update container.serviceAccounts.update
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
# What a fleet member's own project's IAM is asked: GKE authorizes IAM beside RBAC on that member, so
# the container.* permission of each guard right (a guarded namespace, ServiceAccount or pod, the
# escalation rights by which a holder grants itself one, and the admission webhooks) and the IAM
# writers there.
MEMBER_PERMISSIONS=(container.namespaces.create container.namespaces.update container.serviceAccounts.create
  container.serviceAccounts.update container.pods.create container.pods.update
  container.clusters.impersonate container.clusterRoles.bind container.clusterRoles.escalate
  container.clusterRoleBindings.create container.clusterRoleBindings.update container.roles.bind
  container.roles.escalate container.roleBindings.create container.roleBindings.update container.nodes.proxy
  container.mutatingWebhookConfigurations.create container.mutatingWebhookConfigurations.update
  container.validatingWebhookConfigurations.create container.validatingWebhookConfigurations.update
  container.certificateSigningRequests.approve container.clusters.update
  "${IAM_WRITE_PERMISSIONS[@]}")
SQL_PERMISSIONS=(cloudsql.users.create cloudsql.users.update)
# Who can make or change a Deployment Manager deployment on --project: Deployment Manager creates a
# deployment's resources with the credentials of the project's Google APIs service agent
# (<number>@cloudservices.gserviceaccount.com), not the caller's, and that agent holds
# roles/editor on the project unless someone took it away, so either permission acts as roles/editor
# there (names read from roles/deploymentmanager.editor's includedPermissions on 2026-11-23).
DEPLOYMENT_MANAGER_PERMISSIONS=(deploymentmanager.deployments.create deploymentmanager.deployments.update)
# Who can register a cluster to the pool project's fleet: a registered cluster shares the pool's
# Workload Identity, so its ServiceAccount envoy-operator/envoy-operator is the operator's member.
# Beside it, who can make a namespace or RBAC on the fleet's members: a fleet namespace and an RBAC
# role binding of a scope reach every member cluster bound to the scope, so the scopes' and their
# bindings' writes, and the features', are asked too (names read from roles/gkehub.admin's
# includedPermissions on 2026-11-21).
FLEET_PERMISSIONS=(gkehub.memberships.create gkehub.memberships.update gkehub.namespaces.create gkehub.namespaces.update
  gkehub.rbacrolebindings.create gkehub.rbacrolebindings.update gkehub.membershipbindings.create
  gkehub.membershipbindings.update gkehub.scopes.create gkehub.scopes.update gkehub.scopes.setIamPolicy
  gkehub.features.create gkehub.features.update)
# Who could have written a render: write or delete an object, rewrite its metadata (the commit and
# run id the report quotes), or change the bucket's policy or access model.
RENDER_PERMISSIONS=(storage.objects.create storage.objects.delete storage.objects.update storage.buckets.setIamPolicy
  storage.buckets.update)
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
    storage.objects.get) GRANTING_ROLE=roles/storage.objectViewer ;;
    storage.*) GRANTING_ROLE=roles/storage.admin ;;
    gkehub.*) GRANTING_ROLE=roles/gkehub.admin ;;
    deploymentmanager.*) GRANTING_ROLE=roles/deploymentmanager.editor ;;
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
# TABLE_JSON: the namespaced rows of RIGHTS_TABLE ({verb, group, resource, sub, named}), the ways in
# a namespace to become a ServiceAccount of it that follow_becomes asks; `named` rows name the
# ServiceAccount. A patch or update of a Role is left out: RBAC lets one who writes a role add only
# rules it already holds unless it may escalate (asked beside it), so a write of a role alone is no
# way to become anyone, and a chain through one would be a false finding.
TABLE_JSON="$(while read -r verb group resource sub scope; do
  [ -n "$verb" ] && [ "$scope" != cluster ] || continue
  case "$verb $resource" in "patch roles" | "update roles") continue ;; esac
  [ "$group" != - ] || group=""
  [ "$sub" != - ] || sub=""
  printf '%s\t%s\t%s\t%s\t%s\n' "$verb" "$group" "$resource" "$sub" "$scope"
done <<<"$RIGHTS_TABLE" | jq -R -s -c '[split("\n")[] | select(length > 0) | split("\t")
  | {verb: .[0], group: .[1], resource: .[2], sub: .[3], named: (.[4] == "named")}]')"
# GUARD_RIGHTS: the rights no principal the Workload Identity step reviews may hold on a cluster of
# the pool, as rights_json writes rights: create namespaces cluster-wide, and for each guarded
# pair's namespace, create serviceaccounts and create pods there. A pod in the namespace runs as
# any ServiceAccount of it, and the ServiceAccount is the pair the pool binds, so either lets its
# maker act as the pair's Google service account. Beside them, the escalation rights by which a
# holder grants itself any of those: impersonate users, groups and serviceaccounts, escalate and
# bind clusterroles, patch and update clusterroles, and create, patch and update
# clusterrolebindings, cluster-wide, and create and get nodes/proxy; bind and escalate roles,
# patch and update roles, and create, patch and update rolebindings in each guarded namespace
# (guard-rights.jq); and the certificate signing chain's rights (CSR_RIGHTS). The admission rights
# are each cluster's own (admission_rights), and the writes of a binding or a role in each other
# namespace that holds a RoleBinding are binding_rights'.
GUARD_RIGHTS="$(printf '%s\n' "${GUARDED[@]}" | jq -R -s -c -f "$LIB/guard-rights.jq" | jq -c --argjson csr "$CSR_RIGHTS" '. + $csr')"
GUARDED_TEXT="$(printf '%s, ' "${GUARDED[@]}")"
GUARDED_TEXT="${GUARDED_TEXT%, }"
