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

for shape in outside no-path no-manifest; do
  echo "case: --clients, a Claude Code install the gate cannot hold to the dotfiles (${shape})"
  box_case "box-claude-install-${shape}"
  record="${case_root}/home/.dotfiles/.claude/plugins/installed_plugins.json"
  case "$shape" in
    outside)
      mkdir -p "${case_root}/outside/claude-envoy"
      printf '{"name": "@sjawhar/claude-envoy", "version": "4.0.0"}\n' >"${case_root}/outside/claude-envoy/package.json"
      claude_install 4.0.0 4.0.0 "${case_root}/outside/claude-envoy"
      want="${record} records an install of claude-envoy@legion-plugins at ${case_root}/outside/claude-envoy, outside $(readlink -f "${case_root}/home/.dotfiles/.claude")"
      ;;
    no-path)
      mkdir -p "$(dirname "$record")"
      printf '{"version": 2, "plugins": {"claude-envoy@legion-plugins": [{"version": "4.0.0"}]}}\n' >"$record"
      want="${record} records an install of claude-envoy@legion-plugins with no installPath"
      ;;
    no-manifest)
      claude_install 4.0.0 4.0.0
      rm "${case_root}/home/.dotfiles/.claude/plugins/cache/legion-plugins/claude-envoy/4.0.0/package.json"
      want="${record} records an install of claude-envoy@legion-plugins at ${case_root}/home/.dotfiles/.claude/plugins/cache/legion-plugins/claude-envoy/4.0.0, whose package.json cannot be read: "
      ;;
  esac
  run_gate -- --clients "$commit"
  check "exits 3" "$(is "$status" 3)"
  check "refuses UNKNOWN naming the install" \
    "$(grep -qF "UNKNOWN: dotfiles .claude/settings.json: ${want}" "$out" && echo true || echo false)"
done

