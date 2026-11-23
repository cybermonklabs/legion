# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-listeners.sh
#
# The scenarios of the gate's --listeners leg: the listener pins of the envoy, envoy-cluster and
# dispatch stacks, and the verdict each read gives. The VM hub's own scenarios are in
# lib/daemon-release-gate-cases-vm-hub.sh. daemon-release-gate.test.sh runs this file in a process
# of its own (lib/daemon-release-gate-group.sh, which sources the harness and the client fixtures
# first); it is not run on its own.
#
# Negative control, run when this was written: make the gate read an index with
# `{{json .Image.Config.Labels}}`, as a single image is read; every row below that expects a
# comparison prints `UNKNOWN` instead, which is the blindness an index-shaped pin causes.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the group runner

echo "case: --listeners, every listener index at or after the commit"
new_case listeners-open
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$identical_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- --listeners "$commit"
check "exits 0" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"
check "envoy's two platforms agree and are compared" "$(has_line "PASS: envoy runs ${ahead_rev} (ahead of ${commit})")"
check "envoy-cluster's are" "$(has_line "PASS: envoy-cluster runs ${identical_rev} (identical to ${commit})")"
check "dispatch's listener is read from listenerImage" "$(has_line "PASS: dispatch runs ${ahead_rev} (ahead of ${commit})")"
check "dispatch's server is read from envoyImage, under its own key" "$(has_line "PASS: dispatch-server runs ${server_rev} (identical to ${commit})")"
check "each real platform is read, and no attestation entry" \
  "$(grep -qF '(index .Image "linux/arm64")' "$calls" && grep -qF '(index .Image "linux/amd64")' "$calls" && lacks '(index .Image "unknown/unknown")' "$calls")"

# The deployment requires dispatch's listenerImage (its dispatch/main.go loadSettings; its
# pinListenerImage reads that key and no other), so a stack that sets none runs no listener this
# gate can read. Reading the server's envoyImage in its place would open the gate on an image the
# listener does not run, and for a configuration the deployment itself refuses; the server's image
# is judged under dispatch-server alone.
echo "case: --listeners, a dispatch stack with no listenerImage is UNKNOWN, never the server's image"
new_case listeners-no-listener-pin
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$server_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the key it needs" \
  "$(has_line "UNKNOWN: dispatch: dispatch/Pulumi.prod.yaml sets no ${dispatch_project}:listenerImage")"
check "judges the server's image under dispatch-server" "$(has_line "PASS: dispatch-server runs ${ahead_rev} (ahead of ${commit})")"
check "never passes dispatch's listener on the server's image" "$(lacks "PASS: dispatch runs" "$out")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --listeners, a stack whose Pulumi.yaml declares no name, or is missing, is UNKNOWN by name"
new_case listeners-no-project
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
printf 'runtime: go\n' >"${case_root}/files/envoy/Pulumi.yaml"
rm "${case_root}/files/dispatch/Pulumi.yaml"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses envoy, naming its file" \
  "$(has_line "UNKNOWN: envoy: ${deployment} envoy/Pulumi.yaml declares no project name, so no key of envoy/Pulumi.prod.yaml is known to be its own")"
check "refuses dispatch, naming its file" \
  "$(has_line "UNKNOWN: dispatch: cannot read ${deployment} dispatch/Pulumi.yaml: gh: Not Found (HTTP 404)")"
check "never reads envoy's pin" "$(lacks "$envoy_pin" "$calls")"
check "never reads dispatch's pin" "$(lacks "$dispatch_listener_pin" "$calls")"
check "prints one line for each refused pin" "$(is "$(grep -cE '^UNKNOWN: (envoy|dispatch): ' "$out")" 2)"
check "still reads and compares envoy-cluster" "$(has_line "PASS: envoy-cluster runs ${ahead_rev} (ahead of ${commit})")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --listeners, an index whose platforms name different revisions"
new_case listeners-disagree
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$legion" "$ahead_rev" "$identical_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the disagreement" \
  "$(has_line "UNKNOWN: envoy platforms disagree: linux/amd64 names ${legion} at ${ahead_rev}, linux/arm64 names ${legion} at ${identical_rev}")"
check "the verdict is never GATE OPEN" "$(lacks "GATE OPEN" "$out")"

echo "case: --listeners, an index of attestation entries alone"
new_case listeners-attestations
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" attestations-only
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the pin" "$(has_line "UNKNOWN: envoy: ${envoy_pin} is an index with no platform but unknown/unknown")"

