# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-code.sh: the rows of operator-token-minters.test.sh on the code
# read: the RBAC render's grants and the IAM preview's bindings, each run with every live step
# refused (refuse_live), so each finding is the code's. Sourced by the test after its world and
# helpers; its header says what the fakes model. The rows:
#
#   (p)  the code read: a grant the RBAC render or an IAM preview declares is a finding with every
#        live step refused; a binding to a role the render does not hold, a member a preview does
#        not know, an IAM resource type the check does not read, and a missing render or preview,
#        each read UNKNOWN;
#   (af) a render binding the Group cluster-admins@example.com to cluster-admin exits 0 while the
#        operator's cluster has Google Groups for RBAC off: a Group other than Kubernetes' own is
#        judged by where the cluster takes its groups from, as in the live steps;
#   (ap) a render's rule limited by resourceNames on impersonate or escalate is a finding naming the
#        names, with every live step refused;
#   (be) a render's rule limited by resourceNames on bind of clusterroles, or on update or patch of
#        rolebindings, is a finding naming the names, with every live step refused; and a render's
#        bind of clusterroles in example-db, a namespace outside the guarded ones, outright or by
#        name, is a finding naming that namespace (the live steps' binding rights, read in code);
#   (bw) a render's rule limited by resourceNames on update of a mutating webhook configuration, or
#        on patch of a Deployment in secrets-broker, is a finding naming the names;
#   (se) a Secret render carries no printed Secret value; malformed JSON, a file of no document (zero
#        bytes, or blanks), a render that declares no Secret, and a token Secret without its
#        namespace, name or service-account-name annotation each read UNKNOWN, exit 2; a Secret of
#        another type without a namespace is read; controls: a copy that reads the records by tab
#        again reads a token Secret missing its namespace or its name as another type, and a copy
#        without the document and Secret witnesses passes the empty files, each exit 0;
#   (ca) a render's create namespaces, create of a mutating webhook configuration, and the
#        certificate chain (create, patch of the approval, approve of the client signer by name)
#        are each a finding, the chain as one, and create of requests alone is none.

