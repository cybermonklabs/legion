# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-daemons.sh
#
# The scenarios of the gate's daemon leg, the mode no flag names: the image that runs each Legion
# daemon, read from the deployment's Legion projects, and the verdict each read gives.
# daemon-release-gate.test.sh runs this file in a process of its own
# (lib/daemon-release-gate-group.sh, which sources the harness and the client fixtures first); it
# is not run on its own.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the group runner

readonly fleet="    - key: SMOKE
      replicas: 0
      image: ${worker_smoke}
    - key: LINUX
      replicas: 1
      image: us-west1-docker.pkg.dev/example-project/legion/worker-lean@sha256:2222
      daemonImage: ${worker_a}
    - key: INFRA
      replicas: 1
      image: ${infra_image}
      daemonImage: ${worker_b}
    - key: QUIET
      daemon: false
      image: ${worker_smoke}"

echo "case: every running daemon is at or after the commit"
new_case open
legion_stack "$fleet"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 0" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"
check "LINUX passes on its daemonImage" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})")"
check "INFRA passes on its daemonImage" "$(has_line "PASS: INFRA runs ${identical_rev} (identical to ${commit})")"
check "SMOKE at replicas 0 is skipped by name" "$(has_line "SKIPPED: SMOKE runs no daemon (replicas: 0)")"
check "an entry with daemon: false is skipped by name" "$(has_line "SKIPPED: QUIET runs no daemon (daemon: false)")"
check "the image of an entry that sets daemonImage is never read" "$(lacks "$infra_image" "$calls")"
check "the image of an entry that runs no daemon is never read" "$(lacks "$worker_smoke" "$calls")"

echo "case: a running daemon behind the commit"
new_case behind
legion_stack "$fleet"
single "$worker_a" "$legion" "$behind_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$behind_rev" behind 89
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 1" "$(is "$status" 1)"
check "refuses naming the pin and its revision" "$(has_line "GATE SHUT: LINUX runs ${behind_rev} (behind ${commit} by 89)")"
check "the verdict is GATE SHUT" "$(last_line_is "GATE SHUT")"

echo "case: a daemon image that names no revision"
new_case unlabelled
legion_stack "$fleet"
single "$worker_a" "$legion" ""
single "$worker_b" "$legion" "$identical_rev"
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the pin and the missing label" "$(has_line "UNKNOWN: LINUX: ${worker_a} names no org.opencontainers.image.revision label")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

# The compare call takes any ref GitHub resolves, so a revision label that names a branch or a tag
# (`main`) would compare as wherever that ref points today, not as the commit the image was built
# from. The gate compares a revision only when it is 7 to 40 lowercase hex digits.
#
# Negative control, run when this was written: drop the commit-id check from judge; the rows below
# that expect `which is no commit id` print `PASS` instead, with a compare call for the label.
echo "case: a daemon image whose revision label is no commit id"
new_case revision-not-commit
legion_stack "$fleet"
single "$worker_a" "$legion" "main"
single "$worker_b" "$legion" "abcdef1"
compare "main" ahead 0
compare "abcdef1" identical 0
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the pin and its revision" "$(has_line "UNKNOWN: LINUX: ${worker_a} names revision main, which is no commit id")"
check "makes no compare call for that revision" "$(lacks "...main" "$calls")"
check "a revision of 7 hex digits is a commit id, and is compared" "$(has_line "PASS: INFRA runs abcdef1 (identical to ${commit})")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: the compare API cannot be reached"
new_case api-down
legion_stack "$fleet"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
run_gate FAKE_API_DOWN=1 -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the pin and GitHub's answer" "$(has_line "UNKNOWN: LINUX: cannot compare ${commit}...${ahead_rev} in cybermonklabs/legion: error connecting to api.github.com")"
check "the verdict is UNKNOWN" "$(last_line_is "UNKNOWN")"

echo "case: a daemon image built from another repository"
new_case deployment
legion_stack "$fleet"
single "$worker_a" "$other_source" "$other_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$identical_rev" identical 0
compare "$other_rev" ahead 0
run_gate -- "$commit"
check "exits 1" "$(is "$status" 1)"
check "refuses naming the source" "$(has_line "GATE SHUT: LINUX runs an image built from ${other_source}")"
check "makes no compare call for that revision" "$(lacks "...${other_rev}" "$calls")"

echo "case: an image the registry does not answer for"
new_case unreadable
legion_stack "$fleet"
single "$worker_b" "$legion" "$identical_rev"
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the pin and docker's answer" "$(has_line "UNKNOWN: LINUX: cannot read ${worker_a}: ERROR: ${worker_a}: not found")"

