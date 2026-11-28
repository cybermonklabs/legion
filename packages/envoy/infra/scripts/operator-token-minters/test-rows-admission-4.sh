# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-admission-4.sh: the rows of operator-token-minters.test.sh on
# the answers the check reads with kubectl -o json, sent with exit 0 but not as the API server wrote
# them. Sourced by the test after its world and helpers; its header says what the fakes model. The
# rows:
#
#   (cu) each answer the check reads with kubectl -o json (the webhook configurations, the CRDs, the
#        workload allowlists, the admission policies and their bindings, the roles, the bindings,
#        the ServiceAccounts, a webhook's Service and the answer to the reviews), sent with exit 0
#        but not as the API server wrote it (an error page, an empty body, `{}`, for the roles a
#        List with no item, and for the roles and the bindings a List that holds one of its two
#        kinds alone), reads UNKNOWN naming it, exits 2, and its step does not pass, though a
#        principal holds a right only that answer reveals, which the item names.

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
