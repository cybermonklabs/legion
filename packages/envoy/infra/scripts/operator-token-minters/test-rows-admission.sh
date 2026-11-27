# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-admission.sh: the rows of operator-token-minters.test.sh on
# admission power: the rights on the admission objects themselves, and outside them a webhook's
# backend, a policy engine's cluster-scoped resources, and an admission policy's parameter objects.
# Sourced by the test after its world and helpers; its header says what the fakes model. The rows:
#
#   (bf) each admission right (create, update and patch of mutatingwebhookconfigurations,
#        validatingwebhookconfigurations, validatingadmissionpolicies and their bindings, and of
#        mutatingadmissionpolicies and their bindings where the discovery lists them) given on the
#        operator's cluster, and create of each on another cluster, to a ServiceAccount that holds
#        nothing else, is a finding naming it, and no finding of that ServiceAccount names another
#        planted account's right or a right of another verb or resource (two worlds, one
#        ServiceAccount a right);
#   (bg) a discovery of admissionregistration.k8s.io that lists no validatingwebhookconfigurations,
#        or cannot be read on the operator's cluster or another, reads UNKNOWN;
#   (ce) a mutating webhook whose selector can match a guarded namespace, backed by the Service
#        hooks/hook: apps/deployer that may patch deployments in hooks, or create jobs (rv16),
#        cronjobs or replicationcontrollers, create pods/exec or pods/attach, or patch
#        pods/ephemeralcontainers there (N2), exits 1 naming the webhook, on the operator's cluster or
#        another; the same webhook whose namespaceSelector excludes the guarded namespaces by name
#        exits 0; a webhook that calls a URL, and a refused list of webhook configurations, read
#        UNKNOWN; apps/deployer that may update configmaps in hooks (rv17), the injector's template,
#        exits 1; and a backend Service of type ExternalName, or with no selector, reads UNKNOWN
#        (P2-4); a backend Service whose `.spec` jq cannot read ({"spec": {"selector": true}}) reads
#        UNKNOWN naming it; a webhook backed in dispatch whose selector names only legion, with a rule that can
#        match a cluster-scoped object (clusterrolebindings, `*`, or secrets in groups "" and
#        example.io where the cluster serves a cluster-scoped secrets.example.io), exits 1 naming the
#        webhook, and with rules on namespaced objects alone exits 0; a cluster-scoped discovery that
#        cannot be read whole reads UNKNOWN;
#   (cf) Kyverno's ClusterPolicy and its MutatingPolicy of policies.kyverno.io (rv15), and
#        Kubewarden's ClusterAdmissionPolicy of policies.kubewarden.io (N1), cluster-scoped:
#        legion/worker that may create any exits 1 naming it; the CRD with no holder exits 0; a
#        refused list of CRDs reads UNKNOWN;
#   (cg) a MutatingAdmissionPolicy whose binding names the ConfigMap hooks/sidecar-params as its
#        parameter: legion/worker that may update that ConfigMap by name exits 1 naming the policy;
#        a binding that names no namespace has it asked in each guarded namespace, and one with a
#        selector has update asked unnamed, each exiting 1; a parameter kind the discovery does not
#        resolve reads UNKNOWN;
#   (ci) GKE Autopilot's workload allowlists on the operator's cluster: an installed one reads
#        UNKNOWN, a refused list of them reads UNKNOWN, and legion/worker that may create
#        allowlistsynchronizers exits 1;
#   (cm) Backup for GKE installed on a cluster of the pool (a CRD of gkebackup.gke.io) reads UNKNOWN,
#        on the operator's cluster and another: a restore recreates a backup's workloads and RBAC;
#   (cu) each answer the check reads with kubectl -o json (the webhook configurations, the CRDs, the
#        workload allowlists, the admission policies and their bindings, the roles, the bindings,
#        the ServiceAccounts, a webhook's Service and the answer to the reviews), sent with exit 0
#        but not as the API server wrote it (an error page, an empty body, `{}`, for the roles a
#        List with no item, and for the roles and the bindings a List that holds one of its two
#        kinds alone), reads UNKNOWN naming it, exits 2, and its step does not pass, though a
#        principal holds a right only that answer reveals, which the item names;
#   (cy) the answer to the reviews, as kubectl create writes it, one document per review: two or
#        more so answered are judged; an answer that repeats one review and leaves out another, or
#        that holds a document without a status, reads UNKNOWN naming the count; and a follow round
#        whose answer refuses every review, its witness among them, or holds no status, reads
#        UNKNOWN for the witness rather than pass a seeded chain.
#   (cz) review's kubectl calls on a cluster are REVIEW_WORKERS, capped at the nominal seats the
#        API server gives global-default; one call when its /metrics cannot be read or holds no
#        such seats; UNKNOWN for a REVIEW_WORKERS that is no whole number above 0; and UNKNOWN
#        when one later batch alone is refused or answers short.

