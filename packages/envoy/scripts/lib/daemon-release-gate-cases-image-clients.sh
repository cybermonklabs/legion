# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-image-clients.sh
#
# The --clients scenarios of the worker images. daemon-release-gate.test.sh sources this file with
# the harness's fakes; it is not run on its own.
#
# In each image a running project names as its image, and in the image the deployment's sessions/
# stack pins for its agent sessions (built FROM the worker image), the worker build puts the two
# clients a session loads at /opt/legion/pi-envoy and /opt/legion/claude-envoy, built from the
# checkout its legion is built from (packages/daemon/docker/worker.Dockerfile). The gate checks both
# are there and compares the commit that build ran at, which the image's own legion prints. A
# manifest's version names no build: claude-envoy's keeps its source's static 1.0.0, and pi-envoy's
# names a release only when a release build sets it. So each image below carries manifests whose
# versions name release tags behind the commit, and a gate that judged an image by them would shut.
#
# A daemonImage runs the daemon alone, and no session loads its clients; what it delivers to a
# session is the pi-envoy its daemon installs into a kept pod on a stale-plugin relaunch: the
# project's runtime.kubernetes.plugins when its rendered legion.yaml (legion/testdata/<key>-legion.yaml)
# sets it, else the pair its legion links (`legion version`'s second line). The gate judges that
# version by its release tag. A project with no daemonImage runs its daemon from its image, which is
# then judged both ways.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the test that sources this file
# shellcheck disable=SC2034 # gate_under_test is the harness's, which its runners read

echo "case: --clients, every image at or after the commit"
new_case clients-open
clients_open
run_gate -- --clients "$commit"
check "exits 0" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"
check "LINUX's image passes by its build commit, naming both clients" \
  "$(has_line "PASS: LINUX image (pi-envoy and claude-envoy) runs ${identical_rev} (identical to ${commit})")"
check "LINUX's daemonImage passes by the pi-envoy its legion links, by that version's release tag" \
  "$(has_line "PASS: LINUX daemonImage ${daemon_label} runs pi-envoy ${linked_envoy} from pi-envoy-v${linked_envoy} at ${ahead_rev} (ahead of ${commit})$(linked_note LINUX)")"
check "INFRA's image passes" "$(has_line "PASS: INFRA image (pi-envoy and claude-envoy) runs ${ahead_rev} (ahead of ${commit})")"
check "INFRA's daemonImage passes" \
  "$(has_line "PASS: INFRA daemonImage ${daemon_label} runs pi-envoy ${linked_envoy} from pi-envoy-v${linked_envoy} at ${ahead_rev} (ahead of ${commit})$(linked_note INFRA)")"
check "never judges a daemonImage by its build commit" "$(lacks "daemonImage (pi-envoy and claude-envoy)" "$out")"
check "never reads the clients of an image that only runs a daemon" "$(lacks "--entrypoint cat ${worker_a}" "$calls")"
check "reads each running project's rendered legion.yaml" \
  "$(grep -qF "contents/legion/testdata/linux-legion.yaml" "$calls" && grep -qF "contents/legion/testdata/infra-legion.yaml" "$calls" && echo true || echo false)"
check "the sessions/ stack's image passes by its build commit, naming both clients" \
  "$(has_line "PASS: sessions image (pi-envoy and claude-envoy) runs ${ahead_rev} (ahead of ${commit})")"
check "reads the sessions/ pin under the project name sessions/Pulumi.yaml declares" \
  "$(grep -qF "gh api repos/${deployment}/contents/sessions/Pulumi.yaml" "$calls" && grep -qF "gh api repos/${deployment}/contents/sessions/Pulumi.prod.yaml" "$calls" && echo true || echo false)"
check "SMOKE at replicas 0 is skipped by name" "$(has_line "SKIPPED: SMOKE runs no daemon (replicas: 0)")"
check "never runs the image of a project that runs no daemon" "$(lacks "$worker_smoke" "$calls")"
check "reads each client in each image, from the roots the build puts them at" \
  "$(grep -qF -- "--entrypoint cat ${lean_image} /opt/legion/pi-envoy/package.json" "$calls" && grep -qF -- "--entrypoint cat ${lean_image} /opt/legion/claude-envoy/package.json" "$calls" && echo true || echo false)"
