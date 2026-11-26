# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-rbac.sh: the rows of operator-token-minters.test.sh on
# Kubernetes RBAC: each right of the list, who can become a holder, rights limited by name, custom
# resources, and the ServiceAccounts, Users and Groups a binding names, on any cluster of the pool.
# Sourced by the test after its world and helpers; its header says what the fakes model. The rows:
#
#   (b)  each right of the RBAC list, allowed to an agent ServiceAccount that holds nothing else, is
#        a finding naming that ServiceAccount and that right, and nothing else is (one world, one
#        ServiceAccount a right);
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
#   (ak) create on a custom resource (sandboxes.agents.x-k8s.io) in envoy-operator is a finding, and
#        so are update on it and create on workloads.example.k8s.io, a CRD group ending in k8s.io
#        that is not Kubernetes' own; create on Kubernetes' own leases is not, and a discovery that
#        cannot be read is UNKNOWN;
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
#        controller and is one too; and legion/worker allowed, by name in kube-system, a patch of one
#        Deployment, an exec of one pod or an ephemeral container of it is one naming the chain (N3);
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
#        envoy-operator) is reviewed and exits 1;
#   (bd) writes to a binding: legion/worker with update rolebindings in envoy-operator, or with patch
#        of the operator's RoleBinding by name, beside bind of its ClusterRole by name; update of
#        that RoleBinding by name; escalate of that ClusterRole by name; and on another cluster
#        ci/runner with patch of the ClusterRoleBinding cluster-admin and bind of the ClusterRole
#        cluster-admin, each by name: each exits 1 naming the rights;
#   (bf) each admission right (create, update and patch of mutatingwebhookconfigurations,
#        validatingwebhookconfigurations, validatingadmissionpolicies and their bindings, and of
#        mutatingadmissionpolicies and their bindings where the discovery lists them) given on the
#        operator's cluster, and create of each on another cluster, to a ServiceAccount that holds
#        nothing else, is a finding naming it (two worlds, one ServiceAccount a right);
#   (bg) a discovery of admissionregistration.k8s.io that lists no validatingwebhookconfigurations,
#        or cannot be read on the operator's cluster or another, reads UNKNOWN;
#   (bi) on a fleet member that is no GKE cluster, a User that looks like an email
#        (ci-bot@corp.example) bound to cluster-admin reads UNKNOWN: that cluster's own
#        authenticator names its users, so the name proves no person;
#   (bj) a client certificate signed by the cluster: legion/worker that may create certificate
#        signing requests, update their approval and approve the signer
#        kubernetes.io/kube-apiserver-client (rv9, beside a person who may mint the operator's
#        token), one that may approve for kubernetes.io/* without create, ci/runner on another
#        cluster, the approval's patch with the client signer (rv13), its update with the kubelet
#        signer (rv12), the signer kubernetes.io/legacy-unknown, a signer of any name, and on a
#        fleet member that is no GKE cluster create of certificatesigningrequests/nodeclient or
#        selfnodeclient alone, each exit 1 naming the chain or the right; create alone, and create
#        and patch of the request my-csr by name with no signer (pn6), exit 0;
#   (bo) legion/worker that may impersonate, by name, a user whose power is IAM's (owner@example.com,
#        who holds roles/owner on the organization, or admin@example.com, a member of a group holding
#        roles/container.admin) exits 1 naming that user;
#   (br) legion/worker that may get, by name, a token Secret the operator's ServiceAccount lists
#        exits 1 naming it; a list of bindings without the bootstrap cluster-admin binding, on the
#        operator's cluster or another, reads UNKNOWN;
#   (bu) a right a live role limits by name, and a bind of clusterroles in a namespace outside the
#        guarded ones, outright or by name (the reviewer's example-db world); the five probes: update of the mutating
#        webhook configuration existing-webhook on the operator's cluster, and patch of it on
#        another; create of certificate signing requests, update of the approval of my-csr and
#        approve of kubernetes.io/kube-apiserver-client; patch of the Deployment secrets-broker in
#        secrets-broker; update of the CronJob nightly in envoy-operator; and create of pods/exec of
#        secrets-broker-0: each exits 1 naming the name; create pods limited by name beside patch of
#        that name, and rv14's create and patch of the namespace envoy-operator by name on another
#        cluster, each exit 1 (server-side apply creates by patch); create limited by name alone exits 0;
#   (bv) a refused list of roles, on the operator's cluster or another, reads UNKNOWN;
#   (cc) legion/worker that may get, by name, a token Secret of envoy-operator that no ServiceAccount
#        lists exits 1 naming it, and a refused list of the namespace's token Secrets reads UNKNOWN.

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
  "update|rbac.authorization.k8s.io|rolebindings||envoy-operator|=update rolebindings.rbac.authorization.k8s.io in envoy-operator"
  "create|rbac.authorization.k8s.io|clusterrolebindings||||=create clusterrolebindings.rbac.authorization.k8s.io cluster-wide"
  "patch|rbac.authorization.k8s.io|clusterrolebindings||||=patch clusterrolebindings.rbac.authorization.k8s.io cluster-wide"
  "update|rbac.authorization.k8s.io|clusterrolebindings||||=update clusterrolebindings.rbac.authorization.k8s.io cluster-wide"
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
  "create||secrets||envoy-operator|=create secrets in envoy-operator"
  "patch||secrets||envoy-operator|=patch secrets in envoy-operator"
  "create||replicationcontrollers||envoy-operator|=create replicationcontrollers in envoy-operator"
  "patch||replicationcontrollers||envoy-operator|=patch replicationcontrollers in envoy-operator"
  "update||replicationcontrollers||envoy-operator|=update replicationcontrollers in envoy-operator"
  "create||nodes|proxy|||=create nodes/proxy cluster-wide"
  "get||nodes|proxy|||=get nodes/proxy cluster-wide"
)
# All the rights in one world and one run, each allowed to an agent ServiceAccount of its own,
# legion/right-<its place in the list>, which holds nothing else: a run per right cost the harness
# about 55 runs of the check. A right the check does not ask leaves its account's line out, and a
# finding misread onto another principal names an account no right was given to.
rbac_rights_row() {
  local i=0 entry verb group resource sub namespace name
  base_world
  for entry in "${rbac_rights[@]}"; do
    i=$((i + 1))
    IFS='|' read -r verb group resource sub namespace name <<<"${entry%%=*}"
    add_sa operator-cluster legion "right-$i"
    subject_grant operator-cluster "$(sa_subject legion "right-$i")" "$verb" "$group" "$resource" "$sub" "$namespace" "$name"
  done
  run
  want_status 1 "each right of the RBAC list allowed to its own agent ServiceAccount"
  i=0
  for entry in "${rbac_rights[@]}"; do
    i=$((i + 1))
    want_line "FINDING system:serviceaccount:legion:right-$i: ${entry#*=}" "${entry#*=} allowed to legion/right-$i alone"
  done
  # An unnamed grant also allows each name the check asks by name (a bound group or user, a bound
  # role), so every finding must name one of the accounts a right was given to, not another.
  [ "$(count_prefix 'FINDING system:serviceaccount:legion:right-[0-9]*: ')" -eq "$(count_prefix 'FINDING ')" ] ||
    fail "each right of the RBAC list allowed to its own agent ServiceAccount: a finding names another principal"
}
if row b; then
  rbac_rights_row
  passed "(b) each of the ${#rbac_rights[@]} rights of the RBAC list, allowed to an agent ServiceAccount that holds nothing else, is a finding naming it and the right, and nothing else is"
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

  # P3-6: update of one, and a custom resource whose CRD group ends in k8s.io without being one of
  # Kubernetes' own groups.
  base_world
  echo sandboxes.agents.x-k8s.io >"$world/clusters/operator-cluster/api-resources"
  subject_grant operator-cluster "$(sa_subject legion worker)" update agents.x-k8s.io sandboxes "" envoy-operator ""
  run
  want_status 1 "legion/worker with update on sandboxes in envoy-operator"
  want_line "FINDING system:serviceaccount:legion:worker: update sandboxes.agents.x-k8s.io in envoy-operator" \
    "legion/worker with update on sandboxes in envoy-operator"
  base_world
  echo workloads.example.k8s.io >"$world/clusters/operator-cluster/api-resources"
  subject_grant operator-cluster "$(sa_subject legion worker)" create example.k8s.io workloads "" envoy-operator ""
  run
  want_status 1 "legion/worker with create on workloads.example.k8s.io in envoy-operator"
  want_line "FINDING system:serviceaccount:legion:worker: create workloads.example.k8s.io in envoy-operator" \
    "legion/worker with create on workloads.example.k8s.io in envoy-operator"
  passed "(ak) update on a custom resource, and create on one whose group ends in k8s.io but is not Kubernetes' own, are findings (P3-6)"

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

  # N3: a right limited by name in a seed's namespace becomes the seed too: a patch of one Deployment
  # there sets its serviceAccountName, and an exec or ephemeral container of one pod enters it.
  for named in "patch|apps|deployments||gc-controller=patch deployments.apps gc-controller" \
    "create||pods|exec|gc-controller-0=create pods/exec gc-controller-0" \
    "patch||pods|ephemeralcontainers|gc-controller-0=patch pods/ephemeralcontainers gc-controller-0"; do
    IFS='|' read -r verb group resource sub name <<<"${named%%=*}"
    base_world
    subject_grant operator-cluster "$(sa_subject kube-system generic-garbage-collector)" create "" serviceaccounts token envoy-operator envoy-operator
    subject_grant operator-cluster "$(sa_subject legion worker)" "$verb" "$group" "$resource" "$sub" kube-system "$name"
    run
    want_status 1 "legion/worker may ${named#*=} in kube-system, by name"
    want_line "FINDING system:serviceaccount:legion:worker: ${named#*=} in kube-system, so it can become system:serviceaccount:kube-system:generic-garbage-collector, which can create serviceaccounts/token envoy-operator in envoy-operator" \
      "legion/worker may ${named#*=} in kube-system, by name"
  done
  passed "(ar) a right limited by name in the seed's namespace (patch of one Deployment, exec or an ephemeral container of one pod) is a finding naming the chain (N3)"
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

