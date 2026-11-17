#!/usr/bin/env bash
# A stage driver's own home for Oh My Pi. Sourced, never run.
#
# make_omp_home DIR — DIR becomes the HOME every Oh My Pi process of the run runs under (the daemon,
# so every pane, the controller, and each `omp` a lib runs), so the run's profile root is
# DIR/.omp/profiles/<name>, inside its work directory, and the operator's own ~/.omp/profiles is
# never written or deleted. Oh My Pi places a profile from the home directory alone:
# os.homedir()/.omp/profiles/<name> (getProfileConfigRoot, @oh-my-pi/pi-utils dirs.ts).
# XDG_DATA_HOME moves only a named profile's data, and only once $XDG_DATA_HOME/omp/profiles/<name>
# exists, and PI_CONFIG_DIR reaches no pane (neither daemon's pane allow-list carries it).
#
# DIR/.omp/natives links to the operator's native module cache (116 MiB per Oh My Pi version at the
# pin, the one variant the CPU selects; resolved as getNativesDir in @oh-my-pi/pi-natives'
# native/loader-state.js resolves it), which a
# fresh home would download again; removing DIR removes the link, never the cache. MISE_DATA_DIR and
# XDG_CONFIG_HOME are exported first as the operator's own, where each already resolves, so what the
# daemon itself runs under DIR finds what it did under the operator's HOME: `mise` the same pinned
# Oh My Pi, `secrets` its secretsd config (the daemon resolves provider_keys with it), and gh and jj
# their config. Panes are unaffected: the daemon moves their four XDG base directories under
# <state_dir>/home (LEGION-206 P1).
#
# jj reads a leading ~/ in a config value against HOME, which is DIR once the daemon runs, so an
# operator's `signing.key = "~/.ssh/<key>.pub"` would name a key inside DIR, where there is none.
# With `signing.behavior = "own"`, the first commit of every workspace clone the daemon makes (its
# author is the operator) then fails to sign, and no pane ever starts. So every ~/ value of the
# operator's jj config is pinned, in DIR/.jjconfig-operator-paths.toml, to the path it names under
# the operator's HOME, and JJ_CONFIG is exported as the operator's own config files (`jj config
# path --user`, which names jj's default files when JJ_CONFIG is unset) with that overlay last. A
# ~/ value this cannot pin (inside an array or table, or under a quoted key) fails the call,
# naming it. Panes commit as their App's identity (JJ_USER and JJ_EMAIL), which "own" never signs.
make_omp_home() {
  local natives=$HOME/.omp/natives overlay=$1/.jjconfig-operator-paths.toml listing files line
  local pinned='^([A-Za-z0-9_.-]+) = "~/([^"\\]*)"$'
  # Read before HOME moves, and without this run's own overlay (a second call on the same DIR sees
  # JJ_CONFIG naming it), so each value reads as the operator's jj reads it.
  files=$(jj config path --user | awk -v overlay="$overlay" '$0 != overlay' | paste -sd: -) ||
    { echo "make_omp_home: jj config path --user failed, so the operator's jj config cannot be named" >&2; return 1; }
  listing=$(JJ_CONFIG=$files jj config list --user) ||
    { echo "make_omp_home: jj config list --user failed, so the operator's ~/ jj paths cannot be pinned" >&2; return 1; }
  if [ -n "${XDG_DATA_HOME:-}" ] && [ -d "$XDG_DATA_HOME/omp" ]; then natives=$XDG_DATA_HOME/omp/natives; fi
  export MISE_DATA_DIR="${MISE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/mise}"
  export XDG_CONFIG_HOME="${XDG_CONFIG_HOME:-$HOME/.config}"
  mkdir -p "$natives" "$1/.omp"
  : >"$overlay"
  while IFS= read -r line; do
    [[ $line == *'~/'* ]] || continue
    if [[ ! $line =~ $pinned ]]; then
      echo "make_omp_home: the operator's jj config value ${line%% = *} names a ~/ path this cannot pin to $HOME: $line" >&2
      return 1
    fi
    printf '%s = "%s/%s"\n' "${BASH_REMATCH[1]}" "$HOME" "${BASH_REMATCH[2]}" >>"$overlay"
  done <<<"$listing"
  # The overlay is last in JJ_CONFIG, so this wins over whatever fsmonitor backend the operator's
  # own config names (watchman, on a devbox that runs it): every jj the run's Oh My Pi processes
  # and this script's own workspace_jj calls run with no watchman, so a run never leaves the
  # operator's long-running watchman watching a workspace or clone this stage tore down. Signing
  # and identity are untouched: nothing above pins fsmonitor.*, so this is a pure addition.
  printf 'fsmonitor.backend = "none"\n' >>"$overlay"
  export JJ_CONFIG="$files:$overlay"
  # -T reads DIR/.omp/natives as the link's own name, never as a directory to put the link in: a
  # second call replaces the link (plain -s would write natives/natives into the operator's cache,
  # through the first link), and a real directory there fails the call instead of taking a link.
  ln -sfT "$natives" "$1/.omp/natives"
}

# omp_setup_wizard_off FILE: appends to FILE, the agent/config.yml of a profile the run writes under
# its own home, the setting that keeps Oh My Pi's first-run setup wizard shut. An interactive Oh My
# Pi in a terminal (`legion controller start` in a tmux pane, a person's session) opens the wizard
# ("Setup step 1 of 5", "Sign in to your providers") while the profile's setupVersion is below the
# build's CURRENT_SETUP_VERSION, so on a fresh profile the wizard blocks the session's first
# command and normal editor interaction (runInteractiveMode in packages/coding-agent/src/main.ts
# and selectSetupScenes in packages/tui/src/setup/wizard.ts, at the pin). `startup.setupWizard:
# false` shuts it whatever setup version the build counts to; `--mode rpc` never opens it. A named
# profile's own settings file is its agent/config.yml, and it inherits no other config.yml of the
# home (getProfileConfigRoot in @oh-my-pi/pi-utils dirs.ts). Only the working directory's project
# settings and PI_CONFIG_FILES overlays merge over it, so each profile the run writes carries the
# line.
omp_setup_wizard_off() {
  printf 'startup:\n  setupWizard: false\n' >>"$1"
}

# omp_session_unsets is env's -u options for the variables of the operator's own Oh My Pi session
# that no Oh My Pi a rig starts from that session may inherit: OMP_SESSION_ID, that session's id;
# OMPCODE, the mark Oh My Pi puts on the shells its bash tool spawns (buildSpawnEnv in
# @oh-my-pi/pi-utils procmgr.ts); PI_CONFIG_FILES, that session's settings overlays, which Oh My Pi
# lays over the profile it opens; ENVOY_NATS_URL, the NATS that session's Envoy plugin dials; and
# LEGION_OMP_PATH, the Oh My Pi binary that `legion controller start` and the daemon run in place
# of omp_invocation (ResolveInvocation, packages/daemon/internal/omplaunch). `legion controller
# start` hands Oh My Pi its caller's environment, so every rig that runs it, or an interactive omp,
# from the operator's shell drops these first: `env "${omp_session_unsets[@]}" …` in an argv, and
# `env ${omp_session_unsets[*]} …` in a pane's shell command, which no name here needs quoted in.
# shellcheck disable=SC2034 # the scripts that source this file read it
omp_session_unsets=(-u OMP_SESSION_ID -u OMPCODE -u PI_CONFIG_FILES -u ENVOY_NATS_URL -u LEGION_OMP_PATH)
