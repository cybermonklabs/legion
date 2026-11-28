# shellcheck shell=bash
# shellcheck disable=SC2016,SC2034,SC2154 # the world, its names, its helpers and the output and status a run sets are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-fence-1.sh: the rows of operator-token-minters.test.sh on the
# broker's pod fence (broker-fence.sh), the admission refusal the check credits: the fence as
# reviewed, who can write it, who can reach its residual, and a change of a pod in dispatch.
# Sourced by the test after its world and helpers; its header says what the fakes model. Each row's
# world (fence_world) puts the broker in dispatch, guarded as --broker-workload
# dispatch/envoy-broker with --db-secret dispatch/envoy-broker-db, with the fence of fence_objects
# (the reviewed rules broker-fence.json holds, and example workload policies), and gives the three
# grants the fence answers: comment-delivery's exec (create and get) and dispatch-measure's exec in
# dispatch, and legion/worker's port forward there, live, and comment-delivery's exec declared by
# the render. The rows run fence_check, the check whose contract holds the example workload
# policies' digests. The rows:
#
#   (fa) the fence the renders declare and the cluster holds as reviewed, its live objects carrying
#        what GKE and the API server fill in, credits each of those grants, live and declared: exit
#        0, a CREDIT line each naming the use it refuses, an ALLOWANCE line each naming the residual
#        of broker-fence.json, the code step's PASS, the PASS of comment-delivery's minters' step,
#        and --report's `credited` (the policy and the declared and live versions) and `residuals`
#        (the residual and INFRA-4604); each spec broker-fence.json holds is in the fence's shape;
#   (fc) a principal that can write the fence stays a finding: patch of the binding, delete of a
#        policy, and delete of broker-pod-connect by name;
#   (fd) legion/worker, which can become legion/dispatch-measure, whose exec in dispatch is credited
#        and carries the residual, is an ALLOWANCE naming the chain and the residual; once
#        dispatch-measure can patch admission policies, the chain is a finding;
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
#        never one by the residual's.
#
# Each control is the row's own other half: (fa) against (fc), (fd), (fj), (fk), (fl) and (fm) the
# same world with one grant more, and (fd) a half without it. The check as at b14c08df3df is the
# other control of (fd), (fj) and (fk), and as at 9d08e087406 of (fl), (fm) and (fn), which each
# fails.

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
              and .resource == "cluster:operator-cluster" and (.granted_at | startswith("RBAC: allowed by ")))
      and any(.credited[]; .principal == $cd and .granted_at == "RoleBinding dispatch/comment-delivery")
      and ([.residuals[] | {principal, right, resource, granted_at}] == [.credited[] | {principal, right, resource, granted_at}])
      and all(.residuals[]; keys == ["granted_at", "principal", "residual", "resource", "right", "tracked_by"]
              and .residual == $residual and .tracked_by == "INFRA-4604")' "$report" >/dev/null ||
    fail "the fence as reviewed: --report's credited and residuals are not the six credits and their residuals: $(jq -c '{credited, residuals}' "$report")"
  passed "(fa) the fence as reviewed, declared and live, credits comment-delivery's exec, dispatch-measure's exec and legion/worker's port forward on the broker's pods, allows their use on dispatch's other pods as INFRA-4604's residual, and reports each with its evidence"
fi

# --- (fc) Whoever can write the fence stays a finding while it is credited. The three writes share
# one world and one run, each on a ServiceAccount of its own: patch-binding on legion/worker and
# delete-policy on legion/dispatch-measure, each the holder of a grant the fence credits, and
# delete-named on legion/delete-named, which holds nothing else. The check prints each principal's
# finding on a line of its own, naming it, so each write is still judged by its own line, exact,
# beside the fence's credits (want_fence_credits); want_recorded holds the run's last line to the
# FINDING and UNKNOWN lines it printed, so a line printed and not recorded fails the row. ---
fence_writer_shared() {
  local item want entry wants=() what="the writers of the fence ($*), each on a ServiceAccount of its own"
  fence_world
  for item in "$@"; do
    case "$item" in
      patch-binding)
        subject_grant operator-cluster "$(sa_subject legion worker)" patch admissionregistration.k8s.io validatingadmissionpolicybindings "" "" ""
        want="FINDING system:serviceaccount:legion:worker: patch validatingadmissionpolicybindings.admissionregistration.k8s.io cluster-wide"
        ;;
      delete-policy)
        subject_grant operator-cluster "$(sa_subject legion dispatch-measure)" delete admissionregistration.k8s.io validatingadmissionpolicies "" "" ""
        want="FINDING system:serviceaccount:legion:dispatch-measure: delete validatingadmissionpolicies.admissionregistration.k8s.io cluster-wide, which removes the broker's pod fence"
        ;;
      delete-named)
        add_sa operator-cluster legion delete-named
        subject_grant operator-cluster "$(sa_subject legion delete-named)" delete admissionregistration.k8s.io validatingadmissionpolicies "" "" broker-pod-connect
        want="FINDING system:serviceaccount:legion:delete-named: delete validatingadmissionpolicies.admissionregistration.k8s.io broker-pod-connect cluster-wide, which removes the broker's pod fence"
        ;;
    esac
    wants+=("$item"$'\t'"$want")
  done
  fence_run
  want_status 1 "$what"
  want_recorded "$what"
  for entry in "${wants[@]}"; do
    want_line "${entry#*$'\t'}" "a writer of the fence (${entry%%$'\t'*})"
  done
  want_fence_credits "$what"
}
if row fc; then
  fence_writer_shared patch-binding delete-policy delete-named
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

