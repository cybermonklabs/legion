# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-rbac.sh: the rows of operator-token-minters.test.sh on
# Kubernetes RBAC: each right of the list, who can become a holder, rights limited by name, custom
# resources, and the ServiceAccounts, Users and Groups a binding names, on any cluster of the pool.
# Sourced by the test after its world and helpers; its header says what the fakes model. The rows:
#
#   (b)  each right of the RBAC list, allowed to one agent ServiceAccount alone, exits 1 naming that
#        ServiceAccount and that right, and nothing else;
#   (c)  a right allowed to the group system:serviceaccounts:legion exits 1 naming the group;
#   (u)  a binding subject on the operator's own cluster: grade-ci bound to create the operator's
#        token exits 1;
#   (ab) the broker's pair is guarded: an agent ServiceAccount that may create pods in the broker's
#        namespace, or create the broker ServiceAccount's token, and another cluster of the pool
#        holding the broker's namespace, each exit 1;
#   (ac) on the operator's cluster, ci/runner, outside the agent namespaces, with create pods in
#        envoy-operator, and with cluster-admin, each exits 1;
#   (ai) a ServiceAccount that can become ci/runner, which can create pods in envoy-operator, by
#        each of create pods, create serviceaccounts/token, impersonate, create pods/exec and get
#        secrets in ci, is a finding naming the chain;
#   (aj) the Group system:unauthenticated bound to create namespaces on another cluster is a
#        finding for anyone unauthenticated;
#   (ak) create on a custom resource (sandboxes.agents.x-k8s.io) in envoy-operator is a finding,
#        create on Kubernetes' own leases is not, and a discovery that cannot be read is UNKNOWN;
#   (am) an --agent-namespace pattern that matches nothing is UNKNOWN, naming it, beside two that
#        match;
#   (ao) rights limited by name: legion/worker may impersonate the group system:masters by name,
#        impersonate a named user bound to cluster-admin, or escalate a named clusterrole bound to
#        it; each exits 1 naming the name;
#   (aq) deploy/bot can become ci/runner, which can create pods in envoy-operator, by each of create
#        jobs (rv5), create deployments, create pods/attach, patch pods/ephemeralcontainers, patch
#        pods, create rolebindings, bind and escalate roles, and create a custom resource in ci; each
#        is a finding naming the chain;
#   (ar) a kube-system ServiceAccount that may mint the operator's token is no finding, but
#        legion/worker, which may create pods in kube-system, is one naming the chain (rv6); on
#        another cluster, ci/runner, which may create pods in kube-system, becomes its namespace
#        controller and is one too;
#   (aw) a User that is no email is reviewed: deployer-bot bound to cluster-admin on a non-GKE fleet
#        member, a principal:// User allowed to create pods in envoy-operator, and a render binding
#        deployer-bot to cluster-admin each exit 1 naming it;
#   (ax) Kubernetes' own groups are findings: system:authenticated or system:serviceaccounts:apps
#        allowed to create pods in envoy-operator on another cluster, system:serviceaccounts on the
#        operator's, a live binding of system:masters other than the bootstrap one, and a render
#        binding system:masters to cluster-admin each exit 1;
#   (ay) any other Group reads UNKNOWN where the cluster takes its groups from elsewhere: the Group
#        ci-deployers bound to cluster-admin on a non-GKE fleet member, a Group on another cluster
#        or a GKE fleet member with Google Groups for RBAC on, a GKE fleet member whose describe is
#        refused, and the operator's cluster with Google Groups for RBAC on and no Group bound;
#   (bb) a ServiceAccount a binding names that does not exist (ci/ghost, allowed to create pods in
#        envoy-operator) is reviewed and exits 1.