echo "case: a running entry that names no image"
new_case no-image
legion_stack "    - key: LINUX
      replicas: 1"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the entry" "$(has_line "UNKNOWN: LINUX runs a daemon and names neither daemonImage nor image")"

# The deployment's legion/host.go: an entry at `runtime: tmux` runs its daemon on the project's host VM
# from host.legionWorker's legion (checkHost refuses daemonImage, replicas and daemon on it), so its
# `image` says nothing about the daemon. The gate reads no image for it: it is UNKNOWN by name. So is
# a runtime the gate does not know; an entry that names `runtime: kubernetes` is read as before.
#
# Negative control, run when this was written: drop the runtime branch from daemon_pins; the rows
# below that expect the runtime's UNKNOWN line print `PASS` for the entry's image instead.
readonly worker_tmux="ghcr.io/cybermonklabs/legion-worker@sha256:6666"
readonly worker_host="ghcr.io/cybermonklabs/legion-worker@sha256:7777"
echo "case: an entry on runtime tmux runs its daemon on a host, not from its image"
new_case tmux
legion_stack "    - key: LINUX
      replicas: 1
      runtime: kubernetes
      daemonImage: ${worker_a}
    - key: HOSTED
      runtime: tmux
      image: ${worker_tmux}
      host:
        legionWorker: ${worker_host}"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_tmux" "$legion" "$ahead_rev"
single "$worker_host" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the entry and its tmux host" \
  "$(has_line "UNKNOWN: HOSTED runs its daemon on a tmux host (runtime: tmux), from host.legionWorker, an image this gate does not read")"
check "never reads the entry's image" "$(lacks "$worker_tmux" "$calls")"
check "never reads the host's image either" "$(lacks "$worker_host" "$calls")"
check "an entry that names runtime kubernetes is still read" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: an entry on a runtime the gate does not know"
new_case runtime-unknown
legion_stack "    - key: ODD
      replicas: 1
      runtime: nomad
      image: ${worker_tmux}"
single "$worker_tmux" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the entry and its runtime" \
  "$(has_line "UNKNOWN: ODD names runtime nomad, which this gate does not know, so it reads no image for its daemon")"
check "never reads the entry's image" "$(lacks "$worker_tmux" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: a stack that lists no running daemon"
new_case empty
legion_stack "    - key: SMOKE
      replicas: 0
      image: ${worker_smoke}"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "says it read no pin, rather than opening" "$(has_line "UNKNOWN: the deployment's Legion projects run no daemon, so no pin was read")"
check "the verdict is never GATE OPEN" "$(lacks "GATE OPEN" "$out")"

echo "case: the stack file cannot be read"
new_case no-stack
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file" "$(has_line "UNKNOWN: cannot read ${deployment} legion/Pulumi.prod.yaml: gh: Not Found (HTTP 404)")"

echo "case: a commit that is no commit id"
new_case usage
run_gate -- "main"
check "exits 2" "$(is "$status" 2)"
check "says what it takes" "$(grep -qF "usage: daemon-release-gate.sh --deployment <owner/repo> [--listeners | --clients [--dotfiles <dir>]] <commit>" "$err" && echo true || echo false)"

# The deployment repository is the operator's to name, never a name the gate compiles in.
echo "case: no --deployment"
new_case no-deployment
legion_stack "$fleet"
run_bare -- "$commit"
check "exits 2" "$(is "$status" 2)"
check "names the input it needs" "$(grep -qF -- "--deployment <owner/repo> is required" "$err" && echo true || echo false)"
check "reads nothing" "$(is "$(wc -l <"$calls")" 0)"
run_bare -- --listeners "$commit"
check "with --listeners too: exits 2" "$(is "$status" 2)"
check "with --listeners too: reads nothing" "$(is "$(wc -l <"$calls")" 0)"

echo "case: a --deployment that is not owner/repo"
new_case bad-deployment
run_bare -- --deployment example-org "$commit"
check "exits 2" "$(is "$status" 2)"
check "names what it got" "$(grep -qF -- "--deployment example-org is not <owner>/<repo>" "$err" && echo true || echo false)"
check "reads nothing" "$(is "$(wc -l <"$calls")" 0)"

echo "case: a --dotfiles without --clients, which only --clients reads"
new_case dotfiles-without-clients
run_bare -- --deployment "$deployment" --dotfiles /srv/dotfiles "$commit"
check "exits 2" "$(is "$status" 2)"
check "says only --clients reads it" "$(grep -qF -- "--dotfiles is read only by --clients" "$err" && echo true || echo false)"
check "reads nothing" "$(is "$(wc -l <"$calls")" 0)"

