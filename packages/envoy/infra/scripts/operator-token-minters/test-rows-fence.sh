# shellcheck shell=bash
# shellcheck disable=SC2016,SC2034,SC2154 # the world, its names, its helpers and the output and status a run sets are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-fence.sh: the rows of operator-token-minters.test.sh on the
# broker's pod fence (broker-fence.sh), the admission refusal the check credits. Sourced by the test
# after its world and helpers; its header says what the fakes model. Each row's world (fence_world)
# puts the broker in dispatch, guarded as --broker-workload dispatch/envoy-broker with --db-secret
# dispatch/envoy-broker-db, with the fence of fence_objects (the reviewed rules broker-fence.json
# holds, and example workload policies), and gives the three grants the fence answers:
# comment-delivery's exec (create and get) and dispatch-measure's exec in dispatch, and
# legion/worker's port forward there, live, and comment-delivery's exec declared by the render. The
# rows run fence_check, the check whose contract holds the example workload policies' digests, but
# (fg). The rows:
#
#   (fa) the fence the renders declare and the cluster holds as reviewed, its live objects carrying
#        what GKE and the API server fill in, credits each of those grants, live and declared: exit
#        0, a CREDIT line each naming the use it refuses, an ALLOWANCE line each naming the residual
#        of broker-fence.json, the code step's PASS, the PASS of comment-delivery's minters' step,
#        and --report's `credited` (the policy and the declared and live versions) and `residuals`
#        (the residual and INFRA-4604); each spec broker-fence.json holds is in the fence's shape;
#   (fb) each way the fence fails credits nothing, each grant a finding again, and says why: the
#        fence removed, a binding of Warn, a policy of failurePolicy Ignore, a binding whose
#        selector misses dispatch, a policy whose own selector does, an exception added to the
#        connect rule, the cluster's fence differing from the renders', a list of admission
#        policies or of pods that cannot be read or is answered empty (UNKNOWN), a pod outside the
#        fence's names running as envoy-broker or taking --db-secret, a pod outside them taking
#        another Secret a broker pod takes (its NATS issuer seed), Dispatch's own pod once no render
#        declares the Secret it takes, a list with no broker pod, a
#        webhook backed by a Service in dispatch, one whose selector misses the guarded namespaces
#        but whose rule matches a cluster-scoped object, one whose selector names an unguarded
#        namespace alone and whose rules are namespaced, a list of webhooks that cannot be read or
#        is answered empty or as `{}`, one whose webhook's Service webhook-backends.jq cannot read
#        (its jq error printed above the NO CREDIT), and another guarded pair in dispatch;
#   (zd) another pod in dispatch that takes a token Secret of the broker's no render declares and
#        no ServiceAccount lists is NO CREDIT naming the pod and the Secret, exit 1; (ze) the same
#        Secret declared the broker's token Secret, and (zl) declared of another type but listed by
#        the broker's ServiceAccount, each NO CREDIT, exit 1; controls: a copy that holds no other
#        pod to the declared Secrets credits zd's world, and a copy that reads no ServiceAccount's
#        `secrets` credits zl's, each exit 0;
#   (fc) a principal that can write the fence stays a finding: patch of the binding, delete of a
#        policy, and delete of broker-pod-connect by name;
#   (fd) legion/worker, which can become legion/dispatch-measure, whose exec in dispatch is credited
#        and carries the residual, is an ALLOWANCE naming the chain and the residual; once
#        dispatch-measure can patch admission policies, the chain is a finding;
#   (fe) who can enter a pod of dispatch is credited for becoming envoy-broker, which has a finding,
#        with no ALLOWANCE of its own, and found once dispatch/dispatch has one too;
#   (ff) a Google IAM minter of the broker's account stays a finding beside the credits;
#   (fg) the check itself, whose broker-fence.json holds the reviewed workload policies by digest,
#        credits nothing of the example fence: its workload policies hash to other digests;
#   (fh) patch, or update, of pods/ephemeralcontainers in dispatch stays a finding beside the
#        credits: on a pod not the broker's an ephemeral container takes any Secret of the
#        namespace, --db-secret among them;
#   (fj) who can become a credited grant's holder is an ALLOWANCE naming the chain and the residual:
#        an agent that can mint dispatch-measure's token, a Google account that can mint
#        comment-delivery's, and an agent that can become comment-delivery's Workload Identity
#        member (which is an ALLOWANCE too);
#   (fk) patch, or update, of pods in dispatch stays a finding: a label-only patch puts a pod that
#        is not the broker's behind the broker's Services, which select app=envoy-broker alone;
#   (fl) a Google account found through a Workload Identity member of an account with a finding,
#        which also holds a credited exec, has its minters found, never allowed;
#   (fm) a chain Google, Kubernetes, then Google to the residual: comment-delivery's member, a
#        Google account that can mint the member's token, and that account's minter are each named
#        by an ALLOWANCE;
#   (fn) a chain to an account found partway through a round is a finding by the found right,
#        never one by the residual's;
#   (fs) each of the five binding specs broker-fence.json holds decides: widened in the contract and
#        on the cluster alike, the fence is credited; in the contract alone, it is not;
#   (fp) review-answer.jq gives an allowed credited right the policy and the flag `residual`, never
#        the residual's text, and a refused review no fence field;
#   (fq) code-rbac.jq gives a declared credited grant the policy and the flag `residual`;
#   (fr) each right fence_refuses credits is one the reviewed broker-pod-connect rules admit as
#        CONNECT on pods, every rule naming the envoy-broker- prefix; the same reading of a contract
#        whose connect policy lacks port forward, or names no prefix, fails that column.
#
# Each control is the row's own other half: (fa) against (fb), (fc), (fd), (ff), (fg), (fh), (fj),
# (fk), (fl) and (fm) the same world with one fault, one grant more or the reviewed digests, (fd),
# (fe) and (fs) a half without it, and (fr) a contract with one rule or one prefix taken out. The
# check as at b14c08df3df is the other control of (fd), (fj), (fk), (fs), (fp) and (fq), as at
# 9d08e087406 of (fl), (fm), (fn) and fb's webhook-unguarded, and as at 01c4044b4e5 of fb's
# webhook-malformed (it prints the NO CREDIT with no UNKNOWN above naming jq's error), which each
# fails.