# --- (bf) The admission rights. A mutating webhook or policy can add a container to the operator's
# next pod, and a validating one can block or reshape what the operator applies. Create, update and
# patch of each resource of admissionregistration.k8s.io on the operator's cluster, each given to
# legion/right-<its place>, and create of each on another of the pool, each given to
# ci/right-<its place>, each a ServiceAccount that holds nothing else (plant_rights). A GKE
# cluster's discovery lists the four resources of admission_gke; the mutating admission policies,
# beta and off by default, are listed in a world of their own, since the check asks what the
# cluster's discovery lists. So the row is two worlds and two runs. ---
admission_gke=(mutatingwebhookconfigurations validatingwebhookconfigurations validatingadmissionpolicies validatingadmissionpolicybindings)
admission_mutating=(mutatingadmissionpolicies mutatingadmissionpolicybindings)
# admission_rights_row RESOURCE...: the admission rights on each RESOURCE, in one world whose two
# clusters' discovery lists admission_gke's resources and each RESOURCE.
admission_rights_row() {
  local resource verb cluster operator=() other=()
  base_world
  add_cluster other-cluster us-east1
  for resource in "$@"; do
    for verb in create update patch; do
      operator+=("$verb|admissionregistration.k8s.io|$resource|||=$verb $resource.admissionregistration.k8s.io cluster-wide")
    done
    other+=("create|admissionregistration.k8s.io|$resource|||=create $resource.admissionregistration.k8s.io cluster-wide")
  done
  # The discovery lists each name once, in its order, as an API server does.
  for cluster in operator-cluster other-cluster; do
    printf '%s.admissionregistration.k8s.io\n' "${admission_gke[@]}" "$@" | sort -u >"$world/clusters/$cluster/admission-resources"
  done
  plant_rights operator-cluster legion "" "${operator[@]}"
  plant_rights other-cluster ci " on cluster $project/us-east1/other-cluster" "${other[@]}"
  run
  want_planted "the admission rights on $*, each given to a ServiceAccount of its own" own
}
if row bf; then
  admission_rights_row "${admission_gke[@]}"
  admission_rights_row "${admission_mutating[@]}"
  # Four rights a resource: create, update and patch on the operator's cluster, create on another.
  passed "(bf) each of the $((4 * (${#admission_gke[@]} + ${#admission_mutating[@]}))) admission rights, given on the operator's cluster or another of the pool where its discovery lists the resource to a ServiceAccount that holds nothing else, is a finding naming it, and no finding of that ServiceAccount names another planted account's right or a right of another verb or resource"
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