# --- (bd) Writes to a binding. RBAC lets one who may bind a binding's role, by name, update the
# binding, and a name-limited write is refused to a review that names nothing. Each item reaches the
# operator's token through its own RoleBinding envoy-operator/planted-0 and ClusterRole planted-0
# (the fake names each grant's binding and role planted-<its index>, and the operator's token grant
# is the world's first), or cluster-admin through the bootstrap binding on another cluster. ---
binding_write_row() {
  local who=system:serviceaccount:legion:worker suffix="" line want=()
  base_world
  case "$1" in
    update)
      subject_grant operator-cluster "$(sa_subject legion worker)" update rbac.authorization.k8s.io rolebindings "" envoy-operator ""
      subject_grant operator-cluster "$(sa_subject legion worker)" bind rbac.authorization.k8s.io clusterroles "" envoy-operator planted-0
      want=("update rolebindings.rbac.authorization.k8s.io in envoy-operator" "bind clusterroles.rbac.authorization.k8s.io planted-0 in envoy-operator")
      ;;
    patch-named)
      subject_grant operator-cluster "$(sa_subject legion worker)" patch rbac.authorization.k8s.io rolebindings "" envoy-operator planted-0
      subject_grant operator-cluster "$(sa_subject legion worker)" bind rbac.authorization.k8s.io clusterroles "" envoy-operator planted-0
      want=("patch rolebindings.rbac.authorization.k8s.io planted-0 in envoy-operator" "bind clusterroles.rbac.authorization.k8s.io planted-0 in envoy-operator")
      ;;
    update-named)
      subject_grant operator-cluster "$(sa_subject legion worker)" update rbac.authorization.k8s.io rolebindings "" envoy-operator planted-0
      want=("update rolebindings.rbac.authorization.k8s.io planted-0 in envoy-operator")
      ;;
    escalate-named)
      subject_grant operator-cluster "$(sa_subject legion worker)" escalate rbac.authorization.k8s.io clusterroles "" "" planted-0
      want=("escalate clusterroles.rbac.authorization.k8s.io planted-0 cluster-wide")
      ;;
    cluster-admin)
      add_cluster other-cluster us-east1
      add_sa other-cluster ci runner
      subject_grant other-cluster "$(sa_subject ci runner)" patch rbac.authorization.k8s.io clusterrolebindings "" "" cluster-admin
      subject_grant other-cluster "$(sa_subject ci runner)" bind rbac.authorization.k8s.io clusterroles "" "" cluster-admin
      who=system:serviceaccount:ci:runner suffix=" on cluster $project/us-east1/other-cluster"
      want=("patch clusterrolebindings.rbac.authorization.k8s.io cluster-admin cluster-wide" "bind clusterroles.rbac.authorization.k8s.io cluster-admin cluster-wide")
      ;;
  esac
  run
  want_status 1 "a write to a binding ($1)"
  for line in "${want[@]}"; do
    want_line "FINDING $who: $line$suffix" "a write to a binding ($1)"
  done
}
if row bd; then
  each binding_write_row update patch-named update-named escalate-named cluster-admin
  passed "(bd) update of rolebindings, patch or update of the operator's RoleBinding by name, bind or escalate of its ClusterRole by name, and patch of cluster-admin's binding with bind of its role by name on another cluster each exit 1 naming the rights"
