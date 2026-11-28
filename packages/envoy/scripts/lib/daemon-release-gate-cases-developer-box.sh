# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-developer-box.sh
#
# The --clients scenarios of the developer box. daemon-release-gate.test.sh sources this file after
# the harness and the client fixtures (daemon-release-gate-fixtures-clients.sh), whose box_case and
# claude_install it uses; it is not run on its own.
#
# On the developer box --clients reads the dotfiles' pins and what is installed from them: the
# pi-envoy pin of omp/plugins/package.json and the copy installed beside it, each claude-envoy
# install the dotfiles' Claude Code plugin record holds, by the manifest at its installPath, and the
# claude-envoy version the marketplace legion-plugins serves to .claude/settings.json; each version
# by the commit of its release tag. A client installed where the dotfiles' pins say nothing about it
# (~/.omp/plugins or a profile's tree, ~/.claude, another CLAUDE_CONFIG_DIR) is UNKNOWN, and so is
# anything the gate cannot tell is there or not. Each run's HOME is the case's own (run_bare).
# shellcheck disable=SC2154 # every variable read here is set by the harness or the test that sources this file

echo "case: --clients, the developer box at or after the commit"
box_case box-open
run_gate -- --clients "$commit"
check "exits 0" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"
check "reads the dotfiles' pi-envoy pin by its release tag" \
  "$(has_line "PASS: dotfiles omp/plugins/package.json runs ${pi_envoy_ahead}")"
check "reads the claude-envoy version the marketplace serves, the one npm takes" \
  "$(has_line "PASS: dotfiles .claude/settings.json runs ${claude_served}")"
check "reads the marketplace the settings name" \
  "$(grep -qF "repos/example-org/plugins/contents/.claude-plugin/marketplace.json" "$calls" && echo true || echo false)"
check "parses the marketplace's spec with npm's own parser" \
  "$(grep -qxF "npm exec --offline --yes=false -c \"\$npm_node_execpath\" -e \"\$GATE_SPEC_TYPE\"" "$calls" && echo true || echo false)"
check "lets npm pick the version for the range the marketplace names, writing nothing and running no script" \
  "$(grep -qxF "npm pack @sjawhar/claude-envoy@>=1.0.0 --dry-run --ignore-scripts --json" "$calls" && echo true || echo false)"
check "runs npm, each time, outside the gate's working directory and any project's configuration" \
  "$(npm_cwds=$(sed -n 's/^npm-cwd //p' "$calls"); [ "$(grep -c . <<<"$npm_cwds")" -eq 2 ] &&
    while read -r npm_cwd; do [ "$npm_cwd" != "$PWD" ] && [ ! -e "$npm_cwd/package.json" ] && [ ! -e "$npm_cwd/.npmrc" ] || exit 1; done <<<"$npm_cwds" &&
    echo true || echo false)"
check "resolves each version through its release tag" \
  "$(grep -qF "repos/cybermonklabs/legion/commits/refs/tags/pi-envoy-v17.40.0" "$calls" && grep -qF "repos/cybermonklabs/legion/commits/refs/tags/claude-envoy-v4.0.0" "$calls" && echo true || echo false)"
check "judges the one version npm takes, once" \
  "$(is "$(grep -c '^PASS: dotfiles .claude/settings.json runs claude-envoy ' "$out")" 1)"

echo "case: --clients, the developer box's pi-envoy pin behind the commit"
box_case box-pin-behind
dotfiles 17.39.0
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts naming the pin, its version and its tag" \
  "$(has_line "GATE SHUT: dotfiles omp/plugins/package.json runs pi-envoy 17.39.0 from pi-envoy-v17.39.0 at ${envoy_behind_rev} (behind ${commit} by 300)")"
check "the verdict is GATE SHUT" "$(last_line_is "GATE SHUT")"

echo "case: --clients --dotfiles <dir> reads the dotfiles it names"
box_case box-dotfiles-dir
dotfiles 17.39.0
mv "${case_root}/home/.dotfiles" "${case_root}/box-dotfiles"
ln -sfn "${case_root}/box-dotfiles/omp/plugins" "${case_root}/home/.omp/plugins"
run_gate -- --clients --dotfiles "${case_root}/box-dotfiles" "$commit"
check "exits 1, on the pin of the dotfiles it names" "$(is "$status" 1)"
check "shuts naming that pin" \
  "$(has_line "GATE SHUT: dotfiles omp/plugins/package.json runs pi-envoy 17.39.0 from pi-envoy-v17.39.0 at ${envoy_behind_rev} (behind ${commit} by 300)")"
