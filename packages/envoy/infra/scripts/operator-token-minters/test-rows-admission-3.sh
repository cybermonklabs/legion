# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-admission-3.sh: the rows of operator-token-minters.test.sh on
# admission power outside the admission objects: a policy engine's cluster-scoped resources, an
# admission policy's parameter objects, GKE Autopilot's workload allowlists, and Backup for GKE.
# Sourced by the test after its world and helpers; its header says what the fakes model. The rows:
#
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