fi

# --- (bf) The admission rights. A mutating webhook or policy can add a container to the operator's
# next pod, and a validating one can block or reshape what the operator applies. Each entry is the
# verb, the resource of admissionregistration.k8s.io, and the cluster: the operator's, given to
# legion/right-<its place in the list>, or another of the pool, given to ci/right-<its place>, each
# a ServiceAccount that holds nothing else. The fake's discovery lists the four a GKE cluster
# serves; the mutating admission policies, beta and off by default, are listed in a world of their
# own items alone, since the check asks what the cluster's discovery lists. So the row is two
# worlds and two runs (a run an item cost 24 runs of the check). ---
admission_items=()
for admission_resource in mutatingwebhookconfigurations validatingwebhookconfigurations validatingadmissionpolicies \
  validatingadmissionpolicybindings mutatingadmissionpolicies mutatingadmissionpolicybindings; do
  admission_items+=("create|$admission_resource|operator" "update|$admission_resource|operator" "patch|$admission_resource|operator" "create|$admission_resource|other")
done
# admission_rights_row gke|mutating: the items of the four resources a GKE cluster serves, or of
# the mutating admission policies with both clusters' discovery listing them, in one world.
admission_rights_row() {
  local i=0 entry verb resource where cluster pass
  base_world
  add_cluster other-cluster us-east1
  if [ "$1" = mutating ]; then
    for cluster in operator-cluster other-cluster; do
      printf '%s.admissionregistration.k8s.io\n' mutatingadmissionpolicies mutatingadmissionpolicybindings mutatingwebhookconfigurations \
        validatingadmissionpolicies validatingadmissionpolicybindings validatingwebhookconfigurations >"$world/clusters/$cluster/admission-resources"
    done
  fi
  for pass in plant judge; do
    [ "$pass" = plant ] || {
      run
      want_status 1 "the admission rights ($1), each given to its own ServiceAccount"
    }
    i=0
    for entry in "${admission_items[@]}"; do
      i=$((i + 1))
      IFS='|' read -r verb resource where <<<"$entry"
      case "$resource" in mutatingadmission*) [ "$1" = mutating ] || continue ;; *) [ "$1" = gke ] || continue ;; esac
      case "$pass:$where" in
        plant:operator)
          add_sa operator-cluster legion "right-$i"
          subject_grant operator-cluster "$(sa_subject legion "right-$i")" "$verb" admissionregistration.k8s.io "$resource" "" "" ""
          ;;
        plant:other)
          add_sa other-cluster ci "right-$i"
          subject_grant other-cluster "$(sa_subject ci "right-$i")" "$verb" admissionregistration.k8s.io "$resource" "" "" ""
          ;;
        judge:operator)
          want_line "FINDING system:serviceaccount:legion:right-$i: $verb $resource.admissionregistration.k8s.io cluster-wide" \
            "legion/right-$i with $verb $resource alone"
          ;;
        judge:other)
          want_line "FINDING system:serviceaccount:ci:right-$i: $verb $resource.admissionregistration.k8s.io cluster-wide on cluster $project/us-east1/other-cluster" \
            "ci/right-$i with $verb $resource on other-cluster alone"
          ;;
      esac
    done
  done
}
if row bf; then
  admission_rights_row gke
  admission_rights_row mutating
  passed "(bf) each of the ${#admission_items[@]} admission rights, given on the operator's cluster or another of the pool where its discovery lists the resource to a ServiceAccount that holds nothing else, is a finding naming it"
