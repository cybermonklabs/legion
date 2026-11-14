#!/usr/bin/env bash
# The fault rig of ci.yaml's download-retry and download-retry-images jobs: it starts
# .github/scripts/ci-fault-proxy.py in front of one step, reads the proxy's log after that step,
# and prints and checks what the retry did (dispatch://INFRA-1545, T7). Five verbs:
#
#   start <name> --mode <recover|stall> [--hosts <csv>] [--recover-after <s>] [--bind <address>]
#       Starts a proxy of its own for the step that follows, waits for its port, and writes
#       `proxy`, `port` and `started` to $GITHUB_OUTPUT, so the step names the proxy in its own
#       `env:` and nowhere else. One proxy per step and not one per job, because a recovery
#       window is counted from the proxy's first moment: a second step under the first step's
#       proxy would run after the window closed and prove nothing.
#
#   report <name> --what <phrase> --outcome <the step's outcome> --attempts <n> --floor <s>
#          --budget <s>
#       Reads that proxy's log and says, in lines a person reading the run can count, which site
#       it was, how many attempts reached the proxy, the offset of each from the first, the
#       seconds between the first attempt and the last, and the step's own elapsed seconds
#       against its budget. Then it checks what the mode promises (below) and stops the proxy.
#
#   build <name|none> --file <Dockerfile> --tag <tag> [--build-arg K=V]... [--secret <spec>]...
#       `docker buildx build` of one Dockerfile, with <name>'s proxy passed in as the build's
#       http_proxy and https_proxy build arguments, and `none` for a build that gets no proxy.
#       The build is here, in a script, rather than in the job's `run:` because
#       .github/scripts/check-image-trigger-paths.sh refuses any image build it can read in a
#       run step and cannot be told otherwise, and docker/build-push-action — the one form it
#       reads — would then hold this job's trigger paths to everything the three builds read,
#       which is the wide filter the narrow gating exists to avoid. Nothing here pushes, logs in
#       to a registry or tags for one: every build is `--load`, into the runner's own daemon,
#       and the runner is discarded. worker-image.yaml and ci.yaml's envoy-smoke job are where
#       these images are built for real, through that action, and their triggers are what
#       check-image-trigger-paths.sh proves.
#
#   probe <name>
#       Two requests through that proxy, made before the site's own fetch and printed as the
#       pair they read: one to a host the fault names, which must be refused, and one to a host
#       off that list, which must be answered. Each is checked twice over — what the client read
#       (the HTTP status and the seconds it took) and whether the proxy logged a line for it at
#       all — and the verb fails the step, naming which of the two readings was wrong, when the
#       pair reads the other way round.
#
#       It exists because a bypass passes a fault drive in about no time while proving nothing.
#       A pod of this build carries `NO_PROXY=*` and `no_proxy=*`, which every client honouring
#       either one reads as "no host goes through the proxy": the site then fetches from the
#       real host, succeeds, and the report sees a step that never met the fault. The probe is
#       made the way the site makes its own fetch, with http_proxy and https_proxy and nothing
#       else, so whatever bypasses the site bypasses the probe — and a bypassed request leaves
#       no line in the proxy's log, which is the reading that catches it whatever caused it.
#       Both jobs clear the bypass for every step of the job (ci.yaml, "Clear the proxy bypass
#       this runner image may carry"); this verb is what proves the clearing worked, per site.
#
#       The faulted target is the first host of that proxy's own `--hosts`, so the probe reads
#       the fault the site will meet and not some other one. The unfaulted target is that same
#       host under the reserved `.invalid` suffix (RFC 2606): it is derived from `--hosts`
#       rather than named anew, it can never match the fault's list — `github.com.invalid`
#       neither equals `github.com` nor ends in `.github.com` — nobody owns it, and no resolver
#       answers for it, so only the proxy can answer it. Through the proxy it reads HTTP 502
#       (the proxy's own answer, having failed to resolve it), and through a bypass it reads
#       nothing at all. A proxy started with an empty `--hosts` faults every host, so it has no
#       unfaulted host to read: there the verb says so and the faulted reading stands alone.
#
#       The probe's own two requests are the proxy's first lines, and `report` leaves them out
#       of the site's counts: the state file carries the line count they reached.
#
#       Two things here are worth reusing in any fault rig, and both are about not depending on
#       a client's behaviour. Read every probe twice — what the client read, and whether the
#       proxy logged a line for the request at all. The second reading is the one that survives
#       a client nobody has thought of: a bypassed request leaves no line whatever caused it,
#       whether it was a variable, a client's own proxy configuration or a client honouring only
#       CONNECT. And derive the unfaulted control from the fault's own input rather than naming
#       a host of its own, so the control cannot drift from what it controls for: here both
#       targets come from the same `--hosts`, so a change to the fault moves the control with it
#       and no edit can leave them describing different things.
#
#   clear
#       Kills any proxy still running and refuses the job if a proxy variable carries a value in
#       its environment: the rig writes none to $GITHUB_ENV, so the steps after it — the ci-ok
#       report and the upload inside it — must see none. The one verb that names no proxy,
#       because it is about all of them. An emptied `no_proxy=` is not a proxy: it is how both
#       jobs clear the bypass above, so it is printed as accounted for rather than refused, and
#       the refusal is held to a variable that carries a value.
#
# What `report` checks, which is what each of the three modes claims:
#   recover  the step succeeded, the proxy refused at least one attempt (so the fault was in the
#            site's path at all) and passed at least one on (so the site recovered through the
#            same proxy), inside the budget.
#   stall    the step failed, which is the retry exhausting its five attempts against a host
#            that answers nothing; the proxy was reached at least once per attempt; the step
#            took at least --floor, the loop's own sleeps, so every attempt really happened
#            rather than the site failing fast on something else; and it ended inside --budget,
#            which is what a per-attempt bound buys — the step ends instead of consuming the
#            job. Each attempt's own duration is printed, against the bound, rather than
#            asserted: a client with a timeout shorter than the bound ends sooner and still
#            retries, and the budget above is what the check is for.
#   fail     the step succeeded and the proxy refused nothing at all, because the site never
#            asked the faulted hosts. That is the shape of a dependency the tree carries rather
#            than fetches: those hosts answer 504 for the whole step and the install does not
#            notice. A request to a host outside --hosts is passed on and counted, since an
#            install reads the npm registry whatever this proves.
#
# `report` refuses a site no `probe` ran for, whatever the mode: the pair a probe read is half
# the evidence a fault drive is read by, so a site that recorded none has none.
#
# Every path it writes goes under $RUNNER_TEMP (or $TMPDIR off a runner), and each name's state
# lives in a file of its own, so two sites in one job never read each other's proxy.
#
# Run from anywhere; the repository root is derived from this script's own path.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$script_dir/../.." && pwd)
state_dir="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/download-retry"

