# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-developer-box-records.sh
#
# The --clients scenarios for Claude Code install records and developer-box links.
# daemon-release-gate.test.sh sources this file through its group runner after the harness and
# the client fixtures (daemon-release-gate-fixtures-clients.sh). Do not run this file on its own.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the test that sources this file

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