check "reads the claude-envoy its settings enable" "$(has_line "PASS: dotfiles .claude/settings.json runs ${claude_served}")"
run_gate -- --clients --dotfiles="${case_root}/box-dotfiles" "$commit"
check "--dotfiles=<dir> is read as --dotfiles <dir>" "$(is "$status" 1)"

echo "case: --clients, a settings.json that enables no claude-envoy"
box_case box-claude-off
dotfiles 17.40.0 false
run_gate -- --clients "$commit"
check "exits 0" "$(is "$status" 0)"
check "skips it by name" "$(has_line "SKIPPED: dotfiles .claude/settings.json enables no claude-envoy@legion-plugins")"
check "asks npm nothing" "$(lacks "npm " "$calls")"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --clients, a settings.json that enables claude-envoy from another marketplace"
box_case box-claude-other-marketplace
python3 - "${case_root}/home/.dotfiles/.claude/settings.json" <<'PY'
import json
import sys

with open(sys.argv[1]) as handle:
    settings = json.load(handle)
settings["enabledPlugins"]["claude-envoy@example-plugins"] = True
with open(sys.argv[1], "w") as handle:
    json.dump(settings, handle)
PY
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the plugin" \
  "$(has_line "UNKNOWN: dotfiles .claude/settings.json: it enables claude-envoy@example-plugins, from a marketplace this gate does not read")"

echo "case: --clients, a marketplace that is no GitHub repository"
box_case box-marketplace-not-github
printf '{"extraKnownMarketplaces": {"legion-plugins": {"source": {"source": "directory", "path": "/srv/plugins"}}}, "enabledPlugins": {"claude-envoy@legion-plugins": true}}\n' \
  >"${case_root}/home/.dotfiles/.claude/settings.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the marketplace's source" \
  "$(has_line 'UNKNOWN: dotfiles .claude/settings.json: the marketplace legion-plugins it names is {"source":"directory","path":"/srv/plugins"}, which is no GitHub repository this gate reads')"

# A marketplace entry jq cannot index (a string where Claude Code takes an object) is refused by
# name, never a gate that stops with no verdict.
echo "case: --clients, a marketplace legion-plugins that is no object"
box_case box-marketplace-string
printf '{"extraKnownMarketplaces": {"legion-plugins": "sjawhar/legion"}, "enabledPlugins": {"claude-envoy@legion-plugins": true}}\n' \
  >"${case_root}/home/.dotfiles/.claude/settings.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the marketplace and jq's answer" \
  "$(grep -q '^UNKNOWN: dotfiles \.claude/settings\.json: cannot read the marketplace legion-plugins it names: jq: error' "$out" && echo true || echo false)"
check "asks npm nothing" "$(lacks "npm " "$calls")"
check "the verdict is the last line" "$(last_line_is "UNKNOWN")"

echo "case: --clients, a marketplace at a ref that holds a URL's reserved characters"
box_case box-marketplace-ref
printf '{"extraKnownMarketplaces": {"legion-plugins": {"source": {"source": "github", "repo": "example-org/plugins", "ref": "v1&path=x #y"}}}, "enabledPlugins": {"claude-envoy@legion-plugins": true}}\n' \
  >"${case_root}/home/.dotfiles/.claude/settings.json"
run_gate -- --clients "$commit"
check "exits 0" "$(is "$status" 0)"
check "reads the marketplace at that ref, encoded as one query value" \
  "$(grep -qF "repos/example-org/plugins/contents/.claude-plugin/marketplace.json?ref=v1%26path%3Dx%20%23y" "$calls" && echo true || echo false)"

echo "case: --clients, the dotfiles' installed pi-envoy behind its pin"
box_case box-installed-behind
printf '{"name": "@sjawhar/pi-envoy", "version": "17.39.0"}\n' \
  >"${case_root}/home/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy/package.json"
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts naming the installed version and the pin" \
  "$(has_line "GATE SHUT: dotfiles omp/plugins installed @sjawhar/pi-envoy runs pi-envoy 17.39.0 from pi-envoy-v17.39.0 at ${envoy_behind_rev} (behind ${commit} by 300); the pin names 17.40.0")"
check "still passes the pin itself" "$(has_line "PASS: dotfiles omp/plugins/package.json runs ${pi_envoy_ahead}")"

echo "case: --clients, the dotfiles' pi-envoy a link to a checkout outside them"
box_case box-linked-checkout
mkdir -p "${case_root}/outside/pi-envoy"
printf '{"name": "@sjawhar/pi-envoy", "version": "17.40.0"}\n' >"${case_root}/outside/pi-envoy/package.json"
rm -r "${case_root}/home/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy"
ln -s "${case_root}/outside/pi-envoy" "${case_root}/home/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming where the link goes" \
  "$(has_line "UNKNOWN: dotfiles omp/plugins/package.json: @sjawhar/pi-envoy is installed outside ${case_root}/home/.dotfiles, in ${case_root}/home/.dotfiles/omp/plugins, a link to ${case_root}/outside/pi-envoy")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, pi-envoy installed in a plugin tree outside the dotfiles"