# The hosts `legion probe-image`'s TLS trust probe offers each client, which the image build's
# fault leaves alone: two chains it must refuse and the control it must accept. They are read
# from packages/daemon/internal/capabilities, which that command runs, and the `build` verb
# below says why a fault over them proves nothing and breaks the probe.
TRUST_PROBE_HOSTS="example.com,.example.com,we-have-a-problem.com,.we-have-a-problem.com"

# The probe's own bound, in seconds, for each of its two requests. A refusal under --mode
# recover or --mode fail is an immediate 504 and a pass is an immediate 502, so this bound is
# only ever spent under --mode stall, where spending it whole is the reading: a stalled request
# ends with no answer at all after the client's own bound, where a bypassed one is answered in
# about no time. Both requests are bounded, so a probe costs at most 2 x this, and that is the
# term each site's --budget in ci.yaml carries for it.
PROBE_BOUND=10

# RFC 2606's reserved suffix, which the probe hangs on a faulted host to make its unfaulted one.
# Nobody owns a name under it and no resolver answers for one, so only the proxy can answer a
# request for it, and a request that was not made through the proxy reads as nothing at all.
RESERVED_SUFFIX=".invalid"

die() {
  echo "::error::download-retry: $*" >&2
  exit 1
}

state_file() {
  printf '%s/%s.state\n' "$state_dir" "$1"
}