# The CREDIT and ALLOWANCE lines (fa) wants, and the FINDING lines the same grants are once the fence
# is not credited. A credit's sentence names the use the policy refuses; the allowance's, the
# residual of broker-fence.json.
credit_line() { echo "CREDIT $1: $2, its use on envoy-broker-* pods refused by the ValidatingAdmissionPolicy $3 (bound Deny, failurePolicy Fail), declared and live alike"; }
fence_residual="$(jq -r .residual.text "$fence_contract")"
readonly fence_residual
allowance_line() { echo "ALLOWANCE $1: $2, $fence_residual"; }
# The NO CREDIT reason for a pod outside the fence's names, before the pods it names.
readonly fence_outside_why="pods of dispatch run as envoy-broker, take a Secret of the broker's, or take a Secret no --secret-render declares (whose token it may hold, since the check reads no Secret), under a name the fence's connect rule does not hold (envoy-broker-*)"
fence_credits_want=(
  "serviceAccount:$comment_delivery|create pods/exec in dispatch|broker-pod-connect"
  "serviceAccount:$comment_delivery|get pods/exec in dispatch|broker-pod-connect"
  "system:serviceaccount:legion:dispatch-measure|create pods/exec in dispatch|broker-pod-connect"
  "system:serviceaccount:legion:worker|create pods/portforward in dispatch|broker-pod-connect"
  "serviceAccount:$comment_delivery|create pods/exec in dispatch, declared by RoleBinding dispatch/comment-delivery|broker-pod-connect"
  "serviceAccount:$comment_delivery|get pods/exec in dispatch, declared by RoleBinding dispatch/comment-delivery|broker-pod-connect"
)
want_fence_credits() {
  local entry principal right policy
  for entry in "${fence_credits_want[@]}"; do
    IFS='|' read -r principal right policy <<<"$entry"
    want_line "$(credit_line "$principal" "$right" "$policy")" "$1"
    want_line "$(allowance_line "$principal" "$right")" "$1"
  done
}
want_fence_findings() {
  local entry principal right policy
  for entry in "${fence_credits_want[@]}"; do
    IFS='|' read -r principal right policy <<<"$entry"
    want_line "FINDING $principal: $right" "$1"
  done
  [ "$(count_prefix "CREDIT ")" -eq 0 ] || fail "$1: a CREDIT line"
  [ "$(count_prefix "ALLOWANCE ")" -eq 0 ] || fail "$1: an ALLOWANCE line"
}

# --- (fa) The fence as reviewed credits what it refuses, live and declared. ---
if row fa; then
  jq -e '.namespace == "dispatch" and .account == "envoy-broker" and .prefix == "envoy-broker-"
    and [.objects[] | "\(.kind) \(.name)"] == [("broker-pod-connect", "broker-pod-update", "broker-pod-create", "broker-workload-create", "broker-workload-update")
      | "ValidatingAdmissionPolicy \(.)", "ValidatingAdmissionPolicyBinding \(.)"]
    and [.objects[] | select(.digest) | .name] == ["broker-workload-create", "broker-workload-update"]
    and all(.objects[] | select(.digest); (.digest | test("^sha256:[0-9a-f]{64}$")) and .spec == null)
    and all(.objects[] | select(.digest | not); .spec != null)
    and .residual.tracked_by == "INFRA-4604" and (.residual.text | test("INFRA-4604"))' "$fence_contract" >/dev/null ||
    fail "broker-fence.json does not hold the broker's fence in dispatch, each object by its spec but the workload policies by a sha256, and its residual"
  # Each spec the contract holds decides alone (fence_read), so the committed ones are held here to
  # the fence's shape: every binding Deny to dispatch alone, every policy failing closed.
  jq -e -n -L "$(dirname "$check")/operator-token-minters" --slurpfile c "$fence_contract" 'include "rec"; include "broker-fence";
    $c[0] as $c | all($c.objects[] | select(.spec != null); . as $o | ($o.spec | fence_shape($o.kind; $c.namespace; $o.name)) == "")' >/dev/null ||
    fail "a spec broker-fence.json holds is not in the fence's shape"
  fence_world
  report="$temporary_dir/fence-report.json"
  fence_run --report "$report"
  want_status 0 "the fence as reviewed"
  want_fence_credits "the fence as reviewed"
  [ "$(count_prefix "FINDING ")" -eq 0 ] || fail "the fence as reviewed: a FINDING line"
  want_text "PASS code: RBAC: the renders give the rights" "the fence as reviewed"
  want_text ", beside 2 grants whose use on the broker's pods its pod fence refuses, declared and live alike" "the fence as reviewed"
  want_text "PASS live: RBAC on operator-cluster: " "the fence as reviewed"
  # comment-delivery's credited grant carries the residual, so its minters are read; none is planted.
  want_text "PASS live: minters of $comment_delivery, whose credited grant reaches the broker's pod fence's residual: " "the fence as reviewed"
  jq -e --arg cd "serviceAccount:$comment_delivery" --arg residual "$fence_residual" '.findings == [] and (.credited | length) == 6
      and all(.credited[]; . as $c | keys == ["declared", "granted_at", "live", "policy", "principal", "resource", "right"]
              and ($c.declared | test("^sha256:[0-9a-f]{64}$")) and ($c.live | startswith($c.declared + "; "))
              and ($c.live | endswith("; ValidatingAdmissionPolicy \($c.policy) generation 3 resourceVersion 7001; ValidatingAdmissionPolicyBinding \($c.policy) generation 1 resourceVersion 7001")))
      and any(.credited[]; .principal == $cd and .right == "get pods/exec in dispatch" and .policy == "broker-pod-connect"
              and .resource == "cluster:example-project/us-central1/operator-cluster" and (.granted_at | startswith("RBAC: allowed by ")))
      and any(.credited[]; .principal == $cd and .granted_at == "RoleBinding dispatch/comment-delivery")
      and ([.residuals[] | {principal, right, resource, granted_at}] == [.credited[] | {principal, right, resource, granted_at}])
      and all(.residuals[]; keys == ["granted_at", "principal", "residual", "resource", "right", "tracked_by"]
              and .residual == $residual and .tracked_by == "INFRA-4604")' "$report" >/dev/null ||
    fail "the fence as reviewed: --report's credited and residuals are not the six credits and their residuals: $(jq -c '{credited, residuals}' "$report")"
  passed "(fa) the fence as reviewed, declared and live, credits comment-delivery's exec, dispatch-measure's exec and legion/worker's port forward on the broker's pods, allows their use on dispatch's other pods as INFRA-4604's residual, and reports each with its evidence"