# --- (fj) Who can become a credited grant's holder reaches the residual too, and is named. The
# three share one world and one run, each chain from a principal of its own: token's
# agent-sessions/runner-x, which can mint dispatch-measure's token; minter's Google service account
# minter@; and member's Workload Identity member agent-sessions/runner-w, which
# agent-sessions/runner-v can become by minting its token. No plant gives one a way to become
# another, and every item expects exit 0, so the run still prints no FINDING at all. Each is still
# judged by its own line: token's and runner-v's exact, the member's by its own text, and minter's
# by one line that both starts with minter@'s text and ends with comment-delivery's exec and the
# residual (want_line_with), since the member's ALLOWANCE ends the same way. The lines are judged
# before the exit, so a right read as a finding fails naming its item; want_recorded holds the
# run's last line to the FINDING and UNKNOWN lines it printed, so a line printed and not recorded
# fails the row. ---
# The comment-delivery account's resource, whose minters the check reads once its credited exec
# carries the residual.
readonly comment_delivery_res="//iam.googleapis.com/projects/$project/serviceAccounts/$comment_delivery"
residual_chain_shared() {
  local item what="the residual reached through each of $*, each from a principal of its own"
  fence_world
  edit "$world/clusters/operator-cluster/serviceaccounts.json" '. + [{namespace: "agent-sessions", name: "runner-x"}, {namespace: "agent-sessions", name: "runner-w"},
    {namespace: "agent-sessions", name: "runner-v"}]'
  for item in "$@"; do
    case "$item" in
      token)
        # An agent's ServiceAccount that can mint dispatch-measure's token.
        subject_grant operator-cluster "$(sa_subject agent-sessions runner-x)" create "" serviceaccounts token legion dispatch-measure
        ;;
      minter)
        # A Google service account that can mint comment-delivery's token.
        iam "$comment_delivery_res" roles/iam.serviceAccountTokenCreator "serviceAccount:minter@$project.iam.gserviceaccount.com"
        ;;
      member)
        # A Workload Identity member that acts as comment-delivery, and an agent that can become it.
        iam "$comment_delivery_res" roles/iam.workloadIdentityUser "serviceAccount:$project.svc.id.goog[agent-sessions/runner-w]"
        subject_grant operator-cluster "$(sa_subject agent-sessions runner-v)" create "" serviceaccounts token agent-sessions runner-w
        ;;
    esac
  done
  fence_run
  for item in "$@"; do
    case "$item" in
      token)
        want_line "$(allowance_line system:serviceaccount:agent-sessions:runner-x "create serviceaccounts/token dispatch-measure in legion, so it can become system:serviceaccount:legion:dispatch-measure, which can create pods/exec in dispatch")" \
          "the residual reached through $item"
        ;;
      minter)
        want_line_with "ALLOWANCE serviceAccount:minter@$project.iam.gserviceaccount.com: iam.serviceAccounts.getAccessToken" \
          ", so it can act as serviceAccount:$comment_delivery, which can create pods/exec in dispatch, $fence_residual" "the residual reached through $item"
        ;;
      member)
        want_text "ALLOWANCE serviceAccount:$project.svc.id.goog[agent-sessions/runner-w]: " "the residual reached through $item"
        want_line "$(allowance_line system:serviceaccount:agent-sessions:runner-v "create serviceaccounts/token runner-w in agent-sessions, so it can become system:serviceaccount:agent-sessions:runner-w, which can act as $comment_delivery (which can create pods/exec in dispatch) through Workload Identity")" \
          "the residual reached through $item"
        ;;
    esac
  done
  [ "$(count_prefix "FINDING ")" -eq 0 ] || fail "$what: a FINDING line"
  want_status 0 "$what"
  want_recorded "$what"
  want_fence_credits "$what"
}
if row fj; then
  residual_chain_shared token minter member
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
# pod's metadata, and a pod given the broker's label app=envoy-broker takes its Services' traffic.
# The two verbs share one world and one run, each on a ServiceAccount of its own that holds a grant
# the fence credits: patch on legion/dispatch-measure and update on legion/worker. The check prints
# each principal's finding and each credit on a line of its own, naming it, so each verb is still
# judged by its own line, exact, and by no CREDIT of its own principal for its grant, beside the
# fence's credits (want_fence_credits); want_recorded holds the run's last line to the FINDING and
# UNKNOWN lines it printed, so a line printed and not recorded fails the row. ---
pod_change_shared() {
  local entry verb who what="patch or update of pods in dispatch, each on a ServiceAccount of its own"
  fence_world
  for entry in "$@"; do
    IFS='|' read -r verb who <<<"$entry"
    subject_grant operator-cluster "$(sa_subject legion "$who")" "$verb" "" pods "" dispatch ""
  done
  fence_run
  want_status 1 "$what"
  want_recorded "$what"
  for entry in "$@"; do
    IFS='|' read -r verb who <<<"$entry"
    want_line "FINDING system:serviceaccount:legion:$who: $verb pods in dispatch" "legion/$who with $verb pods in dispatch"
    ! has_prefix "CREDIT system:serviceaccount:legion:$who: $verb pods in dispatch" ||
      fail "legion/$who with $verb pods in dispatch: the grant is credited"
  done
  want_fence_credits "$what"
}
if row fk; then
  pod_change_shared "patch|dispatch-measure" "update|worker"
  passed "(fk) patch or update of pods in dispatch, by which a label-only patch puts another pod behind the broker's Services, stays a finding beside the fence's credits"
fi