# --- (ce) A webhook's backend: whoever may write its Service, the workload behind it, its endpoints
# or its TLS Secret decides what the webhook does to the operator's next pod. A webhook whose rules
# can match a cluster-scoped object is judged whatever its namespaceSelector says, since the API
# server applies none to an object with no namespace: one backed in dispatch whose selector names
# only legion, with a rule on clusterrolebindings, on `*`, or on secrets in the groups "" and
# example.io where the cluster serves a cluster-scoped secrets.example.io, is counted; the same
# selector with a rule on pods, or on `*` of scope Namespaced, is not. Run by hand, the control for
# the cluster items is webhook-backends.jq's can_match as at bfb6ea5c85, which read only the
# selector: each exits 0; and for the crd-rule item, cluster_rule without its $cluster test, which
# counted secrets namespaced because the core group serves it so: it exits 0. ---
# hook_config SELECTOR CLIENT [RULES]: one MutatingWebhookConfiguration, hook-config, of one webhook,
# pods.example.com, calling CLIENT; SELECTOR exclude names the guarded namespaces out, legion names
# legion alone in, and empty sets none; RULES, a JSON array, its rules (none when absent).
hook_config() {
  jq -n --arg selector "${1:-}" --argjson client "$2" --argjson rules "${3:-null}" '[{kind: "MutatingWebhookConfiguration", metadata: {name: "hook-config"},
    webhooks: [{name: "pods.example.com", clientConfig: $client}
      + (if $selector == "exclude" then {namespaceSelector: {matchExpressions: [{key: "kubernetes.io/metadata.name", operator: "NotIn",
          values: ["envoy-operator", "secrets-broker"]}]}}
         elif $selector == "legion" then {namespaceSelector: {matchLabels: {"kubernetes.io/metadata.name": "legion"}}}
         else {} end)
      + (if $rules != null then {rules: $rules} else {} end)]}]'
}
backend_row() {
  local service='{"service": {"namespace": "hooks", "name": "hook"}}' want
  base_world
  case "$1" in
    backend)
      hook_config "" "$service" >"$world/clusters/operator-cluster/webhooks.json"
      subject_grant operator-cluster "$(sa_subject apps deployer)" patch apps deployments "" hooks ""
      run
      want_status 1 "apps/deployer may patch the webhook backend's Deployment"
      want_line "FINDING system:serviceaccount:apps:deployer: patch deployments.apps in hooks, backing the webhook MutatingWebhookConfiguration hook-config/pods.example.com" \
        "apps/deployer may patch the webhook backend's Deployment"
      ;;
    other-cluster)
      add_cluster other-cluster us-east1
      add_sa other-cluster ci runner
      hook_config "" "$service" >"$world/clusters/other-cluster/webhooks.json"
      subject_grant other-cluster "$(sa_subject ci runner)" update "" services "" hooks ""
      run
      want_status 1 "ci/runner may update the webhook backend's Service on another cluster"
      want_line "FINDING system:serviceaccount:ci:runner: update services in hooks, backing the webhook MutatingWebhookConfiguration hook-config/pods.example.com on cluster $project/us-east1/other-cluster" \
        "ci/runner may update the webhook backend's Service on another cluster"
      ;;
    jobs | cronjobs | replicationcontrollers | exec | attach | ephemeralcontainers)
      # rv16 and N2: each pod-making or pod-entry right RIGHTS_TABLE asks a guarded namespace.
      local grant right verb group resource sub
      case "$1" in
        jobs) grant="create|batch|jobs|" right="create jobs.batch" ;;
        cronjobs) grant="create|batch|cronjobs|" right="create cronjobs.batch" ;;
        replicationcontrollers) grant="create||replicationcontrollers|" right="create replicationcontrollers" ;;
        exec) grant="create||pods|exec" right="create pods/exec" ;;
        attach) grant="create||pods|attach" right="create pods/attach" ;;
        ephemeralcontainers) grant="patch||pods|ephemeralcontainers" right="patch pods/ephemeralcontainers" ;;
      esac
      IFS='|' read -r verb group resource sub <<<"$grant"
      hook_config "" "$service" >"$world/clusters/operator-cluster/webhooks.json"
      subject_grant operator-cluster "$(sa_subject apps deployer)" "$verb" "$group" "$resource" "$sub" hooks ""
      run
      want_status 1 "apps/deployer may $right in the webhook backend's namespace"
      want_line "FINDING system:serviceaccount:apps:deployer: $right in hooks, backing the webhook MutatingWebhookConfiguration hook-config/pods.example.com" \
        "apps/deployer may $right in the webhook backend's namespace"
      ;;
    excluded)
      hook_config exclude "$service" >"$world/clusters/operator-cluster/webhooks.json"
      subject_grant operator-cluster "$(sa_subject apps deployer)" patch apps deployments "" hooks ""
      run
      want_status 0 "a webhook whose selector excludes the guarded namespaces"
      ;;
    cluster-rule | star-rule | crd-rule | namespaced-rule | star-namespaced)
      case "$1" in
        cluster-rule) rules='[{operations: ["CREATE", "UPDATE"], apiGroups: ["rbac.authorization.k8s.io"], apiVersions: ["v1"], resources: ["clusterrolebindings"]}]' ;;
        star-rule) rules='[{operations: ["*"], apiGroups: ["*"], apiVersions: ["*"], resources: ["*"]}]' ;;
        crd-rule)
          rules='[{operations: ["CREATE"], apiGroups: ["", "example.io"], apiVersions: ["*"], resources: ["secrets"]}]'
          echo secrets.example.io >"$world/clusters/operator-cluster/cluster-resources"
          ;;
        namespaced-rule) rules='[{operations: ["CREATE"], apiGroups: [""], apiVersions: ["v1"], resources: ["pods"]}]' ;;
        star-namespaced) rules='[{operations: ["*"], apiGroups: ["*"], apiVersions: ["*"], resources: ["*"], scope: "Namespaced"}]' ;;
      esac
      hook_config legion '{"service": {"namespace": "dispatch", "name": "hook"}}' "$(jq -n -c "$rules")" >"$world/clusters/operator-cluster/webhooks.json"
      subject_grant operator-cluster "$(sa_subject apps deployer)" patch apps deployments "" dispatch ""
      run
      case "$1" in
        cluster-rule | star-rule | crd-rule)
          want_status 1 "a webhook backed in dispatch whose selector names only legion, with a rule that can match a cluster-scoped object ($1)"
          want_line "FINDING system:serviceaccount:apps:deployer: patch deployments.apps in dispatch, backing the webhook MutatingWebhookConfiguration hook-config/pods.example.com" \
            "a webhook backed in dispatch whose selector names only legion, with a rule that can match a cluster-scoped object ($1)"
          ;;
        *)
          want_status 0 "a webhook backed in dispatch whose selector names only legion, with rules on namespaced objects alone ($1)"
          ;;
      esac
      ;;
    url)
      hook_config "" '{"url": "https://hooks.example.com/mutate"}' >"$world/clusters/operator-cluster/webhooks.json"
      run
      want_status 2 "a webhook that calls a URL"
      want_line "UNKNOWN live: RBAC on operator-cluster: the webhook MutatingWebhookConfiguration hook-config/pods.example.com can match a guarded namespace or a cluster-scoped object and calls https://hooks.example.com/mutate, outside the cluster, whose holders this check cannot read" \
        "a webhook that calls a URL"
      ;;
    configmaps)
      hook_config "" "$service" >"$world/clusters/operator-cluster/webhooks.json"
      subject_grant operator-cluster "$(sa_subject apps deployer)" update "" configmaps "" hooks ""
      run
      want_status 1 "apps/deployer may update configmaps in the webhook backend's namespace (rv17)"
      want_line "FINDING system:serviceaccount:apps:deployer: update configmaps in hooks, backing the webhook MutatingWebhookConfiguration hook-config/pods.example.com" \
        "apps/deployer may update configmaps in the webhook backend's namespace (rv17)"
      ;;
    external-name | no-selector)
      hook_config "" "$service" >"$world/clusters/operator-cluster/webhooks.json"
      if [ "$1" = external-name ]; then
        jq -n '{"hooks/hook": {spec: {type: "ExternalName", externalName: "hooks.example.com"}}}' >"$world/clusters/operator-cluster/services.json"
        want="is of type ExternalName (hooks.example.com), so admission calls leave the cluster"
      else
        jq -n '{"hooks/hook": {spec: {type: "ClusterIP"}}}' >"$world/clusters/operator-cluster/services.json"
        want="has no selector, so its endpoints can point anywhere, inside the cluster or out"
      fi
      run
      want_status 2 "a webhook backend Service that $want"
      want_line "UNKNOWN live: RBAC on operator-cluster: the webhook MutatingWebhookConfiguration hook-config/pods.example.com calls the Service hooks/hook, which $want, whose holders this check cannot read" \
        "a webhook backend Service that $want"
      ;;
    spec-unreadable)
      hook_config "" "$service" >"$world/clusters/operator-cluster/webhooks.json"
      # One JSON object, so is_object passes, whose .spec.selector jq cannot take the length of.
      jq -n '{"hooks/hook": {spec: {selector: true}}}' >"$world/clusters/operator-cluster/services.json"
      run
      want_rbac_unknown "the webhook MutatingWebhookConfiguration hook-config/pods.example.com calls the Service hooks/hook, whose spec could not be read (jq: error" \
        "a webhook backend Service whose spec jq cannot read"
      ;;
    refused)
      : >"$world/clusters/operator-cluster/refuse-webhooks"
      run
      want_status 2 "a refused list of webhook configurations"
      want_text "UNKNOWN live: RBAC on operator-cluster: the API server refused the list of webhook configurations" "a refused list of webhook configurations"
      ;;
    refused-cluster-discovery)
      : >"$world/clusters/operator-cluster/refuse-cluster-resources"
      run
      want_rbac_unknown "the API server's discovery of cluster-scoped resources could not be read whole, so a webhook rule on a resource it leaves out is judged as that resource's namespaced name alone: " \
        "a cluster-scoped discovery that cannot be read whole"
      ;;
  esac
}
if row ce; then
  each backend_row backend jobs cronjobs replicationcontrollers exec attach ephemeralcontainers configmaps other-cluster excluded url \
    external-name no-selector spec-unreadable refused cluster-rule star-rule crd-rule namespaced-rule star-namespaced refused-cluster-discovery
  passed "(ce) a write to a matching webhook's backend (a Deployment's patch, N2's jobs, cronjobs, replicationcontrollers, pods/exec, pods/attach and pods/ephemeralcontainers, and rv17's configmaps) exits 1 naming the webhook, on the operator's cluster or another, and so does one whose rule can match a cluster-scoped object whatever its selector; an excluding selector exits 0; a URL webhook, an ExternalName or selector-less Service, one whose spec jq cannot read, a refused list and a cluster-scoped discovery that cannot be read whole read UNKNOWN"