fi

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
      want="$fence_outside_why: debug-7f9c (runs as dispatch, takes envoy-broker-db)"
      ;;
    other-secret)
      # A Secret the broker's pod takes, as it takes its NATS issuer seed, mounted by Dispatch's pod too.
      edit "$world/clusters/operator-cluster/pods.json" 'map(.spec.volumes = ((.spec.volumes // []) + [{name: "issuer", secret: {secretName: "envoy-broker-nats-issuer"}}]))'
      want="$fence_outside_why: dispatch-6b8f4c-fghij (runs as dispatch, takes envoy-broker-nats-issuer)"
      ;;
    undeclared-own)
      # Dispatch's own Secret, with the render that declares it taken away: the pod that takes it
      # is outside the fence, since no Secret is read to say whose token it holds.
      edit "$world/secret-render.json" '.items |= map(select(.metadata.name != "dispatch-dispatch"))'
      want="$fence_outside_why: dispatch-6b8f4c-fghij (runs as dispatch, takes dispatch-dispatch, which no --secret-render declares)"
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
  each fence_fail_row removed warn ignore selector policy-selector exception drift unread pods-unread rogue-pod other-secret undeclared-own \
    no-pod webhook webhook-cluster webhook-unguarded webhooks-unread webhooks-empty webhooks-object webhook-malformed policies-empty pods-empty shared
  passed "(fb) the fence removed, bound Warn, failing open, missing dispatch in a binding's or a policy's selector, with an exception, differing live from declared, unread or answered empty, beside a pod outside its names that runs as the broker or takes its database login or another of its Secrets, or Dispatch's own Secret once no render declares it, with no broker pod, beside any webhook backed in dispatch (one matching a guarded namespace, a cluster-scoped object or neither) or webhooks unread, answered empty or as {}, or a webhook's Service the walk cannot read (its jq error printed), or beside another guarded pair in dispatch, credits nothing"
fi

# --- (zd, ze, zl) A token Secret of the broker's taken by another pod in dispatch,
# other-7f1a2b-klmno, which runs as dispatch and takes envoy-broker-token-abcde by envFrom. Its
# exec is credited unless the fence's guard sees the Secret: (zd) no render declares it and the
# broker's ServiceAccount lists none, (ze) the render declares it the broker's token Secret, and
# (zl) the render declares it of another type while the broker's ServiceAccount lists it in its
# `secrets`. Each is NO CREDIT and exit 1. Controls: a copy whose fence_pods holds no pod to the
# declared Secrets credits zd's world, and a copy that reads no ServiceAccount's `secrets`
# credits zl's, each exit 0. ---
token_taken_row() {
  local copy script="$fence_check" want
  fence_world
  edit "$world/clusters/operator-cluster/pods.json" '. + [{metadata: {namespace: "dispatch", name: "other-7f1a2b-klmno"},
    spec: {serviceAccountName: "dispatch", containers: [{name: "other", envFrom: [{secretRef: {name: "envoy-broker-token-abcde"}}]}]}}]'
  case "$1" in
    zd | zd-control) want="$fence_outside_why: other-7f1a2b-klmno (runs as dispatch, takes envoy-broker-token-abcde, which no --secret-render declares)" ;;
    ze)
      edit "$world/secret-render.json" '.items += [{apiVersion: "v1", kind: "Secret", type: "kubernetes.io/service-account-token",
        metadata: {namespace: "dispatch", name: "envoy-broker-token-abcde", annotations: {"kubernetes.io/service-account.name": "envoy-broker"}}}]'
      want="$fence_outside_why: other-7f1a2b-klmno (runs as dispatch, takes envoy-broker-token-abcde)"
      ;;
    zl | zl-control)
      edit "$world/secret-render.json" '.items += [{apiVersion: "v1", kind: "Secret", type: "Opaque", metadata: {namespace: "dispatch", name: "envoy-broker-token-abcde"}}]'
      edit "$world/clusters/operator-cluster/serviceaccounts.json" \
        'map(if .namespace == "dispatch" and .name == "envoy-broker" then . + {secrets: [{name: "envoy-broker-token-abcde"}]} else . end)'
      want="$fence_outside_why: other-7f1a2b-klmno (runs as dispatch, takes envoy-broker-token-abcde)"
      ;;
  esac
  case "$1" in
    zd-control)
      copy="$(script_copy any-secret)"
      cp "$(dirname "$fence_check")/operator-token-minters/broker-fence.json" "$(dirname "$copy")/operator-token-minters/broker-fence.json"
      plant "$(dirname "$copy")/operator-token-minters/broker-fence.jq" ' or (.undeclared | length) > 0)' ')'
      script="$copy"
      ;;
    zl-control)
      copy="$(script_copy no-listed-secrets)"
      cp "$(dirname "$fence_check")/operator-token-minters/broker-fence.json" "$(dirname "$copy")/operator-token-minters/broker-fence.json"
      plant "$(dirname "$copy")/operator-token-minters/step-rbac.sh" '| .metadata.namespace as $ns | .secrets | .[]? | .name?' '| .metadata.namespace as $ns | empty'
      script="$copy"
      ;;
  esac
  run_script="$script" fence_run
  case "$1" in
    *-control)
      want_status 0 "a copy blind to the Secret ($1)"
      want_fence_credits "a copy blind to the Secret ($1)"
      ;;
    *)
      want_status 1 "another pod takes the broker's token Secret ($1)"
      want_fence_findings "another pod takes the broker's token Secret ($1)"
      want_text "$want" "another pod takes the broker's token Secret ($1)"
      ;;
  esac
}
if row zd; then
  token_taken_row zd
  passed "(zd) another pod in dispatch taking a token Secret no render declares and no ServiceAccount lists is NO CREDIT, naming it, and exits 1"
  token_taken_row zd-control
  passed "(zd) control: a copy that holds no other pod to the declared Secrets credits zd's world, exit 0"
