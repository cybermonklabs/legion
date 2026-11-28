# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-rbac-2.sh: the rows of operator-token-minters.test.sh on
# Kubernetes RBAC: anyone unauthenticated, custom resources, the --agent-namespace patterns, rights
# limited by name, the namespaced ways to become a holder, and a system namespace's ServiceAccount
# that seeds a chain. Sourced by the test after its world and helpers; its header says what the
# fakes model. The rows:
#
#   (aj) the Group system:unauthenticated bound to create namespaces on another cluster is a
#        finding for anyone unauthenticated;
#   (ak) create on a custom resource (sandboxes.agents.x-k8s.io) in envoy-operator is a finding, and
#        so are update on it and create on workloads.example.k8s.io, a CRD group ending in k8s.io
#        that is not Kubernetes' own; create on Kubernetes' own leases is not, and a discovery that
#        cannot be read is UNKNOWN;
#   (am) an --agent-namespace pattern that matches nothing is UNKNOWN, naming it, beside two that
#        match;
#   (ao) rights limited by name: a ServiceAccount that may impersonate the group system:masters by
#        name, impersonate a named user bound to cluster-admin, or escalate a named clusterrole
#        bound to it is a finding naming the name (one world, one ServiceAccount a right);
#   (aq) a ServiceAccount of deploy can become ci/runner, which can create pods in envoy-operator, by
#        each of create jobs (rv5), create deployments, create pods/attach, patch
#        pods/ephemeralcontainers, patch pods, create rolebindings, bind and escalate roles, and
#        create a custom resource in ci; each is a finding naming the chain (one world, one
#        ServiceAccount a way);
#   (ar) a kube-system ServiceAccount that may mint the operator's token is no finding, but
#        legion/worker, which may create pods in kube-system, is one naming the chain (rv6); on
#        another cluster, ci/runner, which may create pods in kube-system, becomes its namespace
#        controller and is one too; and legion/worker allowed, by name in kube-system, a patch of one
#        Deployment, an exec of one pod or an ephemeral container of it is one naming the chain (N3).

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

# --- (ao) Rights limited by resource name, which a review naming nothing never finds. The three
# share one world and one run, each on a ServiceAccount of its own, legion/<item>: the user item's
# cluster-admin for alice@example.com binds a person, whom the check does not review, and the role
# item escalates the ClusterRole its own get of configmaps is bound by. live_named names the subject
# each named rule's binding gives it, so each item is still judged by its own line, exact, naming its
# own ServiceAccount; want_recorded holds the run's last line to the lines it printed. ---
named_right_shared() {
  local item subject planted want entry wants=()
  base_world
  for item in "$@"; do
    add_sa operator-cluster legion "$item"
    subject="$(sa_subject legion "$item")"
    case "$item" in
      group)
        subject_grant operator-cluster "$subject" impersonate "" groups "" "" system:masters
        want="impersonate groups system:masters cluster-wide"
        ;;
      user)
        cluster_admin operator-cluster "$(user_subject alice@example.com)"
        subject_grant operator-cluster "$subject" impersonate "" users "" "" alice@example.com
        want="impersonate users alice@example.com cluster-wide"
        ;;
      role)
        # The fake names each grant's ClusterRole planted-<its index>; this one is bound to legion/role.
        planted="planted-$(jq length "$world/clusters/operator-cluster/grants.json")"
        subject_grant operator-cluster "$subject" get "" configmaps "" legion ""
        subject_grant operator-cluster "$subject" escalate rbac.authorization.k8s.io clusterroles "" "" "$planted"
        want="escalate clusterroles.rbac.authorization.k8s.io $planted cluster-wide"
        ;;
    esac
    wants+=("$item"$'\t'"$want")
  done
  run
  want_status 1 "the rights limited by name ($*), each on a ServiceAccount of its own"
  want_recorded "the rights limited by name ($*), each on a ServiceAccount of its own"
  for entry in "${wants[@]}"; do
    item="${entry%%$'\t'*}" want="${entry#*$'\t'}"
    want_line "FINDING system:serviceaccount:legion:$item: $want" "legion/$item may $want, by name"
  done
}
if row ao; then
  named_right_shared group user role
  passed "(ao) a ServiceAccount that may impersonate the group system:masters, impersonate a named user bound to cluster-admin, or escalate a named clusterrole bound to it, each by name, exits 1 naming the name"
fi

# --- (aq) Every namespaced way to become a ServiceAccount with a finding. Each entry is a way's grant
# in ci (verb, API group, resource, subresource) and the right the run must name. The nine share one
# world and one run, as (ai)'s five do: the discovery's sandboxes and ci/runner, which may create
# pods in envoy-operator, once, and each way granted to a becomer of its own, deploy/bot-<its place
# in the list>, in deploy, which no grant gives a right in, so each chain names ci/runner alone. The
# fake answers each review by its subject, so each way is still judged by its own line, exact,
# naming its own becomer; want_recorded holds the run's last line to the lines it printed. ---
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
become_way_shared() {
  local way verb group resource sub i=0
  base_world
  echo sandboxes.agents.x-k8s.io >"$world/clusters/operator-cluster/api-resources"
  add_sa operator-cluster ci runner
  subject_grant operator-cluster "$(sa_subject ci runner)" create "" pods "" envoy-operator ""
  for way in "$@"; do
    i=$((i + 1))
    IFS='|' read -r verb group resource sub <<<"${way%%=*}"
    add_sa operator-cluster deploy "bot-$i"
    subject_grant operator-cluster "$(sa_subject deploy "bot-$i")" "$verb" "$group" "$resource" "$sub" ci ""
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
if row aq; then
  become_way_shared "${become_ways[@]}"
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
