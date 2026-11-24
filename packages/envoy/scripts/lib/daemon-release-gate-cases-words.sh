# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-words.sh
#
# The scenarios of the words another party writes. Whatever answers gh's calls in GitHub's place
# holds gh's token, and whatever answers docker's in a registry's holds docker's login, which for
# ghcr.io is gh's token as the gate's header logs docker in. Each writes its tool's error and its
# answer, so each row here puts the marker not-a-secret-token in one of them, through the fake gh,
# docker or a deployment file, and holds the gate to printing no run of 8 of its characters (the
# record rule's run, redacttest.Piece) on stdout or stderr. A twin shows the marker reached the
# gate: the file the gate keeps holds it. The filter rows put gh's own token, which holds the
# marker, in a reading that holds to its grammar (an image pin, a source, a hub file's path): the
# gate prints the reading and filters each run of the token out of the line, and a control shows a
# pin with no run of it prints whole. daemon-release-gate.test.sh runs this file in a process of its
# own (lib/daemon-release-gate-group.sh); it is not run on its own.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the group runner

readonly marker="not-a-secret-token"
readonly words_fleet="    - key: LINUX
      replicas: 1
      daemonImage: ${worker_a}"

# printed: all the gate printed, stdout and stderr.
printed() { cat "$out" "$err"; }
# prints_no_run <what>: the row: the gate printed no run of 8 characters of the marker.
prints_no_run() {
  check "prints no run of the marker $1" "$(holds_run "$(printed)" "$marker" && echo false || echo true)"
}
# kept_marker <text>: the twin: the file the line holding <text> names keeps the marker.
kept_marker() {
  check "twin: the kept file holds the marker" "$(grep -qF "$marker" <<<"$(kept_text "$1")" && echo true || echo false)"
}
# daemons_words <name>: a daemon-leg case with LINUX at worker_a, ahead of the commit.
daemons_words() {
  new_case "$1"
  legion_stack "$words_fleet"
  single "$worker_a" "$legion" "$ahead_rev"
  compare "$ahead_rev" ahead 0
}
# listeners_words <name>: a --listeners case whose program declares no hub and whose pins pass.
listeners_words() {
  new_case "$1"
  listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
  envoy_program none
  index "$cluster_pin" "$legion" "$ahead_rev"
  index "$dispatch_listener_pin" "$legion" "$ahead_rev"
  compare "$ahead_rev" ahead 0
}
# words_plugin <source json>: the marketplace's one claude-envoy entry, with that source.
words_plugin() {
  printf '{"name": "legion-plugins", "plugins": [{"name": "claude-envoy", "source": %s}]}\n' "$1" \
    >"${case_root}/marketplaces/example-org/plugins.json"
}

# --- gh's own words for a call that failed -------------------------------------------------------

echo "case: words: gh's error for a deployment file (fetch)"
daemons_words words-fetch
refuse "repos/${deployment}/contents/legion/Pulumi.yaml" "gh: ${marker} (HTTP 401)"
run_gate -- "$commit"
prints_no_run "gh said for legion/Pulumi.yaml"
check "names the file and the HTTP status" \
  "$(has_kept_line "UNKNOWN: cannot read ${deployment} legion/Pulumi.yaml: gh failed (HTTP 401); its own words are kept in KEPT")"
kept_marker "UNKNOWN: cannot read "

echo "case: words: gh's error for the listing of legion/projects/"
new_case words-listing
legion_settings
fleet_files
run_gate FAKE_LIST_DOWN=1 "FAKE_LIST_DOWN_TEXT=gh: ${marker} (HTTP 500)" -- "$commit"
prints_no_run "gh said for the listing"
kept_marker "UNKNOWN: cannot list "

echo "case: words: gh's error for a compare"
daemons_words words-compare
refuse "repos/cybermonklabs/legion/compare/${commit}...${ahead_rev}" "gh: ${marker} (HTTP 502)"
run_gate -- "$commit"
prints_no_run "gh said for the compare"
kept_marker "UNKNOWN: LINUX: cannot compare "

echo "case: words: gh's error for the deployment's head"
listeners_words words-head
refuse "repos/${deployment}/commits/HEAD" "gh: ${marker} (HTTP 401)"
run_gate -- --listeners "$commit"
prints_no_run "gh said for the head"
kept_marker "UNKNOWN: envoy: cannot read the head"