# --- (b) Each right of the RBAC list, allowed to one agent ServiceAccount. Each entry is the grant
# (verb, API group, resource, subresource, the binding's namespace or none for cluster-wide, the
# resource name or none) and the right the run must name. ---
rbac_rights=(
  "create||serviceaccounts|token|envoy-operator|envoy-operator=create serviceaccounts/token envoy-operator in envoy-operator"
  "impersonate||serviceaccounts||envoy-operator|envoy-operator=impersonate serviceaccounts envoy-operator in envoy-operator"
  "impersonate||users||||=impersonate users cluster-wide"
  "impersonate||groups||||=impersonate groups cluster-wide"
  "escalate|rbac.authorization.k8s.io|roles||envoy-operator|=escalate roles.rbac.authorization.k8s.io in envoy-operator"
  "bind|rbac.authorization.k8s.io|roles||envoy-operator|=bind roles.rbac.authorization.k8s.io in envoy-operator"
  "escalate|rbac.authorization.k8s.io|clusterroles||||=escalate clusterroles.rbac.authorization.k8s.io cluster-wide"
  "bind|rbac.authorization.k8s.io|clusterroles||envoy-operator|=bind clusterroles.rbac.authorization.k8s.io in envoy-operator"
  "create|rbac.authorization.k8s.io|rolebindings||envoy-operator|=create rolebindings.rbac.authorization.k8s.io in envoy-operator"
  "patch|rbac.authorization.k8s.io|rolebindings||envoy-operator|=patch rolebindings.rbac.authorization.k8s.io in envoy-operator"
  "create|rbac.authorization.k8s.io|clusterrolebindings||||=create clusterrolebindings.rbac.authorization.k8s.io cluster-wide"
  "patch|rbac.authorization.k8s.io|clusterrolebindings||||=patch clusterrolebindings.rbac.authorization.k8s.io cluster-wide"
  "create||pods|exec|envoy-operator|=create pods/exec in envoy-operator"
  "create||pods|attach|envoy-operator|=create pods/attach in envoy-operator"
  "create||pods|portforward|envoy-operator|=create pods/portforward in envoy-operator"
  "get||pods|exec|envoy-operator|=get pods/exec in envoy-operator"
  "get||pods|attach|envoy-operator|=get pods/attach in envoy-operator"
  "get||pods|portforward|envoy-operator|=get pods/portforward in envoy-operator"
  "create||pods||envoy-operator|=create pods in envoy-operator"
  "patch||pods||envoy-operator|=patch pods in envoy-operator"
  "update||pods||envoy-operator|=update pods in envoy-operator"
  "patch||pods|ephemeralcontainers|envoy-operator|=patch pods/ephemeralcontainers in envoy-operator"
  "update||pods|ephemeralcontainers|envoy-operator|=update pods/ephemeralcontainers in envoy-operator"
  "create|apps|deployments||envoy-operator|=create deployments.apps in envoy-operator"
  "patch|apps|deployments||envoy-operator|=patch deployments.apps in envoy-operator"
  "update|apps|deployments||envoy-operator|=update deployments.apps in envoy-operator"
  "create|apps|replicasets||envoy-operator|=create replicasets.apps in envoy-operator"
  "patch|apps|replicasets||envoy-operator|=patch replicasets.apps in envoy-operator"
  "update|apps|replicasets||envoy-operator|=update replicasets.apps in envoy-operator"
  "create|apps|statefulsets||envoy-operator|=create statefulsets.apps in envoy-operator"
  "patch|apps|statefulsets||envoy-operator|=patch statefulsets.apps in envoy-operator"
  "update|apps|statefulsets||envoy-operator|=update statefulsets.apps in envoy-operator"
  "create|apps|daemonsets||envoy-operator|=create daemonsets.apps in envoy-operator"
  "patch|apps|daemonsets||envoy-operator|=patch daemonsets.apps in envoy-operator"
  "update|apps|daemonsets||envoy-operator|=update daemonsets.apps in envoy-operator"
  "create|batch|jobs||envoy-operator|=create jobs.batch in envoy-operator"
  "patch|batch|jobs||envoy-operator|=patch jobs.batch in envoy-operator"
  "update|batch|jobs||envoy-operator|=update jobs.batch in envoy-operator"
  "create|batch|cronjobs||envoy-operator|=create cronjobs.batch in envoy-operator"
  "patch|batch|cronjobs||envoy-operator|=patch cronjobs.batch in envoy-operator"
  "update|batch|cronjobs||envoy-operator|=update cronjobs.batch in envoy-operator"
  "get||secrets||envoy-operator|=get secrets in envoy-operator"
  "list||secrets||envoy-operator|=list secrets in envoy-operator"
  "watch||secrets||envoy-operator|=watch secrets in envoy-operator"
  "create||replicationcontrollers||envoy-operator|=create replicationcontrollers in envoy-operator"
  "patch||replicationcontrollers||envoy-operator|=patch replicationcontrollers in envoy-operator"
  "update||replicationcontrollers||envoy-operator|=update replicationcontrollers in envoy-operator"
  "create||nodes|proxy|||=create nodes/proxy cluster-wide"
  "get||nodes|proxy|||=get nodes/proxy cluster-wide"
)
rbac_right_row() {
  local right="${1#*=}" verb group resource sub namespace name
  IFS='|' read -r verb group resource sub namespace name <<<"${1%%=*}"
  base_world
  subject_grant operator-cluster "$(sa_subject legion worker)" "$verb" "$group" "$resource" "$sub" "$namespace" "$name"
  run
  want_status 1 "$right allowed to legion/worker"
  want_line "FINDING system:serviceaccount:legion:worker: $right" "$right allowed to legion/worker"
  # An unnamed grant also allows each name the check asks by name (a bound group or user, a bound
  # role), so every finding must name legion/worker, not only one.
  [ "$(count_prefix 'FINDING system:serviceaccount:legion:worker: ')" -eq "$(count_prefix 'FINDING ')" ] ||
    fail "$right allowed to legion/worker: a finding names another principal"
}
if row b; then
  each rbac_right_row "${rbac_rights[@]}"
  passed "(b) each of the ${#rbac_rights[@]} rights of the RBAC list, allowed to one agent ServiceAccount alone, exits 1 naming it and the right"