fi

# --- (cf) A policy engine's cluster-scoped resources: Kyverno's ClusterPolicy mutates pods. ---
policy_crd_row() {
  base_world
  jq -n '[{spec: {group: "kyverno.io", scope: "Cluster", names: {plural: "clusterpolicies", kind: "ClusterPolicy"}}}]' \
    >"$world/clusters/operator-cluster/crds.json"
  case "$1" in
    holder)
      subject_grant operator-cluster "$(sa_subject legion worker)" create kyverno.io clusterpolicies "" "" ""
      run
      want_status 1 "legion/worker may create Kyverno ClusterPolicies"
      want_line "FINDING system:serviceaccount:legion:worker: create clusterpolicies.kyverno.io cluster-wide, a policy engine's resource" \
        "legion/worker may create Kyverno ClusterPolicies"
      ;;
    mutating-policy)
      jq -n '[{spec: {group: "policies.kyverno.io", scope: "Cluster", names: {plural: "mutatingpolicies", kind: "MutatingPolicy"}}}]' \
        >"$world/clusters/operator-cluster/crds.json"
      subject_grant operator-cluster "$(sa_subject legion worker)" create policies.kyverno.io mutatingpolicies "" "" ""
      run
      want_status 1 "legion/worker may create Kyverno MutatingPolicies"
      want_line "FINDING system:serviceaccount:legion:worker: create mutatingpolicies.policies.kyverno.io cluster-wide, a policy engine's resource" \
        "legion/worker may create Kyverno MutatingPolicies"
      ;;
    kubewarden)
      jq -n '[{spec: {group: "policies.kubewarden.io", scope: "Cluster", names: {plural: "clusteradmissionpolicies", kind: "ClusterAdmissionPolicy"}}}]' \
        >"$world/clusters/operator-cluster/crds.json"
      subject_grant operator-cluster "$(sa_subject legion worker)" create policies.kubewarden.io clusteradmissionpolicies "" "" ""
      run
      want_status 1 "legion/worker may create Kubewarden ClusterAdmissionPolicies"
      want_line "FINDING system:serviceaccount:legion:worker: create clusteradmissionpolicies.policies.kubewarden.io cluster-wide, a policy engine's resource" \
        "legion/worker may create Kubewarden ClusterAdmissionPolicies"
      ;;
    no-holder)
      run
      want_status 0 "Kyverno's ClusterPolicy with no holder"
      ;;
    refused)
      : >"$world/clusters/operator-cluster/refuse-crds"
      run
      want_status 2 "a refused list of CRDs"
      want_text "UNKNOWN live: RBAC on operator-cluster: the API server refused the list of custom resource definitions" "a refused list of CRDs"
      ;;
  esac
}
if row cf; then
  each policy_crd_row holder mutating-policy kubewarden no-holder refused
  passed "(cf) a holder of create on Kyverno's ClusterPolicy or MutatingPolicy (rv15), or Kubewarden's ClusterAdmissionPolicy, exits 1; the CRD with no holder exits 0; a refused list of CRDs reads UNKNOWN"
fi

# --- (cg) An admission policy's parameter object, named by its binding's paramRef. ---
param_row() {
  local dir="$world/clusters/operator-cluster"
  base_world
  printf '%s.admissionregistration.k8s.io\n' mutatingadmissionpolicies mutatingadmissionpolicybindings mutatingwebhookconfigurations \
    validatingadmissionpolicies validatingadmissionpolicybindings validatingwebhookconfigurations >"$dir/admission-resources"
  jq -n --arg api "$([ "$1" = unresolved ] && echo example.com/v1 || echo v1)" --arg kind "$([ "$1" = unresolved ] && echo Widget || echo ConfigMap)" '[
    {kind: "MutatingAdmissionPolicy", metadata: {name: "inject-sidecar"}, spec: {paramKind: {apiVersion: $api, kind: $kind}}},
    {kind: "MutatingAdmissionPolicyBinding", metadata: {name: "inject-sidecar-binding"},
     spec: {policyName: "inject-sidecar", paramRef: (if $ref == "no-namespace" then {name: "sidecar-params"}
       elif $ref == "selector" then {namespace: "hooks", selector: {matchLabels: {app: "sidecar"}}}
       else {name: "sidecar-params", namespace: "hooks"} end)}}]' --arg ref "$1" >"$dir/admission-policies.json"
  jq -n '{"/api/v1": {resources: [{name: "configmaps", kind: "ConfigMap", namespaced: true}]}}' >"$dir/raw.json"
  case "$1" in
    holder)
      subject_grant operator-cluster "$(sa_subject legion worker)" update "" configmaps "" hooks sidecar-params
      run
      want_status 1 "legion/worker may update the admission policy's parameter by name"
      want_line "FINDING system:serviceaccount:legion:worker: update configmaps sidecar-params in hooks, a parameter of the admission policy MutatingAdmissionPolicy inject-sidecar" \
        "legion/worker may update the admission policy's parameter by name"
      ;;
    no-namespace)
      subject_grant operator-cluster "$(sa_subject legion worker)" update "" configmaps "" envoy-operator sidecar-params
      run
      want_status 1 "legion/worker may update the parameter by name in a guarded namespace"
      want_line "FINDING system:serviceaccount:legion:worker: update configmaps sidecar-params in envoy-operator, a parameter of the admission policy MutatingAdmissionPolicy inject-sidecar" \
        "legion/worker may update the parameter by name in a guarded namespace"
      ;;
    selector)
      subject_grant operator-cluster "$(sa_subject legion worker)" update "" configmaps "" hooks ""
      run
      want_status 1 "legion/worker may update the parameters a selector picks"
      want_line "FINDING system:serviceaccount:legion:worker: update configmaps in hooks, a parameter of the admission policy MutatingAdmissionPolicy inject-sidecar" \
        "legion/worker may update the parameters a selector picks"
      ;;
    unresolved)
      run
      want_status 2 "a parameter kind the discovery does not resolve"
      want_line "UNKNOWN live: RBAC on operator-cluster: the parameter kind Widget (example.com/v1) of the admission policy MutatingAdmissionPolicy inject-sidecar is not resolved by the API server's discovery, so who may write its parameters is not reviewed" \
        "a parameter kind the discovery does not resolve"
      ;;
  esac
}
if row cg; then
  each param_row holder no-namespace selector unresolved
  passed "(cg) a holder of update on an admission policy's parameter, by the binding's name, in a guarded namespace when the binding names none, or unnamed for a selector, exits 1; an unresolved parameter kind reads UNKNOWN"