check "an image two projects name is read once" \
  "$(is "$(grep -cF -- "--entrypoint /opt/legion/bin/legion ${worker_a} version" "$calls")" 1)"
check "runs each image with no network, read-only, every capability dropped and no new privileges" \
  "$(grep -q '^docker run' "$calls" && awk '/^docker run/ && !(index($0, " --network none ") && index($0, " --read-only ") && index($0, " --cap-drop ALL ") && index($0, " --security-opt no-new-privileges ")) { bad = 1 } END { exit bad }' "$calls" && echo true || echo false)"
check "never resolves a manifest's version to a release tag" \
  "$([ "$(lacks "pi-envoy-v16.12.1" "$calls")" = true ] && lacks "claude-envoy-v1.0.0" "$calls" || echo false)"
check "reads no image's labels" "$(lacks "imagetools" "$calls")"

echo "case: --clients, an image built behind the commit"
new_case clients-image-behind
clients_open
worker_image "$lean_image" "$behind_rev"
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts naming the image, both clients and the commit it was built at" \
  "$(has_line "GATE SHUT: LINUX image (pi-envoy and claude-envoy) runs ${behind_rev} (behind ${commit} by 12)")"
check "the verdict is GATE SHUT" "$(last_line_is "GATE SHUT")"

echo "case: --clients, an image with no claude-envoy"
new_case clients-no-claude-envoy
clients_open
worker_image "$lean_image" "$ahead_rev" no-claude-envoy
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the image and the package it could not find, the command's words kept" \
  "$(has_kept_line "UNKNOWN: LINUX image: ${lean_image} carries no @sjawhar/claude-envoy at /opt/legion/claude-envoy/package.json: cat failed; its own words are kept in KEPT")"
check "the kept file holds the command's words" \
  "$(is "$(kept_text "UNKNOWN: LINUX image: ")" "cat: /opt/legion/claude-envoy/package.json: No such file or directory")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

# Control: a gate that reads pi-envoy alone in an image opens on the fixture above, whose image
# carries no claude-envoy; so that fixture fails such a gate, by name.
mutant read_clients pi-envoy-alone <<'EOF'
read_clients() {
  local text
  text=$(in_image "$1" cat /opt/legion/pi-envoy/package.json) || { echo "error no pi-envoy"; return; }
  text=$(in_image "$1" /opt/legion/bin/legion version) || { echo "error no legion"; return; }
  [[ $(head -n 1 <<<"$text") =~ ^legion\ [^\ ]+\ commit\ ([0-9a-f]{7,40})$ ]] || { echo "error no commit"; return; }
  printf 'commit %s\n' "${BASH_REMATCH[1]}"
}
EOF
gate_under_test="$mutant_gate"
run_gate -- --clients "$commit"
gate_under_test="$script"
check "control: a gate that reads pi-envoy alone opens on the image with no claude-envoy" "$(last_line_is "GATE OPEN")"

echo "case: --clients, an image with no pi-envoy"
new_case clients-no-pi-envoy
clients_open
worker_image "$infra_image" "$ahead_rev" no-pi-envoy
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the package it could not find, the command's words kept" \
  "$(has_kept_line "UNKNOWN: INFRA image: ${infra_image} carries no @sjawhar/pi-envoy at /opt/legion/pi-envoy/package.json: cat failed; its own words are kept in KEPT")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, an image whose client root holds another package"
new_case clients-other-package
clients_open
printf '{"name": "@example-org/claude-plugin", "version": "1.0.0"}\n' \
  >"$(image_dir "$lean_image")/fs/opt/legion/claude-envoy/package.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN saying it found another package, the manifest kept" \
  "$(has_kept_line "UNKNOWN: LINUX image: ${lean_image}'s /opt/legion/claude-envoy/package.json names another package, not @sjawhar/claude-envoy; the manifest is kept in KEPT")"