# --- (p) The code read. Every live step is refused, so each finding is the code's. ---
if row p; then
  base_world
  refuse_live
  edit "$world/render.json" '.items += [
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", metadata: {name: "worker-runs-pods", namespace: "envoy-operator"},
     roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "pod-runner"},
     subjects: [{kind: "ServiceAccount", name: "worker", namespace: "legion"}]},
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole", metadata: {name: "pod-runner"},
     rules: [{apiGroups: ["", "apps"], resources: ["pods", "deployments"], verbs: ["create"]}]}]'
  run
  want_status 1 "a RoleBinding the render declares"
  want_line "FINDING system:serviceaccount:legion:worker: create pods in envoy-operator, declared by RoleBinding envoy-operator/worker-runs-pods" "the render's grant"
  want_line "FINDING system:serviceaccount:legion:worker: create deployments.apps in envoy-operator, declared by RoleBinding envoy-operator/worker-runs-pods" "the render's grant"
  passed "(p) a grant the RBAC render declares is a finding with every live step refused"

  base_world
  refuse_live
  edit "$world/render.json" '.items += [
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", metadata: {name: "sessions-edit"},
     roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "edit"},
     subjects: [{kind: "Group", name: "system:serviceaccounts:legion"}]}]'
  run
  want_status 2 "a binding to a role the render does not hold"
  want_text "UNKNOWN code: RBAC: ClusterRoleBinding sessions-edit binds group:system:serviceaccounts:legion to ClusterRole edit, which the render does not hold" \
    "a role the render does not hold"
  passed "(p) a binding to a role the render does not hold reads UNKNOWN"

  base_world
  refuse_live
  edit "$world/preview.json" '.steps += [{op: "same", urn: "urn:pulumi:example::iam::gcp:projects/iAMMember:IAMMember::ci-container",
    newState: {type: "gcp:projects/iAMMember:IAMMember", inputs: {project: "example-project", role: "roles/container.admin", member: "serviceAccount:ci-runner@example-project.iam.gserviceaccount.com"}}}]'
  run
  want_status 1 "a binding an IAM preview declares"
  want_text "FINDING serviceAccount:$ci_gsa: container.serviceAccounts.createToken," "the preview's binding"
  want_text " on $project_res through roles/container.admin, declared by urn:pulumi:example::iam::gcp:projects/iAMMember:IAMMember::ci-container" "the preview's binding"
  passed "(p) a binding an IAM preview declares is a finding with every live step refused"

  base_world
  refuse_live
  edit "$world/preview.json" '.steps += [{op: "create", urn: "urn:pulumi:example::iam::gcp:serviceaccount/iAMMember:IAMMember::new-minter",
    newState: {type: "gcp:serviceaccount/iAMMember:IAMMember", inputs: {serviceAccountId: "projects/example-project/serviceAccounts/envoy-operator@example-project.iam.gserviceaccount.com",
      role: "roles/iam.serviceAccountTokenCreator", member: "04da6b54-80e4-46f7-96ec-b56ff0331ba9"}}}]'
  run
  want_status 2 "a member the preview does not know"
  want_text "UNKNOWN code: IAM: urn:pulumi:example::iam::gcp:serviceaccount/iAMMember:IAMMember::new-minter: the preview does not know its member" \
    "a member the preview does not know"
  passed "(p) a binding whose member the preview does not know reads UNKNOWN"

  base_world
  refuse_live
  edit "$world/preview.json" '.steps += [{op: "same", urn: "urn:pulumi:example::iam::gcp:storage/defaultObjectAccessControl:DefaultObjectAccessControl::state",
    newState: {type: "gcp:storage/defaultObjectAccessControl:DefaultObjectAccessControl", inputs: {bucket: "example-state", entity: "allAuthenticatedUsers", role: "READER"}}}]'
  run
  want_status 2 "an IAM resource type the check does not read"
  want_text "UNKNOWN code: IAM: urn:pulumi:example::iam::gcp:storage/defaultObjectAccessControl:DefaultObjectAccessControl::state: the check does not read gcp:storage/defaultObjectAccessControl:DefaultObjectAccessControl" \
    "an unread type"
  passed "(p) an IAM resource type the check does not read reads UNKNOWN"

  base_world
  mapfile -t args < <(args_without --rbac-render --iam-preview)
  run_only "${args[@]}"
  want_status 2 "no render and no preview"
  want_text "UNKNOWN code: RBAC: no --rbac-render" "no render"
  want_text "UNKNOWN code: IAM: no --iam-preview" "no preview"
  passed "(p) a run given no render and no preview reads both code steps UNKNOWN"
fi

# --- (af) One rule for Groups: a Group other than the system ones is no finding in the code step. ---
if row af; then
  base_world
  edit "$world/render.json" '.items += [{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding",
    metadata: {name: "admins", namespace: "envoy-operator"},
    roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "cluster-admin"},
    subjects: [{apiGroup: "rbac.authorization.k8s.io", kind: "Group", name: "cluster-admins@example.com"}]}]'
  run
  want_status 0 "a render binding the Group cluster-admins@example.com to cluster-admin"
  [ "$(count_prefix 'FINDING ')" -eq 0 ] || fail "a render binding the Group cluster-admins@example.com to cluster-admin: a finding"
  passed "(af) a render binding a Group other than Kubernetes' own to cluster-admin exits 0 while Google Groups for RBAC is off, as the live steps judge it"
fi

# --- (ap) The code read of a rule limited by resource name. ---
if row ap; then
  base_world
  refuse_live
  edit "$world/render.json" '.items += [
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole", metadata: {name: "masters-and-view"},
     rules: [{apiGroups: [""], resources: ["groups"], verbs: ["impersonate"], resourceNames: ["system:masters"]},
             {apiGroups: ["rbac.authorization.k8s.io"], resources: ["clusterroles"], verbs: ["escalate"], resourceNames: ["view"]}]},
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", metadata: {name: "worker-masters"},
     roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "masters-and-view"},
     subjects: [{kind: "ServiceAccount", name: "worker", namespace: "legion"}]}]'
  run
  want_status 1 "a render's name-limited impersonate and escalate rules"
  want_line "FINDING system:serviceaccount:legion:worker: impersonate groups system:masters cluster-wide, declared by ClusterRoleBinding worker-masters" \
    "a render's name-limited impersonate rule"
  want_line "FINDING system:serviceaccount:legion:worker: escalate clusterroles.rbac.authorization.k8s.io view cluster-wide, declared by ClusterRoleBinding worker-masters" \
    "a render's name-limited escalate rule"
  passed "(ap) a render's rule limited by resourceNames on impersonate or escalate is a finding naming the names"
