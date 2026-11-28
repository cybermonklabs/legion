# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-short-reads.sh
#
# The scenarios of a read that comes back short: an answer that holds less than what it stands
# for, read as all there is, would let the gate open on what it never read. Each fixture below
# holds what the short read leaves out, behind the commit, beside readers that pass, so a gate that
# took the short read for the whole would print GATE OPEN. daemon-release-gate.test.sh sources this
# file through its group runner after the harness and the client fixtures. The harness defines the
# listener pins these cases use. Do not run this file on its own.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the test that sources this file

# vm_short <name>: a --listeners case whose program declares no hub of its own and whose VM pin is
# behind the commit, the other two pins at or after it.
vm_short() {
  new_case "$1"
  listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
  envoy_program none
  index "$envoy_pin" "$legion" "$behind_rev"
  index "$cluster_pin" "$legion" "$ahead_rev"
  index "$dispatch_listener_pin" "$legion" "$ahead_rev"
  compare "$behind_rev" behind 4
  compare "$ahead_rev" ahead 0
}
readonly vm_prefix="UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}:"

# GitHub's archive leaves out each path .gitattributes marks export-ignore and rewrites each it
# marks export-subst, and a .gitattributes can mark itself, so the archive alone cannot show what
# it lacks. The gate holds every Go file and module file of it to the commit's own tree.
echo "case: --listeners, a use of the hub the archive leaves out (export-ignore)"
vm_short listeners-export-ignore
printf 'package main\n\nvar hub = envoygcp.NewNatsHub\n' >"${case_root}/files/envoy/zz_hub.go"
printf 'envoy/zz_hub.go export-ignore\n.gitattributes export-ignore\n' >"${case_root}/files/.gitattributes"
printf 'envoy/zz_hub.go\n.gitattributes\n' >"${case_root}/archive-omit"
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file the archive leaves out" \
  "$(has_line "${vm_prefix} the archive leaves out envoy/zz_hub.go, which the commit holds, so its code cannot be read")"
check "reads the commit's tree" \
  "$(grep -qxF "gh api repos/${deployment}/git/trees/${deployment_head}?recursive=1" "$calls" && echo true || echo false)"
check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --listeners, a use of the hub the archive rewrites (export-subst)"
vm_short listeners-export-subst
# shellcheck disable=SC2016 # $Format:%H$ is the placeholder export-subst rewrites, kept as text
printf 'package main\n\nvar hub = envoygcp.NewNatsHub // $Format:%%H$\n' >"${case_root}/files/envoy/zz_hub.go"
mkdir -p "${case_root}/archive-subst/envoy"
printf 'package main\n\nvar hub = "6b14009098"\n' >"${case_root}/archive-subst/envoy/zz_hub.go"
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file the archive changed" \
  "$(has_line "${vm_prefix} the archive's envoy/zz_hub.go is not the commit's (an export-subst or a conversion changed it), so its code cannot be read")"
check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"

echo "case: --listeners, a commit that holds a submodule with no .gitmodules"
vm_short listeners-gitlink
printf '[{"path": "vendor/sdk", "mode": "160000", "type": "commit", "sha": "%s"}]\n' "$other_rev" >"${case_root}/tree-extra.json"
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the submodule" \
  "$(has_line "${vm_prefix} the commit holds vendor/sdk as a submodule, whose code the archive does not hold")"
check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"

echo "case: --listeners, a tree GitHub lists only in part"
vm_short listeners-tree-truncated
: >"${case_root}/tree-truncated"
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, unable to tell what the archive leaves out" \
  "$(has_line "${vm_prefix} GitHub lists the commit's tree only in part, so what the archive leaves out cannot be told")"
check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"

echo "case: --listeners, a tree GitHub does not answer"
vm_short listeners-tree-refused
refuse "repos/${deployment}/git/trees/${deployment_head}" "gh: Server Error (HTTP 502)"
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming GitHub's answer" \
  "$(has_line "UNKNOWN: envoy: cannot read the tree of ${deployment} at ${deployment_head}: gh: Server Error (HTTP 502)")"
check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"