# A link whose target is gone, or that never resolves, is something there that cannot be read, never
# nothing there: not a record that is the dotfiles' own, and not a profiles directory with no
# profile in it. ~/.omp, the profiles directory's parent, is read through ~/.omp/plugins. A link to
# a link whose target is gone is named as one that does not resolve: its own target is there. A
# link whose target cannot be reached another way, through a directory the gate may not search or a
# loop part-way along its path, is named with that error, never as one that does not exist.
for shape in package package-chain package-unsearchable package-loop outside-record outside-record-loop outside-record-to-dotfiles dotfiles-record profiles profiles-chain profiles-newline omp omp-chain omp-not-utf8; do
  echo "case: --clients, a link that does not resolve (${shape})"
  box_case "box-dangling-${shape}"
  assign=()
  hide=""
  case "$shape" in
    package)
      at="${case_root}/home/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy"
      rm -r "$at" && ln -s ../gone "$at"
      want="UNKNOWN: dotfiles omp/plugins/package.json: @sjawhar/pi-envoy is installed outside ${case_root}/home/.dotfiles, in ${case_root}/home/.dotfiles/omp/plugins, a link to ../gone, which does not exist"
      ;;
    package-chain)
      at="${case_root}/home/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy"
      rm -r "$at" && ln -s ../gone-link "$at" && ln -s gone "$(dirname "$at")/../gone-link"
      want="UNKNOWN: dotfiles omp/plugins/package.json: @sjawhar/pi-envoy is installed outside ${case_root}/home/.dotfiles, in ${case_root}/home/.dotfiles/omp/plugins, a link to ../gone-link, which does not resolve"
      ;;
    package-unsearchable)
      # Its target is the install, behind a directory the gate may not search.
      at="${case_root}/home/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy"
      hide="${case_root}/home/.dotfiles/omp/plugins/node_modules/locked"
      mkdir "$hide" && mv "$at" "$hide/real" && ln -s ../locked/real "$at"
      want="UNKNOWN: dotfiles omp/plugins/package.json: @sjawhar/pi-envoy is installed outside ${case_root}/home/.dotfiles, in ${case_root}/home/.dotfiles/omp/plugins, a link to ../locked/real, which does not resolve: Permission denied"
      ;;
    package-loop)
      # Its target's path loops part-way: ../loop is a link to itself.
      at="${case_root}/home/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy"
      rm -r "$at" && ln -s loop "$(dirname "$at")/../loop" && ln -s ../loop/x "$at"
      want="UNKNOWN: dotfiles omp/plugins/package.json: @sjawhar/pi-envoy is installed outside ${case_root}/home/.dotfiles, in ${case_root}/home/.dotfiles/omp/plugins, a link to ../loop/x, which does not resolve: Too many levels of symbolic links"
      ;;
    outside-record)
      mkdir -p "${case_root}/home/.claude/plugins" && ln -s gone.json "${case_root}/home/.claude/plugins/installed_plugins.json"
      want="UNKNOWN: dotfiles .claude/settings.json: ${case_root}/home/.claude/plugins/installed_plugins.json cannot be read as JSON: "
      ;;
    outside-record-loop)
      mkdir -p "${case_root}/home/.claude/plugins" && ln -s installed_plugins.json "${case_root}/home/.claude/plugins/installed_plugins.json"
      check "the dotfiles hold no .claude/plugins, so the path of their record does not resolve either" \
        "$(readlink -f "${case_root}/home/.dotfiles/.claude/plugins/installed_plugins.json" >/dev/null && echo false || echo true)"
      want="UNKNOWN: dotfiles .claude/settings.json: ${case_root}/home/.claude/plugins/installed_plugins.json cannot be read as JSON: "
      ;;
    outside-record-to-dotfiles)
      # A link to the path of the dotfiles' own record, where no record is: both paths resolve to
      # one name, and no file is there.
      mkdir -p "${case_root}/home/.claude/plugins" "${case_root}/home/.dotfiles/.claude/plugins" &&
        ln -s ../../.dotfiles/.claude/plugins/installed_plugins.json "${case_root}/home/.claude/plugins/installed_plugins.json"
      want="UNKNOWN: dotfiles .claude/settings.json: ${case_root}/home/.claude/plugins/installed_plugins.json cannot be read as JSON: "
      ;;
    dotfiles-record)
      mkdir -p "${case_root}/home/.dotfiles/.claude/plugins" && ln -s gone.json "${case_root}/home/.dotfiles/.claude/plugins/installed_plugins.json"
      want="UNKNOWN: dotfiles .claude/settings.json: ${case_root}/home/.dotfiles/.claude/plugins/installed_plugins.json cannot be read as JSON: "
      ;;
    profiles)
      ln -s missing-profile-volume "${case_root}/home/.omp/profiles"
      want="UNKNOWN: dotfiles omp/plugins/package.json: cannot tell whether @sjawhar/pi-envoy is installed: ${case_root}/home/.omp/profiles is a link to missing-profile-volume, which does not exist"
      ;;
    profiles-chain)
      ln -s profiles-volume "${case_root}/home/.omp/profiles" && ln -s gone "${case_root}/home/.omp/profiles-volume"
      want="UNKNOWN: dotfiles omp/plugins/package.json: cannot tell whether @sjawhar/pi-envoy is installed: ${case_root}/home/.omp/profiles is a link to profiles-volume, which does not resolve"
      ;;
    profiles-newline)
      # The gate prints the last line of what it could not read, so the target is one line.
      ln -s "$(printf 'missing\nvolume')" "${case_root}/home/.omp/profiles"
      want="UNKNOWN: dotfiles omp/plugins/package.json: cannot tell whether @sjawhar/pi-envoy is installed: ${case_root}/home/.omp/profiles is a link to missing\\x0avolume, which does not exist"
      ;;
    omp)
      rm "${case_root}/home/.omp/plugins" && rmdir "${case_root}/home/.omp" && ln -s gone "${case_root}/home/.omp"
      want="UNKNOWN: dotfiles omp/plugins/package.json: cannot tell whether @sjawhar/pi-envoy is installed: ${case_root}/home/.omp is a link to gone, which does not exist"
      ;;
    omp-chain)
      rm "${case_root}/home/.omp/plugins" && rmdir "${case_root}/home/.omp" &&
        ln -s omp-volume "${case_root}/home/.omp" && ln -s gone "${case_root}/home/omp-volume"
      want="UNKNOWN: dotfiles omp/plugins/package.json: cannot tell whether @sjawhar/pi-envoy is installed: ${case_root}/home/.omp is a link to omp-volume, which does not resolve"
      ;;
    omp-not-utf8)
      # A target name that is no UTF-8, read where Python writes UTF-8 strictly, as it does in a
      # locale such as en_US.UTF-8.
      rm "${case_root}/home/.omp/plugins" && rmdir "${case_root}/home/.omp" && ln -s "$(printf 'gone\377')" "${case_root}/home/.omp"
      assign=(PYTHONIOENCODING=utf-8:strict)
      want="UNKNOWN: dotfiles omp/plugins/package.json: cannot tell whether @sjawhar/pi-envoy is installed: ${case_root}/home/.omp is a link to gone\\udcff, which does not exist"
      ;;
  esac
  if [ -n "$hide" ]; then
    check "the link resolves while its directory may be searched, as the case needs" \
      "$([ -e "$at/package.json" ] && echo true || echo false)"
    chmod 000 "$hide"
    check "this user cannot search the hidden directory, as the case needs" \
      "$([ -e "$at" ] && echo false || echo true)"
  fi
  run_gate "${assign[@]}" -- --clients "$commit"
  [ -z "$hide" ] || chmod 755 "$hide"
  check "exits 3" "$(is "$status" 3)"
  check "refuses UNKNOWN naming the link" "$(grep -qF "$want" "$out" && echo true || echo false)"
  check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"