echo "case: words: gh's error for the commit's tree"
listeners_words words-tree
refuse "repos/${deployment}/git/trees/${deployment_head}" "gh: ${marker} (HTTP 502)"
run_gate -- --listeners "$commit"
prints_no_run "gh said for the tree"
kept_marker "UNKNOWN: envoy: cannot read the tree"

echo "case: words: gh's error for the repository's archive"
listeners_words words-tarball
refuse "repos/${deployment}/tarball/${deployment_head}" "gh: ${marker} (HTTP 403)"
run_gate -- --listeners "$commit"
prints_no_run "gh said for the archive"
kept_marker "UNKNOWN: envoy: cannot fetch "

echo "case: words: gh's error for a release tag"
new_case words-tag
clients_background
# The fake gh refuses an endpoint with a file in refuse/ before it reads the release tag's file.
refuse "repos/cybermonklabs/legion/commits/refs/tags/pi-envoy-v17.40.0" "gh: ${marker} (HTTP 422)"
run_gate -- --clients "$commit"
prints_no_run "gh said for the release tag"
kept_marker "UNKNOWN: dotfiles omp/plugins/package.json: "

echo "case: words: gh's error for the marketplace"
new_case words-marketplace
clients_background
rm -f "${case_root}/marketplaces/example-org/plugins.json"
refuse "repos/example-org/plugins/contents/.claude-plugin/marketplace.json" "gh: ${marker} (HTTP 404)"
run_gate -- --clients "$commit"
prints_no_run "gh said for the marketplace"
kept_marker "UNKNOWN: dotfiles .claude/settings.json: cannot read "

# --- docker's words ------------------------------------------------------------------------------

echo "case: words: docker's error for an image's manifest"
daemons_words words-docker
run_gate "FAKE_DOCKER_SAYS=ERROR: ${marker}: unauthorized" -- "$commit"
prints_no_run "docker said"
kept_marker "UNKNOWN: LINUX: cannot read "

# --- gh's answer where the gate expects a closed value or a grammar -------------------------------

echo "case: words: a compare whose status is no status of GitHub's"
daemons_words words-compare-status
compare "$ahead_rev" "$marker" 0
run_gate -- "$commit"
prints_no_run "the compare answered as its status"
check "says the comparison names none of GitHub's statuses" \
  "$(has_kept_line "UNKNOWN: LINUX: GitHub's comparison of ${commit}...${ahead_rev} names none of its statuses (ahead, behind, diverged, identical); it is kept in KEPT")"
kept_marker "UNKNOWN: LINUX: GitHub's comparison"

echo "case: words: a compare behind by a count that is no number"
daemons_words words-compare-count
printf '{"status":"behind","behind_by":"%s"}\n' "$marker" >"${case_root}/compare/${ahead_rev}"
run_gate -- "$commit"
prints_no_run "the compare answered as its count"
check "control: still shuts the gate on a behind status" "$(last_line_is "GATE SHUT")"
kept_marker "GATE SHUT: LINUX runs "

echo "case: words: a head that is no commit id"
listeners_words words-head-answer
run_gate "FAKE_HEAD=${marker}" -- --listeners "$commit"
prints_no_run "GitHub answered for the head"
kept_marker "UNKNOWN: envoy: GitHub answered"

echo "case: words: a release tag whose commit is no commit id"
new_case words-tag-answer
clients_background
tag pi-envoy-v17.40.0 "$marker"
run_gate -- --clients "$commit"
prints_no_run "GitHub answered for the release tag"
kept_marker "UNKNOWN: dotfiles omp/plugins/package.json: "

echo "case: words: a deployment file served in an encoding GitHub names none by"
daemons_words words-encoding
mkdir -p "${case_root}/encoding"
printf '%s\n' "$marker" >"${case_root}/encoding/repos_${deployment//\//_}_contents_legion_Pulumi.prod.yaml"
run_gate -- "$commit"
prints_no_run "GitHub answered as the encoding"
kept_marker "UNKNOWN: cannot read "