fi

# --- (be) The code read of bind and binding writes limited by resource name. ---
if row be; then
  base_world
  refuse_live
  edit "$world/render.json" '.items += [
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole", metadata: {name: "binding-writer"},
     rules: [{apiGroups: ["rbac.authorization.k8s.io"], resources: ["clusterroles"], verbs: ["bind"], resourceNames: ["cluster-admin"]},
             {apiGroups: ["rbac.authorization.k8s.io"], resources: ["rolebindings"], verbs: ["update", "patch"], resourceNames: ["envoy-operator-runtime"]}]},
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", metadata: {name: "worker-binding-writer", namespace: "envoy-operator"},
     roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "binding-writer"},
     subjects: [{kind: "ServiceAccount", name: "worker", namespace: "legion"}]}]'
  run
  want_status 1 "a render's name-limited bind and binding writes"
  for right in "bind clusterroles.rbac.authorization.k8s.io cluster-admin" "update rolebindings.rbac.authorization.k8s.io envoy-operator-runtime" \
    "patch rolebindings.rbac.authorization.k8s.io envoy-operator-runtime"; do
    want_line "FINDING system:serviceaccount:legion:worker: $right in envoy-operator, declared by RoleBinding envoy-operator/worker-binding-writer" \
      "a render's name-limited $right"
  done
  passed "(be) a render's rule limited by resourceNames on bind, or on update or patch of a binding, is a finding naming the names"

  # A render's bind of clusterroles in example-db, where a person reads the broker's database
  # login: binding-rights.jq over the renders asks it, as the live steps ask it of a cluster's
  # bindings. Run by hand, the control is step-code-rbac.sh as at 26ab8c9a80, whose rights left
  # those out: "PASS code: RBAC" and "no finding, 10 unknown: exit 2", the UNKNOWNs being the
  # refused live steps. The named item limits the bind to one name.
  for named in "" secret-reader; do
    base_world
    refuse_live
    edit "$world/render.json" '.items += [
      {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole", metadata: {name: "db-binder"},
       rules: [{apiGroups: ["rbac.authorization.k8s.io"], resources: ["clusterroles"], verbs: ["bind"]}
               + (if $n == "" then {} else {resourceNames: [$n]} end)]},
      {apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", metadata: {name: "worker-db-binder", namespace: "example-db"},
       roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "db-binder"},
       subjects: [{kind: "ServiceAccount", name: "worker", namespace: "legion"}]}]' --arg n "$named"
    run
    want_status 1 "a render's bind of clusterroles in example-db (${named:-unnamed})"
    want_line "FINDING system:serviceaccount:legion:worker: bind clusterroles.rbac.authorization.k8s.io ${named:+$named }in example-db, declared by RoleBinding example-db/worker-db-binder" \
      "a render's bind of clusterroles in example-db (${named:-unnamed})"
  done
  passed "(be) a render's bind of clusterroles in a namespace outside the guarded ones, outright or by name, is a finding naming that namespace"
fi

# --- (bw) The code read of any asked right limited by name, the admission and workload rights
# included, as the live roles are read. ---
if row bw; then
  base_world
  refuse_live
  edit "$world/render.json" '.items += [
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole", metadata: {name: "webhook-updater"},
     rules: [{apiGroups: ["admissionregistration.k8s.io"], resources: ["mutatingwebhookconfigurations"], verbs: ["update"], resourceNames: ["existing-webhook"]}]},
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", metadata: {name: "worker-webhook-updater"},
     roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "webhook-updater"},
     subjects: [{kind: "ServiceAccount", name: "worker", namespace: "legion"}]},
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "Role", metadata: {name: "broker-patcher", namespace: "secrets-broker"},
     rules: [{apiGroups: ["apps"], resources: ["deployments"], verbs: ["patch"], resourceNames: ["secrets-broker"]}]},
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", metadata: {name: "worker-broker-patcher", namespace: "secrets-broker"},
     roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "Role", name: "broker-patcher"},
     subjects: [{kind: "ServiceAccount", name: "worker", namespace: "legion"}]}]'
  run
  want_status 1 "a render's name-limited webhook and Deployment writes"
  want_line "FINDING system:serviceaccount:legion:worker: update mutatingwebhookconfigurations.admissionregistration.k8s.io existing-webhook cluster-wide, declared by ClusterRoleBinding worker-webhook-updater" \
    "a render's name-limited webhook update"
  want_line "FINDING system:serviceaccount:legion:worker: patch deployments.apps secrets-broker in secrets-broker, declared by RoleBinding secrets-broker/worker-broker-patcher" \
    "a render's name-limited Deployment patch"
  passed "(bw) a render's rule limited by resourceNames on any asked right, a webhook configuration's update and a Deployment's patch included, is a finding naming the names"