done

# A record outside the dotfiles that is theirs, through a link to their Claude Code configuration or
# to the record itself, is the dotfiles' own: a link that resolves to a record hides nothing.
for shape in configuration record; do
  echo "case: --clients, a Claude Code record outside the dotfiles, a link to theirs (${shape})"
  box_case "box-claude-alias-${shape}"
  claude_install 4.0.0 4.0.0
  if [ "$shape" = configuration ]; then
    link="${case_root}/home/.claude" target=.dotfiles/.claude
  else
    link="${case_root}/home/.claude/plugins/installed_plugins.json" target=../../.dotfiles/.claude/plugins/installed_plugins.json
  fi
  mkdir -p "$(dirname "$link")" && ln -s "$target" "$link"
  check "the link is the one the case needs" "$([ "$(readlink "$link")" = "$target" ] && echo true || echo false)"
  run_gate -- --clients "$commit"
  check "exits 0, the record being the dotfiles' own" "$(is "$status" 0)"
  check "judges the copy the dotfiles' record names" \
    "$(has_line "PASS: dotfiles .claude/plugins installed claude-envoy runs claude-envoy 4.0.0 from claude-envoy-v4.0.0 at ${identical_rev} (identical to ${commit})")"
  check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"
done

# A path the gate hands python3 as an argument reaches it changed when its name is no UTF-8 and a
# launcher that reads its arguments as text runs python3: mise's shims hand on each such byte as
# U+FFFD, and the changed path, which is not there, reads as absent. The gate refuses such a name,
# a profile's or a directory it reads under, by its bytes, whatever python3 and locale run it: here a
# python3 that changes its arguments as the shim does, under C.UTF-8, and for a profile also
# Python's strict writing of en_US.UTF-8. Each fixture holds an install that shuts or refuses the
# gate when read. A profile whose name is UTF-8 is read as any other, though a Python whose locale
# is ASCII reads its name as one that is no UTF-8.
mangling_bin="${temporary_dir}/mangling-python"
mkdir -p "$mangling_bin"
quoted_python=$(printf %q "$(python3 -c 'import sys; print(sys.executable)')")
cat >"$mangling_bin/python3" <<EOF
#!/usr/bin/env bash
exec ${quoted_python} -c 'import os, sys; os.execv(sys.argv[1], sys.argv[1:2] + [os.fsencode(a).decode("utf-8", "replace") for a in sys.argv[2:]])' ${quoted_python} "\$@"
EOF
chmod +x "$mangling_bin/python3"
echo "case: --clients, a python3 that changes an argument that is no UTF-8"
check "hands a byte that is no UTF-8 on as U+FFFD, as mise's shim does" \
  "$(is "$(PATH="${mangling_bin}:${PATH}" python3 -c 'import sys; print(ascii(sys.argv[1]))' "$(printf 'x\377')")" "'x\\ufffd'")"
