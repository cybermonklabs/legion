# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-rbac-1.sh: the rows of operator-token-minters.test.sh on
# Kubernetes RBAC: each right of the list, a namespace's group, a binding subject on the operator's
# cluster, the broker's pair, a ServiceAccount outside the agent namespaces, and who can become a
# holder. Sourced by the test after its world and helpers; its header says what the fakes model.
# The rows:
#
#   (b)  each right of the RBAC list, allowed to an agent ServiceAccount that holds nothing else, is
#        a finding naming that ServiceAccount and that right; no finding of that ServiceAccount
#        names another planted account's right or a right of another verb or resource, and no other
#        principal has a finding (one world, one ServiceAccount a right);
#   (c)  a right allowed to the group system:serviceaccounts:legion exits 1 naming the group;
#   (dc) the same right allowed by no binding the list shows (an authorizer other than RBAC) is a
#        finding naming the group, which only namespace_groups puts under review;
#   (u)  a binding subject on the operator's own cluster: grade-ci bound to create the operator's
#        token exits 1;
#   (ab) the broker's pair is guarded: an agent ServiceAccount that may create pods in the broker's
#        namespace, or create the broker ServiceAccount's token, and another cluster of the pool
#        holding the broker's namespace, each exit 1;
#   (ac) on the operator's cluster, ci/runner, outside the agent namespaces, with create pods in
#        envoy-operator, and with cluster-admin, each exits 1;
#   (ai) a ServiceAccount that can become ci/runner, which can create pods in envoy-operator, by
#        each of create pods, create serviceaccounts/token, impersonate, create pods/exec and get
#        secrets in ci, is a finding naming the chain (one world, one ServiceAccount a way).

# --- (b) Each right of the RBAC list, allowed to an agent ServiceAccount of its own that holds
# nothing else, legion/right-<its place in the list>, all in one world and one run (plant_rights,
# want_planted). Each entry is the grant (verb, API group, resource, subresource, the binding's
# namespace or none for cluster-wide, the resource name or none) and the right the run must name. ---
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
if row b; then
  base_world
  plant_rights operator-cluster legion "" "${rbac_rights[@]}"
  run
  want_planted "each right of the RBAC list allowed to an agent ServiceAccount of its own" only
  passed "(b) each of the ${#rbac_rights[@]} rights of the RBAC list, allowed to an agent ServiceAccount that holds nothing else, is a finding naming it and the right; no finding of that ServiceAccount names another planted account's right or a right of another verb or resource, and no other principal has a finding"
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

# --- (dc) A right allowed to the group of a reviewed ServiceAccount's namespace that no listed
# binding shows, as an authorizer other than RBAC (a webhook) would allow it. Only namespace_groups
# puts that group under review, so the group's own finding shows it did; legion's ServiceAccounts,
# whose reviews carry the group, are findings either way. With namespace_groups adding no group,
# every other row passed (Tester5685, #5685). ---
if row dc; then
  base_world
  subject_grant operator-cluster "$(group_subject system:serviceaccounts:legion)" create "" serviceaccounts token envoy-operator envoy-operator
  edit "$world/clusters/operator-cluster/grants.json" '.[-1].unlisted = true'
  run
  want_status 1 "a grant to the group system:serviceaccounts:legion that no listed binding shows"
  want_line "FINDING group:system:serviceaccounts:legion: create serviceaccounts/token envoy-operator in envoy-operator" \
    "a grant to the group system:serviceaccounts:legion that no listed binding shows"
  passed "(dc) a right allowed to the group of a reviewed ServiceAccount's namespace, by no binding the list shows, is a finding naming the group"
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

# --- (ai) Who can become a ServiceAccount with a finding holds what it holds. The five ways share
# one world and one run: ci/runner, which may create pods in envoy-operator, once, and each way
# granted in ci to a becomer of its own, deploy/bot-<its place among the ways>, in deploy, which no
# grant gives a right in, so no becomer becomes another and each chain names ci/runner alone. The
# fake answers each review by its subject, so each way is still judged by its own line, exact,
# naming its own becomer; want_recorded holds the run's last line to the FINDING and UNKNOWN lines
# it printed, so a line printed and not recorded fails the row. ---
become_shared() {
  local way verb resource sub name i=0
  base_world
  add_sa operator-cluster ci runner
  subject_grant operator-cluster "$(sa_subject ci runner)" create "" pods "" envoy-operator ""
  for way in "$@"; do
    i=$((i + 1))
    IFS='|' read -r verb resource sub name <<<"${way%%=*}"
    add_sa operator-cluster deploy "bot-$i"
    subject_grant operator-cluster "$(sa_subject deploy "bot-$i")" "$verb" "" "$resource" "$sub" ci "$name"
  done
  run
  want_status 1 "deploy/bot-1 to bot-$#, each with one way to become ci/runner in ci"
  want_recorded "deploy/bot-1 to bot-$#, each with one way to become ci/runner in ci"
  i=0
  for way in "$@"; do
    i=$((i + 1))
    want_line "FINDING system:serviceaccount:deploy:bot-$i: ${way#*=} in ci, so it can become system:serviceaccount:ci:runner, which can create pods in envoy-operator" \
      "deploy/bot-$i with ${way#*=} in ci"
  done
}
if row ai; then
  become_shared "create|pods||=create pods" "create|serviceaccounts|token|runner=create serviceaccounts/token runner" \
    "impersonate|serviceaccounts||runner=impersonate serviceaccounts runner" "create|pods|exec|=create pods/exec" "get|secrets||=get secrets"
  passed "(ai) a ServiceAccount that can become one with a finding, by each of the five ways in its namespace, is a finding naming the chain"
fi