fi
if row ze; then
  token_taken_row ze
  passed "(ze) the same pod, the Secret declared the broker's token Secret, is NO CREDIT, naming it, and exits 1"
fi
if row zl; then
  token_taken_row zl
  passed "(zl) the same pod, the Secret declared of another type but listed by the broker's ServiceAccount, is NO CREDIT, naming it, and exits 1"
  token_taken_row zl-control
  passed "(zl) control: a copy that reads no ServiceAccount's secrets credits zl's world, exit 0"
fi

# --- (fc) Whoever can write the fence stays a finding while it is credited. ---
fence_writer_row() {
  local want
  fence_world
  case "$1" in
    patch-binding)
      subject_grant operator-cluster "$(sa_subject legion worker)" patch admissionregistration.k8s.io validatingadmissionpolicybindings "" "" ""
      want="FINDING system:serviceaccount:legion:worker: patch validatingadmissionpolicybindings.admissionregistration.k8s.io cluster-wide"
      ;;
    delete-policy)
      subject_grant operator-cluster "$(sa_subject legion worker)" delete admissionregistration.k8s.io validatingadmissionpolicies "" "" ""
      want="FINDING system:serviceaccount:legion:worker: delete validatingadmissionpolicies.admissionregistration.k8s.io cluster-wide, which removes the broker's pod fence"
      ;;
    delete-named)
      subject_grant operator-cluster "$(sa_subject legion worker)" delete admissionregistration.k8s.io validatingadmissionpolicies "" "" broker-pod-connect
      want="FINDING system:serviceaccount:legion:worker: delete validatingadmissionpolicies.admissionregistration.k8s.io broker-pod-connect cluster-wide, which removes the broker's pod fence"
      ;;
  esac
  fence_run
  want_status 1 "a writer of the fence ($1)"
  want_line "$want" "a writer of the fence ($1)"
  want_fence_credits "a writer of the fence ($1)"
}
if row fc; then
  each fence_writer_row patch-binding delete-policy delete-named
  passed "(fc) a principal that can patch the fence's binding or delete its policy, outright or by name, stays a finding beside the credits"
fi

# --- (fd) A chain through dispatch-measure, whose credited exec carries the residual, to a writer of
# the admission policies. ---
if row fd; then
  chain="create serviceaccounts/token dispatch-measure in legion, so it can become system:serviceaccount:legion:dispatch-measure, which can"
  fence_world
  subject_grant operator-cluster "$(sa_subject legion worker)" create "" serviceaccounts token legion dispatch-measure
  fence_run
  want_status 0 "dispatch-measure with its credited rights alone"
  want_fence_credits "dispatch-measure with its credited rights alone"
  want_line "$(allowance_line system:serviceaccount:legion:worker "$chain create pods/exec in dispatch")" "dispatch-measure with its credited rights alone"
  ! has_prefix "FINDING system:serviceaccount:legion:worker: $chain" || fail "dispatch-measure with its credited rights alone: who can become it is a finding"
  subject_grant operator-cluster "$(sa_subject legion dispatch-measure)" patch admissionregistration.k8s.io validatingadmissionpolicies "" "" ""
  fence_run
  want_status 1 "dispatch-measure that can patch admission policies"
  want_line "FINDING system:serviceaccount:legion:dispatch-measure: patch validatingadmissionpolicies.admissionregistration.k8s.io cluster-wide" \
    "dispatch-measure that can patch admission policies"
  want_line "FINDING system:serviceaccount:legion:worker: $chain patch validatingadmissionpolicies.admissionregistration.k8s.io cluster-wide" \
    "dispatch-measure that can patch admission policies"
  want_fence_credits "dispatch-measure that can patch admission policies"
  passed "(fd) who can become dispatch-measure is an ALLOWANCE naming the chain and the residual while dispatch-measure holds its credited exec alone, and a finding once dispatch-measure can patch the admission policies"
fi

