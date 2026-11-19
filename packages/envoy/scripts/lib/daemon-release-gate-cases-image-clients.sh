# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-image-clients.sh
#
# The --clients scenarios of the worker images. daemon-release-gate.test.sh sources this file with
# the harness's fakes; it is not run on its own.
#
# In each image a running project names, its daemonImage and its image, the worker build puts the
# two clients a session loads at /opt/legion/pi-envoy and /opt/legion/claude-envoy, built from the
# checkout its legion is built from (packages/daemon/docker/worker.Dockerfile). The gate checks both
# are there and compares the commit that build ran at, which the image's own legion prints. A
# manifest's version names no build: claude-envoy's keeps its source's static 1.0.0, and pi-envoy's
# names a release only when a release build sets it. So each image below carries manifests whose
# versions name release tags behind the commit, and a gate that judged an image by them would shut.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the test that sources this file
# shellcheck disable=SC2034 # gate_under_test is the harness's, which its runners read

readonly lean_image="us-west1-docker.pkg.dev/example-project/legion/worker-lean@sha256:2222"
readonly client_fleet="    - key: SMOKE
      replicas: 0
      image: ${worker_smoke}
    - key: LINUX
      replicas: 1
      image: ${lean_image}
      daemonImage: ${worker_a}
    - key: INFRA
      replicas: 1
      image: ${infra_image}
      daemonImage: ${worker_a}"
# clients_open: every surface --clients reads at or after the commit: each image of client_fleet by
# its build, and the developer box (dotfiles) by its release tags; the manifests' own versions name
# release tags behind the commit.
clients_open() {
  legion_stack "$client_fleet"
  worker_image "$worker_a" "$ahead_rev"
  worker_image "$lean_image" "$identical_rev"
  worker_image "$infra_image" "$ahead_rev"
  dotfiles 17.40.0
  tag pi-envoy-v17.40.0 "$ahead_rev"
  tag claude-envoy-v4.0.0 "$identical_rev"
  tag pi-envoy-v16.12.1 "$behind_rev"
  tag claude-envoy-v1.0.0 "$behind_rev"
  compare "$ahead_rev" ahead 0
  compare "$identical_rev" identical 0
  compare "$behind_rev" behind 12
}
# clients_background: clients_open for the scenarios of another surface (the developer box, its
# versions, a project's fields), with the release tags the images' manifests name ahead of the
# commit too, so the images pass whatever they are judged by and those scenarios' rows answer for
# their own surface alone.
clients_background() {
  clients_open
  tag pi-envoy-v16.12.1 "$ahead_rev"
  tag claude-envoy-v1.0.0 "$ahead_rev"
}

echo "case: --clients, every image at or after the commit"
new_case clients-open
clients_open
run_gate -- --clients "$commit"
check "exits 0" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"
check "LINUX's image passes by its build commit, naming both clients" \
  "$(has_line "PASS: LINUX image (pi-envoy and claude-envoy) runs ${identical_rev} (identical to ${commit})")"
check "LINUX's daemonImage passes" \
  "$(has_line "PASS: LINUX daemonImage (pi-envoy and claude-envoy) runs ${ahead_rev} (ahead of ${commit})")"
check "INFRA's image passes" "$(has_line "PASS: INFRA image (pi-envoy and claude-envoy) runs ${ahead_rev} (ahead of ${commit})")"
check "INFRA's daemonImage passes" \
  "$(has_line "PASS: INFRA daemonImage (pi-envoy and claude-envoy) runs ${ahead_rev} (ahead of ${commit})")"
check "SMOKE at replicas 0 is skipped by name" "$(has_line "SKIPPED: SMOKE runs no daemon (replicas: 0)")"
check "never runs the image of a project that runs no daemon" "$(lacks "$worker_smoke" "$calls")"
check "reads each client in each image, from the roots the build puts them at" \
  "$(grep -qF -- "--entrypoint cat ${lean_image} /opt/legion/pi-envoy/package.json" "$calls" && grep -qF -- "--entrypoint cat ${lean_image} /opt/legion/claude-envoy/package.json" "$calls" && echo true || echo false)"
check "an image two projects name is read once" \
  "$(is "$(grep -cF -- "--entrypoint /opt/legion/bin/legion ${worker_a} version" "$calls")" 1)"
check "runs each image with no network" \
  "$(grep -q '^docker run' "$calls" && awk '/^docker run/ && !index($0, "--network none") { bad = 1 } END { exit bad }' "$calls" && echo true || echo false)"
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
check "refuses UNKNOWN naming the image and the package it could not find" \
  "$(has_line "UNKNOWN: LINUX image: ${lean_image} carries no @sjawhar/claude-envoy at /opt/legion/claude-envoy/package.json: cat: /opt/legion/claude-envoy/package.json: No such file or directory")"
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
check "refuses UNKNOWN naming the package it could not find" \
  "$(has_line "UNKNOWN: INFRA image: ${infra_image} carries no @sjawhar/pi-envoy at /opt/legion/pi-envoy/package.json: cat: /opt/legion/pi-envoy/package.json: No such file or directory")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, an image whose client root holds another package"
new_case clients-other-package
clients_open
printf '{"name": "@example-org/claude-plugin", "version": "1.0.0"}\n' \
  >"$(image_dir "$lean_image")/fs/opt/legion/claude-envoy/package.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the package it found" \
  "$(has_line "UNKNOWN: LINUX image: ${lean_image}'s /opt/legion/claude-envoy/package.json names @example-org/claude-plugin, not @sjawhar/claude-envoy")"

echo "case: --clients, an image whose legion names no commit"
new_case clients-no-commit
clients_open
worker_image "$lean_image" "$ahead_rev" no-commit
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN quoting what legion version printed" \
  "$(has_line "UNKNOWN: LINUX image: ${lean_image}'s /opt/legion/bin/legion version names no build commit: legion (devel)")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, an image with no legion to run"
new_case clients-no-legion
clients_open
rm "$(image_dir "$lean_image")/legion-version"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming docker's answer, not its help hint" \
  "$(grep -qxF "UNKNOWN: LINUX image: cannot run ${lean_image}'s /opt/legion/bin/legion version: docker: Error response from daemon: failed to create task for container: exec: \"/opt/legion/bin/legion\": stat /opt/legion/bin/legion: no such file or directory: unknown" "$out" && echo true || echo false)"

echo "case: --clients, an image the registry does not answer for"
new_case clients-image-unreadable
clients_open
rm -r "$(image_dir "$lean_image")"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming docker's answer, not its help hint" \
  "$(has_line "UNKNOWN: LINUX image: cannot run ${lean_image}: docker: Error response from daemon: manifest for ${lean_image} not found: manifest unknown")"
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
check "still reads the other project" \
  "$(has_line "PASS: LINUX daemonImage (pi-envoy and claude-envoy) runs ${ahead_rev} (ahead of ${commit})")"
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
