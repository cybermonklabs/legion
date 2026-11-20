#!/usr/bin/env bash
# Fetch, in a step before a job's tests, what those tests would otherwise fetch while they run.
# A test that reaches the module proxy or the checksum database fails whenever that service
# fails, on a change that touched nothing it tests: in run 44317642380, envoy-infra-go failed
# because a `go install` of helm inside a test could not read sum.golang.org. Here the fetch runs
# once, tried again on failure, from the module cache the job restored, and the tests then find
# what they need and fetch nothing.
#
#   go-fetch.sh file FILE
#     Run each line of FILE as one `deps`, `tools` or `tidy` command below, every line read and
#     checked before the first runs. This is the form a CI job's fetch step and the ci-caches.yaml
#     fill of its cache both run, on the same FILE (.github/go-fetch/<job>.txt), and the key of
#     that cache hashes FILE and this script: a change to what a job fetches is then a change to
#     the key, so the next fill holds it and no run downloads the difference while its tests run.
#     One command a line, `#` starting a comment anywhere on it, blank lines passed over:
#       [GOOS=<os>] [GOARCH=<arch>] [CGO_ENABLED=<0|1>] [GOWORK=off] deps DIR [-test] [-tags T] PACKAGE...
#       tools FILE
#       tidy DIR
#     DIR and the tools FILE are read from the repository root, whatever directory this runs in.
#     The settings in front of `deps` are the platform a step builds for (a darwin vet, a
#     CGO_ENABLED=0 build): `go list -deps` follows the imports of one GOOS, GOARCH and cgo
#     setting, and another platform's files import modules this one's do not. GOWORK=off is a build
#     of DIR's own module outside the go.work above it, as an image build that copies the module
#     alone makes: its go.mod alone selects the versions, which the workspace's other modules can
#     raise.
#     A `tidy DIR` line downloads what `go mod tidy` of the module in DIR reads, for
#     .github/scripts/check-go-tidy.sh, which runs `go mod tidy -diff` with the proxy off. That is
#     more than any build of the module loads, and no `deps` line can name it: tidy reads every
#     package any file of the module imports under any build tag and for any platform at once,
#     the tests of each of those packages included (cmd/go's modcmd/tidy.go). So the line runs
#     tidy itself, on a copy of the module's go.mod and go.sum (-modfile): the copy takes what
#     tidy writes, and the module's own two files stay as they are, so the check after this step
#     still sees an untidy module as it is. Not `go mod tidy -diff`, which exits 1 for an untidy
#     module: the retry below would read that as a fault of the proxy and try again, and the
#     check, which names the module and its diff, would not run.
#   go-fetch.sh tools FILE
#     `go install` each pin FILE lists, one package path and version per line
#     (`helm.sh/helm/v3/cmd/helm@v3.20.2`; `#` starts a comment), into the directory
#     legion-live-tools under the first entry of `go env GOPATH`: a directory of these tools' own,
#     where the tests that run a tool look it up and check its build against the pin. It is not
#     GOBIN, which a developer keeps on PATH, so the install replaces no helm or nats-server of
#     theirs. Each install reads the module cache first, its version lists included, so a cache
#     that holds the pin's modules installs it with no network (below, at the install).
#   go-fetch.sh deps DIR [-test] PACKAGE...
#     Download into the module cache every module a `go build` of PACKAGE... in DIR needs
#     (`go list -deps`, which compiles nothing), for tests that build those packages while they
#     run with the module proxy turned off (cmdtest.BuildCommand, testbuild.EnvoyBinary). With
#     -test, the modules of their tests too, for a step that runs `go test` of them with the
#     proxy off, as every test step of a required CI job does. Any other flag go list takes
#     (-tags T) is passed on.
#   go-fetch.sh pulumi-consumer SRC PLUGIN
#     Copy the Go program SRC into a directory of its own, run `pulumi package add PLUGIN` there
#     outside any workspace, then `go build ./...`: the resolution host's
#     TestLiveGoConsumersCanUseTheSDK makes, whose `go mod tidy` reads modules no package of this
#     repository loads, so the test can make it again from the module cache with no proxy. Each
#     attempt starts from a fresh copy, since package add writes into its directory.
#
# The proxy. A required CI job runs every step with GOPROXY=off, so a module its restored cache
# lacks fails the step at once and names the module, rather than being downloaded while a test
# runs. This script is the one step of such a job that goes online, so it takes the proxy from
# GO_FETCH_PROXY, which those workflows set to the proxy their go commands used before
# (https://proxy.golang.org|direct), and runs each of its go commands with GOPROXY set to it. With
# GO_FETCH_PROXY unset it uses the GOPROXY its caller gives (ci-caches.yaml's fills, a developer's
# shell); where that is off too, the module cache alone serves the fetch, and it says so.
#
# Each go command is tried up to GO_FETCH_ATTEMPTS times (4), the waits between them starting
# at GO_FETCH_WAIT seconds (15) and doubling, so a fault of up to about two minutes is ridden
# out. It exits 1 when a command fails every attempt, naming it, and when a step of its own fails
# (a consumer copy that could not be made, say), naming that, with no attempt more: that is no
# fault of a service. It exits 2 on a usage error, a FILE line that is no pin, or a fetch FILE
# line that is no command above, before it runs anything.
set -euo pipefail

