# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-vm-hub.sh
#
# The --listeners scenarios for whether the deployment declares the VM hub and whether its pin
# can be read. daemon-release-gate.test.sh sources this file through its group runner after
# the harness. Do not run this file on its own.
#
# Envoy's listenerImage is read only by the VM hub's listener, gcp:NatsHub, and the hub's removal
# deletes that key with it, so an absent key cannot tell a gone hub from a stack file that lost its
# pin. The leg decides the hub from the deployment's program, positively: it fetches the deployment
# repository at its default-branch commit as one archive and reads every Go file in it that is not a
# test. A declared hub whose pin is gone is UNKNOWN, and only a program that declares none, every
# file of it read, drops the VM pin.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the test that sources this file
# shellcheck disable=SC2034 # gate_under_test is the harness's, which its runners read

echo "case: --listeners, envoy/ declares the VM hub and its pin is read"
new_case listeners-hub-declared
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$identical_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- --listeners "$commit"
check "exits 0" "$(is "$status" 0)"
check "names the use that declares the hub, at the commit it read" \
  "$(has_line "VM HUB: envoy/main.go:3 at ${deployment_head} uses NewNatsHub")"
check "reads the VM pin and compares it" "$(has_line "PASS: envoy runs ${ahead_rev} (ahead of ${commit})")"
check "reads the head of the deployment's default branch" \
  "$(grep -qxF "gh api repos/${deployment}/commits/HEAD --jq .sha" "$calls" && echo true || echo false)"
check "fetches the deployment repository once, at that commit" \
  "$(is "$(grep -c "tarball/${deployment_head}" "$calls")" 1)"
check "prints no NO VM HUB line" "$(lacks "NO VM HUB" "$out")"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --listeners, the VM hub declared and its listenerImage absent"
new_case listeners-hub-no-pin
listener_stacks "" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the key the declared hub needs" \
  "$(has_line "UNKNOWN: envoy: envoy/Pulumi.prod.yaml sets no ${envoy_project}:listenerImage")"
check "prints no NO VM HUB line" "$(lacks "NO VM HUB" "$out")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

# Control: a gate whose VM leg reads an absent listenerImage as no VM hub, the reading this leg
# exists to refuse, opens on the fixture above; so that fixture fails such a gate, by name.
mutant vm_hub hub-from-pin <<'EOF'
vm_hub() {
  hub_head=$(gh api "repos/${deployment}/commits/HEAD" --jq .sha)
  project_of envoy || return 1
  stack_file envoy/Pulumi.prod.yaml || return 1
  if config_value "$work/python.out" "${project}:listenerImage" && [ -n "$(<"$work/python.out")" ]; then
    hub=yes
    hub_use=envoy/Pulumi.prod.yaml:1
  else
    hub=no
  fi
}
EOF
gate_under_test="$mutant_gate"
run_gate -- --listeners "$commit"
gate_under_test="$script"
check "control: a VM leg that reads the absent pin as no hub opens on the declared-and-absent fixture" \
  "$([ "$(last_line_is "GATE OPEN")" = true ] && grep -q '^NO VM HUB: ' "$out" && echo true || echo false)"

echo "case: --listeners, no Go file of the deployment declares the VM hub"
new_case listeners-no-hub
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
envoy_program none
index "$envoy_pin" "$legion" "$behind_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$identical_rev"
compare "$behind_rev" behind 4
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- --listeners "$commit"
check "exits 0" "$(is "$status" 0)"
check "says the program declares no VM hub, at the commit it read" \
  "$(has_line "NO VM HUB: envoy/ at ${deployment_head} declares no gcp:NatsHub")"
check "a func's own name, a method's, a comment, a string, a _test.go, testdata/ and another program declare nothing" \
  "$(grep -q '^VM HUB:' "$out" && echo false || echo true)"
check "reads no VM pin: not envoy's stack files, not its image" \
  "$([ "$(lacks "contents/envoy/" "$calls")" = true ] && lacks "$envoy_pin" "$calls" || echo false)"
check "opens on the other two pins" \
  "$(grep -qxF "PASS: envoy-cluster runs ${ahead_rev} (ahead of ${commit})" "$out" && grep -qxF "PASS: dispatch runs ${identical_rev} (identical to ${commit})" "$out" && echo true || echo false)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --listeners, the head of the deployment repository cannot be read"
new_case listeners-hub-no-head
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
refuse "repos/${deployment}/commits/HEAD" "gh: Bad credentials (HTTP 401)"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming GitHub's answer" \
  "$(has_line "UNKNOWN: envoy: cannot read the head of ${deployment}: gh: Bad credentials (HTTP 401)")"