fi

# --- (bg) The admission resources' discovery. Every API server serves the two webhook
# configurations, so a list without one is not whole, and a discovery that cannot be read leaves
# the admission policies unasked. ---
admission_names_row() {
  base_world
  case "$1" in
    missing)
      printf '%s.admissionregistration.k8s.io\n' mutatingwebhookconfigurations validatingadmissionpolicies validatingadmissionpolicybindings \
        >"$world/clusters/operator-cluster/admission-resources"
      run
      want_status 2 "a discovery without validatingwebhookconfigurations"
      want_text "UNKNOWN live: RBAC on operator-cluster: the API server's discovery of admissionregistration.k8s.io lists no validatingwebhookconfigurations.admissionregistration.k8s.io, which every API server serves, so its list is not whole" \
        "a discovery without validatingwebhookconfigurations"
      ;;
    refused)
      : >"$world/clusters/operator-cluster/refuse-admission-resources"
      run
      want_status 2 "a discovery of admissionregistration.k8s.io that cannot be read"
      want_text "UNKNOWN live: RBAC on operator-cluster: the API server's discovery of admissionregistration.k8s.io could not be read" \
        "a discovery of admissionregistration.k8s.io that cannot be read"
      ;;
    refused-other)
      add_cluster other-cluster us-east1
      : >"$world/clusters/other-cluster/refuse-admission-resources"
      run
      want_status 2 "another cluster's discovery of admissionregistration.k8s.io that cannot be read"
      want_text "UNKNOWN live: Workload Identity on $project/us-east1/other-cluster: the API server's discovery of admissionregistration.k8s.io could not be read" \
        "another cluster's discovery of admissionregistration.k8s.io that cannot be read"
      ;;
  esac
}
if row bg; then
  each admission_names_row missing refused refused-other
  passed "(bg) a discovery of admissionregistration.k8s.io that lists no validatingwebhookconfigurations, or cannot be read on the operator's or another cluster, reads UNKNOWN"