# The docker bridge's gateway, which is the runner as a `RUN` of a build sees it: the build
# container's default route. Named rather than written as 172.17.0.1, which is only the usual
# value of a daemon's default pool.
docker_bridge_gateway() {
  local address
  address=$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')
  [ -n "$address" ] || die "the docker bridge names no gateway, so a build cannot reach the proxy"
  printf '%s\n' "$address"
}

start() {
  local name=$1 mode="" hosts="" recover_after=60 bind=127.0.0.1
  shift
  while [ "$#" -gt 0 ]; do
    case $1 in
      --mode) mode=$2; shift 2 ;;
      --hosts) hosts=$2; shift 2 ;;
      --recover-after) recover_after=$2; shift 2 ;;
      --bind) bind=$2; shift 2 ;;
      *) die "start: unknown argument '$1'" ;;
    esac
  done
  [ -n "$mode" ] || die "start: --mode is required"
  [ "$bind" != docker-bridge ] || bind=$(docker_bridge_gateway)

  mkdir -p "$state_dir"
  local log="$state_dir/$name.log" port_file="$state_dir/$name.port" port="" pid=""
  rm -f "$log" "$port_file"
  # Each line of the proxy's log is stamped with the second it was written, which is what makes
  # the attempts countable afterwards: the proxy prints one line per request it refuses or
  # stalls, and the stamps turn those lines into a schedule. The stamping is a bash `read` loop
  # rather than an awk one, because mawk — the awk of the runner image and of this repository's
  # pods — holds its input in a block buffer, and a log that appears only once the proxy exits
  # is a log the step after it cannot read. $EPOCHSECONDS is bash's own clock, so no line costs
  # a process.
  (
    python3 "$root/.github/scripts/ci-fault-proxy.py" \
      --mode "$mode" --hosts "$hosts" --recover-after "$recover_after" \
      --bind "$bind" --port 0 --port-file "$port_file" 2>&1 \
      | while IFS= read -r line; do printf '%s %s\n' "$EPOCHSECONDS" "$line"; done > "$log"
  ) &
  local started
  started=$(date +%s)
  # The port file is written whole, by rename, before the proxy serves a request, and the
  # recovery window starts the moment it appears (ci-fault-proxy.py's header).
  local waited=0
  while [ ! -s "$port_file" ]; do
    sleep 0.1
    waited=$((waited + 1))
    [ "$waited" -lt 300 ] || { cat "$log" >&2 || true; die "start: the $name proxy reported no port in 30 s"; }
  done
  port=$(cat "$port_file")
  # The proxy announces its port before it logs the line that carries its pid, which `report`
  # stops it by, so the pid is waited for too rather than read from a log that is still empty.
  while [ -z "$pid" ]; do
    pid=$(awk '{ for (i = 1; i <= NF; i++) if ($i ~ /^pid=/) { sub(/^pid=/, "", $i); print $i; exit } }' "$log")
    [ -n "$pid" ] && break
    sleep 0.1
    waited=$((waited + 1))
    [ "$waited" -lt 300 ] || { cat "$log" >&2 || true; die "start: the $name proxy logged no pid in 30 s"; }
  done

  {
    echo "name=$name"
    echo "mode=$mode"
    echo "hosts=$hosts"
    echo "log=$log"
    echo "pid=$pid"
    echo "started=$started"
    echo "proxy=http://$bind:$port"
  } > "$(state_file "$name")"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    {
      echo "proxy=http://$bind:$port"
      echo "port=$port"
      echo "started=$started"
    } >> "$GITHUB_OUTPUT"
  fi
  echo "download-retry: the $name fault proxy is http://$bind:$port (--mode $mode, faulting ${hosts:-every host})"
}

# One request through the proxy, made the way the site makes its own fetch: the two variables
# the site's `env:` carries and nothing else, so whatever would carry the site past the fault
# carries this request past it too. Printed as `<status> <seconds>`, with a status of 000 where
# nothing answered at all. Its failure is the reading rather than a fault, so its error is
# silenced and its status consumed: the shape check-download-retry.sh reads as a probe, which is
# what keeps a reading of the fault out of every budget that runs this script.
through_proxy() {
  local proxy=$1 url=$2 reading
  reading=$(http_proxy="$proxy" https_proxy="$proxy" curl -sS --max-time "$PROBE_BOUND" \
    -o /dev/null -w '%{http_code} %{time_total}' "$url" 2>/dev/null || true)
  printf '%s\n' "${reading:-000 0}"
}

