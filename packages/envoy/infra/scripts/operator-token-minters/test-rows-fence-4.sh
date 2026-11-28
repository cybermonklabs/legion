# shellcheck shell=bash
# shellcheck disable=SC2016,SC2034,SC2154 # the world, its names, its helpers and the output and status a run sets are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-fence-4.sh: the rows of operator-token-minters.test.sh on the
# broker's pod fence (broker-fence.sh), the admission refusal the check credits: the fence's verdict
# on a right as review-answer.jq and code-rbac.jq give it, becoming envoy-broker through a pod of
# dispatch, a Google minter of the broker's account, the check's own contract, the rules
# fence_refuses reads, and an ephemeral container in dispatch. Sourced by the test after its world
# and helpers; its header says what the fakes model. Each row's world (fence_world) puts the broker
# in dispatch, guarded as --broker-workload dispatch/envoy-broker with --db-secret
# dispatch/envoy-broker-db, with the fence of fence_objects (the reviewed rules broker-fence.json
# holds, and example workload policies), and gives the three grants the fence answers:
# comment-delivery's exec (create and get) and dispatch-measure's exec in dispatch, and
# legion/worker's port forward there, live, and comment-delivery's exec declared by the render. The
# rows run fence_check, the check whose contract holds the example workload policies' digests, but
# (fg), and (fp), (fq) and (fr) run only the check's jq programs, in no world. The rows:
#
#   (fe) who can enter a pod of dispatch is credited for becoming envoy-broker, which has a finding,
#        with no ALLOWANCE of its own, and found once dispatch/dispatch has one too;
#   (ff) a Google IAM minter of the broker's account stays a finding beside the credits;
#   (fg) the check itself, whose broker-fence.json holds the reviewed workload policies by digest,
#        credits nothing of the example fence: its workload policies hash to other digests;
#   (fh) patch, or update, of pods/ephemeralcontainers in dispatch stays a finding beside the
#        credits: on a pod not the broker's an ephemeral container takes any Secret of the
#        namespace, --db-secret among them;
#   (fp) review-answer.jq gives an allowed credited right the policy and the flag `residual`, never
#        the residual's text, and a refused review no fence field;
#   (fq) code-rbac.jq gives a declared credited grant the policy and the flag `residual`;
#   (fr) each right fence_refuses credits is one the reviewed broker-pod-connect rules admit as
#        CONNECT on pods, every rule naming the envoy-broker- prefix; the same reading of a contract
#        whose connect policy lacks port forward, or names no prefix, fails that column.
#
# Each control is the row's own other half: (fa), in test-rows-fence-1.sh, against (ff), (fg) and
# (fh) the same world with one grant more or the reviewed digests, (fe) a half without it, and (fr)
# a contract with one rule or one prefix taken out. The check as at b14c08df3df is the other control
# of (fp) and (fq), which each fails.

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
# Secret of the namespace. The two verbs share one world and one run, each on a ServiceAccount of
# its own that holds a grant the fence credits: patch on legion/worker and update on
# legion/dispatch-measure. The check prints each principal's finding and each credit on a line of
# its own, naming it, so each verb is still judged by its own line, exact, and by no CREDIT of its
# own principal for its grant, beside the fence's credits (want_fence_credits); want_recorded holds
# the run's last line to the FINDING and UNKNOWN lines it printed, so a line printed and not
# recorded fails the row. ---
fence_ephemeral_shared() {
  local entry verb who what="patch or update of pods/ephemeralcontainers in dispatch, each on a ServiceAccount of its own"
  fence_world
  for entry in "$@"; do
    IFS='|' read -r verb who <<<"$entry"
    subject_grant operator-cluster "$(sa_subject legion "$who")" "$verb" "" pods ephemeralcontainers dispatch ""
  done
  fence_run
  want_status 1 "$what"
  want_recorded "$what"
  for entry in "$@"; do
    IFS='|' read -r verb who <<<"$entry"
    want_line "FINDING system:serviceaccount:legion:$who: $verb pods/ephemeralcontainers in dispatch" \
      "legion/$who with $verb pods/ephemeralcontainers in dispatch"
    ! has_prefix "CREDIT system:serviceaccount:legion:$who: $verb pods/ephemeralcontainers" ||
      fail "legion/$who with $verb pods/ephemeralcontainers in dispatch: the grant is credited"
  done
  want_fence_credits "$what"
}
if row fh; then
  fence_ephemeral_shared "patch|worker" "update|dispatch-measure"
  passed "(fh) patch or update of pods/ephemeralcontainers in dispatch stays a finding beside the fence's credits"
fi