check "the kept manifest names the package it found" "$(grep -qF '@example-org/claude-plugin' <<<"$(kept_text "UNKNOWN: LINUX image: ")" && echo true || echo false)"

echo "case: --clients, an image whose legion names no commit"
new_case clients-no-commit
clients_open
worker_image "$lean_image" "$ahead_rev" no-commit
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, what legion version printed kept" \
  "$(has_kept_line "UNKNOWN: LINUX image: ${lean_image}'s /opt/legion/bin/legion version names no build commit; its first line is kept in KEPT")"
check "the kept file holds what it printed" "$(is "$(kept_text "UNKNOWN: LINUX image: ")" "legion (devel)")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, an image with no legion to run"
new_case clients-no-legion
clients_open
rm "$(image_dir "$lean_image")/legion-version"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, docker's words kept, its help hint among them" \
  "$(has_kept_line "UNKNOWN: LINUX image: cannot run ${lean_image}'s /opt/legion/bin/legion version: docker failed; its own words are kept in KEPT")"
check "the kept file holds docker's words" \
  "$(grep -qF "docker: Error response from daemon: failed to create task for container" <<<"$(kept_text "UNKNOWN: LINUX image: ")" && echo true || echo false)"

echo "case: --clients, an image the registry does not answer for"
new_case clients-image-unreadable
clients_open
rm -r "$(image_dir "$lean_image")"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, docker's words kept" \
  "$(has_kept_line "UNKNOWN: LINUX image: cannot run ${lean_image}: docker failed; its own words are kept in KEPT")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, a project on a tmux host"
new_case clients-tmux
clients_open
legion_stack "    - key: LINUX
      replicas: 1
      daemonImage: ${worker_a}
    - key: HOSTED
      replicas: 1
      runtime: tmux
      image: ${lean_image}"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the host's clients" \
  "$(has_line "UNKNOWN: HOSTED runs its sessions on a tmux host (runtime: tmux), whose clients this gate does not read")"
check "never runs the project's image" "$(lacks "$lean_image" "$calls")"
check "still reads the other project, its daemonImage by the pi-envoy its legion links" \
  "$(has_line "PASS: LINUX daemonImage ${daemon_label} runs pi-envoy ${linked_envoy} from pi-envoy-v${linked_envoy} at ${ahead_rev} (ahead of ${commit})$(linked_note LINUX)")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, legion/projects/ holding the projects (production)"
new_case clients-production-shape
clients_open
legion_settings
project_file 20-LINUX "key: LINUX
replicas: 1
image: ${lean_image}
daemonImage: ${worker_a}"
run_gate -- --clients "$commit"
check "exits 0" "$(is "$status" 0)"
check "reads the project file's image" \
  "$(has_line "PASS: LINUX image (pi-envoy and claude-envoy) runs ${identical_rev} (identical to ${commit})")"
check "never reads the list left in legion/Pulumi.prod.yaml" "$(lacks "$infra_image" "$calls")"

echo "case: --clients and --listeners together"
new_case clients-and-listeners
clients_open
run_gate -- --clients --listeners "$commit"
check "exits 2" "$(is "$status" 2)"
check "says the two modes exclude each other" \
  "$(grep -qF -- "--listeners and --clients read different readers; give one" "$err" && echo true || echo false)"
check "reads nothing" "$(is "$(wc -l <"$calls")" 0)"

# The deployment's sessions/ stack pins the agent-session image (sessions/Pulumi.prod.yaml
# `<project>:image`, <project> the name sessions/Pulumi.yaml declares). It is built FROM the worker
# image and links its /opt/legion/pi-envoy, and its pods deliver to sessions, so it is judged as a
# project's image is. A stack or pin the gate cannot read is UNKNOWN by name, never left out.
echo "case: --clients, the sessions/ stack's image built behind the commit"
new_case clients-sessions-behind
clients_open
worker_image "$session_image" "$behind_rev"
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts naming the sessions image, both clients and the commit it was built at" \
  "$(has_line "GATE SHUT: sessions image (pi-envoy and claude-envoy) runs ${behind_rev} (behind ${commit} by 12)")"
