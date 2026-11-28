# shellcheck shell=bash
# shellcheck disable=SC2016,SC2034,SC2154 # the world, its names, its helpers and the output and status a run sets are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-fence-2.sh: the rows of operator-token-minters.test.sh on the
# broker's pod fence (broker-fence.sh), the admission refusal the check credits: each way the fence
# fails. Sourced by the test after its world and helpers; its header says what the fakes model. The
# row's world (fence_world) puts the broker in dispatch, guarded as --broker-workload
# dispatch/envoy-broker with --db-secret dispatch/envoy-broker-db, with the fence of fence_objects
# (the reviewed rules broker-fence.json holds, and example workload policies), and gives the three
# grants the fence answers: comment-delivery's exec (create and get) and dispatch-measure's exec in
# dispatch, and legion/worker's port forward there, live, and comment-delivery's exec declared by
# the render. The row runs fence_check, the check whose contract holds the example workload
# policies' digests. The rows:
#
#   (fb) each way the fence fails credits nothing, each grant a finding again, and says why: the
#        fence removed, a binding of Warn, a policy of failurePolicy Ignore, a binding whose
#        selector misses dispatch, a policy whose own selector does, an exception added to the
#        connect rule, the cluster's fence differing from the renders', a list of admission
#        policies or of pods that cannot be read or is answered empty (UNKNOWN), a pod outside the
#        fence's names running as envoy-broker or taking --db-secret, a pod outside them taking
#        another Secret a broker pod takes (its NATS issuer seed), a list with no broker pod, a
#        webhook backed by a Service in dispatch, one whose selector misses the guarded namespaces
#        but whose rule matches a cluster-scoped object, one whose selector names an unguarded
#        namespace alone and whose rules are namespaced, a list of webhooks that cannot be read or
#        is answered empty or as `{}`, one whose webhook's Service webhook-backends.jq cannot read
#        (its jq error printed above the NO CREDIT), and another guarded pair in dispatch.
#
# Its control is its own other half: (fa), in test-rows-fence-1.sh, the same world without the
# fault. The check as at 9d08e087406 is the other control of fb's webhook-unguarded, and as at
# 01c4044b4e5 of fb's webhook-malformed (it prints the NO CREDIT with no UNKNOWN above naming jq's
# error), which each fails.