echo "case: words: an entry of legion/projects/ whose name is no project file's"
new_case words-listing-name
legion_settings
fleet_files
printf '[{"name": "%s.yaml", "type": "file", "download_url": "https://raw.example/x"}]\n' "$marker" >"${case_root}/listing-extra.json"
run_gate -- "$commit"
prints_no_run "GitHub listed as a name"
kept_marker "legion/projects/ entry"

echo "case: words: an entry of legion/projects/ of a type GitHub names none by"
new_case words-listing-type
legion_settings
fleet_files
printf '[{"name": "60-OPS.yaml", "type": "%s", "download_url": null}]\n' "$marker" >"${case_root}/listing-extra.json"
run_gate -- "$commit"
prints_no_run "GitHub listed as a type"
kept_marker "60-OPS.yaml is of a type"

echo "case: words: a Pulumi.yaml that is no YAML, its fault quoting the file"
daemons_words words-yaml
printf 'name: *%s\n' "$marker" >"${case_root}/files/legion/Pulumi.yaml"
run_gate -- "$commit"
prints_no_run "the file held, which YAML's error quotes"
check "names the file and where YAML stopped" "$(grep -q "^UNKNOWN: cannot read ${deployment} legion/Pulumi.yaml: it is not YAML (line [0-9]*, column [0-9]*)$" "$out" && echo true || echo false)"

echo "case: words: a Pulumi.yaml whose name Pulumi refuses"
daemons_words words-project-name
printf 'name: "%s!"\nruntime: go\n' "$marker" >"${case_root}/files/legion/Pulumi.yaml"
run_gate -- "$commit"
prints_no_run "the file named the project"

echo "case: words: project fields the gate does not take"
new_case words-fields
legion_stack "    - key: LINUX
      replicas: 1
      daemonImage: ${worker_a}
    - key: BET
      replicas: \"${marker}\"
      daemonImage: ${worker_a}
    - key: OSSV
      daemon: \"${marker}\"
      daemonImage: ${worker_a}
    - key: ODD
      runtime: ${marker}
      daemonImage: ${worker_a}
    - key: SIZE
      daemonImage: \"${marker} ${worker_a}\"
    - key: \"${marker}\"
      daemonImage: ${worker_a}"
single "$worker_a" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- "$commit"
prints_no_run "the files held as a field"
check "control: the project whose fields hold is still read" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})")"

echo "case: words: a project file whose key is not its name's"
new_case words-file-key
legion_settings
fleet_files
project_file 20-LINUX "key: ${marker}
replicas: 1
daemonImage: ${worker_a}"
run_gate -- "$commit"
prints_no_run "the file declared as its key"

echo "case: words: a listener pin that is no image reference"
listeners_words words-pin
printf 'config:\n  %s:envoyImage: "%s ghcr.io/x"\n' "$cluster_project" "$marker" >"${case_root}/files/envoy-cluster/Pulumi.prod.yaml"
run_gate -- --listeners "$commit"
prints_no_run "the stack set as its pin"
kept_marker "UNKNOWN: envoy-cluster: "

echo "case: words: an image whose media type, platform, source or revision is no reading the gate takes"
listeners_words words-labels
printf 'application/vnd.%s\n' "$marker" >"$(image_dir "$cluster_pin")/mediatype"
single "$dispatch_listener_pin" "https://github.com/x ${marker}" "$ahead_rev"
index "$server_pin" "$legion" "$marker"
run_gate -- --listeners "$commit"
prints_no_run "the registry answered as a label or a media type"

echo "case: words: an index listing a platform that is no <os>/<arch>"
listeners_words words-platform
printf 'linux/%s ' "$marker" >"$(image_dir "$cluster_pin")/platforms"
run_gate -- --listeners "$commit"
prints_no_run "the registry listed as a platform"

echo "case: words: a member of the archive named with the marker"
listeners_words words-member
ln -s missing.txt "${case_root}/files/envoy/${marker}.go"
run_gate -- --listeners "$commit"
prints_no_run "the archive named a member"
check "names the member by its place and kind" \
  "$(has_kept_line "UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: member @N@ of the archive, a symbolic link at the path of a Go file or a module file, links to no file of this repository; the reader's words with the names it read are kept in KEPT")"
kept_marker "UNKNOWN: envoy: cannot read the Go files"