fi

# --- (ci) GKE Autopilot's workload allowlists: an installed one admits the privileged workloads it
# names, which Autopilot otherwise refuses, and an AllowlistSynchronizer installs them. ---
allowlist_row() {
  local dir="$world/clusters/operator-cluster"
  base_world
  jq -n '[{spec: {group: "auto.gke.io", scope: "Cluster", names: {plural: "workloadallowlists", kind: "WorkloadAllowlist"}}}]' >"$dir/crds.json"
  case "$1" in
    installed)
      echo '["example-node-agent"]' >"$dir/allowlists.json"
      run
      want_status 2 "an installed workload allowlist"
      want_line "UNKNOWN live: RBAC on operator-cluster: the workload allowlist example-node-agent is installed, so Autopilot admits the privileged workloads it names, which this check does not read" \
        "an installed workload allowlist"
      ;;
    refused)
      : >"$dir/refuse-allowlists"
      run
      want_status 2 "a refused list of workload allowlists"
      want_text "UNKNOWN live: RBAC on operator-cluster: the API server refused the list of workload allowlists" "a refused list of workload allowlists"
      ;;
    synchronizer)
      subject_grant operator-cluster "$(sa_subject legion worker)" create auto.gke.io allowlistsynchronizers "" "" ""
      run
      want_status 1 "legion/worker may create allowlistsynchronizers"
      want_line "FINDING system:serviceaccount:legion:worker: create allowlistsynchronizers.auto.gke.io cluster-wide, which install the workload allowlists Autopilot admits privileged pods by" \
        "legion/worker may create allowlistsynchronizers"
      ;;
  esac
}
if row ci; then
  each allowlist_row installed refused synchronizer
  passed "(ci) an installed workload allowlist and a refused list of them read UNKNOWN, and a holder of create on allowlistsynchronizers exits 1"
fi

# --- (cm) Backup for GKE: a restore, chosen in Google Cloud by a holder of its restore rights,
# recreates a backup's workloads and RBAC in the cluster, which no review here sees. ---
backup_row() {
  local cluster=operator-cluster step="live: RBAC on operator-cluster"
  base_world
  if [ "$1" = other-cluster ]; then
    add_cluster other-cluster us-east1
    cluster=other-cluster step="live: Workload Identity on $project/us-east1/other-cluster"
  fi
  jq -n '[{spec: {group: "gkebackup.gke.io", scope: "Namespaced", names: {plural: "protectedapplications", kind: "ProtectedApplication"}}}]' \
    >"$world/clusters/$cluster/crds.json"
  run
  want_status 2 "Backup for GKE installed on $cluster"
  want_line "UNKNOWN $step: Backup for GKE is installed (the gkebackup.gke.io API), so a restore can recreate a backup's workloads and RBAC here, chosen in Google Cloud by a holder of its restore rights, which this check does not read" \
    "Backup for GKE installed on $cluster"
}
if row cm; then
  each backup_row operator other-cluster
  passed "(cm) Backup for GKE installed on the operator's cluster or another reads UNKNOWN"
fi