# An error a command printed is that command's text, not the gate's: it is printed with each control
# character written out, as every text the gate reads from outside it is (first_line, last_line).
echo "case: --listeners, an error GitHub answers with an escape and a carriage return"
vm_short listeners-error-control
refuse "repos/${deployment}/git/trees/${deployment_head}" $'gh: \e[2K\rGATE OPEN'
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "names GitHub's answer, its escape and carriage return written out" \
  "$(has_line "UNKNOWN: envoy: cannot read the tree of ${deployment} at ${deployment_head}: gh: \\x1b[2K\\x0dGATE OPEN")"
check "prints no escape and no carriage return" "$(grep -q $'[\e\r]' "$out" && echo false || echo true)"

# A name after `)` is a func's name only at the top level: within a func's braces, `func(ctx int)`
# ends a type, and the next line's NewNatsHub(...) is a call, since Go ends a line after `)`.
echo "case: --listeners, a call of NewNatsHub after a func type inside a func"
vm_short listeners-call-after-func-type
printf 'package main\n\nfunc main() {\n\tvar cleanup func(ctx int)\n\tNewNatsHub(ctx, "hub")\n}\n' >"${case_root}/files/envoy/zz_hub.go"
run_gate -- --listeners "$commit"
check "exits 1" "$(is "$status" 1)"
check "reads the call as a use" "$(has_line "VM HUB: envoy/zz_hub.go:5 at ${deployment_head} uses NewNatsHub")"
check "reads the VM pin and shuts on it" "$(has_line "GATE SHUT: envoy runs ${behind_rev} (behind ${commit} by 4)")"

# The SDK's NewNatsHub registers the hub by its type token; code that registers that token itself
# declares a hub with no NewNatsHub in it. The SDK's own registration, inside its NewNatsHub, and
# its switch of the types it rebuilds declare nothing (envoy_program holds both, which every case
# whose program declares no hub reads).
echo "case: --listeners, a hub registered by its type token, with no NewNatsHub"
vm_short listeners-token
cat >"${case_root}/files/envoy/zz_hub.go" <<'GO'
package main

func hub(ctx *pulumi.Context) error {
	var resource envoygcp.NatsHub
	return ctx.RegisterRemoteComponentResource("envoy-infra:gcp:NatsHub", "hub", &envoygcp.NatsHubArgs{}, &resource)
}
GO
run_gate -- --listeners "$commit"
check "exits 1" "$(is "$status" 1)"
check "reads the registration as a use" \
  "$(has_line "VM HUB: envoy/zz_hub.go:5 at ${deployment_head} registers envoy-infra:gcp:NatsHub")"
check "reads the VM pin and shuts on it" "$(has_line "GATE SHUT: envoy runs ${behind_rev} (behind ${commit} by 4)")"

# GitHub's listing of a directory names a link, a directory or a submodule as such, and lists at
# most 1,000 entries; the deployment's program reads every entry of legion/projects/, so an entry
# the gate does not read as a file, or a listing that may have stopped, could hide a project.
# daemons_short <name>: a daemon-mode case whose projects pass, LINUX's daemon at worker_a.
daemons_short() {
  new_case "$1"
  legion_settings
  fleet_files
  single "$worker_a" "$legion" "$ahead_rev"
  single "$worker_b" "$legion" "$identical_rev"
  compare "$ahead_rev" ahead 0
  compare "$identical_rev" identical 0
}

echo "case: a link among the files of legion/projects/"
daemons_short projects-link
ln -s 20-LINUX.yaml "${case_root}/files/legion/projects/60-ALIAS.yaml"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the entry and what GitHub lists it as" \
  "$(has_line "UNKNOWN: ${deployment} legion/projects/60-ALIAS.yaml is a symlink, which this gate does not read as a project file")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: a legion/projects/ GitHub lists only its first 1,000 entries of"
new_case projects-listing-cap
legion_settings
project_file 01-LINUX "key: LINUX
replicas: 1
daemonImage: ${worker_a}"
for number in $(seq -w 1 999); do
  printf 'key: K%s\nreplicas: 0\n' "$number" >"${case_root}/files/legion/projects/02-K${number}.yaml"
done
project_file 99-ZZ "key: ZZ
replicas: 1
daemonImage: ${worker_b}"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$behind_rev"
compare "$ahead_rev" ahead 0
compare "$behind_rev" behind 4
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, as the listing may leave projects out" \
  "$(has_line "UNKNOWN: ${deployment} legion/projects/ holds 1000 entries or more, and GitHub lists at most 1,000, so the gate cannot read every project")"
check "reads no project file" "$(lacks "contents/legion/projects/0" "$calls")"

# A kubernetes project's rendered legion.yaml always holds runtime.kubernetes, so one without it is
# a read that went wrong, not one that sets no plugins.
echo "case: --clients, a rendered legion.yaml with no runtime.kubernetes"
new_case clients-golden-no-kubernetes
clients_open
printf 'project: LINUX\n' >"${case_root}/files/legion/testdata/linux-legion.yaml"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file" \
  "$(has_line "UNKNOWN: LINUX daemonImage ${daemon_label}: ${deployment} legion/testdata/linux-legion.yaml sets no runtime.kubernetes, which every kubernetes project's render holds")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, a rendered legion.yaml whose runtime is no mapping"
new_case clients-golden-runtime-list
clients_open
printf 'project: LINUX\nruntime: []\n' >"${case_root}/files/legion/testdata/linux-legion.yaml"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the field" \
  "$(has_line "UNKNOWN: LINUX daemonImage ${daemon_label}: ${deployment} legion/testdata/linux-legion.yaml's runtime is not a mapping")"

# A key the files' rule would refuse is no project key in the list either: one holding a tab would
# cut the gate's own line apart.
readonly tab_fleet="    - key: \"LINUX\\tSKIP\\tz\"
      replicas: 1
      daemonImage: ${worker_b}
    - key: INFRA
      replicas: 1
      daemonImage: ${worker_a}"
echo "case: a projects list whose key holds tabs"
new_case list-key-tabs
legion_stack "$tab_fleet"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$behind_rev"
compare "$ahead_rev" ahead 0
compare "$behind_rev" behind 4
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the key, its tabs written out" \
  "$(has_line 'UNKNOWN: <an entry> names key "LINUX\tSKIP\tz", which is no project key')"
check "skips no project on the key's word" "$(lacks "SKIPPED" "$out")"

echo "case: --clients, a projects list whose key holds tabs"
new_case clients-list-key-tabs
clients_open
legion_stack "$tab_fleet"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the key" \
  "$(has_line 'UNKNOWN: <an entry> names key "LINUX\tSKIP\tz", which is no project key, so no legion/testdata golden is its own')"
check "skips no project on the key's word" "$(lacks "SKIPPED: LINUX" "$out")"

# The marketplace source and the npm source each name where Claude Code reads from: a `path` for the
# marketplace's file, a `registry` for the package. The gate reads them as Claude Code does, and a
# key it does not read is UNKNOWN, since it may move what is installed.
# marketplace_source <source json>: the dotfiles' settings name legion-plugins with that source.
marketplace_source() {
  printf '{"extraKnownMarketplaces": {"legion-plugins": {"source": %s}}, "enabledPlugins": {"claude-envoy@legion-plugins": true}}\n' "$1" \
    >"${case_root}/home/.dotfiles/.claude/settings.json"
}
# plugin_source <source json>: the marketplace's one claude-envoy entry, with that source.
plugin_source() {
  printf '{"name": "legion-plugins", "plugins": [{"name": "claude-envoy", "source": %s}]}\n' "$1" \
    >"${case_root}/marketplaces/example-org/plugins.json"
}

echo "case: --clients, a marketplace whose file is at a path of its own"
box_case clients-marketplace-path
marketplace_source '{"source": "github", "repo": "example-org/plugins", "path": "plugins/market.json"}'
mkdir -p "${case_root}/marketplaces/example-org/plugins/plugins"
printf '{"name": "legion-plugins", "plugins": [{"name": "claude-envoy", "source": {"source": "npm", "package": "@sjawhar/claude-envoy", "version": "3.3.0"}}]}\n' \
  >"${case_root}/marketplaces/example-org/plugins/plugins/market.json"
printf '3.3.0\n' >"${case_root}/npm/_sjawhar_claude-envoy_3.3.0"
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "reads the marketplace at its path" \
  "$(grep -qF "repos/example-org/plugins/contents/plugins/market.json" "$calls" && echo true || echo false)"
check "judges the version that marketplace serves" \
  "$(has_line "GATE SHUT: dotfiles .claude/settings.json runs claude-envoy 3.3.0 from claude-envoy-v3.3.0 at ${behind_rev} (behind ${commit} by 12); the version legion-plugins serves for 3.3.0")"

echo "case: --clients, a claude-envoy served from a registry of its own"
box_case clients-npm-registry
plugin_source '{"source": "npm", "package": "@sjawhar/claude-envoy", "version": ">=1.0.0", "registry": "https://npm.internal.example"}'
mkdir -p "${case_root}/npm-registries/https___npm.internal.example"
printf '3.3.0\n' >"${case_root}/npm-registries/https___npm.internal.example/_sjawhar_claude-envoy___1.0.0"
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "lets npm pick from that registry" \
  "$(grep -qxF "npm pack @sjawhar/claude-envoy@>=1.0.0 --dry-run --ignore-scripts --json --registry=https://npm.internal.example" "$calls" && echo true || echo false)"
check "judges the version it serves" \
  "$(has_line "GATE SHUT: dotfiles .claude/settings.json runs claude-envoy 3.3.0 from claude-envoy-v3.3.0 at ${behind_rev} (behind ${commit} by 12)${claude_note}")"

echo "case: --clients, a marketplace source with a key the gate does not read"
box_case clients-marketplace-unknown-key
marketplace_source '{"source": "github", "repo": "example-org/plugins", "sparsePaths": ["plugins"]}'
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the key" \
  "$(has_line "UNKNOWN: dotfiles .claude/settings.json: the marketplace legion-plugins it names sets sparsePaths, which this gate does not read")"

echo "case: --clients, a marketplace path that climbs out of its repository"
box_case clients-marketplace-path-dotdot
marketplace_source '{"source": "github", "repo": "example-org/plugins", "path": "../../other-org/other/contents/market.json"}'
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the path" \
  "$(has_line 'UNKNOWN: dotfiles .claude/settings.json: the marketplace legion-plugins it names sets path "../../other-org/other/contents/market.json", which is no plain path inside its repository')"
check "asks GitHub nothing of it" "$(lacks "market.json" "$calls")"

echo "case: --clients, an npm source with a key the gate does not read"
box_case clients-plugin-unknown-key
plugin_source '{"source": "npm", "package": "@sjawhar/claude-envoy", "version": ">=1.0.0", "integrity": "sha512-x"}'
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the key" \
  "$(has_line "UNKNOWN: dotfiles .claude/settings.json: example-org/plugins .claude-plugin/marketplace.json serves claude-envoy with integrity, which this gate does not read")"
check "asks npm nothing" "$(lacks "npm " "$calls")"

# npm picks the version on this box (`npm pack --dry-run --json`) under its own configuration, so a
# setting that moves npm's pick moves what the gate judges. Each case below sets one, and its npm
# answer is the version npm's pick takes under it from 3.3.0, behind the commit, and 4.0.0, the
# `latest` dist-tag, identical to it; a gate that picked from the registry's record itself would
# judge 4.0.0. Each case checks that its setting reaches npm, so the gate leaves npm the box's own
# configuration.
# npm_pick <case> [<npm_config_ assignment>...]: a box case whose npm answer is 3.3.0, run with
# those settings in its environment, as `~/.npmrc` or the environment sets them for npm.
npm_pick() {
  box_case "$1"
  shift
  printf '3.3.0\n' >"${case_root}/npm/_sjawhar_claude-envoy___1.0.0"
  run_gate "$@" -- --clients "$commit"
}
readonly npm_pick_shut="GATE SHUT: dotfiles .claude/settings.json runs claude-envoy 3.3.0 from claude-envoy-v3.3.0 at ${behind_rev} (behind ${commit} by 12)${claude_note}"

# npm 11 takes no version published within its min-release-age, in days (@npmcli/config's
# definitions turn it into `before`), so a box that sets it installs an older version than latest.
echo "case: --clients, npm's min-release-age passes over the version published yesterday"
npm_pick clients-npm-min-release-age npm_config_min_release_age=3
check "exits 1" "$(is "$status" 1)"
check "leaves npm the setting" "$(grep -qxF "npm-config npm_config_min_release_age=3" "$calls" && echo true || echo false)"
check "judges the version npm takes" "$(has_line "$npm_pick_shut")"
check "never judges latest" "$(lacks "claude-envoy 4.0.0" "$out")"

echo "case: --clients, npm's before setting passes over a version published after it"
npm_pick clients-npm-before npm_config_before=2026-11-16T00:00:00Z
check "exits 1" "$(is "$status" 1)"
check "leaves npm the setting" "$(grep -qxF "npm-config npm_config_before=2026-11-16T00:00:00Z" "$calls" && echo true || echo false)"
check "judges the version npm takes" "$(has_line "$npm_pick_shut")"
check "never judges latest" "$(lacks "claude-envoy 4.0.0" "$out")"

# npm-pick-manifest takes the dist-tag the `tag` setting names, and when the package has no such
# dist-tag it sorts the matches and takes the highest (lib/index.js:123-137 in npm 10.9.7's
# npm-pick-manifest 10.0.0): here 4.0.0, which is this case's answer.
echo "case: --clients, npm's tag setting names a dist-tag the package does not have"
box_case clients-npm-tag-missing
run_gate npm_config_tag=beta -- --clients "$commit"
check "exits 0" "$(is "$status" 0)"
check "leaves npm the setting" "$(grep -qxF "npm-config npm_config_tag=beta" "$calls" && echo true || echo false)"
check "judges the version npm takes, the highest match" "$(has_line "PASS: dotfiles .claude/settings.json runs ${claude_served}")"