fi

# --- (bi) An email-shaped User on a fleet member that is no GKE cluster. Its own authenticator names
# its users, as it names its groups, so ci-bot@corp.example there is no proof of a person. ---
if row bi; then
  base_world
  edge_member
  cluster_admin edge-cluster "$(user_subject ci-bot@corp.example)"
  run
  want_status 2 "ci-bot@corp.example bound to cluster-admin on a non-GKE member"
  want_text "UNKNOWN live: Workload Identity on fleet member edge: it takes its users from its own authenticator, not Google, so the User subjects bound here that look like emails (ci-bot@corp.example) are not known to be people" \
    "ci-bot@corp.example bound to cluster-admin on a non-GKE member"
  passed "(bi) an email-shaped User bound on a fleet member that is no GKE cluster reads UNKNOWN"
fi

# --- (bj) A client certificate the cluster signs. A certificate signed for
# kubernetes.io/kube-apiserver-client authenticates as any user and groups its request names but
# system:masters, and one for the kubelet signer as a node, so whoever may write a request's
# approval (update or patch) and approve such a signer becomes anyone, a person who may mint the
# operator's token included. ---
csr_row() {
  local who=system:serviceaccount:legion:worker cluster=operator-cluster subject suffix="" want
  local clause="cluster-wide, so it can have the cluster sign it a client certificate for a user, group or node it names"
  base_world
  subject="$(sa_subject legion worker)"
  case "$1" in
    other-cluster)
      add_cluster other-cluster us-east1
      add_sa other-cluster ci runner
      cluster=other-cluster subject="$(sa_subject ci runner)" who=system:serviceaccount:ci:runner
      suffix=" on cluster $project/us-east1/other-cluster"
      ;;
    nodeclient | selfnodeclient)
      edge_member
      cluster=edge-cluster subject="$(sa_subject apps deployer)" who=system:serviceaccount:apps:deployer
      suffix=" on fleet member $project/global/edge"
      ;;
  esac
  case "$1" in
    rv9)
      subject_grant operator-cluster "$(user_subject ops@example.com)" create "" serviceaccounts token envoy-operator envoy-operator
      subject_grant "$cluster" "$subject" create certificates.k8s.io certificatesigningrequests "" "" ""
      subject_grant "$cluster" "$subject" update certificates.k8s.io certificatesigningrequests approval "" ""
      subject_grant "$cluster" "$subject" approve certificates.k8s.io signers "" "" kubernetes.io/kube-apiserver-client
      want="create certificatesigningrequests, update certificatesigningrequests/approval and approve signers kubernetes.io/kube-apiserver-client $clause"
      ;;
    wildcard | other-cluster)
      subject_grant "$cluster" "$subject" update certificates.k8s.io certificatesigningrequests approval "" ""
      subject_grant "$cluster" "$subject" approve certificates.k8s.io signers "" "" "kubernetes.io/*"
      want="update certificatesigningrequests/approval and approve signers kubernetes.io/* $clause"
      ;;
    rv13)
      subject_grant "$cluster" "$subject" create certificates.k8s.io certificatesigningrequests "" "" ""
      subject_grant "$cluster" "$subject" patch certificates.k8s.io certificatesigningrequests approval "" ""
      subject_grant "$cluster" "$subject" approve certificates.k8s.io signers "" "" kubernetes.io/kube-apiserver-client
      want="create certificatesigningrequests, patch certificatesigningrequests/approval and approve signers kubernetes.io/kube-apiserver-client $clause"
      ;;
    rv12)
      subject_grant "$cluster" "$subject" update certificates.k8s.io certificatesigningrequests approval "" ""
      subject_grant "$cluster" "$subject" approve certificates.k8s.io signers "" "" kubernetes.io/kube-apiserver-client-kubelet
      want="update certificatesigningrequests/approval and approve signers kubernetes.io/kube-apiserver-client-kubelet $clause"
      ;;
    legacy)
      subject_grant "$cluster" "$subject" update certificates.k8s.io certificatesigningrequests approval "" ""
      subject_grant "$cluster" "$subject" approve certificates.k8s.io signers "" "" kubernetes.io/legacy-unknown
      want="update certificatesigningrequests/approval and approve signers kubernetes.io/legacy-unknown $clause"
      ;;
    any-signer)
      subject_grant "$cluster" "$subject" update certificates.k8s.io certificatesigningrequests approval "" ""
      subject_grant "$cluster" "$subject" approve certificates.k8s.io signers "" "" ""
      want="update certificatesigningrequests/approval and approve signers of any name $clause"
      ;;
    nodeclient | selfnodeclient)
      subject_grant "$cluster" "$subject" create certificates.k8s.io certificatesigningrequests "$1" "" ""
      want="create certificatesigningrequests/$1.certificates.k8s.io cluster-wide, which kube-controller-manager approves by itself"
      ;;
    create-patch-named)
      # pn6: a create limited by name, beside a patch of that name, is no chain without a signer.
      subject_grant "$cluster" "$subject" create certificates.k8s.io certificatesigningrequests "" "" my-csr
      subject_grant "$cluster" "$subject" patch certificates.k8s.io certificatesigningrequests "" "" my-csr
      run
      want_status 0 "create and patch of the certificate signing request my-csr by name, alone"
      return
      ;;
    create-only)
      subject_grant "$cluster" "$subject" create certificates.k8s.io certificatesigningrequests "" "" ""
      run
      want_status 0 "create certificatesigningrequests alone"
      return
      ;;
  esac
  run
  want_status 1 "a certificate signing chain ($1)"
  want_line "FINDING $who: $want$suffix" "a certificate signing chain ($1)"
}
if row bj; then
  each csr_row rv9 wildcard other-cluster rv13 rv12 legacy any-signer nodeclient selfnodeclient create-only create-patch-named
  passed "(bj) who may write a certificate signing request's approval (update or patch) and approve a client, kubelet, legacy or any signer, on the operator's cluster or another, and who may create a node client request on a non-GKE member, exits 1 naming the chain; create alone, or create and patch of one request by name (pn6), exits 0"