fi

# --- (c) A right allowed to the group of every ServiceAccount of namespace legion. ---
if row c; then
  base_world
  subject_grant operator-cluster "$(group_subject system:serviceaccounts:legion)" create "" serviceaccounts token envoy-operator envoy-operator
  run
  want_status 1 "a grant to the group system:serviceaccounts:legion"
  want_line "FINDING group:system:serviceaccounts:legion: create serviceaccounts/token envoy-operator in envoy-operator" "a grant to the group"
  want_line "FINDING system:serviceaccount:legion:default: create serviceaccounts/token envoy-operator in envoy-operator" "a grant to the group"
  passed "(c) a right allowed to the group system:serviceaccounts:legion exits 1 naming the group and each of its ServiceAccounts"
fi

# --- (u) The binding subjects of the operator's own cluster. ---
if row u; then
  base_world
  subject_grant operator-cluster "$(user_subject "$grade_ci")" create "" serviceaccounts token envoy-operator envoy-operator
  run
  want_status 1 "grade-ci bound to create the operator's token on its cluster"
  want_line "FINDING serviceAccount:$grade_ci: create serviceaccounts/token envoy-operator in envoy-operator" \
    "grade-ci bound to create the operator's token on its cluster"
  passed "(u) a Google service account bound on the operator's own cluster to create its token exits 1 naming it"
fi

# --- (ab) The broker's pair, guarded as the operator's. ---
if row ab; then
  base_world
  subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods "" secrets-broker ""
  run
  want_status 1 "an agent ServiceAccount that may create pods in secrets-broker"
  want_line "FINDING system:serviceaccount:legion:worker: create pods in secrets-broker" "an agent ServiceAccount that may create pods in secrets-broker"
  passed "(ab) an agent ServiceAccount that may create pods in the broker's namespace exits 1"

  base_world
  add_cluster other-cluster us-east1
  echo secrets-broker >>"$world/clusters/other-cluster/namespaces"
  run
  want_status 1 "another cluster of the pool holding secrets-broker"
  want_line "FINDING cluster:$project/us-east1/other-cluster: namespace secrets-broker exists there, so its ServiceAccount secrets-broker/secrets-broker acts as the broker account $broker through the workload pool $project.svc.id.goog" \
    "another cluster of the pool holding secrets-broker"
  passed "(ab) another cluster of the pool holding the broker's namespace exits 1"

  base_world
  subject_grant operator-cluster "$(sa_subject legion worker)" create "" serviceaccounts token secrets-broker secrets-broker
  run
  want_status 1 "an agent ServiceAccount that may create the broker's token"
  want_line "FINDING system:serviceaccount:legion:worker: create serviceaccounts/token secrets-broker in secrets-broker" \
    "an agent ServiceAccount that may create the broker's token"
  passed "(ab) an agent ServiceAccount that may create the broker ServiceAccount's token exits 1"
fi

