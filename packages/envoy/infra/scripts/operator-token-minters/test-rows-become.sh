# shellcheck shell=bash
# shellcheck disable=SC2016,SC2034,SC2154 # the world, its names, its helpers and the output and status a run sets are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-become.sh: the rows of operator-token-minters.test.sh on who can
# become a principal with a finding through a right a live role limits by name. Sourced by the test
# after its world and helpers; its header says what the fakes model. The rows:
#
#   (ck) a system namespace's ServiceAccount allowed a guarded right by name alone is followed, never
#        reported itself: kube-system/x that may patch the Deployment op in envoy-operator (rv18),
#        gke-foo/agent that may update the webhook configuration existing-webhook, gke-bar/agent
#        that may patch the Deployment secrets-broker in secrets-broker, and gke-baz/agent bound as
#        the User system:serviceaccount:gke-baz:agent allowed that patch (p2u, N-C), each beside a
#        becomer of its own, legion/<item>, which may create pods in that namespace: the run exits 1
#        naming each becomer's chain;
#   (cl) a copy of the script whose by-name read of who can become a found ServiceAccount fails
#        reads UNKNOWN naming the failure, not clean;
#   (cn) with gke-foo/agent seeded (allowed the operator's token), legion/patch, legion/update and
#        legion/create, each allowed its verb on the custom resource sandboxes.agents.x-k8s.io in
#        gke-foo, become it, and the run exits 1 naming each chain (N-D);
#   (ct) a chain finding's text, printed and in --report, is the same whichever order the API server
#        answers the reviews in.

# --- (ck) A system namespace's ServiceAccount allowed a guarded right by name: who can become it holds
# that right, so it seeds the chain as one allowed the right outright does. The four share one world
# and one run, each seed in a system namespace of its own (kube-system/x, gke-foo/agent,
# gke-bar/agent, and gke-baz/agent bound as the User system:serviceaccount:gke-baz:agent), so no
# seed's right hides another's, and each becomer a ServiceAccount of its own, legion/<item>, which
# may create pods in its seed's namespace alone: legion is a namespace no plant gives a right in, so
# no becomer becomes another. live_named reads each seed from its own rule's binding, and the fake
# answers each review by its subject, so each item is still judged by its own chain, naming its own
# becomer and seed, and by no FINDING of its own seed's namespace; want_recorded holds the run's last
# line to the FINDING and UNKNOWN lines it printed, so a line printed and not recorded fails the
# row. ---
named_seed_shared() {
  local item ns sa verb group resource name rns want subject entry wants=()
  base_world
  for item in "$@"; do
    case "$item" in
      rv18) ns=kube-system sa=x verb=patch group=apps resource=deployments name=op rns=envoy-operator
        want="patch deployments.apps op in envoy-operator" ;;
      webhook) ns=gke-foo sa=agent verb=update group=admissionregistration.k8s.io resource=mutatingwebhookconfigurations name=existing-webhook rns=""
        want="update mutatingwebhookconfigurations.admissionregistration.k8s.io existing-webhook cluster-wide" ;;
      deployment) ns=gke-bar sa=agent verb=patch group=apps resource=deployments name=secrets-broker rns=secrets-broker
        want="patch deployments.apps secrets-broker in secrets-broker" ;;
      p2u) ns=gke-baz sa=agent verb=patch group=apps resource=deployments name=secrets-broker rns=secrets-broker
        want="patch deployments.apps secrets-broker in secrets-broker" ;;
    esac
    add_sa operator-cluster "$ns" "$sa"
    # p2u binds its account as a User subject, the form RBAC also accepts.
    if [ "$item" = p2u ]; then subject="$(user_subject "system:serviceaccount:$ns:$sa")"; else subject="$(sa_subject "$ns" "$sa")"; fi
    subject_grant operator-cluster "$subject" "$verb" "$group" "$resource" "" "$rns" "$name"
    add_sa operator-cluster legion "$item"
    subject_grant operator-cluster "$(sa_subject legion "$item")" create "" pods "" "$ns" ""
    wants+=("$item|$ns|$sa|$want")
  done
  run
  want_status 1 "the seeds ($*), each allowed its right by name in a system namespace of its own"
  want_recorded "the seeds ($*), each allowed its right by name in a system namespace of its own"
  for entry in "${wants[@]}"; do
    IFS='|' read -r item ns sa want <<<"$entry"
    want_text "FINDING system:serviceaccount:legion:$item: create pods in $ns, so it can become system:serviceaccount:$ns:$sa, which can $want" \
      "$ns/$sa allowed $want by name, and legion/$item may create pods in $ns"
    [ "$(count_prefix "FINDING system:serviceaccount:$ns:")" -eq 0 ] || fail "$ns/$sa, a system namespace's ServiceAccount, is itself a finding"
  done
}
if row ck; then
  named_seed_shared rv18 webhook deployment p2u
  passed "(ck) a system namespace's ServiceAccount allowed a guarded right by name alone, as a ServiceAccount or a User subject (p2u), seeds the chain, unreported itself, so who can become it exits 1"
fi

