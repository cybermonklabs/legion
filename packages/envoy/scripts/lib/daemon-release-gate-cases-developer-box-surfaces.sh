# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-developer-box-surfaces.sh
#
# The --clients scenarios for developer-box paths the gate cannot read. daemon-release-gate.test.sh
# sources this file through its group runner after the harness and the client fixtures
# (daemon-release-gate-fixtures-clients.sh). Do not run this file on its own.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the test that sources this file

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

# The gate's python3 reader can end before it answers, as an out-of-memory kill ends it, and the
# gate reads no silence as an answer: each question it then asks is UNKNOWN by name. Here the reader
# ends when it is asked whether the dotfiles' own pi-envoy, a directory and no link, resolves. No pin
# shut the gate, so the run ends UNKNOWN, never with the GATE SHUT status and no verdict line.
ending_bin="${temporary_dir}/ending-python"
mkdir -p "$ending_bin"
real_python=$(python3 -c 'import sys; print(sys.executable)')
fake "$ending_bin/python3" <<'EOF'
if [[ ${1-} == */gate-reader.py ]]; then
  reader=$1
  shift
  while IFS= read -r line; do
    printf '%s\n' "$line"
    if [ "$line" = '    verb = question[0]' ]; then
      printf '%s\n' '    if verb == b"unresolved":' \
        '        open(os.environ["FAKE_ROOT"] + "/reader-ended", "w").close()' \
        '        os.kill(os.getpid(), 9)'
    fi
  done <"$reader" >"$reader.ending"
  exec "$REAL_PYTHON" "$reader.ending" "$@"
fi
exec "$REAL_PYTHON" "$@"
EOF
echo "case: --clients, a python3 reader that ends before it answers"
box_case box-reader-ends
run_gate "PATH=${ending_bin}:${temporary_dir}/bin:${PATH}" "REAL_PYTHON=${real_python}" -- --clients "$commit"
check "control: the reader ended when asked whether the dotfiles' pi-envoy resolves" \
  "$([ -e "${case_root}/reader-ended" ] && echo true || echo false)"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the reader that did not answer" \
  "$(grep -q '^UNKNOWN: .*reader did not answer' "$out" && echo true || echo false)"
check "the verdict is UNKNOWN, never GATE SHUT or no verdict at all" "$(last_line_is "UNKNOWN")"