# --- (fj) Who can become a credited grant's holder reaches the residual too, and is named. ---
# The comment-delivery account's resource, whose minters the check reads once its credited exec
# carries the residual.
readonly comment_delivery_res="//iam.googleapis.com/projects/$project/serviceAccounts/$comment_delivery"
residual_chain_row() {
  local want
  fence_world
  edit "$world/clusters/operator-cluster/serviceaccounts.json" '. + [{namespace: "agent-sessions", name: "runner-x"}, {namespace: "agent-sessions", name: "runner-w"}]'
  case "$1" in
    token)
      # An agent's ServiceAccount that can mint dispatch-measure's token.
      subject_grant operator-cluster "$(sa_subject agent-sessions runner-x)" create "" serviceaccounts token legion dispatch-measure
      want="$(allowance_line system:serviceaccount:agent-sessions:runner-x "create serviceaccounts/token dispatch-measure in legion, so it can become system:serviceaccount:legion:dispatch-measure, which can create pods/exec in dispatch")"
      ;;
    minter)
      # A Google service account that can mint comment-delivery's token.
      iam "$comment_delivery_res" roles/iam.serviceAccountTokenCreator "serviceAccount:minter@$project.iam.gserviceaccount.com"
      want="ALLOWANCE serviceAccount:minter@$project.iam.gserviceaccount.com: iam.serviceAccounts.getAccessToken"
      ;;
    member)
      # A Workload Identity member that acts as comment-delivery, and an agent that can become it.
      iam "$comment_delivery_res" roles/iam.workloadIdentityUser "serviceAccount:$project.svc.id.goog[agent-sessions/runner-w]"
      subject_grant operator-cluster "$(sa_subject agent-sessions runner-x)" create "" serviceaccounts token agent-sessions runner-w
      want="$(allowance_line system:serviceaccount:agent-sessions:runner-x "create serviceaccounts/token runner-w in agent-sessions, so it can become system:serviceaccount:agent-sessions:runner-w, which can act as $comment_delivery (which can create pods/exec in dispatch) through Workload Identity")"
      ;;
  esac
  fence_run
  want_status 0 "the residual reached through $1"
  want_fence_credits "the residual reached through $1"
  [ "$(count_prefix "FINDING ")" -eq 0 ] || fail "the residual reached through $1: a FINDING line"
  case "$1" in
    minter)
      want_text "$want" "the residual reached through $1"
      want_text ", so it can act as serviceAccount:$comment_delivery, which can create pods/exec in dispatch, $fence_residual" "the residual reached through $1"
      ;;
    member)
      want_text "ALLOWANCE serviceAccount:$project.svc.id.goog[agent-sessions/runner-w]: " "the residual reached through $1"
      want_line "$want" "the residual reached through $1"
      ;;
    *) want_line "$want" "the residual reached through $1" ;;
  esac
}
if row fj; then
  each residual_chain_row token minter member
  passed "(fj) an agent that can mint dispatch-measure's token, a Google account that can mint comment-delivery's, and an agent that can become comment-delivery's Workload Identity member each print an ALLOWANCE naming the chain and the residual"
fi

# --- (fl), (fm) and (fn): the Google service accounts the chains name are read to a fixed point,
# so a chain that runs Google, Kubernetes, then Google again is named, and by the right verdict. ---
# gsa_res NAME: the resource of the Google service account NAME@<project>.
gsa_res() { echo "//iam.googleapis.com/projects/$project/serviceAccounts/$1@$project.iam.gserviceaccount.com"; }
fixed_point_world() {
  fence_world
  edit "$world/clusters/operator-cluster/serviceaccounts.json" '. + [{namespace: "agent-sessions", name: "runner-w"}, {namespace: "agent-sessions", name: "runner-y"},
    {namespace: "agent-sessions", name: "runner-z"}]'
}
# (fl) An account found by a chain through a Workload Identity member, which also holds a credited
# exec: its minters are findings, not ALLOWANCEs. fg@ can patch admission policies (a finding);
# agent-sessions/runner-w is its Workload Identity member; g2@ can mint runner-w's token and holds
# create pods/exec in dispatch; minter4@ can mint g2@'s token.
if row fl; then
  fixed_point_world
  subject_grant operator-cluster "$(user_subject "fg@$project.iam.gserviceaccount.com")" patch admissionregistration.k8s.io validatingadmissionpolicies "" "" ""
  iam "$(gsa_res fg)" roles/iam.workloadIdentityUser "serviceAccount:$project.svc.id.goog[agent-sessions/runner-w]"
  subject_grant operator-cluster "$(user_subject "g2@$project.iam.gserviceaccount.com")" create "" serviceaccounts token agent-sessions runner-w
  subject_grant operator-cluster "$(user_subject "g2@$project.iam.gserviceaccount.com")" create "" pods exec dispatch ""
  iam "$(gsa_res g2)" roles/iam.serviceAccountTokenCreator "serviceAccount:minter4@$project.iam.gserviceaccount.com"
  fence_run
  want_status 1 "an account found through a member, holding a credited exec"
  want_text "FINDING serviceAccount:g2@$project.iam.gserviceaccount.com: create serviceaccounts/token runner-w in agent-sessions, so it can become system:serviceaccount:agent-sessions:runner-w, which can act as fg@$project.iam.gserviceaccount.com through Workload Identity" \
    "an account found through a member, holding a credited exec"
  want_text "FINDING serviceAccount:minter4@$project.iam.gserviceaccount.com: iam.serviceAccounts.getAccessToken" \
    "an account found through a member, holding a credited exec"
  ! has_prefix "ALLOWANCE serviceAccount:minter4@" || fail "an account found through a member, holding a credited exec: minter4 is an ALLOWANCE"
  passed "(fl) a Google account found through a Workload Identity member, which also holds a credited exec, has its minters found, never allowed"
fi
# (fm) Google, Kubernetes, then Google: runner-w is comment-delivery's Workload Identity member, g3@
# can mint runner-w's token, and minter5@ can mint g3@'s.
if row fm; then
  fixed_point_world
  iam "$comment_delivery_res" roles/iam.workloadIdentityUser "serviceAccount:$project.svc.id.goog[agent-sessions/runner-w]"
  subject_grant operator-cluster "$(user_subject "g3@$project.iam.gserviceaccount.com")" create "" serviceaccounts token agent-sessions runner-w
  iam "$(gsa_res g3)" roles/iam.serviceAccountTokenCreator "serviceAccount:minter5@$project.iam.gserviceaccount.com"
  fence_run
  want_status 0 "a chain Google, Kubernetes, then Google to the residual"
  [ "$(count_prefix "FINDING ")" -eq 0 ] || fail "a chain Google, Kubernetes, then Google to the residual: a FINDING line"
  want_line "$(allowance_line "serviceAccount:g3@$project.iam.gserviceaccount.com" "create serviceaccounts/token runner-w in agent-sessions, so it can become system:serviceaccount:agent-sessions:runner-w, which can act as $comment_delivery (which can create pods/exec in dispatch) through Workload Identity")" \
    "a chain Google, Kubernetes, then Google to the residual"
  want_text "ALLOWANCE serviceAccount:minter5@$project.iam.gserviceaccount.com: iam.serviceAccounts.getAccessToken" \
    "a chain Google, Kubernetes, then Google to the residual"
  want_text ", so it can act as serviceAccount:g3@$project.iam.gserviceaccount.com, which can create serviceaccounts/token runner-w in agent-sessions" \
    "a chain Google, Kubernetes, then Google to the residual"
  want_text "PASS live: minters of g3@$project.iam.gserviceaccount.com, whose credited grant reaches the broker's pod fence's residual: " \
    "a chain Google, Kubernetes, then Google to the residual"
  passed "(fm) a chain Google, Kubernetes, then Google to the residual names its last minter by an ALLOWANCE"