fi

# --- (bo) Impersonating, by name, a user whose power comes from IAM: GKE's IAM authorizer judges an
# impersonated user by its own IAM, so whoever may impersonate an IAM holder takes what it holds. ---
iam_user_row() {
  base_world
  subject_grant operator-cluster "$(sa_subject legion worker)" impersonate "" users "" "" "$1"
  run
  want_status 1 "legion/worker may impersonate $1 by name"
  want_line "FINDING system:serviceaccount:legion:worker: impersonate users $1 cluster-wide" "legion/worker may impersonate $1 by name"
}
if row bo; then
  each iam_user_row owner@example.com admin@example.com
  passed "(bo) a ServiceAccount that may impersonate, by name, a user holding the GKE IAM permissions exits 1 naming the user"
fi

# --- (br) A token Secret by name, and the bindings' witness. A review naming no Secret is refused for
# a get limited to a name, so each token Secret a guarded ServiceAccount lists is asked by name; and
# every cluster has the bootstrap cluster-admin binding, so a list without it is not whole. ---
secret_witness_row() {
  base_world
  case "$1" in
    named-secret)
      edit "$world/clusters/operator-cluster/serviceaccounts.json" \
        'map(if .namespace == "envoy-operator" and .name == "envoy-operator" then . + {secrets: [{name: "envoy-operator-token-x7k2p"}]} else . end)'
      subject_grant operator-cluster "$(sa_subject legion worker)" get "" secrets "" envoy-operator envoy-operator-token-x7k2p
      run
      want_status 1 "legion/worker may get the operator's token Secret by name"
      want_line "FINDING system:serviceaccount:legion:worker: get secrets envoy-operator-token-x7k2p in envoy-operator" \
        "legion/worker may get the operator's token Secret by name"
      ;;
    no-bootstrap)
      : >"$world/clusters/operator-cluster/no-bootstrap-binding"
      run
      want_status 2 "a bindings' list without the bootstrap binding"
      want_text "UNKNOWN live: RBAC on operator-cluster: the bindings' list holds no bootstrap cluster-admin ClusterRoleBinding, which every cluster has, so it is not the cluster's whole list" \
        "a bindings' list without the bootstrap binding"
      ;;
    no-bootstrap-other)
      add_cluster other-cluster us-east1
      : >"$world/clusters/other-cluster/no-bootstrap-binding"
      run
      want_status 2 "another cluster's bindings' list without the bootstrap binding"
      want_text "UNKNOWN live: Workload Identity on $project/us-east1/other-cluster: the bindings' list holds no bootstrap cluster-admin ClusterRoleBinding" \
        "another cluster's bindings' list without the bootstrap binding"
      ;;
  esac
}
if row br; then
  each secret_witness_row named-secret no-bootstrap no-bootstrap-other
  passed "(br) a get of the operator's token Secret by name exits 1, and a list of bindings without the bootstrap binding reads UNKNOWN"
fi