# --- (fb) A fence that fails any part of its credit credits nothing. ---
fence_fail_row() {
  local want
  fence_world
  case "$1" in
    removed)
      edit "$world/render.json" '.items |= map(select(.kind | startswith("ValidatingAdmissionPolicy") | not))'
      echo '[]' >"$world/clusters/operator-cluster/admission-policies.json"
      want="NO CREDIT live: RBAC on operator-cluster: the broker's pod fence (broker-pod-connect, broker-pod-update, broker-pod-create, broker-workload-create, broker-workload-update in dispatch) is not credited, so what it would refuse stays a finding: the renders hold none of its policies and bindings"
      ;;
    warn)
      fence_edit both 'if .kind == "ValidatingAdmissionPolicyBinding" and .metadata.name == "broker-pod-connect" then .spec.validationActions = ["Warn"] else . end'
      want="in the renders, the ValidatingAdmissionPolicyBinding broker-pod-connect binds with validationActions [\"Warn\"], not [\"Deny\"]"
      ;;
    ignore)
      fence_edit both 'if .kind == "ValidatingAdmissionPolicy" and .metadata.name == "broker-pod-update" then .spec.failurePolicy = "Ignore" else . end'
      want="in the renders, the ValidatingAdmissionPolicy broker-pod-update has failurePolicy \"Ignore\", not \"Fail\""
      ;;
    selector)
      fence_edit both 'if .kind == "ValidatingAdmissionPolicyBinding" and .metadata.name == "broker-pod-connect"
        then .spec.matchResources.namespaceSelector.matchExpressions[0].values = ["dispatch-staging"] else . end'
      want="in the renders, the ValidatingAdmissionPolicyBinding broker-pod-connect does not select dispatch alone by its name"
      ;;
    policy-selector)
      fence_edit both 'if .kind == "ValidatingAdmissionPolicy" and .metadata.name == "broker-pod-connect"
        then .spec.matchConstraints.namespaceSelector = {matchExpressions: [{key: "kubernetes.io/metadata.name", operator: "NotIn", values: ["dispatch"]}]} else . end'
      want="in the renders, the ValidatingAdmissionPolicy broker-pod-connect is narrowed or widened beside its rules (namespaceSelector)"
      ;;
    exception)
      fence_edit both 'if .kind == "ValidatingAdmissionPolicy" and .metadata.name == "broker-pod-connect"
        then .spec.validations[0].expression = "request.userInfo.username == '"'system:serviceaccount:legion:worker'"' || " + .spec.validations[0].expression else . end'
      want="in the renders, the ValidatingAdmissionPolicy broker-pod-connect differs from the reviewed fence in validations"
      ;;
    drift)
      fence_edit live 'if .kind == "ValidatingAdmissionPolicy" and .metadata.name == "broker-pod-update"
        then .spec.matchConstraints.resourceRules |= map(select(.resources != ["pods/ephemeralcontainers"])) else . end'
      want="NO CREDIT live: RBAC on operator-cluster: the broker's pod fence (broker-pod-connect, broker-pod-update, broker-pod-create, broker-workload-create, broker-workload-update in dispatch) is not credited, so what it would refuse stays a finding: in the cluster, the ValidatingAdmissionPolicy broker-pod-update differs from the reviewed fence in matchConstraints"
      ;;
    unread)
      : >"$world/clusters/operator-cluster/refuse-policies"
      want="UNKNOWN live: RBAC on operator-cluster: the API server refused the list of admission policies and their bindings (it needs list on validatingadmissionpolicies and validatingadmissionpolicybindings), so the broker's pod fence (broker-pod-connect, broker-pod-update, broker-pod-create, broker-workload-create, broker-workload-update) is unread and nothing it refuses is credited: "
      ;;
    pods-unread)
      : >"$world/clusters/operator-cluster/refuse-pods"
      want="UNKNOWN live: RBAC on operator-cluster: the API server refused the list of pods in dispatch (it needs list on pods there, container.pods.list on GKE), so whether a pod outside the broker's pod fence runs as envoy-broker or takes its Secrets is unread and nothing the fence refuses is credited: "
      ;;
    rogue-pod)
      edit "$world/clusters/operator-cluster/pods.json" '. + [{metadata: {namespace: "dispatch", name: "debug-7f9c"},
        spec: {serviceAccountName: "dispatch", volumes: [{name: "db", secret: {secretName: "envoy-broker-db"}}], containers: [{name: "sh"}]}}]'
      want="pods of dispatch run as envoy-broker or take a Secret of the broker's under a name the fence's connect rule does not hold (envoy-broker-*): debug-7f9c (runs as dispatch, takes envoy-broker-db)"
      ;;
    other-secret)
      # A Secret the broker's pod takes, as it takes its NATS issuer seed, mounted by Dispatch's pod too.
      edit "$world/clusters/operator-cluster/pods.json" 'map(.spec.volumes = ((.spec.volumes // []) + [{name: "issuer", secret: {secretName: "envoy-broker-nats-issuer"}}]))'
      want="pods of dispatch run as envoy-broker or take a Secret of the broker's under a name the fence's connect rule does not hold (envoy-broker-*): dispatch-6b8f4c-fghij (runs as dispatch, takes envoy-broker-nats-issuer)"
      ;;
    no-pod)
      echo '[]' >"$world/clusters/operator-cluster/pods.json"
      want="dispatch lists no pod named envoy-broker-* that runs as envoy-broker, so its list of pods cannot be told from one that saw nothing"
      ;;
    webhook)
      # A mutating webhook that can match every namespace, backed by a Service in dispatch.
      jq -n '[{apiVersion: "admissionregistration.k8s.io/v1", kind: "MutatingWebhookConfiguration", metadata: {name: "hooks"},
        webhooks: [{name: "inject.example.com", clientConfig: {service: {namespace: "dispatch", name: "hook"}}}]}]' \
        >"$world/clusters/operator-cluster/webhooks.json"
      want="a webhook calls a Service in dispatch, whose pods the fence does not keep anyone from entering: MutatingWebhookConfiguration hooks/inject.example.com calls the Service dispatch/hook"
      ;;
    webhook-cluster)
      # A validating webhook backed in dispatch whose selector names legion alone, but whose rule
      # matches ClusterRoleBindings, which have no namespace for a selector to hold back.
      jq -n '[{apiVersion: "admissionregistration.k8s.io/v1", kind: "ValidatingWebhookConfiguration", metadata: {name: "hooks"},
        webhooks: [{name: "rbac.example.com", clientConfig: {service: {namespace: "dispatch", name: "hook"}},
          namespaceSelector: {matchExpressions: [{key: "kubernetes.io/metadata.name", operator: "In", values: ["legion"]}]},
          rules: [{apiGroups: ["rbac.authorization.k8s.io"], apiVersions: ["v1"], operations: ["CREATE"], resources: ["clusterrolebindings"]}]}]}]' \
        >"$world/clusters/operator-cluster/webhooks.json"
      want="a webhook calls a Service in dispatch, whose pods the fence does not keep anyone from entering: ValidatingWebhookConfiguration hooks/rbac.example.com calls the Service dispatch/hook"
      ;;
    webhook-unguarded)
      # A mutating webhook backed in dispatch whose selector names an unguarded namespace alone and
      # whose rules are namespaced: webhook-backends.jq leaves it out, but whoever enters its backend
      # changes every pod made in that namespace, so the fence's credit still stands on its absence.
      jq -n '[{apiVersion: "admissionregistration.k8s.io/v1", kind: "MutatingWebhookConfiguration", metadata: {name: "hooks"},
        webhooks: [{name: "pods.example.com", clientConfig: {service: {namespace: "dispatch", name: "hook"}},
          namespaceSelector: {matchExpressions: [{key: "kubernetes.io/metadata.name", operator: "In", values: ["unguarded"]}]},
          rules: [{apiGroups: [""], apiVersions: ["v1"], operations: ["CREATE"], resources: ["pods"], scope: "Namespaced"}]}]}]' \
        >"$world/clusters/operator-cluster/webhooks.json"
      want="a webhook calls a Service in dispatch, whose pods the fence does not keep anyone from entering: MutatingWebhookConfiguration hooks/pods.example.com calls the Service dispatch/hook"
      ;;
    webhooks-unread)
      : >"$world/clusters/operator-cluster/refuse-webhooks"
      want="the webhook configurations were not read (above), so whether a webhook's backend is in dispatch, where the fence does not refuse entry into it, is unknown"
      ;;
    webhooks-empty | webhooks-object)
      # The list answered with exit 0 and an empty body, or with `{}`: is_list reads neither as no
      # webhook, so the credit cannot stand on it.
      if [ "$1" = webhooks-empty ]; then : >"$world/clusters/operator-cluster/empty-webhooks"; else : >"$world/clusters/operator-cluster/object-webhooks"; fi
      want="the webhook configurations were not read (above), so whether a webhook's backend is in dispatch, where the fence does not refuse entry into it, is unknown"
      fence_run
      want_status 1 "the fence $1"
      want_fence_findings "the fence $1"
      want_text "$want" "the fence $1"
      want_text "UNKNOWN live: RBAC on operator-cluster: the API server's list of webhook configurations could not be read, so no webhook's backend is reviewed: the answer " "the fence $1"
      return
      ;;
    webhook-malformed)
      # A webhook webhook-backends.jq does not judge (an unguarded namespace, namespaced rules), whose
      # Service is a string where an object belongs: the walk that lists every webhook's Service
      # cannot read it, and jq's error must be the UNKNOWN the NO CREDIT points above to.
      jq -n '[{apiVersion: "admissionregistration.k8s.io/v1", kind: "MutatingWebhookConfiguration", metadata: {name: "hooks"},
        webhooks: [{name: "pods.example.com", clientConfig: {service: "dispatch/hook"},
          namespaceSelector: {matchExpressions: [{key: "kubernetes.io/metadata.name", operator: "In", values: ["unguarded"]}]},
          rules: [{apiGroups: [""], apiVersions: ["v1"], operations: ["CREATE"], resources: ["pods"], scope: "Namespaced"}]}]}]' \
        >"$world/clusters/operator-cluster/webhooks.json"
      fence_run
      want_status 1 "the fence $1"
      want_fence_findings "the fence $1"
      want_text "the webhook configurations were not read (above), so whether a webhook's backend is in dispatch, where the fence does not refuse entry into it, is unknown" "the fence $1"
      want_text "UNKNOWN live: RBAC on operator-cluster: the API server's list of webhook configurations could not be read, so no webhook's backend is reviewed: jq: error" "the fence $1"
      want_text 'Cannot index string with' "the fence $1"
      return
      ;;
    policies-empty | pods-empty)
      # The fence's list, or the pods', answered with exit 0 and an empty body: UNKNOWN naming it.
      : >"$world/clusters/operator-cluster/empty-${1%-empty}"
      if [ "$1" = pods-empty ]; then
        want="UNKNOWN live: RBAC on operator-cluster: the API server's list of pods in dispatch is not one JSON list, so nothing the broker's pod fence refuses is credited: the answer was empty"
      else
        want="UNKNOWN live: RBAC on operator-cluster: the API server's list of admission policies is not one JSON list, so the broker's pod fence is unread and nothing it refuses is credited: the answer was empty"
      fi
      ;;
    shared)
      fence_run --guarded dispatch/dispatch
      want_status 1 "another guarded pair in dispatch"
      want_fence_findings "another guarded pair in dispatch"
      return
      ;;
  esac
  fence_run
  want_status 1 "the fence $1"
  want_fence_findings "the fence $1"
  want_text "$want" "the fence $1"
}
if row fb; then
  each fence_fail_row removed warn ignore selector policy-selector exception drift unread pods-unread rogue-pod other-secret no-pod \
    webhook webhook-cluster webhook-unguarded webhooks-unread webhooks-empty webhooks-object webhook-malformed policies-empty pods-empty shared
  passed "(fb) the fence removed, bound Warn, failing open, missing dispatch in a binding's or a policy's selector, with an exception, differing live from declared, unread or answered empty, beside a pod outside its names that runs as the broker or takes its database login or another of its Secrets, with no broker pod, beside any webhook backed in dispatch (one matching a guarded namespace, a cluster-scoped object or neither) or webhooks unread, answered empty or as {}, or a webhook's Service the walk cannot read (its jq error printed), or beside another guarded pair in dispatch, credits nothing"
fi