for row in profile:mangling profile:strict home:mangling dotfiles:mangling claude-config:mangling; do
  shape=${row%%:*} python=${row#*:}
  echo "case: --clients, a name that is no UTF-8 (${shape}, ${python} python3)"
  box_case "box-not-utf8-${shape}-${python}"
  home="${case_root}/home"
  args=(--clients "$commit")
  if [ "$python" = mangling ]; then
    assign=(--unset PYTHONIOENCODING LC_ALL=C.UTF-8 "PATH=${mangling_bin}:${temporary_dir}/bin:${PATH}")
  else
    assign=(PYTHONIOENCODING=utf-8:strict)
  fi
  case "$shape" in
    profile)
      at="$home/.omp/profiles/$(printf 'work\377')/plugins/node_modules/@sjawhar/pi-envoy"
      mkdir -p "$at" && printf '{"name": "@sjawhar/pi-envoy", "version": "17.39.0"}\n' >"$at/package.json"
      want="UNKNOWN: dotfiles omp/plugins/package.json: cannot tell whether @sjawhar/pi-envoy is installed: $home/.omp/profiles/work\\xff: the profile's name is no UTF-8"
      ;;
    home)
      # The dotfiles are elsewhere, so only the paths under HOME change: ~/.claude records claude-envoy.
      mv "$home/.dotfiles" "${case_root}/box-dotfiles"
      ln -sfn "${case_root}/box-dotfiles/omp/plugins" "$home/.omp/plugins"
      mkdir -p "$home/.claude/plugins"
      printf '{"version": 2, "plugins": {"claude-envoy@legion-plugins": [{"version": "1.0.0"}]}}\n' >"$home/.claude/plugins/installed_plugins.json"
      mv "$home" "${case_root}/$(printf 'home\377')"
      assign+=("HOME=${case_root}/$(printf 'home\377')")
      args=(--clients --dotfiles "${case_root}/box-dotfiles" "$commit")
      want="UNKNOWN: dotfiles: HOME is no UTF-8, so "
      ;;
    dotfiles)
      # The dotfiles' Claude Code configuration holds a copy behind the commit.
      claude_install 3.3.0 3.3.0
      mv "$home/.dotfiles" "${case_root}/$(printf 'dot\377files')"
      sed -i "s|$home/.dotfiles|${case_root}/$(printf 'dot\377files')|" "${case_root}/$(printf 'dot\377files')/.claude/plugins/installed_plugins.json"
      ln -sfn "${case_root}/$(printf 'dot\377files')/omp/plugins" "$home/.omp/plugins"
      args=(--clients --dotfiles "${case_root}/$(printf 'dot\377files')" "$commit")
      want="UNKNOWN: dotfiles: the dotfiles directory is no UTF-8, so "
      ;;
    claude-config)
      mkdir -p "${case_root}/$(printf 'claude\377')/plugins"
      printf '{"version": 2, "plugins": {"claude-envoy@legion-plugins": [{"version": "4.0.0"}]}}\n' \
        >"${case_root}/$(printf 'claude\377')/plugins/installed_plugins.json"
      assign+=("CLAUDE_CONFIG_DIR=${case_root}/$(printf 'claude\377')")
      want="UNKNOWN: dotfiles: CLAUDE_CONFIG_DIR is no UTF-8, so "
      ;;
  esac
  run_gate "${assign[@]}" -- "${args[@]}"
  check "exits 3" "$(is "$status" 3)"
  check "refuses UNKNOWN naming the name that is no UTF-8" "$(grep -qF "$want" "$out" && echo true || echo false)"
  check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"