echo "case: a --deployment whose value is left out"
new_case deployment-no-value
run_bare -- --deployment "$commit"
check "exits 2" "$(is "$status" 2)"
check "says the flag took the commit and no commit follows" \
  "$(grep -qF -- "no <commit> follows the flags; --deployment took ${commit} as its <owner/repo>" "$err" && echo true || echo false)"
check "reads nothing" "$(is "$(wc -l <"$calls")" 0)"

echo "case: --deployment=<owner/repo> is read as --deployment <owner/repo>"
new_case deployment-equals
legion_stack "$fleet"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_bare -- "--deployment=${deployment}" "$commit"
check "exits 0" "$(is "$status" 0)"
check "reads the deployment it names" \
  "$(grep -qF "gh api repos/${deployment}/contents/legion/Pulumi.prod.yaml" "$calls" && echo true || echo false)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

# Each stack's config keys are under the name its own Pulumi.yaml declares: the gate reads that name
# and never guesses one. A stack whose Pulumi.yaml is missing or declares no name is UNKNOWN, by name.
echo "case: the projects list is read under the name legion/Pulumi.yaml declares"
new_case project-name
project legion example-other-legion
legion_stack "$fleet" example-other-legion
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 0" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"
check "reads its stack files from the deployment, and from nothing else" \
  "$(grep -qF "gh api repos/${deployment}/contents/legion/Pulumi.yaml" "$calls" && grep -qF "gh api repos/${deployment}/contents/legion/Pulumi.prod.yaml" "$calls" && ! grep -F 'gh api repos/' "$calls" | grep -vF '/compare/' | grep -qvF "gh api repos/${deployment}/contents/" && echo true || echo false)"

echo "case: the deployment read is the one --deployment names, not a fixed one"
new_case another-deployment
legion_stack "$fleet"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_bare FAKE_DEPLOYMENT=example-other-org/example-other-repo -- --deployment example-other-org/example-other-repo "$commit"
check "exits 0" "$(is "$status" 0)"
check "reads its stack files from that repository" \
  "$(grep -qF "gh api repos/example-other-org/example-other-repo/contents/legion/Pulumi.prod.yaml" "$calls" && echo true || echo false)"
check "never reads the default test repository" "$(lacks "repos/${deployment}/" "$calls")"

echo "case: keys under another project's name are not the stack's"
new_case project-mismatch
legion_stack "$fleet" example-other-legion
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming both places it looked" \
  "$(has_line "UNKNOWN: ${deployment} declares no project: legion/projects/ does not exist, and legion/Pulumi.prod.yaml holds no ${legion_project}:projects list")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

# Production keeps one file per project in legion/projects/ (<nn>-<KEY>.yaml, one mapping each,
# read in file-name order) and no projects list in legion/Pulumi.prod.yaml. A gate that read only
# the list would say UNKNOWN about every deployment of that shape.
echo "case: legion/projects/ holds the projects and legion/Pulumi.prod.yaml no list (production)"
new_case production-shape
legion_settings
fleet_files
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 0" "$(is "$status" 0)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"
check "reads LINUX's file" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})")"
check "reads INFRA's file" "$(has_line "PASS: INFRA runs ${identical_rev} (identical to ${commit})")"
check "skips SMOKE by name" "$(has_line "SKIPPED: SMOKE runs no daemon (replicas: 0)")"
check "skips QUIET by name" "$(has_line "SKIPPED: QUIET runs no daemon (daemon: false)")"
check "lists the directory in the named deployment" \
  "$(grep -qF "gh api repos/${deployment}/contents/legion/projects --jq" "$calls" && echo true || echo false)"

# The deployment's legion program reads legion/projects/ alone once the directory exists: a list
# left in legion/Pulumi.prod.yaml then names pins no daemon runs, so the gate reads it no more.
echo "case: a deployment with legion/projects/ is read from its files alone, a leftover list unread"
new_case both-shapes
legion_stack "    - key: LINUX
      replicas: 1
      daemonImage: ${worker_b}
    - key: OLDONLY
      replicas: 1
      daemonImage: ${worker_b}"
project_file 20-LINUX "key: LINUX
replicas: 1
daemonImage: ${worker_a}"
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- "$commit"
check "exits 0" "$(is "$status" 0)"
check "LINUX is judged by its file's image" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})")"
check "LINUX is judged once" "$(is "$(grep -c '^PASS: LINUX ' "$out")" 1)"
check "the leftover list's project is not read" "$(lacks "OLDONLY" "$out")"
check "the leftover list's image is never read" "$(lacks "$worker_b" "$calls")"

echo "case: legion/projects/ exists and holds no file"
new_case project-directory-empty
legion_stack "$fleet"
mkdir -p "${case_root}/files/legion/projects"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the empty directory" \
  "$(has_line "UNKNOWN: ${deployment} declares no project: legion/projects/ holds no file")"
