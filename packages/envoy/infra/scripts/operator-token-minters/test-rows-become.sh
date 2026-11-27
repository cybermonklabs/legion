# shellcheck shell=bash
# shellcheck disable=SC2016,SC2034,SC2154 # the world, its names, its helpers and the output and status a run sets are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-become.sh: the rows of operator-token-minters.test.sh on who can
# become a principal with a finding through a right a live role limits by name. Sourced by the test
# after its world and helpers; its header says what the fakes model. The rows:
#
#   (ck) a system namespace's ServiceAccount allowed a guarded right by name alone is followed, never
#        reported itself: kube-system/x that may patch the Deployment op in envoy-operator (rv18),
#        gke-foo/agent that may update the webhook configuration existing-webhook, gke-foo/agent
#        that may patch the Deployment secrets-broker in secrets-broker, and the same account bound
#        as the User system:serviceaccount:gke-foo:agent allowed that patch (p2u, N-C), each beside
#        legion/worker, which may create pods in that namespace: each exits 1 naming legion/worker's
#        chain;
#   (cl) a copy of the script whose by-name read of who can become a found ServiceAccount fails
#        reads UNKNOWN naming the failure, not clean;
#   (cn) with gke-foo/agent seeded (allowed the operator's token), legion/worker allowed patch, or
#        update, of the custom resource sandboxes.agents.x-k8s.io in gke-foo becomes it, and exits 1
#        naming the chain (N-D);
#   (ct) a chain finding's text, printed and in --report, is the same whichever order the API server
#        answers the reviews in.

# --- (ck) A system namespace's ServiceAccount allowed a guarded right by name: who can become it holds
# that right, so it seeds the chain as one allowed the right outright does. ---
named_seed_row() {
  local ns sa verb group resource name rns want subject
  case "$1" in
    rv18) ns=kube-system sa=x verb=patch group=apps resource=deployments name=op rns=envoy-operator
      want="patch deployments.apps op in envoy-operator" ;;
    webhook) ns=gke-foo sa=agent verb=update group=admissionregistration.k8s.io resource=mutatingwebhookconfigurations name=existing-webhook rns=""
      want="update mutatingwebhookconfigurations.admissionregistration.k8s.io existing-webhook cluster-wide" ;;
    deployment | p2u) ns=gke-foo sa=agent verb=patch group=apps resource=deployments name=secrets-broker rns=secrets-broker
      want="patch deployments.apps secrets-broker in secrets-broker" ;;
  esac
  base_world
  add_sa operator-cluster "$ns" "$sa"
  # p2u binds the same account as a User subject, the form RBAC also accepts.
  if [ "$1" = p2u ]; then subject="$(user_subject "system:serviceaccount:$ns:$sa")"; else subject="$(sa_subject "$ns" "$sa")"; fi
  subject_grant operator-cluster "$subject" "$verb" "$group" "$resource" "" "$rns" "$name"
  subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods "" "$ns" ""
  run
  want_status 1 "$ns/$sa allowed $want by name, and legion/worker may create pods in $ns"
  want_text "FINDING system:serviceaccount:legion:worker: create pods in $ns, so it can become system:serviceaccount:$ns:$sa, which can $want" \
    "$ns/$sa allowed $want by name, and legion/worker may create pods in $ns"
  [ "$(count_prefix "FINDING system:serviceaccount:$ns:")" -eq 0 ] || fail "$ns/$sa, a system namespace's ServiceAccount, is itself a finding"
}
if row ck; then
  each named_seed_row rv18 webhook deployment p2u
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
# makes, so each is asked there as create is. ---
custom_become_row() {
  base_world
  echo sandboxes.agents.x-k8s.io >"$world/clusters/operator-cluster/api-resources"
  add_sa operator-cluster gke-foo agent
  subject_grant operator-cluster "$(sa_subject gke-foo agent)" create "" serviceaccounts token envoy-operator envoy-operator
  subject_grant operator-cluster "$(sa_subject legion worker)" "$1" agents.x-k8s.io sandboxes "" gke-foo ""
  run
  want_status 1 "legion/worker with $1 on sandboxes in gke-foo, whose agent may mint the operator's token"
  want_line "FINDING system:serviceaccount:legion:worker: $1 sandboxes.agents.x-k8s.io in gke-foo, so it can become system:serviceaccount:gke-foo:agent, which can create serviceaccounts/token envoy-operator in envoy-operator" \
    "legion/worker with $1 on sandboxes in gke-foo, whose agent may mint the operator's token"
}
if row cn; then
  each custom_become_row patch update create
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
