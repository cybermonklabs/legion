# shellcheck shell=bash
# shellcheck disable=SC2154 # every lowercase global read here is dev-stack.test.sh's own
# packages/envoy/scripts/lib/dev-stack-test-project-access.sh
# The --project-access half of scripts/dev-stack.test.sh: assert_project_access, which a run with
# the option and a run without it are each held to, and the cases that run with it, (q), (r) and
# (s) of that test's header, as two functions it calls where those cases run in its order. It is
# sourced, never run, and dev-stack.test.sh is its one caller. Every helper and global these read
# is that file's: fail, exported, run_instance, wait_for_exit, refuse_run, refuse_binaries,
# refused_files_are, the assert_* checks and the globals they set, and its runs' log and call
# files.

# PRIV's pod and the broker's own access account (project-secrets-check.sh's production path):
# every run mints the pod's two tokens and the broker's token for Dispatch's audience, lists the
# pod's account in the exchange for PRIV and as the service legion-priv's login, and exports
# them. Only a run with --project-access (want=true) points the broker at its own Dispatch's
# access routes and has Dispatch admit the broker's account; a run without it changes neither, as
# before the option existed. Reads the globals assert_instance_ready set.
assert_project_access() {
  local label="$1" log_file="$2" want="$3" broker_log="${work_dir}/logs/broker.log" dispatch_log="${work_dir}/logs/dispatch.log"
  local priv_account='system:serviceaccount:legion-priv:legion-priv-worker' access_account='system:serviceaccount:example-ns:example-broker'
  local file
  for file in priv-exchange-token priv-pod-token broker-access-token; do
    [[ -s "${work_dir}/${file}" ]] || fail "$label: its issuer wrote no ${file}"
  done
  grep -q "fake-token-for-${priv_account}" "${work_dir}/priv-exchange-token" ||
    fail "$label: priv-exchange-token is not PRIV's pod's token"
  grep -q "fake-token-for-${access_account}" "${work_dir}/broker-access-token" ||
    fail "$label: broker-access-token is not the broker's own access account's token"
  grep -q "exchange=.* ${priv_account}=PRIV=carol@example.com=legion-priv-worker-session " "$broker_log" ||
    fail "$label: the broker's BROKER_EXCHANGE_ACCOUNTS does not map PRIV's pod to PRIV for carol under its fixed session"
  grep -q "service-logins=.* legion-priv=${priv_account} " "$broker_log" ||
    fail "$label: the broker's BROKER_SERVICE_LOGINS does not bind PRIV's pod as legion-priv"
  [[ "$(exported "$log_file" DEV_STACK_PRIV_EXCHANGE_TOKEN_FILE)" == "${work_dir}/priv-exchange-token" ]] ||
    fail "$label: the banner does not export PRIV's pod's exchange token file"
  [[ "$(exported "$log_file" DEV_STACK_PROJECT_ACCESS)" == "$want" ]] ||
    fail "$label: the banner exports DEV_STACK_PROJECT_ACCESS=$(exported "$log_file" DEV_STACK_PROJECT_ACCESS), want $want"
  if [[ "$want" == true ]]; then
    grep -q "project-access=http://127\.0\.0\.1:${kept_port} project-access-token=${work_dir}/broker-access-token\$" "$broker_log" ||
      fail "$label: the broker was not pointed at its own Dispatch's access routes with its own token"
    grep -q "oidc-issuer=http://127\.0\.0\.1:${process_port[dev-oidc-issuer]} oidc-audience=dispatch oidc-accounts=${access_account} access-accounts=${access_account}\$" "$dispatch_log" ||
      fail "$label: Dispatch does not admit the broker's account to its access routes"
    # The airlock: the profiles file declaring the one kind priv, whose one host is the stand-in
    # host, by the way swap, and naming no profile, since PRIV_KEY is behind the kind by its own
    # proxy tag; the banner exporting that kind, which project-secrets-check.sh writes on PRIV_KEY;
    # the authority the stands wrote into this run's own directory, the proxy URL on the port the
    # run picked, and the stands' own address in both of the settings that let the proxy dial it.
    grep -q "profiles=${work_dir}/profiles\.json airlock-ca=${work_dir}/stands/ca\.crt airlock-proxy=http://127\.0\.0\.1:[0-9][0-9]* airlock-resolve=priv\.airlock\.test=172\.20\.0\.9 airlock-dial=priv\.airlock\.test:443\$" "$broker_log" ||
      fail "$label: the broker was not given the profiles file, the airlock's authority, proxy URL and the stands' own address"
    jq -e '. == {"kinds": [{"name": "priv", "hosts": ["priv.airlock.test"], "ways": ["swap"]}], "profiles": []}' "${work_dir}/profiles.json" >/dev/null 2>&1 ||
      fail "$label: the profiles file is not the one kind priv, whose one host is the stand-in host, with no profile: $(jq -c . "${work_dir}/profiles.json" 2>&1)"
    [[ "$(exported "$log_file" DEV_STACK_AIRLOCK_KIND)" == priv ]] ||
      fail "$label: the banner does not export the airlock's kind, priv"
    [[ "$(exported "$log_file" DEV_STACK_AIRLOCK_BOUND_URL)" == "https://priv.airlock.test" ]] ||
      fail "$label: the banner does not export the airlock's bound host"
  else
    grep -q 'project-access= project-access-token=$' "$broker_log" ||
      fail "$label: a run without --project-access gave the broker a project-access client"
    grep -q 'oidc-issuer= oidc-audience= oidc-accounts= access-accounts=$' "$dispatch_log" ||
      fail "$label: a run without --project-access changed Dispatch's service-account settings"
    grep -q 'profiles= airlock-ca= airlock-proxy= airlock-resolve= airlock-dial=$' "$broker_log" ||
      fail "$label: a run without --project-access started the airlock"
    [[ ! -e "${work_dir}/profiles.json" ]] ||
      fail "$label: a run without --project-access wrote a profiles file"
    grep -qxF '  export DEV_STACK_AIRLOCK_KIND=' "$log_file" ||
      fail "$label: a run without --project-access does not export an empty airlock kind"
  fi
}