# --- (cu) An answer read with kubectl -o json, sent with exit 0 but not as the API server wrote it:
# a proxy's error page, an empty body (jq reads no value in it, prints nothing and exits 0), or `{}`
# (jq's .items[]? is nothing there); for the roles, a List with no item; and for the roles and the
# bindings, a List that holds one of its two kinds alone. Each list and object is held to its shape
# first (is_list, is_object), and the roles' and the bindings' lists each to two witnesses, one of
# each kind, labelled kubernetes.io/bootstrapping=rbac-defaults (the ClusterRole cluster-admin and
# the Role kube-system/extension-apiserver-authentication-reader; the ClusterRoleBinding
# cluster-admin and the RoleBinding kube-system/system::extension-apiserver-authentication-reader);
# the answer to the reviews is no list but one document per review, which review-answer.jq holds to
# its count and its shape. So each reads UNKNOWN naming it, exits 2, runs to its end, and its step
# does not pass. Each world holds what the answer would show and a principal a right only that
# answer reveals, which the item names, so a run that took the answer for an empty one would miss
# it: the webhook's backend Service, the policy engine's resource, the installed allowlist, the
# admission policy's parameter, a right a role limits by name (for the empty List, the reviewer's
# impersonation of system:masters limited to that name; for the roles with no Role, legion/worker's
# patch of the Deployment secrets-broker, by a Role's name), legion/worker's bind of clusterroles in
# example-db for the bindings with no RoleBinding, and (the answer to the reviews) legion/worker's
# create of pods in envoy-operator, or, on another cluster, whose reviews the Workload Identity step
# makes, ci/runner's create of namespaces. The other bindings items and the ServiceAccounts have no
# planted right: the first names the subjects reviewed, the second the accounts, and the
# ServiceAccounts' list has its witness, default/default. Run by hand, the control is the script and
# its parts as at bfb6ea5c85 for the lists and as at 33e20b2de7 for the reviews: the allowlists,
# policies and roles items with an empty
# body or `{}`, the CRDs and webhooks with `{}`, the Service with an empty body, and the reviews with
# an empty body each pass their step and exit 0 (the operator's reviews with "16 principals …: none
# allowed"), and the webhooks with an empty body stop the run mid-way; and as at 846057fdc7 for the
# empty roles List, which passes its step and exits 0; and as at 26ab8c9a80 for the half lists (the
# roles with no Role, the bindings with no RoleBinding), each of which passes its step and exits 0
# on a world whose whole list exits 1. ---
unread_list_row() {
  local dir="$world/clusters/operator-cluster" list="${1%-*}" shape="${1##*-}" want why missed
  local step="RBAC on operator-cluster" marker="${1%-*}"
  base_world
  case "$shape" in
    garbage) why="" ;;
    empty) why="the answer was empty" ;;
    object) why="the answer is JSON, but not one object with a list of items" ;;
    emptylist | halflist) why="" ;;
  esac
  case "$list" in
    webhooks)
      hook_config "" '{"service": {"namespace": "hooks", "name": "hook"}}' >"$dir/webhooks.json"
      subject_grant operator-cluster "$(sa_subject legion worker)" update "" services "" hooks hook
      want="the API server's list of webhook configurations could not be read, so no webhook's backend is reviewed: $why"
      missed="legion/worker's update of the webhook's backend Service hooks/hook"
      ;;
    service)
      hook_config "" '{"service": {"namespace": "hooks", "name": "hook"}}' >"$dir/webhooks.json"
      jq -n '{"hooks/hook": {spec: {type: "ExternalName", externalName: "hooks.example.com"}}}' >"$dir/services.json"
      want="the webhook MutatingWebhookConfiguration hook-config/pods.example.com calls the Service hooks/hook, whose answer is not one JSON object ($why"
      [ -z "$why" ] || want+="), so where it sends admission calls is not read"
      missed="the backend Service's type ExternalName"
      ;;
    crds)
      jq -n '[{spec: {group: "kyverno.io", scope: "Cluster", names: {plural: "clusterpolicies", kind: "ClusterPolicy"}}}]' >"$dir/crds.json"
      subject_grant operator-cluster "$(sa_subject legion worker)" create kyverno.io clusterpolicies "" "" ""
      want="the API server's list of custom resource definitions could not be read, so a policy engine's cluster-scoped resources are not reviewed: $why"
      missed="legion/worker's create on clusterpolicies.kyverno.io"
      ;;
    allowlists)
      jq -n '[{spec: {group: "auto.gke.io", scope: "Cluster", names: {plural: "workloadallowlists", kind: "WorkloadAllowlist"}}}]' >"$dir/crds.json"
      echo '["example-node-agent"]' >"$dir/allowlists.json"
      want="the API server's list of workload allowlists could not be read, so whether Autopilot admits a privileged pod here is unknown: $why"
      missed="the installed workload allowlist example-node-agent"
      ;;
    policies)
      printf '%s.admissionregistration.k8s.io\n' mutatingadmissionpolicies mutatingadmissionpolicybindings mutatingwebhookconfigurations \
        validatingadmissionpolicies validatingadmissionpolicybindings validatingwebhookconfigurations >"$dir/admission-resources"
      jq -n '[{kind: "MutatingAdmissionPolicy", metadata: {name: "inject-sidecar"}, spec: {paramKind: {apiVersion: "v1", kind: "ConfigMap"}}},
        {kind: "MutatingAdmissionPolicyBinding", metadata: {name: "inject-sidecar-binding"},
         spec: {policyName: "inject-sidecar", paramRef: {name: "sidecar-params", namespace: "hooks"}}}]' >"$dir/admission-policies.json"
      jq -n '{"/api/v1": {resources: [{name: "configmaps", kind: "ConfigMap", namespaced: true}]}}' >"$dir/raw.json"
      subject_grant operator-cluster "$(sa_subject legion worker)" update "" configmaps "" hooks sidecar-params
      want="the API server's list of admission policies and their bindings could not be read, so a policy's parameter objects are not reviewed: $why"
      missed="legion/worker's update on configmaps/sidecar-params in hooks"
      ;;
    roles)
      if [ "$shape" = emptylist ]; then
        subject_grant operator-cluster "$(sa_subject legion worker)" impersonate "" groups "" "" system:masters
        want="the API server's list of roles holds no bootstrap ClusterRole cluster-admin (labelled kubernetes.io/bootstrapping=rbac-defaults), which every cluster has, so it is not whole and a right a role limits by name is not read"
        missed="legion/worker's impersonation of the group system:masters, by name"
      elif [ "$shape" = halflist ]; then
        # The reviewer's Role-limited path: a Role in secrets-broker that lets legion/worker patch
        # the Deployment secrets-broker, by name; the list answers its ClusterRoles alone.
        subject_grant operator-cluster "$(sa_subject legion worker)" patch apps deployments "" secrets-broker secrets-broker
        edit "$dir/grants.json" '.[-1].role_kind = "Role"'
        : >"$dir/no-roles"
        want="the API server's list of roles holds no bootstrap Role kube-system/extension-apiserver-authentication-reader (labelled kubernetes.io/bootstrapping=rbac-defaults), which every cluster has, so its Roles are not whole and a right a Role limits by name is not read"
        missed="legion/worker's patch of the Deployment secrets-broker, by a Role's name"
      else
        subject_grant operator-cluster "$(sa_subject legion worker)" update admissionregistration.k8s.io mutatingwebhookconfigurations "" "" existing-webhook
        want="the API server's list of roles could not be read, so a right a role limits by name is not read: $why"
        missed="legion/worker's update of mutatingwebhookconfigurations existing-webhook, by name"
      fi
      ;;
    bindings)
      if [ "$shape" = halflist ]; then
        # The reviewer's example-db world, the list answering its ClusterRoleBindings alone:
        # binding_rights would ask nothing in example-db.
        subject_grant operator-cluster "$(user_subject dba@example.com)" get "" secrets "" example-db broker-db-login
        subject_grant operator-cluster "$(sa_subject legion worker)" create rbac.authorization.k8s.io rolebindings "" example-db ""
        subject_grant operator-cluster "$(sa_subject legion worker)" bind rbac.authorization.k8s.io clusterroles "" example-db ""
        : >"$dir/no-rolebindings"
        want="the bindings' list holds no bootstrap RoleBinding kube-system/system::extension-apiserver-authentication-reader (labelled kubernetes.io/bootstrapping=rbac-defaults), which every cluster has, so its RoleBindings are not whole and a right given in a namespace outside the guarded ones is not read"
        missed="legion/worker's bind of clusterroles in example-db"
      else
        want="the API server's list of bindings could not be read: $why"
        missed="the bindings' subjects"
      fi
      ;;
    serviceaccounts)
      want="the API server's list of ServiceAccounts could not be read: $why"
      missed="the cluster's ServiceAccounts"
      ;;
    reviews | reviews-other)
      # The answer to the reviews is not a list but one document per review (review-answer.jq), so
      # each shape reads as that program says: an empty answer holds no review of the pairs asked,
      # `{}` is one document that is no SubjectAccessReview, and an error page is no JSON at all.
      case "$shape" in
        garbage) want="the API server's answer to the reviews could not be read: " ;;
        empty) want="the API server answered 0 of the " ;;
        object) want="the API server's answer to the reviews could not be read: " ;;
      esac
      if [ "$1" = "${1#reviews-other}" ]; then
        subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods "" envoy-operator ""
        missed="legion/worker's create of pods in envoy-operator"
      else
        add_cluster other-cluster us-east1
        add_sa other-cluster ci runner
        subject_grant other-cluster "$(sa_subject ci runner)" create "" namespaces "" "" ""
        dir="$world/clusters/other-cluster" marker=reviews step="Workload Identity on $project/us-east1/other-cluster"
        missed="ci/runner's create of namespaces on another cluster"
      fi
      ;;
  esac
  : >"$dir/$shape-$marker"
  run
  want_step_unknown "$step" "$want" "the answer of $list sent as $shape, where $missed would go unseen"
}
if row cu; then
  each unread_list_row webhooks-garbage webhooks-empty webhooks-object service-garbage service-empty \
    crds-garbage crds-empty crds-object allowlists-garbage allowlists-empty allowlists-object \
    policies-garbage policies-empty policies-object roles-garbage roles-empty roles-object roles-emptylist roles-halflist \
    bindings-garbage bindings-empty bindings-object bindings-halflist serviceaccounts-garbage serviceaccounts-empty serviceaccounts-object \
    reviews-garbage reviews-empty reviews-object reviews-other-empty reviews-other-object
  passed "(cu) each answer read with kubectl -o json (every list, a webhook's Service, the answer to the reviews on the operator's cluster or another), sent with exit 0 as an error page, an empty body or {}, the roles' list as a List with no item, and the roles' and the bindings' lists with one of their two kinds alone, reads UNKNOWN naming it, exits 2, and its step does not pass"