attempts=${GO_FETCH_ATTEMPTS:-4}
first_wait=${GO_FETCH_WAIT:-15}
# The repository root, two directories above this script, which a fetch FILE's paths are read from.
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)

usage() {
  echo "usage: go-fetch.sh file FILE | go-fetch.sh tools FILE | go-fetch.sh deps DIR PACKAGE... | go-fetch.sh pulumi-consumer SRC PLUGIN" >&2
  exit 2
}

# retry <command...>: runs the command until it succeeds or has failed `attempts` times. Its own
# lines go to stderr, so a caller may discard the command's output and still see each attempt.
retry() {
  local n=1 wait=$first_wait
  while :; do
    echo "go-fetch: $* (attempt $n of $attempts)" >&2
    if "$@"; then
      return 0
    fi
    if [ "$n" -ge "$attempts" ]; then
      echo "::error::go-fetch: failed $attempts times: $*" >&2
      return 1
    fi
    echo "go-fetch: $* failed; trying again in ${wait}s" >&2
    sleep "$wait"
    n=$((n + 1))
    wait=$((wait * 2))
  done
}

# read_pins FILE: the pins of a tools FILE, one a line, into the array `pins`; exits 2 naming the
# line that is no pin, and when the file pins nothing.
read_pins() {
  local file=$1 line pin n=0
  [ -f "$file" ] || { echo "go-fetch: no file $file" >&2; exit 2; }
  pins=()
  while IFS= read -r line || [ -n "$line" ]; do
    n=$((n + 1))
    pin=${line%%#*}
    pin=${pin#"${pin%%[![:space:]]*}"}
    pin=${pin%"${pin##*[![:space:]]}"}
    [ -n "$pin" ] || continue
    # A package path and a semantic version: a query such as @latest would install whatever
    # the proxy answers that day, which no test's check of the build would then match.
    if ! [[ "$pin" =~ ^[^@[:space:]]+@v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$ ]]; then
      echo "go-fetch: $file:$n: $pin is not a package path and a semantic version, path@vX.Y.Z" >&2
      exit 2
    fi
    pins+=("$pin")
  done < "$file"
  [ "${#pins[@]}" -gt 0 ] || { echo "go-fetch: $file pins no tool" >&2; exit 2; }
}

install_tools() {
  local file=$1 goenv gopath modcache goproxy tools proxy pin
  read_pins "$file"
  # go-offline: this is the fetch step itself, run before the tests and tried again on failure.
  goenv=$(go env GOPATH GOMODCACHE GOPROXY)
  gopath=$(sed -n 1p <<<"$goenv") modcache=$(sed -n 2p <<<"$goenv") goproxy=$(sed -n 3p <<<"$goenv")
  [ -n "${gopath%%:*}" ] || { echo "go-fetch: go env GOPATH names no directory to install the tools under" >&2; exit 1; }
  [ -n "$modcache" ] || { echo "go-fetch: go env GOMODCACHE names no module cache" >&2; exit 1; }
  tools="${gopath%%:*}/legion-live-tools"
  # `go install PATH@VERSION` asks for the module's version list even at an exact pin: it reads
  # the go.mod of the module's latest version for a deprecation notice and for retractions
  # (cmd/go's load.PackagesAndErrorsOutsideModule, modload.CheckDeprecation). From the proxy
  # the job names, that is a network request on every run, which failed this step whenever the
  # network did even with every module in the cache the job restored (with GOPROXY=off the
  # install refuses: "loading deprecation for helm.sh/helm/v3: module lookup disabled"). So the
  # proxy list starts with the module cache's own download directory, which serves the version
  # lists and the modules the cache holds, and the fetch's proxy follows it: a `,` passes a
  # request on only after a 404 or 410, which a file proxy answers for a file it does not hold, so
  # the network is asked only for what the cache lacks. The cost: a deprecation or retraction
  # published after the cache was filled is not read, which an exact pin a person reviews makes
  # no difference to.
  proxy="file://$modcache/cache/download,${GO_FETCH_PROXY:-${goproxy:-https://proxy.golang.org,direct}}"
  echo "go-fetch: installing ${#pins[@]} tools into $tools, the module cache $modcache first" >&2
  for pin in "${pins[@]}"; do
    # go-offline: this is the fetch step itself, run before the tests and tried again on failure.
    GOBIN=$tools GOPROXY=$proxy retry go install "$pin"
  done
}

list_deps() {
  local dir=$1
  shift
  [ -d "$dir" ] || { echo "go-fetch: no directory $dir" >&2; exit 2; }
  cd "$dir"
  # go-offline: this is the fetch step itself, run before the tests and tried again on failure.
  retry go list -deps "$@" > /dev/null
}

tidy_module() {
  local dir=$1
  [ -f "$dir/go.mod" ] || { echo "go-fetch: no go.mod in $dir" >&2; exit 2; }
  copy=$(mktemp -d) || { echo "::error::go-fetch: could not make a directory for a copy of $dir/go.mod" >&2; exit 1; }
  trap 'rm -rf "$copy"' EXIT
  cd "$dir"
  retry tidy_on_copy "$dir"
}

# tidy_on_copy DIR: one attempt of tidy_module, in DIR. A function that `retry` calls in an `if`
# runs with errexit off, so each command of its own preparation says so itself, and a failure of
# one ends the step at once, no fault of a service.
tidy_on_copy() {
  cp go.mod "$copy/go.mod" || { echo "::error::go-fetch: could not copy $1/go.mod into $copy" >&2; exit 1; }
  rm -f "$copy/go.sum" || { echo "::error::go-fetch: could not clear $copy/go.sum" >&2; exit 1; }
  if [ -f go.sum ]; then
    cp go.sum "$copy/go.sum" || { echo "::error::go-fetch: could not copy $1/go.sum into $copy" >&2; exit 1; }
  fi
  # go-offline: this is the fetch step itself, run before the tests and tried again on failure.
  go mod tidy -modfile="$copy/go.mod" > /dev/null
}

# The settings a fetch FILE line may put in front of `deps`, and what each may be set to.
PLATFORM_SETTING='^(GOOS|GOARCH)=[a-z0-9]+$|^CGO_ENABLED=[01]$|^GOWORK=off$'

run_file() {
  local file=$1 line text n=0 i
  local -a words settings commands=() where=()
  [ -f "$file" ] || { echo "go-fetch: no file $file" >&2; exit 2; }
  while IFS= read -r line || [ -n "$line" ]; do
    n=$((n + 1))
    text=${line%%#*}
    read -ra words <<<"$text"
    [ "${#words[@]}" -gt 0 ] || continue
    settings=()
    while [ "${#words[@]}" -gt 0 ] && [[ ${words[0]} == *=* ]]; do
      if ! [[ ${words[0]} =~ $PLATFORM_SETTING ]]; then
        echo "go-fetch: $file:$n: ${words[0]} is no setting a fetch line may give: GOOS, GOARCH, CGO_ENABLED or GOWORK=off" >&2
        exit 2
      fi
      settings+=("${words[0]}")
      words=("${words[@]:1}")
    done
    case "${words[0]:-}" in
      deps)
        if [ "${#words[@]}" -lt 3 ]; then
          echo "go-fetch: $file:$n: deps names a directory and at least one package: deps DIR PACKAGE..." >&2
          exit 2
        fi
        [ -d "$root/${words[1]}" ] || { echo "go-fetch: $file:$n: no directory ${words[1]} under the repository root" >&2; exit 2; }
        ;;
      tools)
        if [ "${#words[@]}" -ne 2 ] || [ "${#settings[@]}" -gt 0 ]; then
          echo "go-fetch: $file:$n: tools names one file and no setting: tools FILE" >&2
          exit 2
        fi
        [ -f "$root/${words[1]}" ] || { echo "go-fetch: $file:$n: no file ${words[1]} under the repository root" >&2; exit 2; }
        read_pins "$root/${words[1]}"
        ;;
      tidy)
        if [ "${#words[@]}" -ne 2 ] || [ "${#settings[@]}" -gt 0 ]; then
          echo "go-fetch: $file:$n: tidy names one module directory and no setting: tidy DIR" >&2
          exit 2
        fi
        [ -f "$root/${words[1]}/go.mod" ] || { echo "go-fetch: $file:$n: no go.mod in ${words[1]} under the repository root" >&2; exit 2; }
        ;;
      *)
        echo "go-fetch: $file:$n: $text is no fetch command: deps DIR PACKAGE..., tools FILE or tidy DIR" >&2
        exit 2
        ;;
    esac
    commands+=("${settings[*]:+${settings[*]} }${words[*]}")
    where+=("$file:$n")
  done < "$file"
  [ "${#commands[@]}" -gt 0 ] || { echo "go-fetch: $file fetches nothing" >&2; exit 2; }
  echo "go-fetch: $file: ${#commands[@]} commands" >&2
  for i in "${!commands[@]}"; do
    read -ra words <<<"${commands[$i]}"
    settings=()
    while [[ ${words[0]} == *=* ]]; do
      settings+=("${words[0]}")
      words=("${words[@]:1}")
    done
    echo "go-fetch: ${where[$i]}: ${commands[$i]}" >&2
    case ${words[0]} in
      deps)
        (
          for setting in "${settings[@]}"; do export "${setting?}"; done
          list_deps "$root/${words[1]}" "${words[@]:2}"
        ) || exit $?
        ;;
      tidy)
        (tidy_module "$root/${words[1]}") || exit $?
        ;;
      *)
        install_tools "$root/${words[1]}"
        ;;
    esac
  done
}