fi
# (fn) A target found partway through a round: dispatch-measure, listed for the residual, can mint
# runner-z's token, and runner-z can patch admission policies; runner-y can mint dispatch-measure's
# token. runner-y's lines name dispatch-measure by the right its verdict reads: an ALLOWANCE by the
# residual's, then a FINDING by the found one's, and no FINDING by the residual's.
if row fn; then
  fixed_point_world
  subject_grant operator-cluster "$(sa_subject agent-sessions runner-z)" patch admissionregistration.k8s.io validatingadmissionpolicies "" "" ""
  subject_grant operator-cluster "$(sa_subject legion dispatch-measure)" create "" serviceaccounts token agent-sessions runner-z
  subject_grant operator-cluster "$(sa_subject agent-sessions runner-y)" create "" serviceaccounts token legion dispatch-measure
  fence_run
  want_status 1 "a target found partway through a round"
  become_measure="create serviceaccounts/token dispatch-measure in legion, so it can become system:serviceaccount:legion:dispatch-measure, which can"
  want_line "FINDING system:serviceaccount:agent-sessions:runner-y: $become_measure create serviceaccounts/token runner-z in agent-sessions, so it can become system:serviceaccount:agent-sessions:runner-z, which can patch validatingadmissionpolicies.admissionregistration.k8s.io cluster-wide" \
    "a target found partway through a round"
  ! has_line "FINDING system:serviceaccount:agent-sessions:runner-y: $become_measure create pods/exec in dispatch" ||
    fail "a target found partway through a round: a FINDING names the chain to the residual alone"
  passed "(fn) a chain to an account found partway through a round is a finding by the found right, never by the residual's"
fi

# --- (fk) A change of a pod in dispatch is no credit: broker-pod-update admits a change of any
# pod's metadata, and a pod given the broker's label app=envoy-broker takes its Services' traffic. ---
pod_change_row() {
  fence_world
  subject_grant operator-cluster "$(sa_subject legion dispatch-measure)" "$1" "" pods "" dispatch ""
  fence_run
  want_status 1 "dispatch-measure with $1 pods in dispatch"
  want_line "FINDING system:serviceaccount:legion:dispatch-measure: $1 pods in dispatch" "dispatch-measure with $1 pods in dispatch"
  ! has_prefix "CREDIT system:serviceaccount:legion:dispatch-measure: $1 pods in dispatch" ||
    fail "dispatch-measure with $1 pods in dispatch: the grant is credited"
  want_fence_credits "dispatch-measure with $1 pods in dispatch"
}
if row fk; then
  each pod_change_row patch update
  passed "(fk) patch or update of pods in dispatch, by which a label-only patch puts another pod behind the broker's Services, stays a finding beside the fence's credits"
fi

# --- (fs) Each binding spec the contract holds decides alone. ---
# fence_variant NAME PROGRAM [ARGS...]: a copy of the script (script_copy) whose broker-fence.json is
# PROGRAM's rewrite of fence_check's (jq, with ARGS); prints the copy's script, for fence_run.
fence_variant() {
  local copy program="$2"
  copy="$(script_copy "fence-$1")"
  shift 2
  jq "$program" "$@" "$(dirname "$fence_check")/operator-token-minters/broker-fence.json" >"$(dirname "$copy")/operator-token-minters/broker-fence.json"
  printf '%s' "$copy"
}
# The binding $b's selector widened to dispatch-canary too, which still holds dispatch: a contract
# object ({kind, name, spec}) or a render's or a cluster's.
readonly fence_widen='if .kind == "ValidatingAdmissionPolicyBinding" and (.name // .metadata.name) == $b
  then .spec.matchResources.namespaceSelector.matchExpressions[0].values = ["dispatch", "dispatch-canary"] else . end'
binding_spec_row() {
  local variant
  variant="$(fence_variant "binding-$1" ".objects |= map($fence_widen)" --arg b "$1")"
  # The contract, the renders and the cluster agree on the widened binding: credited.
  fence_world
  fence_edit both "$fence_widen" --arg b "$1"
  run_script="$variant" fence_run
  want_status 0 "the binding $1 widened in the contract and on the cluster alike"
  want_fence_credits "the binding $1 widened in the contract and on the cluster alike"
  # The contract's widened binding against the fence as fence_objects writes it: no credit.
  fence_world
  run_script="$variant" fence_run
  want_status 1 "the binding $1 widened in the contract alone"
  want_fence_findings "the binding $1 widened in the contract alone"
  want_text "in the renders, the ValidatingAdmissionPolicyBinding $1 differs from the reviewed fence in matchResources" \
    "the binding $1 widened in the contract alone"
}
if row fs; then
  each binding_spec_row broker-pod-connect broker-pod-update broker-pod-create broker-workload-create broker-workload-update
  passed "(fs) each binding spec broker-fence.json holds decides: one widened to dispatch-canary in the contract and on the cluster is credited, and in the contract alone credits nothing"
fi