check "never falls back to the list" "$(lacks "docker" "$calls")"

echo "case: a project file holding a date, which YAML reads and JSON has no type for"
new_case project-file-date
legion_settings
project_file 20-LINUX "key: LINUX
replicas: 1
since: 2026-11-20
daemonImage: ${worker_a}"
single "$worker_a" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- "$commit"
check "exits 0" "$(is "$status" 0)"
check "LINUX is read and compared" "$(has_line "PASS: LINUX runs ${ahead_rev} (ahead of ${commit})")"

echo "case: a file of legion/projects/ that is not one mapping"
new_case project-file-not-mapping
legion_settings
fleet_files
project_file 30-ODD "- key: ODD"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file" \
  "$(has_line "UNKNOWN: ${deployment} legion/projects/30-ODD.yaml is not one mapping of a project's settings")"
check "reads no image" "$(lacks "docker" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: legion/projects/ cannot be listed"
new_case project-listing-down
legion_settings
fleet_files
run_gate FAKE_LIST_DOWN=1 -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the listing and GitHub's answer" \
  "$(has_line "UNKNOWN: cannot list ${deployment} legion/projects: gh: Server Error (HTTP 500)")"
check "reads no image" "$(lacks "docker" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

# A failed listing is never read as a missing directory: with a list left in legion/Pulumi.prod.yaml,
# a gate that took the 500 for a 404 would read that list and judge pins no daemon runs.
echo "case: legion/projects/ cannot be listed, with a list left in legion/Pulumi.prod.yaml"
new_case project-listing-down-leftover
legion_stack "$fleet"
fleet_files
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate FAKE_LIST_DOWN=1 -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the listing and GitHub's answer" \
  "$(has_line "UNKNOWN: cannot list ${deployment} legion/projects: gh: Server Error (HTTP 500)")"
check "never reads the leftover list's pins" "$(lacks "docker" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

# A failed listing whose words quote a 404 is no missing directory either: only gh's own last line
# for a 404 (`gh: <message> (HTTP 404)`) is one, so a 502 that quotes an upstream's 404 is refused,
# as the 500 above is, and the leftover list is never read.
echo "case: legion/projects/ cannot be listed, the refusal quoting an HTTP 404 it is not"
new_case project-listing-quotes-404
legion_stack "$fleet"
fleet_files
single "$worker_a" "$legion" "$ahead_rev"
single "$worker_b" "$legion" "$identical_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate FAKE_LIST_DOWN=1 "FAKE_LIST_DOWN_TEXT=gh: the proxy answered HTTP 404 from its upstream (HTTP 502)" -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the listing and GitHub's answer" \
  "$(has_line "UNKNOWN: cannot list ${deployment} legion/projects: gh: the proxy answered HTTP 404 from its upstream (HTTP 502)")"
check "never reads the leftover list's pins" "$(lacks "docker" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: a project file that is not UTF-8"
new_case project-file-not-utf8
legion_settings
fleet_files
printf 'key: \xff\xfe\n' >"${case_root}/files/legion/projects/30-ODD.yaml"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file" "$(has_line "UNKNOWN: ${deployment} legion/projects/30-ODD.yaml is not UTF-8 text")"
check "reads no image" "$(lacks "docker" "$calls")"

echo "case: a projects list whose entry is not a mapping"
new_case list-entry-not-mapping
legion_stack "    - key: LINUX
      replicas: 1
      daemonImage: ${worker_a}
    - just a string"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the list and the entry" \
  "$(has_line "UNKNOWN: ${deployment} legion/Pulumi.prod.yaml's ${legion_project}:projects list holds an entry that is not a mapping (entry 2)")"
check "reads no image" "$(lacks "docker" "$calls")"

echo "case: legion/Pulumi.yaml is missing"
new_case no-project-file
legion_stack "$fleet"
rm "${case_root}/files/legion/Pulumi.yaml"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file" "$(has_line "UNKNOWN: cannot read ${deployment} legion/Pulumi.yaml: gh: Not Found (HTTP 404)")"
check "reads no image" "$(lacks "docker" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: legion/Pulumi.yaml declares no name"
new_case no-project-name
legion_stack "$fleet"
printf 'runtime: go\n' >"${case_root}/files/legion/Pulumi.yaml"
run_gate -- "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the file" \
  "$(has_line "UNKNOWN: ${deployment} legion/Pulumi.yaml declares no project name, so no key of legion/Pulumi.prod.yaml is known to be its own")"
check "reads no image" "$(lacks "docker" "$calls")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"