check "fetches no archive" "$(lacks "/tarball/" "$calls")"
check "neither reads the VM pin nor says the hub is gone" \
  "$([ "$(lacks "$envoy_pin" "$calls")" = true ] && lacks "NO VM HUB" "$out" || echo false)"
check "still reads the other two" "$(has_line "PASS: envoy-cluster runs ${ahead_rev} (ahead of ${commit})")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --listeners, a head of the deployment repository that is no commit id"
new_case listeners-hub-head-not-commit
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate FAKE_HEAD=main -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the answer" \
  "$(has_line "UNKNOWN: envoy: GitHub answered main for the head of ${deployment}, which is no commit id")"
check "fetches no archive" "$(lacks "/tarball/" "$calls")"
check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"

echo "case: --listeners, the deployment repository cannot be fetched"
new_case listeners-hub-unfetched
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
refuse "repos/${deployment}/tarball/${deployment_head}" "gh: Resource not accessible by integration (HTTP 403)"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming GitHub's answer" \
  "$(has_line "UNKNOWN: envoy: cannot fetch ${deployment} at ${deployment_head}: gh: Resource not accessible by integration (HTTP 403)")"
check "neither reads the VM pin nor says the hub is gone" \
  "$([ "$(lacks "$envoy_pin" "$calls")" = true ] && lacks "NO VM HUB" "$out" || echo false)"
check "still reads the other two" "$(has_line "PASS: envoy-cluster runs ${ahead_rev} (ahead of ${commit})")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --listeners, an archive of the repository that cannot be read"
new_case listeners-hub-corrupt
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
envoy_program none
: >"${case_root}/corrupt-archive"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the archive" \
  "$(grep -qF "UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: the archive cannot be read: " "$out" && echo true || echo false)"
check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"

echo "case: --listeners, envoy/ holds no Go file"
new_case listeners-hub-no-go
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
find "${case_root}/files/envoy" -name '*.go' -delete
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, having nothing to decide by" \
  "$(has_line "UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: envoy/ holds no Go file to read the VM hub from")"
check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"

# Every form of a use declares the hub, wherever the repository's code holds it: a space or a
# comment between the name and its call, a reference called later, a helper package of envoy/, the
# module envoy/go.mod replaces with a local directory, a package under testdata/ or an _ directory,
# which go build compiles once something imports it, and another program of the repository, which
# can only lean the gate toward reading the VM pin. Each fixture's VM pin is behind, so a gate that
# missed the use would print NO VM HUB and open.
for shape in space comment reference helper replaced testdata underscore other-program; do
  echo "case: --listeners, the VM hub declared by a use of shape ${shape}"
  new_case "listeners-use-${shape}"
  listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
  envoy_program none
  use='package hub\n\nfunc Run() { envoygcp.NewNatsHub(ctx, "hub") }\n'
  case "$shape" in
    space) printf 'package main\n\n\thub, err := envoygcp.NewNatsHub (ctx, "hub")\n' >"${case_root}/files/envoy/main.go" ;;
    comment) printf 'package main\n\n\thub, err := envoygcp.NewNatsHub /* the VM hub */ (ctx, "hub")\n' >"${case_root}/files/envoy/main.go" ;;
    reference) printf 'package main\n\nvar newHub = envoygcp.NewNatsHub\n' >"${case_root}/files/envoy/main.go" ;;
    helper) mkdir -p "${case_root}/files/envoy/infra" && printf '%b' "$use" >"${case_root}/files/envoy/infra/hub.go" ;;
    replaced) printf '%b' "$use" >"${case_root}/files/shared/hub.go" ;;
    testdata) mkdir -p "${case_root}/files/envoy/testdata/hub" && printf '%b' "$use" >"${case_root}/files/envoy/testdata/hub/hub.go" ;;
    underscore) mkdir -p "${case_root}/files/envoy/_infra" && printf '%b' "$use" >"${case_root}/files/envoy/_infra/hub.go" ;;
    other-program) printf '%b' "$use" >"${case_root}/files/envoy-cluster/hub.go" ;;
  esac
  index "$envoy_pin" "$legion" "$behind_rev"
  index "$cluster_pin" "$legion" "$ahead_rev"
  index "$dispatch_listener_pin" "$legion" "$ahead_rev"
  compare "$behind_rev" behind 4
  compare "$ahead_rev" ahead 0
  run_gate -- --listeners "$commit"
  check "exits 1" "$(is "$status" 1)"
  check "names a use" "$(grep -q "^VM HUB: .* at ${deployment_head} uses NewNatsHub$" "$out" && echo true || echo false)"
  check "reads the VM pin and shuts on it" "$(has_line "GATE SHUT: envoy runs ${behind_rev} (behind ${commit} by 4)")"
  check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"
done