check "the verdict is GATE SHUT" "$(last_line_is "GATE SHUT")"

echo "case: --clients, the sessions/ stack's image with no claude-envoy"
new_case clients-sessions-no-claude-envoy
clients_open
worker_image "$session_image" "$ahead_rev" no-claude-envoy
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the image and the package it could not find" \
  "$(has_kept_line "UNKNOWN: sessions image: ${session_image} carries no @sjawhar/claude-envoy at /opt/legion/claude-envoy/package.json: cat failed; its own words are kept in KEPT")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, a deployment with no sessions/ stack"
new_case clients-sessions-no-stack
clients_open
rm -r "${case_root}/files/sessions"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file it could not read, gh's words kept" \
  "$(has_kept_line "UNKNOWN: sessions image: cannot read ${deployment} sessions/Pulumi.yaml: gh failed (HTTP 404); its own words are kept in KEPT")"
check "never runs the sessions image" "$(lacks "$session_image" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, a sessions/ stack that pins no image"
new_case clients-sessions-no-pin
clients_open
printf 'config:\n  %s:defaultSize: default\n' "$sessions_project" >"${case_root}/files/sessions/Pulumi.prod.yaml"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the key it needs" \
  "$(has_line "UNKNOWN: sessions image: sessions/Pulumi.prod.yaml sets no ${sessions_project}:image")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, the sessions/ pin read under the project name sessions/Pulumi.yaml declares"
new_case clients-sessions-project-name
clients_open
sessions_stack "$session_image" example-other-sessions
project sessions example-other-sessions
worker_image "$session_image" "$behind_rev"
run_gate -- --clients "$commit"
check "exits 1, on the pin under that name" "$(is "$status" 1)"
check "shuts naming the sessions image" \
  "$(has_line "GATE SHUT: sessions image (pi-envoy and claude-envoy) runs ${behind_rev} (behind ${commit} by 12)")"

echo "case: --clients, a sessions/ pin that is no image reference"
new_case clients-sessions-not-reference
clients_open
printf 'config:\n  %s:image: "--privileged %s"\n' "$sessions_project" "$session_image" >"${case_root}/files/sessions/Pulumi.prod.yaml"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the setting, the pin kept" \
  "$(has_kept_line "UNKNOWN: sessions image: sessions/Pulumi.prod.yaml sets ${sessions_project}:image to a value that is no image reference, kept in KEPT")"
check "the kept file holds the pin" "$(is "$(kept_text "UNKNOWN: sessions image: ")" "--privileged ${session_image}")"
check "never runs docker with it" "$(lacks "--privileged" "$calls")"

# An image is read only by digest: under a tag, docker would run whatever image it holds by that name,
# which need not be the one deployed. The deployment pins by digest, and the gate holds it to that.
echo "case: --clients, a sessions/ pin by tag, not by digest"
new_case clients-sessions-tag
clients_open
sessions_stack "ghcr.io/cybermonklabs/agent-session:latest"
worker_image "ghcr.io/cybermonklabs/agent-session:latest" "$ahead_rev"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN saying the image is not pinned by digest" \
  "$(grep -qF "UNKNOWN: sessions image: cannot run ghcr.io/cybermonklabs/agent-session:latest: ghcr.io/cybermonklabs/agent-session:latest is not pinned by digest" "$out" && echo true || echo false)"
check "never runs docker with it" "$(grep '^docker run' "$calls" | grep -qF 'agent-session:latest' && echo false || echo true)"

# The daemon's pi-envoy: the rendered legion.yaml's runtime.kubernetes.plugins.pi_envoy when it sets
# one, else the pair `legion version` names on its second line, each judged by its release tag and
# never by the image's build commit.
echo "case: --clients, a daemonImage whose linked pi-envoy is behind the commit, its build ahead"
box_case clients-daemon-linked-behind
plugins_line "$worker_a" "plugins @sjawhar/pi-legion 19.28.1 @sjawhar/pi-envoy 17.39.0"
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts LINUX's daemonImage naming the linked version and its tag" \
  "$(has_line "GATE SHUT: LINUX daemonImage ${daemon_label} runs pi-envoy 17.39.0 from pi-envoy-v17.39.0 at ${envoy_behind_rev} (behind ${commit} by 300)$(linked_note LINUX)")"