# --- (bu) Rights a live role limits by name. RBAC applies a rule with resourceNames only to a request
# that names one of them, so a review naming nothing is refused for each of these; the check reads
# the roles the bindings name. ---
name_limited_row() {
  local who=system:serviceaccount:legion:worker cluster=operator-cluster subject suffix="" want
  base_world
  subject="$(sa_subject legion worker)"
  case "$1" in
    webhook-operator)
      subject_grant "$cluster" "$subject" update admissionregistration.k8s.io mutatingwebhookconfigurations "" "" existing-webhook
      want="update mutatingwebhookconfigurations.admissionregistration.k8s.io existing-webhook cluster-wide"
      ;;
    webhook-other)
      add_cluster other-cluster us-east1
      add_sa other-cluster ci runner
      cluster=other-cluster subject="$(sa_subject ci runner)" who=system:serviceaccount:ci:runner
      suffix=" on cluster $project/us-east1/other-cluster"
      subject_grant "$cluster" "$subject" patch admissionregistration.k8s.io mutatingwebhookconfigurations "" "" existing-webhook
      want="patch mutatingwebhookconfigurations.admissionregistration.k8s.io existing-webhook cluster-wide"
      ;;
    csr)
      subject_grant "$cluster" "$subject" create certificates.k8s.io certificatesigningrequests "" "" ""
      subject_grant "$cluster" "$subject" update certificates.k8s.io certificatesigningrequests approval "" my-csr
      subject_grant "$cluster" "$subject" approve certificates.k8s.io signers "" "" kubernetes.io/kube-apiserver-client
      want="create certificatesigningrequests, update certificatesigningrequests/approval my-csr and approve signers kubernetes.io/kube-apiserver-client cluster-wide, so it can have the cluster sign it a client certificate for a user, group or node it names"
      ;;
    deployment)
      subject_grant "$cluster" "$subject" patch apps deployments "" secrets-broker secrets-broker
      want="patch deployments.apps secrets-broker in secrets-broker"
      ;;
    cronjob)
      subject_grant "$cluster" "$subject" update batch cronjobs "" envoy-operator nightly
      want="update cronjobs.batch nightly in envoy-operator"
      ;;
    exec)
      subject_grant "$cluster" "$subject" create "" pods exec secrets-broker secrets-broker-0
      want="create pods/exec secrets-broker-0 in secrets-broker"
      ;;
    create-patch)
      subject_grant "$cluster" "$subject" create "" pods "" envoy-operator some-pod
      subject_grant "$cluster" "$subject" patch "" pods "" envoy-operator some-pod
      want="create pods some-pod in envoy-operator, which its patch or update of that name makes"
      ;;
    rv14)
      add_cluster other-cluster us-east1
      add_sa other-cluster ci runner
      cluster=other-cluster subject="$(sa_subject ci runner)" who=system:serviceaccount:ci:runner
      suffix=" on cluster $project/us-east1/other-cluster"
      subject_grant "$cluster" "$subject" create "" namespaces "" "" envoy-operator
      subject_grant "$cluster" "$subject" patch "" namespaces "" "" envoy-operator
      want="create namespaces envoy-operator cluster-wide, which its patch or update of that name makes"
      ;;
    create-named)
      subject_grant "$cluster" "$subject" create "" pods "" envoy-operator some-pod
      run
      want_status 0 "create pods limited by name alone, which no request is allowed"
      return
      ;;
    handed-off)
      # The hand-off: a name-limited grant is a finding, and no review names a resource for a
      # write of a role or a binding, or an impersonation of a user or a group: those are asked
      # with no name (RIGHTS_TABLE, and binding_rights for each namespace that holds a
      # RoleBinding), and live_named reads from the cluster's roles each rule that limits one of
      # them by name. The rights the check still reviews by name are its own fixed ones, not a
      # binding's: the signers of the certificate chain, a guarded pair's ServiceAccount, and each
      # token Secret it listed.
      subject_grant "$cluster" "$subject" bind rbac.authorization.k8s.io roles "" envoy-operator operator-role
      run
      want_status 1 "a bind limited to one role's name, read from the cluster's roles"
      want_line "FINDING $who: bind roles.rbac.authorization.k8s.io operator-role in envoy-operator" \
        "a bind limited to one role's name, read from the cluster's roles"
      asked="$temporary_dir/asked-$cluster.tsv"
      cat "$world/clusters/$cluster"/review-requests-*.tsv >"$asked" 2>/dev/null || true
      [ -s "$asked" ] || fail "the check reviewed nothing, so the item proves nothing"
      from_bindings='$6 != "" && ((($1 == "bind" || $1 == "escalate" || $1 == "patch" || $1 == "update") \
        && ($3 == "roles" || $3 == "clusterroles" || $3 == "rolebindings" || $3 == "clusterrolebindings")) \
        || ($1 == "impersonate" && ($3 == "users" || $3 == "groups")))'
      named="$(awk -F'\t' "$from_bindings" "$asked" | wc -l)"
      [ "$named" -eq 0 ] ||
        fail "the check reviewed $named rights a binding limits by name, which live_named reads from the roles: $(awk -F'\t' "$from_bindings" "$asked" | awk -F'\t' '{ print $1, $3, $6 }' | sort -u | head -n 3 | tr '\n' ';')"
      return
      ;;
    outside-namespace | outside-namespace-named)
      # A namespace outside the guarded ones, where a person reads the broker's database login:
      # legion/worker may create rolebindings there and bind clusterroles there, so it can bind
      # itself a ClusterRole that reads that Secret, and the login's holders are hold writers. Only
      # binding_rights asks the bind in that namespace; without it the run passes, none allowed.
      # The named item limits the bind to one ClusterRole's name, which no review answers and
      # live_named reads from the cluster's roles for the unnamed right binding_rights asks.
      subject_grant "$cluster" "$(user_subject dba@example.com)" get "" secrets "" example-db broker-db-login
      subject_grant "$cluster" "$subject" create rbac.authorization.k8s.io rolebindings "" example-db ""
      if [ "$1" = outside-namespace ]; then
        subject_grant "$cluster" "$subject" bind rbac.authorization.k8s.io clusterroles "" example-db ""
        want="bind clusterroles.rbac.authorization.k8s.io in example-db"
      else
        subject_grant "$cluster" "$subject" bind rbac.authorization.k8s.io clusterroles "" example-db secret-reader
        want="bind clusterroles.rbac.authorization.k8s.io secret-reader in example-db"
      fi
      run
      want_status 1 "a bind in a namespace outside the guarded ones ($1)"
      want_line "FINDING $who: $want" "a bind in a namespace outside the guarded ones ($1)"
      return
      ;;
  esac
  run
  want_status 1 "a right limited by name ($1)"
  want_line "FINDING $who: $want$suffix" "a right limited by name ($1)"
}
if row bu; then
  each name_limited_row webhook-operator webhook-other csr deployment cronjob exec create-patch rv14 create-named handed-off \
    outside-namespace outside-namespace-named
  passed "(bu) each right a live role limits by name (a webhook configuration on two clusters, a CSR approval, a Deployment, a CronJob, a pod's exec, a create with its patch) exits 1 naming the name, read from the cluster's roles and reviewed by no request that names a resource; create limited by name alone exits 0; and a bind of clusterroles in a namespace outside the guarded ones, outright or limited by name, exits 1 naming that namespace"