# One reading, in the words a person reading the run counts it in.
what_it_read() {
  if [ "$1" = 000 ]; then
    printf 'no answer at all, after %ss\n' "$2"
  else
    printf 'HTTP %s, after %ss\n' "$1" "$2"
  fi
}

# Whether a request spent its whole bound, which under --mode stall is the refusal itself: the
# proxy accepts the connection and sends nothing, so the client ends at its own bound. A
# bypassed request is answered in about no time, and the seconds are what tell the two apart.
spent_its_bound() {
  awk -v spent="$1" -v bound="$PROBE_BOUND" 'BEGIN { exit !(spent >= bound - 1) }'
}

# The line the proxy wrote for the request just made, or nothing where it wrote none. The log is
# `start`'s stamping loop, a line at a time, so a line lands a moment after the answer it belongs
# to and is waited for rather than missed; a request that never reached the proxy leaves none
# however long this waits, which is the reading that catches a bypass whatever caused it.
proxy_line_after() {
  local log=$1 skip=$2 waited=0 line=""
  while [ -z "$line" ] && [ "$waited" -lt 50 ]; do
    line=$(awk -v skip="$skip" 'NR > skip { print; exit }' "$log")
    [ -n "$line" ] || sleep 0.1
    waited=$((waited + 1))
  done
  printf '%s\n' "$line"
}