# --- (ac) Every ServiceAccount of the operator's cluster, not only the agents'. ---
if row ac; then
  base_world
  add_sa operator-cluster ci runner
  subject_grant operator-cluster "$(sa_subject ci runner)" create "" pods "" envoy-operator ""
  run
  want_status 1 "ci/runner with create pods in envoy-operator"
  want_line "FINDING system:serviceaccount:ci:runner: create pods in envoy-operator" "ci/runner with create pods in envoy-operator"
  passed "(ac) ci/runner, outside the agent namespaces, with create pods in envoy-operator exits 1"

  base_world
  add_sa operator-cluster ci runner
  cluster_admin operator-cluster "$(sa_subject ci runner)"
  run
  want_status 1 "ci/runner with cluster-admin"
  want_line "FINDING system:serviceaccount:ci:runner: create serviceaccounts/token envoy-operator in envoy-operator" "ci/runner with cluster-admin"
  passed "(ac) ci/runner, outside the agent namespaces, with cluster-admin exits 1"
fi

# --- (ai) Who can become a ServiceAccount with a finding holds what it holds. ---
become_row() {
  local verb resource sub name
  IFS='|' read -r verb resource sub name <<<"${1%%=*}"
  base_world
  add_sa operator-cluster ci runner
  add_sa operator-cluster deploy bot
  subject_grant operator-cluster "$(sa_subject ci runner)" create "" pods "" envoy-operator ""
  subject_grant operator-cluster "$(sa_subject deploy bot)" "$verb" "" "$resource" "$sub" ci "$name"
  run
  want_status 1 "deploy/bot with ${1#*=} in ci"
  want_line "FINDING system:serviceaccount:deploy:bot: ${1#*=} in ci, so it can become system:serviceaccount:ci:runner, which can create pods in envoy-operator" \
    "deploy/bot with ${1#*=} in ci"
}
if row ai; then
  each become_row "create|pods||=create pods" "create|serviceaccounts|token|runner=create serviceaccounts/token runner" \
    "impersonate|serviceaccounts||runner=impersonate serviceaccounts runner" "create|pods|exec|=create pods/exec" "get|secrets||=get secrets"
  passed "(ai) a ServiceAccount that can become one with a finding, by each of the five ways in its namespace, is a finding naming the chain"
fi

# --- (aj) Anyone unauthenticated, on every cluster. ---
if row aj; then
  base_world
  add_cluster other-cluster us-east1
  subject_grant other-cluster "$(group_subject system:unauthenticated)" create "" namespaces "" "" ""
  run
  want_status 1 "system:unauthenticated may create namespaces on another cluster"
  want_line "FINDING user:system:anonymous: create namespaces cluster-wide on cluster $project/us-east1/other-cluster" \
    "system:unauthenticated may create namespaces on another cluster"
  passed "(aj) the Group system:unauthenticated bound to create namespaces on another cluster is a finding"
fi

# --- (ak) Custom resources in a guarded namespace, from the API server's discovery. ---
if row ak; then
  base_world
  echo sandboxes.agents.x-k8s.io >"$world/clusters/operator-cluster/api-resources"
  subject_grant operator-cluster "$(sa_subject legion worker)" create agents.x-k8s.io sandboxes "" envoy-operator ""
  run
  want_status 1 "legion/worker with create on sandboxes in envoy-operator"
  want_line "FINDING system:serviceaccount:legion:worker: create sandboxes.agents.x-k8s.io in envoy-operator" \
    "legion/worker with create on sandboxes in envoy-operator"
  passed "(ak) create on a custom resource in a guarded namespace is a finding"

  base_world
  subject_grant operator-cluster "$(sa_subject legion worker)" create coordination.k8s.io leases "" envoy-operator ""
  run
  want_status 0 "legion/worker with create on Kubernetes' own leases in envoy-operator"
  passed "(ak) create on a resource of Kubernetes' own groups is no custom resource"

  base_world
  : >"$world/clusters/operator-cluster/refuse-api-resources"
  run
  want_status 2 "a discovery that cannot be read whole"
  want_text "UNKNOWN live: RBAC on operator-cluster: the API server's discovery of namespaced resources could not be read whole" \
    "a discovery that cannot be read whole"
  passed "(ak) a discovery that cannot be read whole reads UNKNOWN"
fi