box_case box-outside-tree
mkdir -p "${case_root}/home/.omp/profiles/work/plugins/node_modules/@sjawhar/pi-envoy"
printf '{"name": "@sjawhar/pi-envoy", "version": "16.0.0"}\n' \
  >"${case_root}/home/.omp/profiles/work/plugins/node_modules/@sjawhar/pi-envoy/package.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the tree" \
  "$(has_line "UNKNOWN: dotfiles omp/plugins/package.json: @sjawhar/pi-envoy is installed outside ${case_root}/home/.dotfiles, in ${case_root}/home/.omp/profiles/work/plugins")"

echo "case: --clients, a profile's plugin tree whose pi-envoy links to the dotfiles' copy"
box_case box-profile-link
mkdir -p "${case_root}/home/.omp/profiles/work/plugins/node_modules/@sjawhar"
ln -s "${case_root}/home/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy" \
  "${case_root}/home/.omp/profiles/work/plugins/node_modules/@sjawhar/pi-envoy"
run_gate -- --clients "$commit"
check "exits 0, the copy being the dotfiles' own" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --clients, claude-envoy installed outside the dotfiles, in ~/.claude"
box_case box-claude-outside
mkdir -p "${case_root}/home/.claude/plugins"
printf '{"version": 2, "plugins": {"claude-envoy@legion-plugins": [{"version": "1.0.0"}]}}\n' \
  >"${case_root}/home/.claude/plugins/installed_plugins.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming where it is installed" \
  "$(has_line "UNKNOWN: dotfiles .claude/settings.json: claude-envoy is installed outside ${case_root}/home/.dotfiles, in ${case_root}/home/.claude/plugins")"

echo "case: --clients, claude-envoy installed in the CLAUDE_CONFIG_DIR of another configuration"
box_case box-claude-config-dir
mkdir -p "${case_root}/other-claude/plugins"
printf '{"version": 2, "plugins": {"claude-envoy@legion-plugins": [{"version": "4.0.0"}]}}\n' \
  >"${case_root}/other-claude/plugins/installed_plugins.json"
run_gate CLAUDE_CONFIG_DIR="${case_root}/other-claude" -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming where it is installed" \
  "$(has_line "UNKNOWN: dotfiles .claude/settings.json: claude-envoy is installed outside ${case_root}/home/.dotfiles, in ${case_root}/other-claude/plugins")"

for shape in malformed directory; do
  echo "case: --clients, a Claude Code plugin record that is ${shape}"
  box_case "box-claude-record-${shape}"
  mkdir -p "${case_root}/home/.claude/plugins"
  if [ "$shape" = malformed ]; then
    printf '{"version": 2, "plugins": {"claude-envoy@legion-plugins": [' >"${case_root}/home/.claude/plugins/installed_plugins.json"
  else
    mkdir "${case_root}/home/.claude/plugins/installed_plugins.json"
  fi
  run_gate -- --clients "$commit"
  check "exits 3" "$(is "$status" 3)"
  check "refuses UNKNOWN naming the record" \
    "$(grep -qF "UNKNOWN: dotfiles .claude/settings.json: ${case_root}/home/.claude/plugins/installed_plugins.json cannot be read as JSON: " "$out" && echo true || echo false)"
  check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"
done

echo "case: --clients, claude-envoy installed in the dotfiles' Claude Code configuration, behind"
box_case box-claude-installed-behind
claude_install 3.3.0 3.3.0
run_gate CLAUDE_CONFIG_DIR="${case_root}/home/.dotfiles/.claude" -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts naming the installed copy" \
  "$(has_line "GATE SHUT: dotfiles .claude/plugins installed claude-envoy runs claude-envoy 3.3.0 from claude-envoy-v3.3.0 at ${behind_rev} (behind ${commit} by 12)")"
check "still reads the version the marketplace serves" "$(has_line "PASS: dotfiles .claude/settings.json runs ${claude_served}")"

echo "case: --clients, a Claude Code install whose copy is behind what its record says"
box_case box-claude-copy-behind
claude_install 4.0.0 3.3.0
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "judges the copy, naming the record's word" \
  "$(has_line "GATE SHUT: dotfiles .claude/plugins installed claude-envoy runs claude-envoy 3.3.0 from claude-envoy-v3.3.0 at ${behind_rev} (behind ${commit} by 12); the plugin record says 4.0.0")"