probe() {
  local name=$1
  shift
  [ "$#" -eq 0 ] || die "probe: takes the proxy's name and nothing else"
  local file
  file=$(state_file "$name")
  [ -f "$file" ] || die "probe: no proxy named $name was started"
  local mode hosts log proxy
  # shellcheck disable=SC1090 # the file this verb's own `start` wrote, seven name=value lines
  . "$file"

  local faulted control
  if [ -n "$hosts" ]; then
    faulted=${hosts%%,*}
    faulted=${faulted#.}
    control="$faulted$RESERVED_SUFFIX"
  else
    # This proxy names no host, so it faults every one of them and has no unfaulted host left to
    # read. A name nobody owns is as faulted here as any other, and it is one no bypass can
    # answer for, since no resolver answers for it.
    faulted="every-host$RESERVED_SUFFIX"
    control=""
  fi

  echo "download-retry: the probe through the $name proxy, before the site (--mode $mode)"
  local problems=0
  probe_wrong() {
    echo "::error::download-retry: the probe before $name: $*" >&2
    problems=$((problems + 1))
  }
  local before reading code seconds line pair

  before=$(wc -l < "$log")
  reading=$(through_proxy "$proxy" "http://$faulted/")
  code=${reading%% *}
  seconds=${reading##* }
  line=$(proxy_line_after "$log" "$before")
  pair="the faulted host $faulted read $(what_it_read "$code" "$seconds")"
  echo "  $pair"
  echo "  the proxy's own line for it: ${line:-none at all}"
  if [ "$mode" = stall ]; then
    if [ "$code" != 000 ] || ! spent_its_bound "$seconds"; then
      probe_wrong "the faulted reading was wrong. This proxy accepts a connection for $faulted" \
        "and then sends nothing, so the request must end at its own ${PROBE_BOUND}s bound with no" \
        "answer; it read $(what_it_read "$code" "$seconds"). A faulted host that answers is the" \
        "proxy bypassed, which is what this probe is for: read no_proxy and NO_PROXY in this" \
        "job's environment, since either one set to a star sends every host past the proxy."
    fi
  elif [ "$code" != 504 ]; then
    probe_wrong "the faulted reading was wrong. This proxy answers HTTP 504 for $faulted, so the" \
      "request must be refused with one; it read $(what_it_read "$code" "$seconds"). A faulted" \
      "host that answers is the proxy bypassed, which is what this probe is for: read no_proxy" \
      "and NO_PROXY in this job's environment, since either one set to a star sends every host" \
      "past the proxy."
  fi
  if [ -z "$line" ]; then
    probe_wrong "the faulted reading was wrong: the proxy logged no line for the request to" \
      "$faulted, so that request was never made through it at all."
  fi

  if [ -n "$control" ]; then
    before=$(wc -l < "$log")
    reading=$(through_proxy "$proxy" "http://$control/")
    code=${reading%% *}
    seconds=${reading##* }
    line=$(proxy_line_after "$log" "$before")
    pair="$pair, and the unfaulted host $control read $(what_it_read "$code" "$seconds")"
    echo "  the unfaulted host $control read $(what_it_read "$code" "$seconds")"
    echo "  the proxy's own line for it: ${line:-none at all}"
    if [ "$code" = 000 ] || [ "$code" = 504 ]; then
      probe_wrong "the unfaulted reading was wrong. $control is off this proxy's --hosts, so the" \
        "proxy must answer it — HTTP 502, the answer it gives for a name nothing resolves —" \
        "rather than fault it; it read $(what_it_read "$code" "$seconds"). Nothing answering at" \
        "all is the request going nowhere near the proxy, and a 504 is the fault reaching a host" \
        "it was never given."
    fi
    if [ -z "$line" ]; then
      probe_wrong "the unfaulted reading was wrong: the proxy logged no line for the request to" \
        "$control, so that request was never made through it at all."
    fi
  else
    pair="$pair; this fault names no host, so every host is faulted and it has none to read unfaulted"
    echo "  no unfaulted host to read: this proxy's --hosts is empty, so it faults every host"
  fi

  [ "$problems" -eq 0 ] || exit 1
  echo "  the pair reads as it must, so this site's fetch meets the fault rather than the host"
  # What `report` prints beside its result, and the count of the log lines these two requests
  # left, which are the proxy's first and belong to no site.
  {
    printf 'probe_lines=%s\n' "$(wc -l < "$log")"
    printf "probe_pair='%s'\n" "$pair"
  } >> "$file"
}

report() {
  local name=$1 what="" outcome="" attempts=1 floor=0 budget=0
  shift
  while [ "$#" -gt 0 ]; do
    case $1 in
      --what) what=$2; shift 2 ;;
      --outcome) outcome=$2; shift 2 ;;
      --attempts) attempts=$2; shift 2 ;;
      --floor) floor=$2; shift 2 ;;
      --budget) budget=$2; shift 2 ;;
      *) die "report: unknown argument '$1'" ;;
    esac
  done
  local file
  file=$(state_file "$name")
  [ -f "$file" ] || die "report: no proxy named $name was started"
  local mode log pid started probe_pair probe_lines
  # shellcheck disable=SC1090 # the file this verb's own `start` and `probe` wrote, name=value lines
  . "$file"
  [ -n "${probe_pair:-}" ] ||
    die "report: no probe ran before $name's site, so this result would carry no reading of its own. Every fault drive records the pair a probe read through the proxy before the site (dispatch://INFRA-1545, the Verification document's proxy clauses): run \`download-retry-proof.sh probe $name\` in the step that starts the proxy."
  local now elapsed
  now=$(date +%s)
  elapsed=$((now - started))

  # The probe's own two requests are the proxy's first lines and belong to no site, so they are
  # left out of everything below: they would count as an attempt this site never made, and under
  # --mode fail — where the promise is that the proxy refused nothing — the probe's own refusal
  # would be read as the site fetching from a host the tree is supposed to carry.
  local site_log="$log.site"
  awk -v skip="${probe_lines:-0}" 'NR > skip' "$log" > "$site_log"

  local faults passes fault_count first last fault_line pass_line
  # One line per request the proxy refused or stalled, as offsets from the step's start, and the
  # same for the ones it passed on. An attempt may open more than one connection — a package
  # manager fetches in parallel — so these are requests, not attempts; the count below is held
  # to at least one per attempt, and the offsets are what a reader counts the attempts by.
  faults=$(awk -v t="$started" '$3 == "504" || $3 == "stall" { n++; if (n <= 40) printf "+%ds ", $1 - t }
                                END { if (n > 40) printf "and %d more", n - 40 }' "$site_log")
  passes=$(awk '$3 == "pass"' "$site_log" | wc -l)
  fault_count=$(awk '$3 == "504" || $3 == "stall"' "$site_log" | wc -l)
  first=$(awk -v t="$started" '$3 == "504" || $3 == "stall" { print $1 - t; exit }' "$site_log")
  last=$(awk -v t="$started" '$3 == "504" || $3 == "stall" { v = $1 - t } END { if (v != "") print v }' "$site_log")
  # The proxy's own words for this site's request, which a count cannot say: which host it was
  # asked for, and whether the 504 it injected answered a CONNECT or a plain request, since a
  # client retries the two differently.
  fault_line=$(awk '$3 == "504" || $3 == "stall" { print; exit }' "$site_log")
  pass_line=$(awk '$3 == "pass" || $3 == "502" { print; exit }' "$site_log")

  echo "download-retry: $what, under --mode $mode"
  echo "  the step's own outcome: $outcome"
  echo "  the probe before this site read: $probe_pair"
  echo "  the proxy was reached $fault_count time(s) while it faulted, at ${faults:-no offset at all}"
  if [ -n "$first" ] && [ -n "$last" ]; then
    echo "  the first of those was $first s into the step and the last $last s, $((last - first)) s apart"
  fi
  echo "  the proxy's own line for the first request it faulted: ${fault_line:-none at all, it faulted no request of this site}"
  echo "  the proxy's own line for the first request it answered itself: ${pass_line:-none at all, it answered no request of this site itself}"
  echo "  requests the proxy passed on: $passes"
  echo "  it ran ${elapsed}s from the proxy's start, against a budget of ${budget}s and a floor of ${floor}s for its $attempts attempt(s)"

  kill "$pid" 2> /dev/null || true

  local problems=0
  fail() { echo "::error::download-retry: $what: $*" >&2; problems=$((problems + 1)); }
  case $mode in
    recover)
      [ "$outcome" = success ] || fail "the step did not succeed, so the site did not recover from the fault"
      [ "$fault_count" -ge 1 ] || fail "the proxy refused nothing, so the fault was never in this site's path"
      [ "$passes" -ge 1 ] || fail "the proxy passed nothing on, so the site never fetched through it after the window"
      ;;
    stall)
      [ "$outcome" = failure ] || fail "the step did not fail, so its download never reached the stalling proxy"
      [ "$fault_count" -ge "$attempts" ] || fail "the proxy was reached $fault_count time(s), fewer than the $attempts attempts the retry makes"
      [ "$elapsed" -ge "$floor" ] || fail "it ended after ${elapsed}s, under the ${floor}s its attempts' sleeps alone cost, so they did not all run"
      ;;
    fail)
      [ "$outcome" = success ] || fail "the step did not succeed, though every host it reads but the faulted ones answered"
      [ "$fault_count" -eq 0 ] || fail "the proxy refused $fault_count request(s), so this site still fetches from a host the tree is supposed to carry"
      ;;
    *) fail "unknown mode $mode" ;;
  esac
  [ "$elapsed" -le "$budget" ] || fail "it took ${elapsed}s, over its ${budget}s budget"
  [ "$problems" -eq 0 ] || exit 1
  echo "  as this mode promises"
}