# --- (am) Each --agent-namespace pattern matches something, or reads UNKNOWN. ---
if row am; then
  base_world
  mapfile -t args < <(args_without --agent-namespace)
  run_only "${args[@]}" --agent-namespace legion --agent-namespace agent-sessions --agent-namespace 'legion-*-session'
  want_status 2 "a mistyped --agent-namespace beside two that match"
  want_line "UNKNOWN live: RBAC on operator-cluster: --agent-namespace legion-*-session matches no namespace that holds a ServiceAccount, so the agents it was to name are not reviewed as agents" \
    "a mistyped --agent-namespace beside two that match"
  passed "(am) an --agent-namespace pattern that matches nothing is UNKNOWN, naming it"
fi

# --- (ao) Rights limited by resource name, which a review naming nothing never finds. ---
named_right_row() {
  local planted want
  base_world
  case "$1" in
    group)
      subject_grant operator-cluster "$(sa_subject legion worker)" impersonate "" groups "" "" system:masters
      want="impersonate groups system:masters cluster-wide"
      ;;
    user)
      cluster_admin operator-cluster "$(user_subject alice@example.com)"
      subject_grant operator-cluster "$(sa_subject legion worker)" impersonate "" users "" "" alice@example.com
      want="impersonate users alice@example.com cluster-wide"
      ;;
    role)
      # The fake names each grant's ClusterRole planted-<its index>; this one is bound to legion/worker.
      planted="planted-$(jq length "$world/clusters/operator-cluster/grants.json")"
      subject_grant operator-cluster "$(sa_subject legion worker)" get "" configmaps "" legion ""
      subject_grant operator-cluster "$(sa_subject legion worker)" escalate rbac.authorization.k8s.io clusterroles "" "" "$planted"
      want="escalate clusterroles.rbac.authorization.k8s.io $planted cluster-wide"
      ;;
  esac
  run
  want_status 1 "legion/worker may $want, by name"
  want_line "FINDING system:serviceaccount:legion:worker: $want" "legion/worker may $want, by name"
}
if row ao; then
  each named_right_row group user role
  passed "(ao) a ServiceAccount that may impersonate the group system:masters, impersonate a named user bound to cluster-admin, or escalate a named clusterrole bound to it, each by name, exits 1 naming the name"
fi

# --- (aq) Every namespaced way to become a ServiceAccount with a finding. Each entry is the grant to
# deploy/bot in ci (verb, API group, resource, subresource) and the right the run must name. ---
become_ways=(
  "create|batch|jobs|=create jobs.batch"
  "create|apps|deployments|=create deployments.apps"
  "create||pods|attach=create pods/attach"
  "patch||pods|ephemeralcontainers=patch pods/ephemeralcontainers"
  "patch||pods|=patch pods"
  "create|rbac.authorization.k8s.io|rolebindings|=create rolebindings.rbac.authorization.k8s.io"
  "bind|rbac.authorization.k8s.io|roles|=bind roles.rbac.authorization.k8s.io"
  "escalate|rbac.authorization.k8s.io|roles|=escalate roles.rbac.authorization.k8s.io"
  "create|agents.x-k8s.io|sandboxes|=create sandboxes.agents.x-k8s.io"
)
become_way_row() {
  local right="${1#*=}" verb group resource sub
  IFS='|' read -r verb group resource sub <<<"${1%%=*}"
  base_world
  echo sandboxes.agents.x-k8s.io >"$world/clusters/operator-cluster/api-resources"
  add_sa operator-cluster ci runner
  add_sa operator-cluster deploy bot
  subject_grant operator-cluster "$(sa_subject ci runner)" create "" pods "" envoy-operator ""
  subject_grant operator-cluster "$(sa_subject deploy bot)" "$verb" "$group" "$resource" "$sub" ci ""
  run
  want_status 1 "deploy/bot with $right in ci"
  want_line "FINDING system:serviceaccount:deploy:bot: $right in ci, so it can become system:serviceaccount:ci:runner, which can create pods in envoy-operator" \
    "deploy/bot with $right in ci"
}
if row aq; then
  each become_way_row "${become_ways[@]}"
  passed "(aq) a ServiceAccount that can become one with a finding by each of ${#become_ways[@]} more namespaced rights is a finding naming the chain"
fi