# --- (cl) The by-name read of who can become a found ServiceAccount fails: a copy whose code-rbac.jq
# breaks on a become right must read UNKNOWN, never clean. ---
if row cl; then
  base_world
  subject_grant operator-cluster "$(sa_subject kube-system generic-garbage-collector)" create "" serviceaccounts token envoy-operator envoy-operator
  script="$(script_copy broken-become)"
  # A become right carries `targets`, which no other right the jq program is given does.
  plant "$(dirname "$script")/operator-token-minters/code-rbac.jq" '        ($rights | map(.key)) as $plain' \
    '        if any($named_rights[]; has("targets")) then error("a read broken by the test") else . end
        | ($rights | map(.key)) as $plain'
  run_script="$script" run
  want_status 2 "a by-name read of who can become a found ServiceAccount that fails"
  want_text "UNKNOWN live: RBAC on operator-cluster: the live roles could not be read for who can become a found ServiceAccount by a right limited by name: " \
    "a by-name read of who can become a found ServiceAccount that fails"
  passed "(cl) a by-name read of who can become a found ServiceAccount that fails reads UNKNOWN"
fi

# --- (cn) N-D: who can become a found ServiceAccount by a custom resource in its namespace. A patch
# that finds no object creates it (server-side apply), and an update changes what its controller
# makes, so each is asked there as create is. The three verbs share one world and one run: the seed
# gke-foo/agent, allowed the operator's token, once, and each verb on sandboxes in gke-foo granted
# to a becomer of its own, legion/<verb>: legion is a namespace no plant gives a right in, so no
# becomer becomes another. The fake answers each review by its subject, so each verb is still judged
# by its own line, exact, naming its own becomer; want_recorded holds the run's last line to the
# FINDING and UNKNOWN lines it printed, so a line printed and not recorded fails the row. ---
custom_become_shared() {
  local verb
  base_world
  echo sandboxes.agents.x-k8s.io >"$world/clusters/operator-cluster/api-resources"
  add_sa operator-cluster gke-foo agent
  subject_grant operator-cluster "$(sa_subject gke-foo agent)" create "" serviceaccounts token envoy-operator envoy-operator
  for verb in "$@"; do
    add_sa operator-cluster legion "$verb"
    subject_grant operator-cluster "$(sa_subject legion "$verb")" "$verb" agents.x-k8s.io sandboxes "" gke-foo ""
  done
  run
  want_status 1 "legion/<verb> for each of $*, with its verb on sandboxes in gke-foo, whose agent may mint the operator's token"
  want_recorded "legion/<verb> for each of $*, with its verb on sandboxes in gke-foo, whose agent may mint the operator's token"
  for verb in "$@"; do
    want_line "FINDING system:serviceaccount:legion:$verb: $verb sandboxes.agents.x-k8s.io in gke-foo, so it can become system:serviceaccount:gke-foo:agent, which can create serviceaccounts/token envoy-operator in envoy-operator" \
      "legion/$verb with $verb on sandboxes in gke-foo, whose agent may mint the operator's token"
  done
}
if row cn; then
  custom_become_shared patch update create
  passed "(cn) with gke-foo/agent seeded, legion/worker allowed patch, update or create of a custom resource in gke-foo becomes it and exits 1 (N-D)"
fi

# --- (ct) The text of a chain finding does not hang on the order the reviews are answered in.
# tenant/x may create pods and create jobs in envoy-operator, two findings, and legion/worker may
# create pods in tenant, so it can become tenant/x. The same world is run twice, its API server
# answering the reviews in the order asked and then reversed (review-answers-reversed), and the two
# runs print the same findings and write the same findings to --report. Run by hand, the control is
# reviews.sh as it was before this row, which named a found ServiceAccount by the first of its
# findings it read: the chain names create pods in one run and create jobs in the other, and the
# row fails naming both. ---
order_run() {
  local report="$temporary_dir/ct-$1.json"
  run --report "$report"
  want_status 1 "tenant/x's two findings and legion/worker's way to become it, answered $1"
  want_text "FINDING system:serviceaccount:legion:worker: create pods in tenant, so it can become system:serviceaccount:tenant:x, which can " \
    "legion/worker becoming tenant/x, answered $1"
  grep '^FINDING ' <<<"$output" | LC_ALL=C sort >"$temporary_dir/ct-$1.findings"
  jq -S '.findings | sort_by(.principal, .right)' "$report" >"$temporary_dir/ct-$1.report"
}
if row ct; then
  base_world
  add_sa operator-cluster tenant x
  subject_grant operator-cluster "$(sa_subject tenant x)" create "" pods "" envoy-operator ""
  subject_grant operator-cluster "$(sa_subject tenant x)" create batch jobs "" envoy-operator ""
  subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods "" tenant ""
  order_run asked
  : >"$world/clusters/operator-cluster/review-answers-reversed"
  order_run reversed
  cmp -s "$temporary_dir/ct-asked.findings" "$temporary_dir/ct-reversed.findings" ||
    fail "the findings differ with the reviews answered in another order: $(diff "$temporary_dir/ct-asked.findings" "$temporary_dir/ct-reversed.findings" | grep '^[<>]' | tr '\n' ' ')"
  cmp -s "$temporary_dir/ct-asked.report" "$temporary_dir/ct-reversed.report" ||
    fail "the report's findings differ with the reviews answered in another order"
  passed "(ct) a chain finding's text, printed and in --report, is the same whichever order the API server answers the reviews in"
fi