fi

# --- (cy) The answer to the reviews, as kubectl create writes it: given a List, kubectl creates each
# object on its own and prints one JSON document per object, never a List (kubectl 1.35.9's
# create.go; measured at 1.36.4 against a mock API server: a List of 500 reviews made 500 POSTs and
# printed 500 documents). review-answer.jq reads that answer, holds each document to a
# SubjectAccessReview with an object status, and counts the principal-right pairs it answers against
# the reviews asked. The items:
#   judged: two reviews so answered are judged, and the grant in them is a finding;
#   repeated: the answer repeats the first review in place of the last, keeping the count, so one
#     pair is missing: UNKNOWN naming the pairs answered, the reviews asked and the documents;
#   witness-copied: the witness's reviews are copies of another, which is the same fault;
#   follow-refused and follow-nostatus: the step's own reviews are answered as they are, and each
#     follow_becomes round after them refuses every review, the witness's among them, or holds no
#     status; the round reads UNKNOWN for the witness rather than pass the seeded chain.
# Run by hand, the control is the script and its parts as at dabc806da7, whose review held the
# answer to is_list: judged reads "UNKNOWN live: RBAC on operator-cluster: the API server's answer
# to the reviews could not be read: the answer is JSON, but not one object with a list of items",
# exit 2, so no live step could ever judge a review; and with the fake's List of that head, which
# that code reads, follow-refused and follow-nostatus exit 0 with "PASS live: RBAC on
# operator-cluster … none allowed", where the chain is a finding. ---
review_answer_row() {
  local dir="$world/clusters/operator-cluster" chain
  base_world
  case "$1" in
    judged | chunked | repeated | witness-copied)
      subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods "" envoy-operator ""
      ;;
    follow-refused | follow-nostatus)
      # A seeded chain: kube-system/generic-garbage-collector may create the operator's token, and
      # legion/worker may create pods in kube-system, so follow_becomes finds the chain in a round
      # after the step's own reviews.
      subject_grant operator-cluster "$(sa_subject kube-system generic-garbage-collector)" create "" serviceaccounts token envoy-operator envoy-operator
      subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods "" kube-system ""
      ;;
  esac
  case "$1" in
    repeated) : >"$dir/review-repeated" ;;
    witness-copied) : >"$dir/review-witness-copied" ;;
    follow-refused) : >"$dir/review-follow-refused" ;;
    follow-nostatus) : >"$dir/review-follow-nostatus" ;;
  esac
  # The chunked item asks the same reviews in four kubectl calls at once, as a live run does with
  # REVIEW_WORKERS workers: the finding and the run's exit are the same, and review reads the
  # chunks back in the order it asked them.
  if [ "$1" = chunked ]; then REVIEW_WORKERS=4 run; else run; fi
  case "$1" in
    judged | chunked)
      want_status 1 "two or more reviews answered as separate documents ($1)"
      want_line "FINDING system:serviceaccount:legion:worker: create pods in envoy-operator" \
        "two or more reviews answered as separate documents ($1)"
      ! has_prefix "UNKNOWN live: RBAC on operator-cluster: the API server's answer to the reviews" ||
        fail "two or more reviews answered as separate documents ($1): the answer could not be read"
      ;;
    repeated | witness-copied)
      want_rbac_unknown "the API server answered " "an answer that repeats one review and leaves out another ($1)"
      want_text " of the " "an answer that repeats one review and leaves out another ($1)"
      want_text " reviews asked, in " "an answer that repeats one review and leaves out another ($1)"
      ;;
    follow-refused | follow-nostatus)
      chain="create pods in kube-system, so it can become system:serviceaccount:kube-system:generic-garbage-collector"
      if [ "$1" = follow-refused ]; then
        want_rbac_unknown "the witness review, the cluster-admin binding's group system:masters, was not allowed, so a refusal from this API server proves nothing" \
          "a follow round answered $1, where the seeded chain would go unseen"
      else
        want_rbac_unknown "the API server's answer to the reviews could not be read: " \
          "a follow round answered $1, where the seeded chain would go unseen"
        want_text "is not a SubjectAccessReview with a status" "a follow round answered $1"
      fi
      ! has_prefix "FINDING system:serviceaccount:legion:worker: $chain" ||
        fail "a follow round answered $1: the chain was still found, so the item plants nothing"
      ;;
  esac
}
if row cy; then
  each review_answer_row judged chunked repeated witness-copied follow-refused follow-nostatus
  passed "(cy) reviews answered as one document each are judged, asked in one call or in four at once; an answer that repeats one and leaves out another reads UNKNOWN naming the count; and a follow round whose answer is all refused or has no status reads UNKNOWN for its witness"