# --- (ar) A system namespace's ServiceAccount allowed a right seeds the chain, unreported itself. ---
if row ar; then
  base_world
  subject_grant operator-cluster "$(sa_subject kube-system generic-garbage-collector)" create "" serviceaccounts token envoy-operator envoy-operator
  subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods "" kube-system ""
  run
  want_status 1 "legion/worker may create pods in kube-system, whose ServiceAccount may mint the operator's token"
  want_line "FINDING system:serviceaccount:legion:worker: create pods in kube-system, so it can become system:serviceaccount:kube-system:generic-garbage-collector, which can create serviceaccounts/token envoy-operator in envoy-operator" \
    "legion/worker may create pods in kube-system (rv6)"
  [ "$(count_prefix 'FINDING system:serviceaccount:kube-system:')" -eq 0 ] || fail "rv6: a kube-system ServiceAccount is itself a finding"
  passed "(ar) a kube-system ServiceAccount that may mint the operator's token is no finding, but who may create pods in kube-system is one, naming the chain (rv6)"

  base_world
  add_cluster other-cluster us-east1
  add_sa other-cluster ci runner
  subject_grant other-cluster "$(sa_subject ci runner)" create "" pods "" kube-system ""
  run
  want_status 1 "ci/runner may create pods in kube-system on another cluster"
  want_line "FINDING system:serviceaccount:ci:runner: create pods in kube-system, so it can become system:serviceaccount:kube-system:namespace-controller, which can create namespaces cluster-wide on cluster $project/us-east1/other-cluster" \
    "ci/runner may create pods in kube-system on another cluster"
  passed "(ar) on another cluster, who may create pods in kube-system becomes its namespace controller and is a finding"
fi

# --- (aw) A User that is no email is reviewed, not taken for a person. ---
principal_user="principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/ci/subject/runner"
user_row() {
  local want
  base_world
  case "$1" in
    member)
      edge_member
      cluster_admin edge-cluster "$(user_subject deployer-bot)"
      want="FINDING user:deployer-bot: create namespaces cluster-wide on fleet member $project/global/edge"
      ;;
    principal)
      subject_grant operator-cluster "$(user_subject "$principal_user")" create "" pods "" envoy-operator ""
      want="FINDING user:$principal_user: create pods in envoy-operator"
      ;;
    code)
      refuse_live
      edit "$world/render.json" '.items += [{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding",
        metadata: {name: "bot", namespace: "envoy-operator"},
        roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "cluster-admin"},
        subjects: [{apiGroup: "rbac.authorization.k8s.io", kind: "User", name: "deployer-bot"}]}]'
      want="FINDING user:deployer-bot: create serviceaccounts/token envoy-operator in envoy-operator, declared by RoleBinding envoy-operator/bot"
      ;;
  esac
  run
  want_status 1 "a User that is no email ($1)"
  want_line "$want" "a User that is no email ($1)"
}
if row aw; then
  each user_row member principal code
  passed "(aw) a User that is no email (deployer-bot on a non-GKE fleet member, a principal:// name, a render's deployer-bot) is reviewed and exits 1 naming it"
fi

# --- (ax) Kubernetes' own groups, whatever Google Groups for RBAC says. ---
own_group_row() {
  local want planted
  base_world
  case "$1" in
    authenticated)
      add_cluster other-cluster us-east1
      subject_grant other-cluster "$(group_subject system:authenticated)" create "" pods "" envoy-operator ""
      want="FINDING group:system:authenticated: create pods in envoy-operator, guarding envoy-operator/envoy-operator on cluster $project/us-east1/other-cluster"
      ;;
    namespace)
      add_cluster other-cluster us-east1
      subject_grant other-cluster "$(group_subject system:serviceaccounts:apps)" create "" pods "" envoy-operator ""
      want="FINDING group:system:serviceaccounts:apps: create pods in envoy-operator, guarding envoy-operator/envoy-operator on cluster $project/us-east1/other-cluster"
      ;;
    serviceaccounts)
      subject_grant operator-cluster "$(group_subject system:serviceaccounts)" create "" pods "" envoy-operator ""
      want="FINDING group:system:serviceaccounts: create pods in envoy-operator"
      ;;
    masters)
      # The fake binds each grant by RoleBinding planted-<its index> to ClusterRole planted-<its index>.
      planted="planted-$(jq length "$world/clusters/operator-cluster/grants.json")"
      subject_grant operator-cluster "$(group_subject system:masters)" get "" configmaps "" legion ""
      want="FINDING group:system:masters: bound by RoleBinding legion/$planted to ClusterRole $planted, so every holder of a system:masters certificate takes it"
      ;;
    masters-code)
      refuse_live
      edit "$world/render.json" '.items += [{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding",
        metadata: {name: "masters", namespace: "envoy-operator"},
        roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "cluster-admin"},
        subjects: [{apiGroup: "rbac.authorization.k8s.io", kind: "Group", name: "system:masters"}]}]'
      want="FINDING group:system:masters: create serviceaccounts/token envoy-operator in envoy-operator, declared by RoleBinding envoy-operator/masters"
      ;;
  esac
  run
  want_status 1 "Kubernetes' own group ($1)"
  want_line "$want" "Kubernetes' own group ($1)"
}
if row ax; then
  each own_group_row authenticated namespace serviceaccounts masters masters-code
  passed "(ax) system:authenticated, system:serviceaccounts, system:serviceaccounts:apps, a non-bootstrap live binding of system:masters, and a render's system:masters binding each exit 1"