echo "case: words: a module file that names a directory outside the repository"
listeners_words words-replace
printf 'module example.org/deployment/envoy\n\nreplace example.org/far => ../../%s\n' "$marker" >"${case_root}/files/envoy/go.mod"
run_gate -- --listeners "$commit"
prints_no_run "envoy/go.mod named as a directory"
kept_marker "UNKNOWN: envoy: cannot read the Go files"

echo "case: words: a marketplace entry the gate does not take"
new_case words-plugin
clients_background
words_plugin "{\"source\": \"npm\", \"package\": \"@sjawhar/claude-envoy\", \"version\": \">=1.0.0\", \"registry\": \"http://${marker}.example\"}"
run_gate -- --clients "$commit"
prints_no_run "the marketplace served as a registry"
kept_marker "from a registry that"

echo "case: words: a marketplace version npm cannot parse, which npm's error quotes"
new_case words-spec
clients_background
words_plugin "{\"source\": \"npm\", \"package\": \"@sjawhar/claude-envoy\", \"version\": \"${marker}@@ x\"}"
run_gate -- --clients "$commit"
prints_no_run "the marketplace served as a version"
kept_marker "npm cannot resolve"

echo "case: words: a rendered legion.yaml whose plugins name no version"
new_case words-golden
clients_open
printf 'runtime:\n  kubernetes:\n    plugins: "%s"\n' "$marker" >"${case_root}/files/legion/testdata/linux-legion.yaml"
run_gate -- --clients "$commit"
prints_no_run "the rendered legion.yaml set as its plugins"

echo "case: words: an image whose manifest and legion version print the marker"
new_case words-image
clients_open
printf '{"name": "%s"}\n' "$marker" >"$(image_dir "$lean_image")/fs/opt/legion/claude-envoy/package.json"
printf 'legion %s\n' "$marker" >"$(image_dir "$infra_image")/legion-version"
plugins_line "$worker_a" "plugins @sjawhar/pi-legion 1.0.0 ${marker}"
run_gate -- --clients "$commit"
prints_no_run "the images printed"

# --- the filter: a reading that holds to its grammar ---------------------------------------------

readonly token="ghs_${marker}_0123456789abcdefXYZ"

echo "case: words: the filter: an image pin, a source and a hub file holding gh's token"
new_case words-filter
legion_stack "$words_fleet"
listener_stacks "ghcr.io/${token}/envoy@sha256:e001" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
envoy_program none
# The Dispatch server's pin is one the registry does not hold, so its line names it: the control.
rm -rf "$(image_dir "$server_pin")"
printf 'package main\n\nvar hub = envoygcp.NewNatsHub\n' >"${case_root}/files/envoy/${token}.go"
index "$cluster_pin" "https://github.com/${token}/legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate "FAKE_GH_TOKEN=${token}" -- --listeners "$commit"
prints_no_run "gh's token in a pin, a source and a hub file's path"
check "prints the hub's file with the token's run written as [redacted]" \
  "$(grep -q '^VM HUB: envoy/\[redacted\]\.go:3 at ' "$out" && echo true || echo false)"
check "prints the pin with the token's run written as [redacted]" \
  "$(grep -qF "ghcr.io/[redacted]/envoy@sha256:e001" "$out" && echo true || echo false)"
check "prints the source with the token's run written as [redacted]" \
  "$(has_line "GATE SHUT: envoy-cluster runs an image built from https://github.com/[redacted]/legion")"
check "control: a pin with no run of the token prints whole" \
  "$(has_kept_line "UNKNOWN: dispatch-server: cannot read ${server_pin}: docker failed; its own words are kept in KEPT")"
check "says nothing of a missing token" "$(lacks "no line below is filtered" "$err")"
run_gate "FAKE_GH_TOKEN=" -- --listeners "$commit"
check "twin: with no token to filter by, the pin prints the token" "$(holds_run "$(printed)" "$token" && echo true || echo false)"
check "twin: and the gate says it filters for none" "$(grep -qF "no line below is filtered for one" "$err" && echo true || echo false)"
run_gate "FAKE_GH_TOKEN=" "GH_TOKEN=${token}" -- --listeners "$commit"
check "a token in GH_TOKEN is filtered as gh's own" "$(holds_run "$(printed)" "$token" && echo false || echo true)"