done

echo "case: --clients, a profile whose name is UTF-8, read by a Python whose locale is ASCII"
box_case box-utf8-profile-ascii-python
mkdir -p "${case_root}/home/.omp/profiles/$(printf 'caf\303\251')/plugins/node_modules/@sjawhar"
ln -s "${case_root}/home/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy" \
  "${case_root}/home/.omp/profiles/$(printf 'caf\303\251')/plugins/node_modules/@sjawhar/pi-envoy"
check "this Python reads names as ASCII, as the case needs" \
  "$(is "$(LC_ALL=C PYTHONUTF8=0 PYTHONCOERCECLOCALE=0 python3 -c 'import sys; print(sys.getfilesystemencoding())')" ascii)"
run_gate LC_ALL=C PYTHONUTF8=0 PYTHONCOERCECLOCALE=0 -- --clients "$commit"
check "exits 0, the copy being the dotfiles' own" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

# The same profile under the same Python, holding a copy of its own: the gate reads a profile by
# the bytes of its name, so that copy is one outside the dotfiles, UNKNOWN by name. A reader that
# hands the name back as other text (`caf\udcc3\udca9` in place of its two UTF-8 bytes) finds no
# profile at that path and opens the gate, as the row above opens it.
echo "case: --clients, a UTF-8 profile holding its own copy, read by a Python whose locale is ASCII"
box_case box-utf8-profile-own-copy-ascii-python
at="${case_root}/home/.omp/profiles/$(printf 'caf\303\251')/plugins/node_modules/@sjawhar/pi-envoy"
mkdir -p "$at" && printf '{"name": "@sjawhar/pi-envoy", "version": "17.40.0"}\n' >"$at/package.json"
run_gate LC_ALL=C PYTHONUTF8=0 PYTHONCOERCECLOCALE=0 -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the profile's tree as one outside the dotfiles" \
  "$(grep -qF "UNKNOWN: dotfiles omp/plugins/package.json: @sjawhar/pi-envoy is installed outside ${case_root}/home/.dotfiles, in ${case_root}/home/.omp/profiles/caf" "$out" && echo true || echo false)"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

# Something the gate cannot tell is there or not is never read as not there: a directory it may
# not search or list, and a link along the way whose target is gone, each hide an install that would
# shut the gate. Each fixture holds one, and each readable control beside it shuts or refuses.
for shape in package-parent claude-record-parent outside-claude profiles omp-tree claude-parent; do
  echo "case: --clients, a surface the gate cannot tell is there (${shape})"
  box_case "box-unseen-${shape}"
  home="${case_root}/home"
  hide=""
  case "$shape" in
    package-parent)
      printf '{"name": "@sjawhar/pi-envoy", "version": "17.39.0"}\n' \
        >"$home/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy/package.json"
      hide="$home/.dotfiles/omp/plugins/node_modules/@sjawhar"
      want="UNKNOWN: dotfiles omp/plugins/package.json: cannot tell whether @sjawhar/pi-envoy is installed: $hide/pi-envoy: Permission denied"
      ;;
    claude-record-parent)
      claude_install 3.3.0 3.3.0
      hide="$home/.dotfiles/.claude/plugins"
      want="UNKNOWN: dotfiles .claude/settings.json: cannot tell whether a Claude Code plugin record is there: $hide/installed_plugins.json: Permission denied"
      ;;
    outside-claude)
      mkdir -p "$home/.claude/plugins"
      printf '{"version": 2, "plugins": {"claude-envoy@legion-plugins": [{"version": "4.0.0"}]}}\n' >"$home/.claude/plugins/installed_plugins.json"
      hide="$home/.claude"
      want="UNKNOWN: dotfiles .claude/settings.json: cannot tell whether a Claude Code plugin record is there: $hide/plugins/installed_plugins.json: Permission denied"
      ;;
    profiles)
      mkdir -p "$home/.omp/profiles/work/plugins/node_modules/@sjawhar/pi-envoy"
      printf '{"name": "@sjawhar/pi-envoy", "version": "17.39.0"}\n' \
        >"$home/.omp/profiles/work/plugins/node_modules/@sjawhar/pi-envoy/package.json"
      hide="$home/.omp/profiles"
      want="UNKNOWN: dotfiles omp/plugins/package.json: cannot tell whether @sjawhar/pi-envoy is installed: $hide cannot be listed: Permission denied"
      ;;
    omp-tree)
      rm "$home/.omp/plugins" && ln -s ../gone "$home/.omp/plugins"
      want="UNKNOWN: dotfiles omp/plugins/package.json: cannot tell whether @sjawhar/pi-envoy is installed: $home/.omp/plugins is a link to ../gone, which does not exist"
      ;;
    claude-parent)
      ln -s ../gone "$home/.claude"
      want="UNKNOWN: dotfiles .claude/settings.json: cannot tell whether a Claude Code plugin record is there: $home/.claude is a link to ../gone, which does not exist"
      ;;
  esac
  if [ -n "$hide" ]; then
    run_gate -- --clients "$commit"
    check "control: readable, the fixture shuts or refuses" "$([ "$status" != 0 ] && echo true || echo false)"
    chmod 000 "$hide"
    check "this user cannot search the hidden directory, as the case needs" \
      "$(ls "$hide/" >/dev/null 2>&1 && echo false || echo true)"
  fi
  run_gate -- --clients "$commit"
  [ -z "$hide" ] || chmod 755 "$hide"
  check "exits 3" "$(is "$status" 3)"
  check "refuses UNKNOWN naming what it could not see" "$(grep -qF "$want" "$out" && echo true || echo false)"
  check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"