# --- (fp) and (fq): the fence's verdict on a right travels as a policy and a flag, never as the
# residual's text, which credit and residual_allowance read from the credited fence alone. ---
# The fence as fence_refuses reads it, with the contract's residual beside it, as the check before
# this round passed it.
fence_with_residual="$(jq -c '{namespace, account, direct: true, residual}' "$fence_contract")"
readonly fence_with_residual
fence_right() {
  jq -n -c --arg v "$1" --arg s "$2" --argjson t "${3:-null}" '{verb: $v, group: "", resource: "pods", sub: $s, ns: "dispatch", name: "",
    label: "\($v) pods/\($s) in dispatch"} + (if $t then {targets: $t} else {} end) | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}'
}
if row fp; then
  # Two principals, the first allowed and the second refused both rights: an exec without targets,
  # and a port forward that becomes envoy-broker. The answer is one SubjectAccessReview document per
  # review, as kubectl create prints them and as review reads them (jq -s).
  jq -n -c --argjson r1 "$(fence_right create exec)" --argjson r2 "$(fence_right create portforward '["dispatch:envoy-broker"]')" '[$r1, $r2]' \
    >"$temporary_dir/fp-rights.json"
  jq -n -c '[{principal: "p1", user: "u1", groups: []}, {principal: "p2", user: "u2", groups: []}]' >"$temporary_dir/fp-principals.json"
  jq -n -c --slurpfile r "$temporary_dir/fp-rights.json" '("u1", "u2") as $u | $r[0][]
    | {apiVersion: "authorization.k8s.io/v1", kind: "SubjectAccessReview",
       spec: {user: $u, groups: [], resourceAttributes: {verb, group, resource, subresource: .sub, namespace: .ns, name}},
       status: {allowed: ($u == "u1")}}' >"$temporary_dir/fp-answer.json"
  got="$(jq -r -s --slurpfile principals "$temporary_dir/fp-principals.json" --slurpfile rights "$temporary_dir/fp-rights.json" \
    --argjson fence "$fence_with_residual" -L "$(dirname "$check")/operator-token-minters" -f "$(dirname "$check")/operator-token-minters/review-answer.jq" \
    "$temporary_dir/fp-answer.json" | awk -F'\x1f' '{ print $1 "|" $3 "|" $6 "|" $7 }' | sort)"
  want="$(printf '%s\n' "p1|allowed|broker-pod-connect|residual" "p1|allowed|broker-pod-connect|" "p2|refused||" "p2|refused||" | sort)"
  [ "$got" = "$want" ] || fail "review-answer.jq's fence fields are not a policy and a flag, and none for a refused review: $got"
  passed "(fp) review-answer.jq gives an allowed credited right the policy and the flag residual (none for one with targets), and a refused review no fence field"
fi
if row fq; then
  jq -n -c "[$(fence_right create exec)]" >"$temporary_dir/fq-rights.json"
  got="$(jq -n '{kind: "List", items: [{apiVersion: "rbac.authorization.k8s.io/v1", kind: "Role", metadata: {name: "r", namespace: "dispatch"},
      rules: [{apiGroups: [""], resources: ["pods/exec"], verbs: ["create"]}]},
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", metadata: {name: "r", namespace: "dispatch"},
      roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "Role", name: "r"}, subjects: [{kind: "User", name: "u1"}]}]}' |
    jq -r -n --slurpfile rights "$temporary_dir/fq-rights.json" --slurpfile named_rights "$temporary_dir/fq-rights.json" --argjson live false \
      --argjson fence "$fence_with_residual" -L "$(dirname "$check")/operator-token-minters" -f "$(dirname "$check")/operator-token-minters/code-rbac.jq" |
    awk -F'\x1f' '$1 == "grant" { print $(NF - 1) "|" $NF }')"
  [ "$got" = "broker-pod-connect|residual" ] || fail "code-rbac.jq's fence fields of a declared credited grant are not the policy and the flag: $got"
  passed "(fq) code-rbac.jq gives a declared credited grant, which the code step holds for fence_settle, the policy and the flag residual"
fi

# --- (fe) Becoming envoy-broker by entering a pod of dispatch, which the fence refuses. ---
if row fe; then
  fence_world
  subject_grant operator-cluster "$(sa_subject dispatch envoy-broker)" create "" pods "" dispatch ""
  fence_run
  want_status 1 "envoy-broker with a finding"
  want_line "FINDING system:serviceaccount:dispatch:envoy-broker: create pods in dispatch" "envoy-broker with a finding"
  chain_right="create pods/portforward in dispatch, so it can become system:serviceaccount:dispatch:envoy-broker, which can create pods in dispatch"
  want_line "$(credit_line system:serviceaccount:legion:worker "$chain_right" broker-pod-connect)" "envoy-broker with a finding"
  ! has_prefix "ALLOWANCE system:serviceaccount:legion:worker: $chain_right" ||
    fail "envoy-broker with a finding: the chain carries an ALLOWANCE of its own, beside its grant's"
  [ "$(count_prefix "FINDING system:serviceaccount:legion:worker:")" -eq 0 ] || fail "envoy-broker with a finding: legion/worker is a finding"
  subject_grant operator-cluster "$(sa_subject dispatch dispatch)" create "" pods "" dispatch ""
  fence_run
  want_status 1 "envoy-broker and dispatch with a finding"
  want_text "FINDING system:serviceaccount:legion:worker: create pods/portforward in dispatch, so it can become system:serviceaccount:dispatch:dispatch, which can create pods in dispatch, or system:serviceaccount:dispatch:envoy-broker" \
    "envoy-broker and dispatch with a finding"
  passed "(fe) who can enter a pod of dispatch is credited for becoming envoy-broker, and found once it can become dispatch/dispatch too"
fi

# --- (ff) A Google IAM minter of the broker's account is no pod route: the fence excuses it not. ---
if row ff; then
  fence_world
  iam "$broker_res" roles/iam.serviceAccountTokenCreator "serviceAccount:$comment_delivery"
  fence_run
  want_status 1 "comment-delivery minting the broker's token"
  want_text "FINDING serviceAccount:$comment_delivery: iam.serviceAccounts.getAccessToken," "comment-delivery minting the broker's token"
  want_text " on $broker_res through roles/iam.serviceAccountTokenCreator" "comment-delivery minting the broker's token"
  want_fence_credits "comment-delivery minting the broker's token"
  passed "(ff) a holder of roles/iam.serviceAccountTokenCreator on the broker's account stays a finding beside the fence's credits"