build() {
  local name=$1 file="" tag="" proxy="" args=()
  shift
  while [ "$#" -gt 0 ]; do
    case $1 in
      --file) file=$2; shift 2 ;;
      --tag) tag=$2; shift 2 ;;
      --build-arg) args+=(--build-arg "$2"); shift 2 ;;
      --secret) args+=(--secret "$2"); shift 2 ;;
      *) die "build: unknown argument '$1'" ;;
    esac
  done
  [ -n "$file" ] || die "build: --file is required"
  [ -n "$tag" ] || die "build: --tag is required"
  if [ "$name" != none ]; then
    local state
    state=$(state_file "$name")
    [ -f "$state" ] || die "build: no proxy named $name was started"
    proxy=$(sed -n 's/^proxy=//p' "$state")
    [ -n "$proxy" ] || die "build: the $name state names no proxy"
    # Docker's predefined proxy build arguments: BuildKit sets each as an environment variable
    # of every RUN and keeps it out of the cache key, so the retries inside the Dockerfile meet
    # the fault without the Dockerfile naming a proxy anywhere. The address is the docker bridge
    # gateway (`start --bind docker-bridge`), which is the runner as a RUN's own network
    # namespace can route to it; 127.0.0.1 there is the build container itself.
    #
    # no_proxy carries the hosts of the worker image's TLS trust probe, which is no download:
    # `legion probe-image` offers each client a revoked chain and a control and reads which it
    # refuses. Through the proxy, Chromium answered ERR_PROXY_CONNECTION_FAILED for all three,
    # the control among them, so the probe could tell nothing and the build failed at a step
    # this fault has no business reaching (run 42380686327, job 128427571683). Those hosts go
    # direct, which leaves the fault over the downloads alone, which is what it is for.
    args+=(--build-arg "http_proxy=$proxy" --build-arg "https_proxy=$proxy" \
      --build-arg "no_proxy=$TRUST_PROBE_HOSTS" --build-arg "NO_PROXY=$TRUST_PROBE_HOSTS")
  fi
  echo "download-retry: building $file as $tag${proxy:+ through $proxy}"
  # --no-cache: the layers this build must run are the ones that fetch, and a cached layer
  # fetches nothing, so a build that reused one would meet no fault. --load puts the result in
  # the runner's daemon; nothing is pushed and no registry is logged in to.
  docker buildx build --no-cache --load --file "$file" --tag "$tag" "${args[@]}" "$root"
  # The tag exists only once the last stage is exported, so this is the build reaching it.
  docker image inspect "$tag" --format 'download-retry: {{.RepoTags}} reached its last stage, {{.Id}}'
}