check "and INFRA's, which names the same image" \
  "$(has_line "GATE SHUT: INFRA daemonImage ${daemon_label} runs pi-envoy 17.39.0 from pi-envoy-v17.39.0 at ${envoy_behind_rev} (behind ${commit} by 300)$(linked_note INFRA)")"
check "the build commit, ahead, opens nothing" "$(lacks "daemonImage (pi-envoy and claude-envoy)" "$out")"
check "the verdict is GATE SHUT" "$(last_line_is "GATE SHUT")"

# Every pin of the image passes: its build commit and the pi-envoy its legion links are ahead. The
# project's rendered legion.yaml sets an older pi-envoy, which is what its daemon installs, so a gate
# that read the linked pair alone would open on this fixture.
echo "case: --clients, a daemonImage whose every pin passes, its rendered legion.yaml setting an older pi-envoy"
box_case clients-daemon-golden
golden LINUX 17.39.0
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts LINUX's daemonImage naming the version its legion.yaml sets and that version's tag" \
  "$(has_line "GATE SHUT: LINUX daemonImage ${daemon_label} runs pi-envoy 17.39.0 from pi-envoy-v17.39.0 at ${envoy_behind_rev} (behind ${commit} by 300)$(golden_note LINUX)")"
check "the linked pair, ahead, does not open LINUX's" "$(lacks "PASS: LINUX daemonImage" "$out")"
check "a rendered legion.yaml that sets no plugins is judged by the linked pair" \
  "$(has_line "PASS: INFRA daemonImage ${daemon_label} runs pi-envoy ${linked_envoy} from pi-envoy-v${linked_envoy} at ${ahead_rev} (ahead of ${commit})$(linked_note INFRA)")"
check "the verdict is GATE SHUT" "$(last_line_is "GATE SHUT")"

echo "case: --clients, a running project whose rendered legion.yaml is missing"
new_case clients-daemon-no-golden
clients_open
rm "${case_root}/files/legion/testdata/linux-legion.yaml"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file it could not read, gh's words kept" \
  "$(has_kept_line "UNKNOWN: LINUX daemonImage ${daemon_label}: cannot read ${deployment} legion/testdata/linux-legion.yaml: gh failed (HTTP 404); its own words are kept in KEPT")"
check "still reads INFRA's" \
  "$(has_line "PASS: INFRA daemonImage ${daemon_label} runs pi-envoy ${linked_envoy} from pi-envoy-v${linked_envoy} at ${ahead_rev} (ahead of ${commit})$(linked_note INFRA)")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

for shape in not-yaml not-mapping plugins-not-mapping; do
  echo "case: --clients, a rendered legion.yaml that is ${shape}"
  new_case "clients-daemon-golden-${shape}"
  clients_open
  file="${case_root}/files/legion/testdata/linux-legion.yaml"
  case "$shape" in
    not-yaml)
      printf 'runtime: [\n' >"$file"
      expected="UNKNOWN: LINUX daemonImage ${daemon_label}: ${deployment} legion/testdata/linux-legion.yaml is not YAML (line "
      ;;
    not-mapping)
      printf -- '- project: LINUX\n' >"$file"
      expected="UNKNOWN: LINUX daemonImage ${daemon_label}: ${deployment} legion/testdata/linux-legion.yaml is not a mapping"
      ;;
    plugins-not-mapping)
      printf 'runtime:\n  kubernetes:\n    plugins: latest\n' >"$file"
      expected="UNKNOWN: LINUX daemonImage ${daemon_label}: ${deployment} legion/testdata/linux-legion.yaml sets runtime.kubernetes.plugins to a value that names no pi_envoy this gate can read"
      ;;
  esac
  run_gate -- --clients "$commit"
  check "exits 3" "$(is "$status" 3)"
  check "refuses UNKNOWN naming the file" "$(grep -qF -- "$expected" "$out" && grep -q '^UNKNOWN: LINUX daemonImage ' "$out" && echo true || echo false)"
  check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"