fi

# --- (fg) The check's own contract holds the reviewed workload policies by digest, which the
# example ones do not hash to. ---
if row fg; then
  fence_world
  run --broker-workload dispatch/envoy-broker --db-secret dispatch/envoy-broker-db
  want_status 1 "the example fence against the reviewed digests"
  want_fence_findings "the example fence against the reviewed digests"
  want_text "in the renders, the ValidatingAdmissionPolicy broker-workload-create's rules hash to $(jq -r '.["broker-workload-create"]' <<<"$fence_example_digests"), not to the reviewed $(jq -r '.objects[] | select(.name == "broker-workload-create" and .digest) | .digest' "$fence_contract"); the ValidatingAdmissionPolicy broker-workload-update's rules hash to " \
    "the example fence against the reviewed digests"
  passed "(fg) the check itself credits nothing of a fence whose workload policies hash to other digests than its contract's"
fi

# --- (fr) What fence_refuses credits is what the reviewed rules refuse. ---
# fence_rule_cover CONTRACT: for each right on pods in the contract's namespace that fence_refuses
# credits, a record of the verb, the subresource, the policy, whether that policy's committed rules
# hold the operation and resource the API server admits it as (CONNECT for exec, attach and port
# forward; UPDATE for patch and update), and whether every validation of it names the pod prefix.
fence_rule_cover() {
  jq -r -n -L "$(dirname "$fence_check")/operator-token-minters" --slurpfile contract "$1" '
    include "rec"; include "broker-fence";
    $contract[0] as $c
    | {namespace: $c.namespace, account: $c.account, direct: true, residual: $c.residual} as $fence
    | ("create", "get", "patch", "update", "delete", "deletecollection", "list", "watch") as $verb
    | ("", "exec", "attach", "portforward", "ephemeralcontainers", "log", "proxy", "eviction", "binding", "status") as $sub
    | {verb: $verb, group: "", resource: "pods", sub: $sub, ns: $c.namespace, name: ""} as $r
    | fence_refuses($fence; $r) as $policy
    | (if $verb == "create" or $verb == "get" then "CONNECT" else "UPDATE" end) as $op
    | (if $sub == "" then "pods" else "pods/\($sub)" end) as $res
    | ([$c.objects[] | select(.kind == "ValidatingAdmissionPolicy" and .name == $policy) | .spec][0]) as $spec
    | [$verb, $sub, $policy,
       ($spec != null and any($spec.matchConstraints.resourceRules[];
         any(.operations[]; . == $op) and any(.resources[]; . == $res) and any(.apiGroups[]; . == ""))),
       ($spec != null and all($spec.validations[]; .expression | contains("startsWith(\u0027\($c.prefix)\u0027)")))] | @tsv'
}
if row fr; then
  cover="$(fence_rule_cover "$fence_contract")"
  want="$(printf '%s\n' "create	exec	broker-pod-connect	true	true" "create	attach	broker-pod-connect	true	true" \
    "create	portforward	broker-pod-connect	true	true" "get	exec	broker-pod-connect	true	true" "get	attach	broker-pod-connect	true	true" \
    "get	portforward	broker-pod-connect	true	true" | sort)"
  [ "$(sort <<<"$cover")" = "$want" ] ||
    fail "fence_refuses credits other rights than the reviewed rules refuse on the broker's pods: $cover"
  # The controls: the same reading of a contract whose connect policy lacks port forward, and of
  # one whose connect policy's rule no longer names the prefix, each fails its column.
  jq '(.objects[] | select(.kind == "ValidatingAdmissionPolicy" and .name == "broker-pod-connect") | .spec.matchConstraints.resourceRules)
    |= map(select(.resources != ["pods/portforward"]))' "$fence_contract" >"$temporary_dir/fence-contract-no-portforward.json"
  grep -qxF "create	portforward	broker-pod-connect	false	true" <<<"$(fence_rule_cover "$temporary_dir/fence-contract-no-portforward.json")" ||
    fail "the reading of a contract whose connect policy lacks port forward still finds port forward covered"
  jq '(.objects[] | select(.kind == "ValidatingAdmissionPolicy" and .name == "broker-pod-connect") | .spec.validations[0].expression) = "true"' \
    "$fence_contract" >"$temporary_dir/fence-contract-no-prefix.json"
  grep -qxF "create	exec	broker-pod-connect	true	false" <<<"$(fence_rule_cover "$temporary_dir/fence-contract-no-prefix.json")" ||
    fail "the reading of a contract whose connect rule names no prefix still finds the prefix named"
  passed "(fr) each right fence_refuses credits is one the reviewed broker-pod-connect rules admit as CONNECT on pods, every rule naming the envoy-broker- prefix; a contract without the port forward rule, or without the prefix, shows it"
fi

# --- (fh) An ephemeral container in dispatch is no credit: on a pod not the broker's it takes any
# Secret of the namespace. ---
fence_ephemeral_row() {
  fence_world
  subject_grant operator-cluster "$(sa_subject legion worker)" "$1" "" pods ephemeralcontainers dispatch ""
  fence_run
  want_status 1 "legion/worker with $1 pods/ephemeralcontainers in dispatch"
  want_line "FINDING system:serviceaccount:legion:worker: $1 pods/ephemeralcontainers in dispatch" \
    "legion/worker with $1 pods/ephemeralcontainers in dispatch"
  ! has_prefix "CREDIT system:serviceaccount:legion:worker: $1 pods/ephemeralcontainers" ||
    fail "legion/worker with $1 pods/ephemeralcontainers in dispatch: the grant is credited"
  want_fence_credits "legion/worker with $1 pods/ephemeralcontainers in dispatch"
}
if row fh; then
  each fence_ephemeral_row patch update
  passed "(fh) patch or update of pods/ephemeralcontainers in dispatch stays a finding beside the fence's credits"
fi