fi

# --- (ay) Any other Group, judged by where the cluster takes its groups from. ---
readonly security_group=gke-security-groups@example.com
other_group_row() {
  local want
  base_world
  case "$1" in
    member)
      edge_member
      cluster_admin edge-cluster "$(group_subject ci-deployers)"
      want="UNKNOWN live: Workload Identity on fleet member edge: it takes its users and groups from its own authenticator, not Google, so the Group subjects bound here (ci-deployers) name members this check cannot list"
      ;;
    google-on)
      add_cluster other-cluster us-east1
      edit "$world/clusters.json" 'map(if .name == "other-cluster" then . + {authenticatorGroupsConfig: {enabled: true, securityGroup: $g}} else . end)' \
        --arg g "$security_group"
      cluster_admin other-cluster "$(group_subject devs@example.com)"
      want="UNKNOWN live: Workload Identity on $project/us-east1/other-cluster: Google Groups for RBAC is on (security group $security_group), so a Group subject gives its Google group's members its rights, which this check neither lists nor judges (Group subjects bound now: devs@example.com)"
      ;;
    google-on-member)
      grade_member
      edit "$world/fleet.json" '.memberships |= map(if .id == "grade" then . + {groups: {enabled: true, securityGroup: $g}} else . end)' \
        --arg g "$security_group"
      cluster_admin grade-cluster "$(group_subject devs@example.com)"
      want="UNKNOWN $grade_step: Google Groups for RBAC is on (security group $security_group), so a Group subject gives its Google group's members its rights, which this check neither lists nor judges (Group subjects bound now: devs@example.com)"
      ;;
    describe-refused)
      grade_member
      : >"$world/refuse-describe-grade-cluster"
      cluster_admin grade-cluster "$(group_subject devs@example.com)"
      want="UNKNOWN $grade_step: whether Google Groups for RBAC is on is unknown (gcloud container clusters describe grade-cluster was refused (it needs container.clusters.get)"
      ;;
    google-on-operator)
      edit "$world/clusters.json" 'map(if .name == "operator-cluster" then . + {authenticatorGroupsConfig: {enabled: true, securityGroup: $g}} else . end)' \
        --arg g "$security_group"
      want="UNKNOWN live: RBAC on operator-cluster: Google Groups for RBAC is on (security group $security_group), so a Group subject gives its Google group's members its rights, which this check neither lists nor judges (Group subjects bound now: none)"
      ;;
  esac
  run
  want_status 2 "another Group ($1)"
  want_text "$want" "another Group ($1)"
}
if row ay; then
  each other_group_row member google-on google-on-member describe-refused google-on-operator
  passed "(ay) a Group other than Kubernetes' own reads UNKNOWN on a non-GKE member, where Google Groups for RBAC is on, where whether it is on is unknown, and on the operator's cluster with it on"
fi

# --- (bb) A ServiceAccount a binding names, which does not exist yet. ---
if row bb; then
  base_world
  subject_grant operator-cluster "$(sa_subject ci ghost)" create "" pods "" envoy-operator ""
  run
  want_status 1 "a bound ServiceAccount that does not exist"
  want_line "FINDING system:serviceaccount:ci:ghost: create pods in envoy-operator" "a bound ServiceAccount that does not exist"
  passed "(bb) a ServiceAccount a binding names, which does not exist yet, is reviewed and exits 1"
fi