# (q) and (r), run after (h): the --project-access run, and the front its airlock's proxy is named by.
project_access_run_cases() {
  # (q) --project-access: the same stack, its broker pointed at its own Dispatch's access routes
  # with a token of its own account, and that account admitted to both of Dispatch's lists.
  # Instance 1, run without it, is the control: neither is set there. It builds
  # dev-airlock-stands beside the eight, which instance 1 did not (s).
  go_calls_access="${temporary_dir}/go-calls-project-access"
  DEV_STACK_TEST_GO_CALL_FILE="$go_calls_access" run_instance 1 "$log1" "$psql_calls1" "$curl_calls1" --project-access
  assert_instance_ready 'the --project-access run' "$log1" "$psql_calls1" "$curl_calls1"
  grep -q '^go build -o .* \./cmd/dev-proxy-stands' "$go_calls_access" ||
    fail "the --project-access run did not build dev-airlock-stands: $(cat "$go_calls_access")"
  assert_project_access 'the --project-access run' "$log1" true
  kill -INT "${instance_pid[1]}"
  wait_for_exit 1 15 'the --project-access run'
  [[ "$exited_status" -eq 130 ]] || fail "the --project-access run exited $exited_status on INT, not 130"
  assert_everything_stopped 'the --project-access run'
  assert_databases_dropped 'the --project-access run' "$psql_calls1" "$run_suffix"
  printf 'PASS: --project-access pointed the broker at its own Dispatch with its own token, started the airlock in a container of its own, and Dispatch admits that account; a run without it sets none of it\n'

  # (r) The airlock's proxy binds no port this run picked: the broker binds it on port 0 and logs
  # the port it got (its stub reports 1 and its own PID), and the release's proxy URL names a front
  # the run started before the broker, on a port of the front's own (its stub's PID), which the
  # run points at the address the broker logged. So there is no port another process could take
  # between a pick and the broker's bind, and the run reaches ready. The front is stopped with the
  # rest.
  run_instance 1 "$log1" "$psql_calls1" "$curl_calls1" --project-access
  assert_instance_ready 'the --project-access run of the front' "$log1" "$psql_calls1" "$curl_calls1"
  local broker_log="${work_dir}/logs/broker.log" front_log="${work_dir}/logs/airlock-proxy-front.log"
  local proxy_url proxy_addr front_port
  proxy_url="$(exported "$log1" DEV_STACK_AIRLOCK_PROXY_URL)"
  front_port="${proxy_url##*:}"
  [[ "$proxy_url" =~ ^http://127\.0\.0\.1:[0-9]+$ ]] ||
    fail "the banner's DEV_STACK_AIRLOCK_PROXY_URL is '$proxy_url', not a loopback URL"
  grep -qxF "fake front: listen=127.0.0.1:0 backends-file=${work_dir}/airlock-proxy-backends" "$front_log" ||
    fail "the front was not started on a port of its own choosing with the run's backends file: $(cat "$front_log")"
  grep -qF "airlock-proxy=${proxy_url} " "$broker_log" ||
    fail "the broker's BROKER_AIRLOCK_PROXY_URL is not the front's URL $proxy_url"
  grep -qxF 'fake broker: airlock-listen=127.0.0.1:0' "$broker_log" ||
    fail "the broker's proxy was not given port 0 (BROKER_AIRLOCK_LISTEN_ADDR)"
  proxy_addr="$(jq -Rnr '[inputs | fromjson? | select(.message == "airlock proxy listening") | .addr][0] // empty' "$broker_log")"
  [[ -n "$proxy_addr" && "$proxy_addr" != 127.0.0.1:0 ]] ||
    fail "the broker's stub logged no address its proxy bound ('$proxy_addr')"
  [[ "$(cat "${work_dir}/airlock-proxy-backends")" == "$proxy_addr" ]] ||
    fail "the front's backends file holds '$(cat "${work_dir}/airlock-proxy-backends")', not $proxy_addr, the address the broker's proxy logged"
  kill -INT "${instance_pid[1]}"
  wait_for_exit 1 15 'the --project-access run of the front'
  [[ "$exited_status" -eq 130 ]] || fail "the --project-access run of the front exited $exited_status on INT, not 130"
  assert_everything_stopped 'the --project-access run of the front'
  if kill -0 "$front_port" 2>/dev/null; then
    fail "the --project-access run left its front, process $front_port, running"
  fi
  printf 'PASS: --project-access names the airlock proxy by a front on a port of its own, gives the broker port 0 for the proxy, and points the front at the address the broker logged\n'
}

# (s), run after (i), whose refusal checks it uses: --project-access given --binaries, and given
# no docker on PATH.
project_access_binaries_cases() {
  # (s) --project-access given --binaries: that run starts the airlock's stands, so it needs a
  # ninth binary, dev-airlock-stands. The eight-binary directory (h) ran is refused, naming that
  # binary and no other; the control is (h) itself, the same directory running without the flag.
  refuse_binaries 'a --project-access run given the eight binaries' "$prebuilt" --project-access
  refused_files_are 'a --project-access run given the eight binaries' "$prebuilt" "  ${prebuilt}/dev-airlock-stands: missing"
  printf 'PASS: a --project-access run given the eight binaries is refused with exit 2, naming %s/dev-airlock-stands, before anything is created or started\n' "$prebuilt"

  # The same directory with dev-airlock-stands added runs under --project-access, copying all
  # nine and starting the stands: the control that the refusal above was that binary's alone.
  with_stands="${temporary_dir}/prebuilt-with-stands"
  cp -R "$prebuilt" "$with_stands"
  # The stands' package is cmd/dev-proxy-stands and their binary dev-airlock-stands, so the fake
  # build is renamed as dev-stack.sh renames the real one.
  env -u DEV_STACK_TEST_BROKER_FAILS -u DEV_STACK_TEST_GO_CALL_FILE "${fake_bin_dir}/go" build -o "${with_stands}/" ./cmd/dev-proxy-stands
  mv "${with_stands}/dev-proxy-stands" "${with_stands}/dev-airlock-stands"
  DEV_STACK_TEST_GO_CALL_FILE="$go_calls_prebuilt" run_instance 1 "$log1" "$psql_calls1" "$curl_calls1" --binaries "$with_stands" --project-access
  assert_instance_ready 'the --project-access --binaries run' "$log1" "$psql_calls1" "$curl_calls1"
  assert_project_access 'the --project-access --binaries run' "$log1" true
  [[ ! -s "$go_calls_prebuilt" ]] || fail "the --project-access --binaries run called go: $(cat "$go_calls_prebuilt")"
  for binary in "${prebuilt_binaries[@]}" dev-airlock-stands; do
    cmp -s "${with_stands}/${binary}" "${work_dir}/bin/${binary}" ||
      fail "the --project-access --binaries run's own $binary is not the prebuilt one it was given"
  done
  kill -INT "${instance_pid[1]}"
  wait_for_exit 1 15 'the --project-access --binaries run'
  [[ "$exited_status" -eq 130 ]] || fail "the --project-access --binaries run exited $exited_status on INT, not 130"
  assert_everything_stopped 'the --project-access --binaries run'
  assert_databases_dropped 'the --project-access --binaries run' "$psql_calls1" "$run_suffix"
  printf 'PASS: a --project-access run given the nine binaries copied each of them and ran\n'

  # docker: --project-access runs the stands in a container, so a PATH with no docker is refused
  # before anything is created or started, naming docker. The PATH is this test's own, every
  # command on it but docker linked into one directory; the control is the same PATH and the same
  # nine binaries without --project-access, which runs to ready, so the refusal is the option's.
  no_docker_bin="${temporary_dir}/bin-without-docker"
  mkdir -p "$no_docker_bin"
  IFS=: read -r -a path_dirs <<<"${fake_bin_dir}:${PATH}"
  for dir in "${path_dirs[@]}"; do
    [[ "$dir" == /* && -d "$dir" ]] || continue
    for tool in "$dir"/*; do
      name="${tool##*/}"
      [[ "$name" != docker && -f "$tool" && -x "$tool" ]] || continue
      [[ -e "${no_docker_bin}/${name}" || -L "${no_docker_bin}/${name}" ]] || ln -s "$tool" "${no_docker_bin}/${name}"
    done
  done
  if PATH="$no_docker_bin" command -v docker >/dev/null; then
    fail "the PATH built without docker still finds one, so the refusal below could not be seen"
  fi
  DEV_STACK_TEST_PATH="$no_docker_bin" run_instance 1 "$log1" "$psql_calls1" "$curl_calls1" --binaries "$with_stands"
  assert_instance_ready 'the run without docker or --project-access' "$log1" "$psql_calls1" "$curl_calls1"
  kill -INT "${instance_pid[1]}"
  wait_for_exit 1 15 'the run without docker or --project-access'
  [[ "$exited_status" -eq 130 ]] || fail "the run without docker or --project-access exited $exited_status on INT, not 130"
  assert_everything_stopped 'the run without docker or --project-access'
  assert_databases_dropped 'the run without docker or --project-access' "$psql_calls1" "$run_suffix"
  DEV_STACK_TEST_PATH="$no_docker_bin" refuse_run 'a --project-access run with no docker on PATH' 1 --binaries "$with_stands" --project-access
  grep -qx 'dev-stack: docker is required on PATH' "$refusal" ||
    fail "a --project-access run with no docker on PATH did not name docker: $(cat "$refusal")"
  printf 'PASS: a --project-access run with no docker on PATH is refused with exit 1, naming docker, before anything is created or started; the same run without the option needs none\n'
}
