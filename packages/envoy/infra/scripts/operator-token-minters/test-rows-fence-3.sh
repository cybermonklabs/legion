# shellcheck shell=bash
# shellcheck disable=SC2016,SC2034,SC2154 # the world, its names, its helpers and the output and status a run sets are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-fence-3.sh: the rows of operator-token-minters.test.sh on the
# broker's pod fence (broker-fence.sh), the admission refusal the check credits: each binding spec
# its contract holds. Sourced by the test after its world and helpers; its header says what the
# fakes model. The row's world (fence_world) puts the broker in dispatch, guarded as
# --broker-workload dispatch/envoy-broker with --db-secret dispatch/envoy-broker-db, with the fence
# of fence_objects (the reviewed rules broker-fence.json holds, and example workload policies), and
# gives the three grants the fence answers: comment-delivery's exec (create and get) and
# dispatch-measure's exec in dispatch, and legion/worker's port forward there, live, and
# comment-delivery's exec declared by the render. The row runs copies of fence_check, the check
# whose contract holds the example workload policies' digests, each with one binding of the
# contract widened (fence_variant). The rows:
#
#   (fs) each of the five binding specs broker-fence.json holds decides: widened in the contract and
#        on the cluster alike, the fence is credited; in the contract alone, it is not.
#
# Its control is its own other half, the contract's widened binding without the cluster's; the check
# as at b14c08df3df is its other control, which fails.

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
