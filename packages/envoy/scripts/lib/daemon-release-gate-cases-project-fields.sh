# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-project-fields.sh
#
# The scenarios of a project's fields, read by the daemon leg and --clients alike.
# daemon-release-gate.test.sh sources this file after the harness and the client fixtures
# (daemon-release-gate-fixtures-clients.sh), whose clients_background it uses; it is not run on its
# own.
#
# A field of the wrong type is never a reason to skip a project, and an image reference with a tab
# in it is never read as the reference before the tab: YAML's false equals 0 in Python, and a tab
# cut the gate's own line apart. The deployment's legion program refuses a file of legion/projects/
# not named <nn>-<KEY>.yaml and a file whose key is not its name's KEY (projectFileRule), so the
# gate takes neither as a project, and says so by name.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the test that sources this file

readonly invalid_fleet="    - key: LINUX
      replicas: 1
      daemonImage: ${worker_a}
    - key: BET
      replicas: false
      daemonImage: ${worker_b}
    - key: OSSV
      daemon: \"false\"
      daemonImage: ${worker_b}
    - key: RISK
      replicas: -1
      daemonImage: ${worker_b}
    - key: SIZE
      replicas: \"1\"
      daemonImage: ${worker_b}
    - key: INFRA
      replicas: 1
      daemonImage: \"${worker_a}\tsuffix\""

echo "case: projects whose fields are of the wrong type, or whose image is no plain reference"
new_case fields-invalid
legion_stack "$invalid_fleet"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$behind_rev"
compare "$ahead_rev" ahead 0
compare "$behind_rev" behind 3
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "a replicas of false is no count, not a skip" "$(has_line "UNKNOWN: BET names replicas false, which is no count of replicas")"
check "a daemon that is a string is no boolean" "$(has_line "UNKNOWN: OSSV names daemon \"false\", which is neither true nor false")"
check "a negative replicas is no count" "$(has_line "UNKNOWN: RISK names replicas -1, which is no count of replicas")"
check "a replicas that is a string is no count" "$(has_line "UNKNOWN: SIZE names replicas \"1\", which is no count of replicas")"
check "an image with a tab is no reference, and is not read cut short" \
  "$(has_line "UNKNOWN: INFRA names daemonImage \"${worker_a}\\tsuffix\", which is no image reference")"
check "no project is skipped" "$(lacks "SKIPPED" "$out")"
check "the image of a project whose fields are refused is never read" "$(lacks "$worker_b" "$calls")"
check "the valid project is still read" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, projects whose fields are of the wrong type"
new_case fields-invalid-clients
clients_background
legion_stack "$invalid_fleet"
worker_image "$worker_b" "$behind_rev"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "a replicas of false is no count, not a skip" "$(has_line "UNKNOWN: BET names replicas false, which is no count of replicas")"
check "an image with a tab is no reference" \
  "$(has_line "UNKNOWN: INFRA names daemonImage \"${worker_a}\\tsuffix\", which is no image reference")"
check "the image of a project whose fields are refused is never run" "$(lacks "$worker_b" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: a project file whose key is not its name's"
new_case fields-wrong-key
legion_settings
fleet_files
project_file 20-LINUX "key: BET
replicas: 1
daemonImage: ${worker_a}"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file and its key" \
  "$(has_line "UNKNOWN: ${deployment} legion/projects/20-LINUX.yaml declares key 'BET', not LINUX")"
check "reads no image" "$(lacks "docker" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: a project file that declares no key"
new_case fields-no-key
legion_settings
fleet_files
project_file 20-LINUX "replicas: 1
daemonImage: ${worker_a}"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file" \
  "$(has_line "UNKNOWN: ${deployment} legion/projects/20-LINUX.yaml declares key None, not LINUX")"
check "reads no image" "$(lacks "docker" "$calls")"

echo "case: a file of legion/projects/ not named <nn>-<KEY>.yaml"
new_case fields-stray-file
legion_settings
fleet_files
printf 'key: NOTES\n' >"${case_root}/files/legion/projects/README.yaml"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file" \
  "$(has_line "UNKNOWN: ${deployment} legion/projects/README.yaml is not named <nn>-<KEY>.yaml, so no key is known to be its own")"
check "reads no image from a directory it could not take" "$(lacks "docker" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"