# sjawhar/legion is this repository under its name before the transfer, and the deployment records
# the two names as one repository in the repositoryRenames of its Legion projects, the value the
# daemon's ghrepo.Renames reads (packages/daemon/internal/ghrepo/renames.go). A build made under
# the old name is a build of this repository: it is compared like any other, and refused only for
# what the comparison says. A source no recorded pair makes one with this repository is still
# refused by name, and an unreadable record is UNKNOWN rather than a guess either way.
#
# Negative control, run when this was written: drop the alias check from judge; the rows below
# that record the pair print `GATE SHUT: envoy runs an image built from …` and make no compare.
readonly renamed_fleet="    - key: INFRA
      replicas: 0
      repositoryRenames:
        - from: sjawhar/legion
          to: cybermonklabs/legion"
readonly renamed_note="; built as ${old_name}, which the repositoryRenames of the deployment's Legion projects make one repository with ${legion}"

echo "case: --listeners, a pin built under the repository's old name, the rename recorded, ahead"
new_case listeners-renamed-ahead
legion_stack "$renamed_fleet"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 0" "$(is "$status" 0)"
check "compares it and names the recorded rename" "$(has_line "PASS: envoy runs ${other_rev} (ahead of ${commit})${renamed_note}")"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --listeners, the measured shape: envoy's pin built under the old name, behind"
new_case listeners-measured
legion_stack "$renamed_fleet"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" behind 400
run_gate -- --listeners "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts naming its revision and how far behind, not its source" \
  "$(has_line "GATE SHUT: envoy runs ${other_rev} (behind ${commit} by 400)${renamed_note}")"
check "compares its revision in cybermonklabs/legion" "$(grep -qF "compare/${commit}...${other_rev}" "$calls" && echo true || echo false)"
check "still reads and compares the other two" \
  "$(grep -qxF "PASS: envoy-cluster runs ${ahead_rev} (ahead of ${commit})" "$out" && grep -qxF "PASS: dispatch runs ${ahead_rev} (ahead of ${commit})" "$out" && echo true || echo false)"

echo "case: --listeners, a pin built under the old name with no rename recorded"
new_case listeners-no-rename
legion_stack "    - key: INFRA
      replicas: 0"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 1" "$(is "$status" 1)"
check "refuses naming the source" "$(has_line "GATE SHUT: envoy runs an image built from ${old_name}")"
check "makes no compare call for its revision" "$(lacks "...${other_rev}" "$calls")"

echo "case: --listeners, a pin built under the old name when the rename record cannot be read"
new_case listeners-renames-unreadable
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the record it could not read" \
  "$(has_line "UNKNOWN: envoy: built from ${old_name}, and the repository renames of ${deployment}'s Legion projects could not be read: cannot read ${deployment} legion/Pulumi.prod.yaml: gh: Not Found (HTTP 404)")"
check "makes no compare call for its revision" "$(lacks "...${other_rev}" "$calls")"

echo "case: --listeners, a pin built under the old name when legion/Pulumi.yaml is missing"
new_case listeners-renames-no-project
legion_stack "$renamed_fleet"
rm "${case_root}/files/legion/Pulumi.yaml"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file it could not read, never a guessed record" \
  "$(has_line "UNKNOWN: envoy: built from ${old_name}, and the repository renames of ${deployment}'s Legion projects could not be read: cannot read ${deployment} legion/Pulumi.yaml: gh: Not Found (HTTP 404)")"
check "makes no compare call for its revision" "$(lacks "...${other_rev}" "$calls")"
check "still reads and compares envoy-cluster" "$(has_line "PASS: envoy-cluster runs ${ahead_rev} (ahead of ${commit})")"

# daemon mode and --listeners read the same projects: where the deployment declares none, the rename
# record is no record, and a pin built under the old name is UNKNOWN, never shut as built elsewhere.
echo "case: --listeners, a pin built under the old name when the deployment declares no project"
new_case listeners-renames-no-projects
legion_stack "$renamed_fleet" example-other-legion
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN saying no project is declared, never GATE SHUT" \
  "$(has_line "UNKNOWN: envoy: built from ${old_name}, and the repository renames of ${deployment}'s Legion projects could not be read: ${deployment} declares no project: legion/projects/ does not exist, and legion/Pulumi.prod.yaml holds no ${legion_project}:projects list")"
check "makes no compare call for its revision" "$(lacks "...${other_rev}" "$calls")"

# An empty list is no declaration either: a deployment whose legion/Pulumi.prod.yaml lists no
# project declares none, so its rename record is no record, in both modes.
echo "case: an empty projects list declares no project"
new_case empty-list
legion_stack "    []"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the empty list" \
  "$(has_line "UNKNOWN: ${deployment} declares no project: legion/projects/ does not exist, and legion/Pulumi.prod.yaml's ${legion_project}:projects list is empty")"
check "reads no image" "$(lacks "docker" "$calls")"