# The rig writes no proxy variable to $GITHUB_ENV, so none of them reaches the steps after it —
# the ci-ok report and the upload inside it. This is where that is checked rather than assumed,
# and where a proxy a `report` could not stop is stopped.
#
# What both jobs do write to $GITHUB_ENV is an emptied `no_proxy=` and `NO_PROXY=`, which is how
# they clear the bypass a runner image may carry for every step of the job. An emptied one is no
# proxy and routes nothing: it is the absence of a bypass list, which is what a client with no
# such variable at all reads. So the refusal below is held to a variable that carries a value,
# and an emptied one is printed as what it is. Reading the two the same way would leave this
# check with one of two faults: it would refuse every job that cleared its bypass, or — were the
# pattern loosened to let any no_proxy through — it would go blind to a `no_proxy=registry.
# npmjs.org` that quietly sent the report's own upload around the proxy.
clear() {
  [ "$#" -eq 0 ] || die "clear: takes no argument"
  pkill -f ci-fault-proxy.py || true
  local set_to_something emptied
  set_to_something=$(env | grep -Ei '^(http_proxy|https_proxy|all_proxy|no_proxy)=.' || true)
  emptied=$(env | grep -Ei '^(http_proxy|https_proxy|all_proxy|no_proxy)=$' | sort | tr '\n' ' ' || true)
  if [ -n "$set_to_something" ]; then
    # Two different faults share one refusal, so the message names the kind rather than calling
    # both a proxy: an `http_proxy` left behind routes the ci-ok report through a proxy that is
    # gone, and a `no_proxy` left behind routes it past one that is still there. Either is a
    # variable this job wrote and did not take back.
    echo "::error::the job's environment still carries a proxy variable the ci-ok report would read — an http, https or all proxy it would route through, or a no_proxy it would route past: $set_to_something"
    exit 1
  fi
  echo "no proxy variable carries a value in this job's environment${emptied:+, and these are emptied: $emptied}"
}

verb=${1:-}
# Every verb but `clear` names the proxy it is about; `clear` is about all of them.
[ "$verb" = clear ] || [ "$#" -ge 2 ] ||
  die "usage: $(basename "$0") <start|probe|report|build> <name> [options], or <clear>"
shift
case $verb in
  start) start "$@" ;;
  probe) probe "$@" ;;
  report) report "$@" ;;
  build) build "$@" ;;
  clear) clear "$@" ;;
  *) die "unknown verb '$verb'; it takes start, probe, report, build or clear" ;;
esac