[ $# -ge 1 ] || usage
mode=$1
shift
if [ -n "${GO_FETCH_PROXY:-}" ]; then
  export GOPROXY=$GO_FETCH_PROXY
  echo "go-fetch: fetching through GO_FETCH_PROXY, $GO_FETCH_PROXY" >&2
elif [ "${GOPROXY:-}" = off ]; then
  echo "go-fetch: GO_FETCH_PROXY is unset and GOPROXY is off, so the module cache alone serves this fetch" >&2
fi
case "$mode" in
  file)
    [ $# -eq 1 ] || usage
    run_file "$1"
    ;;
  tools)
    [ $# -eq 1 ] || usage
    install_tools "$1"
    ;;
  deps)
    [ $# -ge 2 ] || usage
    list_deps "$@"
    ;;
  pulumi-consumer)
    [ $# -eq 2 ] || usage
    src=$1 plugin=$2
    [ -f "$src/go.mod" ] || { echo "go-fetch: no Go module at $src" >&2; exit 2; }
    [ -x "$plugin" ] || { echo "go-fetch: no plugin binary at $plugin" >&2; exit 2; }
    src=$(cd "$src" && pwd)
    work=$(mktemp -d)
    trap 'rm -rf "$work"' EXIT
    # One attempt: a fresh copy, package add in it, then the build package add's tidy prepared.
    # The CLI runs as pulumitest.NewCLI runs it in the test: a PULUMI_HOME and a file backend of
    # its own, so it reads and writes nothing of the runner's and asks no service who is logged
    # in (with no backend the CLI looks up an account on Pulumi Cloud and, logged out, makes one),
    # no update check (which asks api.pulumi.com for the CLI's latest version), and the Pulumi
    # Cloud API at a local port that refuses: where an AI coding agent's variable is set
    # (CLAUDECODE and the others the CLI's agentdetect reads), package add asks the registry at
    # api.pulumi.com for the package it added, and no setting of the CLI turns that lookup off.
    # The attempt's own preparation - the directory, the copy, the backend and home - is no
    # service's fault, so a failure of it ends the step at once, naming what failed, rather than
    # leaving package add and the build to run on a copy that is not the consumer: a function
    # that `retry` calls in an `if` runs with errexit off, so each command says so itself.
    consume() {
      local dir
      dir=$(mktemp -d "$work/consumer.XXXXXX") ||
        { echo "::error::go-fetch: could not make a directory under $work for a copy of $src" >&2; exit 1; }
      cp -R "$src/." "$dir" ||
        { echo "::error::go-fetch: could not copy the consumer $src into $dir" >&2; exit 1; }
      mkdir -p "$work/backend" "$work/home" ||
        { echo "::error::go-fetch: could not make the Pulumi backend and home under $work" >&2; exit 1; }
      # go-offline: this is the fetch step itself, run before the tests and tried again on failure.
      (cd "$dir" && GOWORK=off PULUMI_HOME="$work/home" PULUMI_BACKEND_URL="file://$work/backend" \
        PULUMI_CONFIG_PASSPHRASE=test PULUMI_SKIP_UPDATE_CHECK=true PULUMI_API=http://127.0.0.1:1 \
        pulumi package add "$plugin" &&
        GOWORK=off go build ./...)
    }
    echo "go-fetch: the Pulumi consumer $src, on the plugin $plugin" >&2
    retry consume
    ;;
  *)
    usage
    ;;
esac
