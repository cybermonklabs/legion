# shellcheck shell=bash
# shellcheck disable=SC2154,SC2034 # the leg that sources this file sets what it reads, and reads what it sets
# The deployed pins and the build Dispatch serves (rows pins and pins-end): outside-agent-tailnet.sh
# sources this file from beside it, after outside-agent-lib.sh. It defines functions alone. They
# read the leg's `work`, `newline`, `checkout`, `pin_names` (the deployments whose pins are read:
# `broker dispatch edge-listener`, or `broker dispatch` for the browser gate) and the inputs
# OUTSIDE_PINS, OUTSIDE_PINS_REPO, OUTSIDE_IMAGE_REVISION_COMMAND and OUTSIDE_TAILNET_DISPATCH_URL,
# and set `pins_text`, `pins_problem`, `serving_commit`, `serving_fleet` and `serving_problem`.
#
# What each service says of the build it serves: Dispatch's /healthz names `commit`, the legion
# commit its binary was built from, and `fleet_commit`, the commit every live server process serves
# (each null when unknown); the broker's and the edge listener's /healthz name a status alone.

# pin_file FILE OUT: the pin file FILE into OUT: FILE at OUTSIDE_PINS_REPO's main, read with
# `gh api`, or a local file; false when it could not be read.
pin_file() {
  if [ -n "${OUTSIDE_PINS_REPO:-}" ]; then
    command -v gh >/dev/null 2>&1 || return 1
    gh api "repos/$OUTSIDE_PINS_REPO/contents/$1?ref=main" --jq .content 2>/dev/null </dev/null |
      base64 -d >"$2" 2>/dev/null && [ -s "$2" ]
  else
    [ -r "$1" ] && cp "$1" "$2"
  fi
}
# read_pins: sets pins_text, the `name image revision` lines of each name pin_names holds, or
# pins_problem. A line of another name is not read.
read_pins() {
  local line name file key value image revision found copy
  pins_text=''
  pins_problem=''
  for name in $pin_names; do
    found=''
    while IFS= read -r line || [ -n "$line" ]; do
      line=${line%%#*}
      set -f
      # shellcheck disable=SC2086 # the fields of one pin line, split on purpose
      set -- $line
      set +f
      [ "$#" -eq 3 ] && [ "$1" = "$name" ] || continue
      # One line a name: a second could name another image than the first, and only one is held.
      if [ -n "$found" ]; then
        pins_problem="${pins_problem:+$pins_problem; }$OUTSIDE_PINS has more than one $name line"
        continue
      fi
      file=$2 key=$3 found=yes
      image=''
      copy="$work/pin-$name"
      if pin_file "$file" "$copy"; then
        while IFS= read -r value || [ -n "$value" ]; do
          case $value in *"$key:"*) image=${value#*"$key:"} ;; esac
        done <"$copy"
      fi
      image=${image#"${image%%[! ]*}"}
      image=${image%"${image##*[! ]}"}
      if [ -z "$image" ]; then
        pins_problem="${pins_problem:+$pins_problem; }$name: $file${OUTSIDE_PINS_REPO:+ at the main of $OUTSIDE_PINS_REPO} holds no $key"
        continue
      fi
      revision=''
      if [ -n "${OUTSIDE_IMAGE_REVISION_COMMAND:-}" ]; then
        set -f
        # shellcheck disable=SC2086 # a command line, split on blanks on purpose
        revision=$($OUTSIDE_IMAGE_REVISION_COMMAND "$image" 2>/dev/null </dev/null)
        set +f
      elif command -v crane >/dev/null 2>&1; then
        revision=$(json_field org.opencontainers.image.revision "$(crane config "$image" 2>/dev/null </dev/null)")
      fi
      case $revision in
        [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]*) ;;
        *)
          pins_problem="${pins_problem:+$pins_problem; }$name: no commit read for $image${OUTSIDE_IMAGE_REVISION_COMMAND:+ through OUTSIDE_IMAGE_REVISION_COMMAND}$(command -v crane >/dev/null 2>&1 || [ -n "${OUTSIDE_IMAGE_REVISION_COMMAND:-}" ] || printf ' (crane not on PATH)')"
          continue
          ;;
      esac
      pins_text="${pins_text:+$pins_text$newline}$name $image $revision"
    done <"$OUTSIDE_PINS"
    [ -n "$found" ] || pins_problem="${pins_problem:+$pins_problem; }$OUTSIDE_PINS has no $name line"
  done
}
# read_serving: Dispatch's /healthz at its private origin, the build it serves: sets
# serving_commit (`commit`, the commit this process was built from) and serving_fleet
# (`fleet_commit`, the commit every live server process serves), or serving_problem.
read_serving() {
  local answer
  serving_commit=''
  serving_fleet=''
  serving_problem=''
  answer=$(curl -q -sS -g --noproxy '*' --max-time 15 "$OUTSIDE_TAILNET_DISPATCH_URL/healthz" 2>&1 </dev/null | tr -d ' \n')
  serving_commit=$(json_field commit "$answer")
  serving_fleet=$(json_field fleet_commit "$answer")
  case $serving_commit in
    [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]*) ;;
    *)
      serving_problem="Dispatch's /healthz names no commit it serves: ${answer:0:200}"
      serving_commit=''
      ;;
  esac
  # Every live process must serve that build: a half rollout holds the gate at the start and at
  # the end alike.
  if [ -n "$serving_commit" ] && [ "$serving_fleet" != "$serving_commit" ]; then
    serving_problem="not every Dispatch process serves $serving_commit: fleet_commit is ${serving_fleet:-none}"
  fi
}
# holds BUILD COMMIT: whether the build made from commit BUILD holds COMMIT: yes, no, or what could
# not be read.
holds() {
  local sha
  for sha in "$2" "$1"; do
    git -C "$checkout" cat-file -e "$sha^{commit}" 2>/dev/null ||
      git -C "$checkout" fetch -q origin "$sha" >/dev/null 2>&1 </dev/null
  done
  git -C "$checkout" merge-base --is-ancestor "$2" "$1" 2>"$work/merge-base.err"
  case $? in
    0) printf yes ;;
    1) printf no ;;
    *) printf 'git merge-base --is-ancestor %s %s: %s' "$2" "$1" "$(tr '\n' ' ' <"$work/merge-base.err")" ;;
  esac
}