done

# Two surfaces hidden at once, the profiles directory and the dotfiles' @sjawhar directory, which
# ~/.omp/plugins links to: a profiles directory the gate cannot list still leaves it ~/.omp/plugins
# to read, so the one UNKNOWN line names both, and the operator sees every surface to fix at once.
echo "case: --clients, the profiles and ~/.omp/plugins' copy both hidden"
box_case box-unseen-profiles-and-plugins
home="${case_root}/home"
mkdir -p "$home/.omp/profiles/work/plugins"
chmod 000 "$home/.omp/profiles" "$home/.dotfiles/omp/plugins/node_modules/@sjawhar"
run_gate -- --clients "$commit"
chmod 755 "$home/.omp/profiles" "$home/.dotfiles/omp/plugins/node_modules/@sjawhar"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the unlisted profiles and ~/.omp/plugins' copy in one line" \
  "$(grep -qF "$home/.omp/profiles cannot be listed: Permission denied, $home/.omp/plugins/node_modules/@sjawhar/pi-envoy: Permission denied" "$out" && echo true || echo false)"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

# A plugin name the gate cannot read, a tab in it among them, is refused whole: the record's fields
# are never shifted into one another.
echo "case: --clients, a Claude Code record whose plugin name holds a tab"
box_case box-claude-key-tab
claude_install 4.0.0 1.0.0
tag claude-envoy-v1.0.0 "$behind_rev"
record="${case_root}/home/.dotfiles/.claude/plugins/installed_plugins.json"
sed -i 's/"claude-envoy@legion-plugins"/"claude-envoy@legion-plugins\\t4.0.0"/' "$record"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the plugin name" \
  "$(has_line "UNKNOWN: dotfiles .claude/settings.json: ${record} records a plugin named \"claude-envoy@legion-plugins\\t4.0.0\", which is no plugin name this gate can read")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, a run with no HOME"
box_case box-no-home
run_gate --unset HOME -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming HOME" \
  "$(has_line "UNKNOWN: dotfiles: HOME is not set, so this box's plugin trees (~/.omp, ~/.claude) cannot be found")"
check "still reads the images" \
  "$(grep -q '^PASS: LINUX image (pi-envoy and claude-envoy) runs ' "$out" && echo true || echo false)"
check "the verdict is the last line" "$(last_line_is "UNKNOWN")"
