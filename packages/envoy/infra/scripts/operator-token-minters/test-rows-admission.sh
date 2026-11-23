# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-admission.sh: the rows of operator-token-minters.test.sh on
# admission power outside the admission objects: a webhook's backend, a policy engine's
# cluster-scoped resources, and an admission policy's parameter objects. Sourced by the test after
# its world and helpers; its header says what the fakes model. The rows:
#
#   (ce) a mutating webhook whose selector can match a guarded namespace, backed by the Service
#        hooks/hook: apps/deployer that may patch deployments in hooks, or create jobs (rv16),
#        cronjobs or replicationcontrollers, create pods/exec or pods/attach, or patch
#        pods/ephemeralcontainers there (N2), exits 1 naming the webhook, on the operator's cluster or
#        another; the same webhook whose namespaceSelector excludes the guarded namespaces by name
#        exits 0; a webhook that calls a URL, and a refused list of webhook configurations, read
#        UNKNOWN; apps/deployer that may update configmaps in hooks (rv17), the injector's template,
#        exits 1; and a backend Service of type ExternalName, or with no selector, reads UNKNOWN
#        (P2-4);
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
#        on the operator's cluster and another: a restore recreates a backup's workloads and RBAC.

# --- (ce) A webhook's backend: whoever may write its Service, the workload behind it, its endpoints
# or its TLS Secret decides what the webhook does to the operator's next pod. ---
hook_config() {
  jq -n --arg selector "${1:-}" --argjson client "$2" '[{kind: "MutatingWebhookConfiguration", metadata: {name: "hook-config"},
    webhooks: [{name: "pods.example.com", clientConfig: $client}
      + (if $selector == "exclude" then {namespaceSelector: {matchExpressions: [{key: "kubernetes.io/metadata.name", operator: "NotIn",
          values: ["envoy-operator", "secrets-broker"]}]}} else {} end)]}]'
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
    url)
      hook_config "" '{"url": "https://hooks.example.com/mutate"}' >"$world/clusters/operator-cluster/webhooks.json"
      run
      want_status 2 "a webhook that calls a URL"
      want_line "UNKNOWN live: RBAC on operator-cluster: the webhook MutatingWebhookConfiguration hook-config/pods.example.com can match a guarded namespace and calls https://hooks.example.com/mutate, outside the cluster, whose holders this check cannot read" \
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
    refused)
      : >"$world/clusters/operator-cluster/refuse-webhooks"
      run
      want_status 2 "a refused list of webhook configurations"
      want_text "UNKNOWN live: RBAC on operator-cluster: the API server refused the list of webhook configurations" "a refused list of webhook configurations"
      ;;
  esac
}
if row ce; then
  each backend_row backend jobs cronjobs replicationcontrollers exec attach ephemeralcontainers configmaps other-cluster excluded url \
    external-name no-selector refused
  passed "(ce) a write to a matching webhook's backend (a Deployment's patch, N2's jobs, cronjobs, replicationcontrollers, pods/exec, pods/attach and pods/ephemeralcontainers, and rv17's configmaps) exits 1 naming the webhook, on the operator's cluster or another; an excluding selector exits 0; a URL webhook, an ExternalName or selector-less Service, and a refused list read UNKNOWN"
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