echo "case: --listeners, a pin built under the old name when the projects list is empty"
new_case listeners-renames-empty-list
legion_stack "    []"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN saying the list is empty, never GATE SHUT" \
  "$(has_line "UNKNOWN: envoy: built from ${old_name}, and the repository renames of ${deployment}'s Legion projects could not be read: ${deployment} declares no project: legion/projects/ does not exist, and legion/Pulumi.prod.yaml's ${legion_project}:projects list is empty")"
check "never shuts the gate on it" "$(lacks "GATE SHUT: envoy" "$out")"
check "makes no compare call for its revision" "$(lacks "...${other_rev}" "$calls")"

echo "case: --listeners, a recorded rename holds whatever the case of the source's name"
new_case listeners-renamed-case
legion_stack "$renamed_fleet"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "https://github.com/SJawhar/Legion" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 0" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --listeners, a recorded rename makes no other repository one with this one"
new_case listeners-renamed-other-source
legion_stack "$renamed_fleet"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$other_source" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 1" "$(is "$status" 1)"
check "refuses naming the source" "$(has_line "GATE SHUT: envoy runs an image built from ${other_source}")"
check "makes no compare call for its revision" "$(lacks "...${other_rev}" "$calls")"

echo "case: a daemon built under the old name, the rename recorded on another project's entry"
new_case daemons-renamed
legion_stack "$renamed_fleet
    - key: LINUX
      replicas: 1
      daemonImage: ${worker_a}"
single "$worker_a" "$old_name" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- "$commit"
check "exits 0" "$(is "$status" 0)"
check "compares it and names the recorded rename" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})${renamed_note}")"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

# In production the pair sits in a project's own file of legion/projects/, not in a list in
# legion/Pulumi.prod.yaml: a rename record read from the list alone reads empty there, and shuts as
# "built from" a build of this very repository.
echo "case: a daemon built under the old name, the rename recorded in a project's file (production)"
new_case daemons-renamed-files
legion_settings
project_file 40-INFRA "key: INFRA
replicas: 0
image: ${infra_image}
repositoryRenames:
  - from: sjawhar/legion
    to: cybermonklabs/legion"
project_file 20-LINUX "key: LINUX
replicas: 1
daemonImage: ${worker_a}"
single "$worker_a" "$old_name" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- "$commit"
check "exits 0" "$(is "$status" 0)"
check "compares it and names the recorded rename" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})${renamed_note}")"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --listeners, a pin built under the old name, the rename recorded in a project's file (production)"
new_case listeners-renamed-files
legion_settings
project_file 40-INFRA "key: INFRA
replicas: 0
repositoryRenames:
  - from: sjawhar/legion
    to: cybermonklabs/legion"
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$old_name" "$other_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$other_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 0" "$(is "$status" 0)"
check "compares it, not shuts it, and names the recorded rename" "$(has_line "PASS: envoy runs ${other_rev} (ahead of ${commit})${renamed_note}")"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --listeners, a listener pin behind the commit"
new_case listeners-behind
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$behind_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$behind_rev" diverged 3
run_gate -- --listeners "$commit"
check "exits 1" "$(is "$status" 1)"
check "refuses naming the pin and its revision" "$(has_line "GATE SHUT: envoy-cluster runs ${behind_rev} (diverged from ${commit}, behind by 3)")"

echo "case: --listeners, a single-image listener pin"
new_case listeners-single
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
single "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 0" "$(is "$status" 0)"
check "reads the single image's own labels" "$(has_line "PASS: envoy runs ${ahead_rev} (ahead of ${commit})")"

echo "case: --listeners, revision labels that are no commit id"
new_case listeners-revision-not-commit
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$legion" "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
index "$cluster_pin" "$legion" "abcdef"
index "$dispatch_listener_pin" "$legion" "${ahead_rev}a"
index "$server_pin" "$legion" "main"
compare "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" ahead 0
compare "abcdef" ahead 0
compare "${ahead_rev}a" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses upper-case hex" \
  "$(has_line "UNKNOWN: envoy: ${envoy_pin} names revision AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA, which is no commit id")"
check "refuses 6 hex digits" "$(has_line "UNKNOWN: envoy-cluster: ${cluster_pin} names revision abcdef, which is no commit id")"
check "refuses 41 hex digits" "$(has_line "UNKNOWN: dispatch: ${dispatch_listener_pin} names revision ${ahead_rev}a, which is no commit id")"
check "refuses a ref name" "$(has_line "UNKNOWN: dispatch-server: ${server_pin} names revision main, which is no commit id")"
check "makes no compare call" "$(lacks "/compare/" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --listeners, a stack file that is not UTF-8"
new_case listener-stack-not-utf8
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
printf 'config:\n  %s:envoyImage: \xff\n' "$cluster_project" >"${case_root}/files/envoy-cluster/Pulumi.prod.yaml"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file in one line" \
  "$(has_line "UNKNOWN: envoy-cluster: cannot read envoy-cluster/Pulumi.prod.yaml: it is not UTF-8 text")"