fi

# --- (bv) The roles' list, which the named rights are read from. ---
roles_refused_row() {
  base_world
  case "$1" in
    operator)
      : >"$world/clusters/operator-cluster/refuse-roles"
      run
      want_status 2 "a refused list of roles on the operator's cluster"
      want_text "UNKNOWN live: RBAC on operator-cluster: the API server refused the roles' list (it needs list on roles and clusterroles" \
        "a refused list of roles on the operator's cluster"
      ;;
    other)
      add_cluster other-cluster us-east1
      : >"$world/clusters/other-cluster/refuse-roles"
      run
      want_status 2 "a refused list of roles on another cluster"
      want_text "UNKNOWN live: Workload Identity on $project/us-east1/other-cluster: the API server refused the roles' list (it needs list on roles and clusterroles" \
        "a refused list of roles on another cluster"
      ;;
  esac
}
if row bv; then
  each roles_refused_row operator other
  passed "(bv) a refused list of roles, on the operator's cluster or another, reads UNKNOWN naming the permission"
fi

# --- (cc) A token Secret of a guarded namespace no ServiceAccount lists, from the namespace's list of
# that type, by name; a refused list reads UNKNOWN. ---
listed_secret_row() {
  base_world
  case "$1" in
    unlisted)
      jq -n '[{namespace: "envoy-operator", name: "envoy-operator-token-legacy"}]' >"$world/clusters/operator-cluster/token-secrets.json"
      subject_grant operator-cluster "$(sa_subject legion worker)" get "" secrets "" envoy-operator envoy-operator-token-legacy
      run
      want_status 1 "legion/worker may get a token Secret no ServiceAccount lists, by name"
      want_line "FINDING system:serviceaccount:legion:worker: get secrets envoy-operator-token-legacy in envoy-operator" \
        "legion/worker may get a token Secret no ServiceAccount lists, by name"
      ;;
    refused)
      : >"$world/clusters/operator-cluster/refuse-token-secrets"
      run
      want_status 2 "a refused list of token Secrets"
      want_text "UNKNOWN live: RBAC on operator-cluster: the API server refused the list of token Secrets in envoy-operator (it needs list on secrets there, container.secrets.list on GKE)" \
        "a refused list of token Secrets"
      ;;
  esac
}
if row cc; then
  each listed_secret_row unlisted refused
  passed "(cc) a guarded namespace's token Secret no ServiceAccount lists is asked by name, and a refused list of them reads UNKNOWN"
fi