done

echo "case: --clients, a daemonImage whose legion links no plugin pair"
new_case clients-daemon-no-plugins
clients_open
worker_image "$worker_a" "$ahead_rev" no-plugins
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the image and the rendered legion.yaml" \
  "$(has_line "UNKNOWN: LINUX daemonImage ${daemon_label}: ${worker_a}'s /opt/legion/bin/legion version links no plugin pair, and legion/testdata/linux-legion.yaml sets no runtime.kubernetes.plugins")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, a daemonImage whose legion version's second line names no pi-envoy"
new_case clients-daemon-plugins-unparseable
clients_open
plugins_line "$worker_a" "plugins @sjawhar/pi-legion 19.28.1"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, the line kept" \
  "$(has_kept_line "UNKNOWN: LINUX daemonImage ${daemon_label}: ${worker_a}'s /opt/legion/bin/legion version prints a second line that names no @sjawhar/pi-envoy version, kept in KEPT")"
check "the kept file holds the line" "$(is "$(kept_text "UNKNOWN: LINUX daemonImage ")" "plugins @sjawhar/pi-legion 19.28.1")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, a project with no daemonImage runs its daemon from its image"
new_case clients-image-runs-daemon
clients_open
legion_stack "    - key: SOLO
      replicas: 1
      image: ${lean_image}"
golden SOLO
tag pi-envoy-v17.39.0 "$envoy_behind_rev"
compare "$envoy_behind_rev" behind 300
plugins_line "$lean_image" "plugins @sjawhar/pi-legion 19.28.1 @sjawhar/pi-envoy 17.39.0"
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "judges the image by its build commit" \
  "$(has_line "PASS: SOLO image (pi-envoy and claude-envoy) runs ${identical_rev} (identical to ${commit})")"
check "and by the pi-envoy its daemon installs" \
  "$(has_line "GATE SHUT: SOLO image ${daemon_label} runs pi-envoy 17.39.0 from pi-envoy-v17.39.0 at ${envoy_behind_rev} (behind ${commit} by 300)$(linked_note SOLO)")"
check "runs the image's legion once" "$(is "$(grep -cF -- "--entrypoint /opt/legion/bin/legion ${lean_image} version" "$calls")" 1)"

echo "case: --clients, a running entry with no key, which names no rendered legion.yaml"
new_case clients-daemon-no-key
clients_open
legion_stack "    - replicas: 1
      daemonImage: ${worker_a}"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the key's kind" \
  "$(has_line "UNKNOWN: <an entry with no key> names key (null), which is no project key, so no legion/testdata golden is its own")"
check "never runs its image" "$(lacks "$worker_a" "$calls")"

# Text read from an image is printed with each control character written as \x<hh>, so no image
# can start a line of the gate's output, a verdict among them.
echo "case: --clients, an image whose package name holds a newline and a verdict"
new_case clients-name-newline
clients_open
printf '{"name": "@example-org/claude-plugin\\nGATE OPEN\\n#", "version": "1.0.0"}\n' \
  >"$(image_dir "$lean_image")/fs/opt/legion/claude-envoy/package.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN saying it found another package, the manifest with its newlines kept" \
  "$(has_kept_line "UNKNOWN: LINUX image: ${lean_image}'s /opt/legion/claude-envoy/package.json names another package, not @sjawhar/claude-envoy; the manifest is kept in KEPT")"
check "no line of its output is the image's GATE OPEN" "$(grep -qxF "GATE OPEN" "$out" && echo false || echo true)"
check "the verdict is UNKNOWN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, an image whose legion version holds a carriage return"
new_case clients-version-control
clients_open
printf 'legion (devel)\rGATE OPEN\n' >"$(image_dir "$lean_image")/legion-version"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, the line with its carriage return kept, not printed" \
  "$(has_kept_line "UNKNOWN: LINUX image: ${lean_image}'s /opt/legion/bin/legion version names no build commit; its first line is kept in KEPT")"
