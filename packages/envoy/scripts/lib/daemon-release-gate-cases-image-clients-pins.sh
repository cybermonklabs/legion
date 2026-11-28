# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-image-clients-pins.sh
#
# The --clients scenarios for sessions image pins, the pi-envoy each daemon installs, and text
# read from images. daemon-release-gate.test.sh sources this file through its group runner after
# the harness and the client fixtures (daemon-release-gate-fixtures-clients.sh).
# Do not run this file on its own.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the test that sources this file

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
check "refuses UNKNOWN naming the pin" \
  "$(has_line "UNKNOWN: sessions image: sessions/Pulumi.prod.yaml sets ${sessions_project}:image to --privileged ${session_image}, which is no image reference")"
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
box_case_full_fleet clients-daemon-linked-behind
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
box_case_full_fleet clients-daemon-golden
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
check "refuses UNKNOWN naming the file it could not read" \
  "$(has_line "UNKNOWN: LINUX daemonImage ${daemon_label}: cannot read ${deployment} legion/testdata/linux-legion.yaml: gh: Not Found (HTTP 404)")"
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
      expected="UNKNOWN: LINUX daemonImage ${daemon_label}: ${deployment} legion/testdata/linux-legion.yaml is not YAML: "
      ;;
    not-mapping)
      printf -- '- project: LINUX\n' >"$file"
      expected="UNKNOWN: LINUX daemonImage ${daemon_label}: ${deployment} legion/testdata/linux-legion.yaml is not a mapping"
      ;;
    plugins-not-mapping)
      printf 'runtime:\n  kubernetes:\n    plugins: latest\n' >"$file"
      expected="UNKNOWN: LINUX daemonImage ${daemon_label}: ${deployment} legion/testdata/linux-legion.yaml sets runtime.kubernetes.plugins to \"latest\", which names no pi_envoy this gate can read"
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
check "refuses UNKNOWN quoting the line" \
  "$(has_line "UNKNOWN: LINUX daemonImage ${daemon_label}: ${worker_a}'s /opt/legion/bin/legion version prints \"plugins @sjawhar/pi-legion 19.28.1\" as its second line, which names no @sjawhar/pi-envoy version")"
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
check "refuses UNKNOWN naming the key" \
  "$(has_line "UNKNOWN: <an entry with no key> names key null, which is no project key, so no legion/testdata golden is its own")"
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
check "refuses UNKNOWN naming the package, its newlines written out" \
  "$(has_line "UNKNOWN: LINUX image: ${lean_image}'s /opt/legion/claude-envoy/package.json names @example-org/claude-plugin\\x0aGATE OPEN\\x0a#, not @sjawhar/claude-envoy")"
check "no line of its output is the image's GATE OPEN" "$(grep -qxF "GATE OPEN" "$out" && echo false || echo true)"
check "the verdict is UNKNOWN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, an image whose legion version holds a carriage return"
new_case clients-version-control
clients_open
printf 'legion (devel)\rGATE OPEN\n' >"$(image_dir "$lean_image")/legion-version"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN quoting the line, its carriage return written out" \
  "$(has_line "UNKNOWN: LINUX image: ${lean_image}'s /opt/legion/bin/legion version names no build commit: legion (devel)\\x0dGATE OPEN")"