fi

# --- (ca) The code read of the guard rights, the admission rights and the certificate chain, outright:
# a render must not read clean where a live cluster would not. ---
if row ca; then
  base_world
  refuse_live
  edit "$world/render.json" '.items += [
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole", metadata: {name: "wide-writer"},
     rules: [{apiGroups: [""], resources: ["namespaces"], verbs: ["create"]},
             {apiGroups: ["admissionregistration.k8s.io"], resources: ["mutatingwebhookconfigurations"], verbs: ["create"]},
             {apiGroups: ["certificates.k8s.io"], resources: ["certificatesigningrequests"], verbs: ["create"]},
             {apiGroups: ["certificates.k8s.io"], resources: ["certificatesigningrequests/approval"], verbs: ["patch"]},
             {apiGroups: ["certificates.k8s.io"], resources: ["signers"], verbs: ["approve"], resourceNames: ["kubernetes.io/kube-apiserver-client"]}]},
    {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", metadata: {name: "worker-wide-writer"},
     roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "wide-writer"},
     subjects: [{kind: "ServiceAccount", name: "worker", namespace: "legion"}]}]'
  run
  want_status 1 "a render's guard, admission and certificate rights"
  want_line "FINDING system:serviceaccount:legion:worker: create namespaces cluster-wide, declared by ClusterRoleBinding worker-wide-writer" \
    "a render's create namespaces"
  want_line "FINDING system:serviceaccount:legion:worker: create mutatingwebhookconfigurations.admissionregistration.k8s.io cluster-wide, declared by ClusterRoleBinding worker-wide-writer" \
    "a render's create mutatingwebhookconfigurations"
  want_line "FINDING system:serviceaccount:legion:worker: create certificatesigningrequests, patch certificatesigningrequests/approval and approve signers kubernetes.io/kube-apiserver-client cluster-wide, so it can have the cluster sign it a client certificate for a user, group or node it names, declared by the renders" \
    "a render's certificate chain"
  ! has_prefix "FINDING system:serviceaccount:legion:worker: create certificatesigningrequests.certificates.k8s.io" ||
    fail "a render's create certificatesigningrequests is reported alone"
  passed "(ca) a render's guard right, admission right and certificate chain are each a finding, the chain as one"
fi