fi

# --- (cz) review's calls on a cluster are REVIEW_WORKERS, capped at the nominal seats the API server
# gives the priority level global-default (its /metrics): asked with REVIEW_WORKERS=4 in a clean
# world, a cluster with 2 seats gets 2 calls and one with 40 gets 4; a /metrics refused, or one
# holding no global-default seats, gets one call; each run passes its step as one call does. A
# REVIEW_WORKERS that is no whole number above 0 reads UNKNOWN naming it, exit 2. With four batches,
# one later batch alone (one that does not hold the witness, read from a first run's recorded
# requests) refused, or answering one review short, reads UNKNOWN, exit 2. Run by hand, the control
# is reviews.sh with review_workers' cap taken out (WORKERS set to REVIEW_WORKERS): the capped item
# gets 4 calls, and the refused and no-seats items 4; and reviews.sh waiting on batch 0's call
# alone: the later-refused item passes its step. ---
worker_cap_row() {
  local dir="$world/clusters/operator-cluster" calls want later file
  base_world
  case "$1" in
    capped) echo 2 >"$dir/global-default-seats"; want=2 ;;
    upper) echo 40 >"$dir/global-default-seats"; want=4 ;;
    refused) : >"$dir/metrics-refused"; want=1 ;;
    no-seats) : >"$dir/metrics-without-seats"; want=1 ;;
    bad-workers)
      REVIEW_WORKERS=abc run
      want_rbac_unknown "REVIEW_WORKERS is not a whole number from 1 to 9999, so the reviews were not asked" \
        "a REVIEW_WORKERS that is no number"
      return
      ;;
    later-refused | later-short)
      REVIEW_WORKERS=4 run
      # The witness's line is the first of its batch, so `cut | grep -q` under pipefail could fail
      # with cut's SIGPIPE once grep matched it, and read the witness's batch as a later one: the
      # item would then refuse batch 0, and pass a reviews.sh that waits on batch 0's call alone.
      # grep reads the subjects through a process substitution instead.
      for file in "$dir"/review-requests-*.tsv; do
        if ! grep -qxF group:system:masters < <(cut -f7 "$file"); then later="$(cut -f7 "$file" | sed -n 1p)"; break; fi
      done
      [ -n "$later" ] || fail "no batch of the first run went without the witness, so the item ($1) proves nothing"
      rm -f "$dir"/review-call-*.json "$dir"/review-requests-*.tsv "$dir/review-calls"
      if [ "$1" = later-refused ]; then
        printf '%s\n' "$later" >"$dir/review-refuse-holding"
        want="the API server refused the review (it needs create on subjectaccessreviews"
      else
        printf '%s\n' "$later" >"$dir/review-short-holding"
        want="the API server answered "
      fi
      REVIEW_WORKERS=4 run
      want_rbac_unknown "$want" "the batch holding $later alone, of four, $1"
      return
      ;;
  esac
  REVIEW_WORKERS=4 run
  want_status 0 "a clean world asked in capped calls ($1)"
  want_text "PASS live: RBAC on operator-cluster: " "a clean world asked in capped calls ($1)"
  calls=$(find "$dir" -maxdepth 1 -name 'review-call-*.json' | wc -l)
  [ "$calls" -eq "$want" ] ||
    fail "the review on operator-cluster ($1) was asked in $calls kubectl calls, want $want"
}
if row cz; then
  each worker_cap_row capped upper refused no-seats bad-workers later-refused later-short
  passed "(cz) review's calls on a cluster are REVIEW_WORKERS capped at the API server's global-default seats (2 seats: 2 calls of 4; 40 seats: 4), one call when the seats cannot be read, UNKNOWN for a REVIEW_WORKERS that is no number, and UNKNOWN when one later batch alone is refused or answers short"
fi