# --- (se) The Secret render: a Kubernetes-object render that does not expose Secret values, holds a
# JSON document and a Secret, and keeps a token Secret's empty field empty. ---
# secret_token JSON: a token Secret of envoy-operator's, its metadata JSON merged over the full one's.
secret_token() {
  jq -n --argjson m "$1" '{apiVersion: "v1", kind: "List", items: [{apiVersion: "v1", kind: "Secret", type: "kubernetes.io/service-account-token",
    metadata: ({namespace: "envoy-operator", name: "envoy-operator-token-legacy",
                annotations: {"kubernetes.io/service-account.name": "envoy-operator"}} + $m | with_entries(select(.value != null)))}]}'
}
secret_render_row() {
  local copy script="$check" file="$world/secret-render.json"
  base_world
  case "$1" in
    data)
      jq -n --arg canary "SECRET-RENDER-DATA-CANARY" '{apiVersion: "v1", kind: "List", items: [
        {apiVersion: "v1", kind: "Secret",
         metadata: {namespace: "envoy-operator", name: "envoy-operator-token",
                    annotations: {"kubernetes.io/service-account.name": "envoy-operator"}},
         type: "kubernetes.io/service-account-token", data: {token: $canary}, stringData: {token: $canary}}
      ]}' >"$file"
      run
      want_status 0 "a Secret render that carries Secret data"
      want_text "PASS code: Secrets: the Secret renders declare 1 Secret(s), 1 of them guarded service-account token Secret(s)" "a Secret render that carries Secret data"
      ! has_prefix "SECRET-RENDER-DATA-CANARY" || fail "the Secret render's data was printed"
      return
      ;;
    malformed) printf 'not JSON\n' >"$file" ;;
    zero-bytes | zero-bytes-control) : >"$file" ;;
    blanks | blanks-control) printf ' \n\t\n' >"$file" ;;
    no-secret | no-secret-control) jq -n '{apiVersion: "v1", kind: "List", items: [{apiVersion: "v1", kind: "ConfigMap", metadata: {namespace: "dispatch", name: "settings"}}]}' >"$file" ;;
    no-namespace | no-namespace-control) secret_token '{"namespace": null}' >"$file" ;;
    no-name | no-name-control) secret_token '{"name": null}' >"$file" ;;
    no-annotation | no-annotation-control) secret_token '{"annotations": null}' >"$file" ;;
    other-type)
      jq -n '{apiVersion: "v1", kind: "List", items: [{apiVersion: "v1", kind: "Secret", type: "Opaque", metadata: {name: "settings"}}]}' >"$file"
      run
      want_status 0 "an Opaque Secret without a namespace"
      want_text "PASS code: Secrets: the Secret renders declare 1 Secret(s), 0 of them guarded service-account token Secret(s)" "an Opaque Secret without a namespace"
      return
      ;;
  esac
  case "$1" in
    zero-bytes-control | blanks-control | no-secret-control)
      copy="$(script_copy "no-render-witness-$1")"
      plant "$(dirname "$copy")/operator-token-minters/step-code-secrets.sh" 'if length == 0 then error("it holds no JSON document") else . end' '.'
      plant "$(dirname "$copy")/operator-token-minters/step-code-secrets.sh" 'if [ ! -s "$WORK/secret-render-$n.rec" ]; then' 'if false; then'
      script="$copy"
      ;;
    *-control)
      copy="$(script_copy "tab-fields-$1")"
      plant "$(dirname "$copy")/operator-token-minters/step-code-secrets.sh" '      | rec' '      | @tsv'
      plant "$(dirname "$copy")/operator-token-minters/step-code-secrets.sh" 'while IFS="$US" read -r namespace name type service_account' "while IFS=\$'\\t' read -r namespace name type service_account"
      script="$copy"
      ;;
  esac
  run_script="$script" run
  case "$1" in
    *-control)
      want_status 0 "a copy blind to the render's fault ($1)"
      ! has_prefix "UNKNOWN code: Secrets: " || fail "a copy blind to the render's fault ($1) still read UNKNOWN"
      ;;
    malformed | zero-bytes | blanks)
      want_status 2 "a Secret render that is no render ($1)"
      want_text "UNKNOWN code: Secrets: --secret-render entry 1 is not a Kubernetes-object JSON render" "a Secret render that is no render ($1)"
      [ "$1" = malformed ] || want_text "it holds no JSON document" "a Secret render that is no render ($1)"
      ;;
    no-secret)
      want_status 2 "a Secret render that declares no Secret"
      want_line "UNKNOWN code: Secrets: --secret-render entry 1 declares no Secret, so it cannot be told from a render of nothing" "a Secret render that declares no Secret"
      ;;
    *)
      want_status 2 "a token Secret missing a field ($1)"
      want_line "UNKNOWN code: Secrets: --secret-render entry 1 declares a service-account token Secret without its namespace, name or service-account-name annotation" "a token Secret missing a field ($1)"
      ;;
  esac
}
if row se; then
  each secret_render_row data malformed zero-bytes blanks no-secret no-namespace no-name no-annotation other-type \
    zero-bytes-control blanks-control no-secret-control no-namespace-control no-name-control
  passed "(se) a Secret render prints no Secret value; one that is not JSON, holds no document or declares no Secret, and a token Secret missing a field, each read UNKNOWN; each control copy passes the fault, exit 0"
fi
