# shellcheck shell=bash disable=SC2034 # dev-stack.sh reads what this file sets
# shellcheck disable=SC2153 # CALLOUT, WORK_DIR and the other capitals read here are dev-stack.sh's
# packages/envoy/scripts/lib/callout-stack.sh
#
# The NATS auth service's half of scripts/dev-stack.sh, in one place: the listener's machine id
# and every broker's database URL, which every run uses, and what --callout adds. With the option
# the hub runs behind the NATS auth service (internal/broker/natscallout), as the deployment's hub
# does once its static users are gone: natsd runs with -callout-keys, so every client but the
# service's own user is sent to the service, and the service runs in two brokers on the one
# database, in one queue group, as two replicas do. The brokers admit a session by its broker
# access token and a proof of its key, and the listener and Dispatch by the platform token the
# issuer writes each of them for the NATS audience (envoy-nats), listed in BROKER_NATS_SERVICES
# under their own machine ids. Neither holds a seed. The banner then also exports
# DEV_STACK_CALLOUT=1, NATS_AUTH=broker, DEV_STACK_NATS_SERVICE_TOKEN_FILE (the listener's NATS
# token), DEV_STACK_EXCHANGE_SESSION (the session this run registered the pod for),
# DEV_STACK_BROKER_PIDS, the two brokers' pids, and DEV_STACK_NATS_MONITOR_URL, natsd's monitor.
# The second broker is the one replica the stack outlives: killing it stops nothing else, which is
# how scripts/callout-check.sh proves a stopped replica delays no connection. Without --callout
# each addition below is empty and each step does nothing, so the run is the stack it always was.
#
# It is sourced, never run, and dev-stack.sh is its one caller: it sources this file on every run,
# once its work directory, its accounts and its databases' URLs are named, and calls each step
# below at the point of the run the step names. dev-stack.sh keeps the order its processes start
# in and the cleanup that stops them, the second broker among them (BROKER2_PID). The steps read
# dev-stack.sh's own names: its processes, await and its readers, and start_broker.

# The listener's ENVOY_MACHINE_ID, which names its durable and its queue group; with --callout,
# BROKER_NATS_SERVICES lists the listener under it too.
LISTENER_MACHINE_ID="dev-stack-$RUN_SUFFIX"

# Every broker's own URL caps its pool as production caps it: theorem-labs/agent-infra's
# broker/main.go sets brokerPoolMaxConns = 10, which broker/deployment.go puts on the deployment's
# BROKER_DATABASE_URL as pool_max_conns, a parameter the broker's pgxpool.ParseConfig honours.
# Without it pgx sizes each pool by the machine's CPUs, and on a box with many of them a flood of
# connections through the auth service opens more Postgres connections than a dev server allows.
# psql and Dispatch both refuse the parameter, so the brokers alone are given this URL.
broker_pool_separator="?"
[ -z "$POSTGRES_QUERY" ] || broker_pool_separator="&"
BROKER_POOL_DATABASE_URL="${BROKER_DATABASE_URL}${broker_pool_separator}pool_max_conns=10"

# What --callout adds to the issuer, natsd, every broker, the listener and Dispatch, and the
# banner's lines; each is empty without the option.
callout_issuer_args=() callout_natsd_args=() callout_broker_env=() callout_listener_env=() callout_dispatch_env=()
callout_exports=""

# The audience a service's NATS token carries, the tokens the issuer writes for the listener and
# for Dispatch at it, and the directory natsd keeps the auth callout's three seeds in, which the
# brokers read.
callout_audience="envoy-nats"
callout_listener_token="$WORK_DIR/listener-nats-token"
callout_dispatch_token="$WORK_DIR/dispatch-nats-token"
callout_dir="$WORK_DIR/nats-callout"

if [ "$CALLOUT" = true ]; then
  # The issuer also writes the listener's and Dispatch's tokens for the NATS audience, and each
  # presents its own to the hub as a service (NATS_SERVICE_TOKEN_FILE).
  callout_issuer_args=(--token "$callout_audience=$LISTENER_ACCOUNT=$callout_listener_token"
    --token "$callout_audience=$API_ACCOUNT=$callout_dispatch_token")
  callout_listener_env=(NATS_SERVICE_TOKEN_FILE="$callout_listener_token")
  callout_dispatch_env=(NATS_SERVICE_TOKEN_FILE="$callout_dispatch_token")
  # natsd runs behind the auth callout, making its seeds in callout_dir, and serves its monitor
  # port too, the kernel's choice, whose connection list (/connz?auth=true) names the user the
  # auth service admitted each connection as.
  callout_natsd_args=(-callout-keys "$callout_dir" -m -1)
fi

# callout_after_natsd, with --callout, waits for natsd to report that monitor port, and sets
# callout_broker_env, which has every broker also run the NATS auth service on that natsd: the
# seeds natsd made, the listener and Dispatch listed by account under their machine ids, and the
# NATS audience their tokens carry.
callout_monitor_url=""
nats_monitor_in() { sed -n 's/.*Starting http monitor on 127\.0\.0\.1:\([0-9][0-9]*\)$/\1/p' "$1" | head -n1; }
callout_after_natsd() {
  [ "$CALLOUT" = true ] || return 0
  await natsd "$NATS_PID" "$NATS_LOG" "$NATS_LOG" 'its monitor port' nats_monitor_in
  # shellcheck disable=SC2154 # await sets awaited
  callout_monitor_url="http://127.0.0.1:$awaited"
  echo "dev-stack: natsd monitor $callout_monitor_url" >&2
  callout_broker_env=(
    BROKER_NATS_URLS="$NATS_URL"
    BROKER_NATS_SEED_FILE="$callout_dir/service.seed"
    BROKER_NATS_ISSUER_SEED_FILE="$callout_dir/issuer.seed"
    BROKER_NATS_XKEY_SEED_FILE="$callout_dir/xkey.seed"
    BROKER_NATS_SERVICES="listener/$LISTENER_MACHINE_ID/*=$LISTENER_ACCOUNT dispatch/dev-stack-dispatch/*=$API_ACCOUNT"
    BROKER_NATS_K8S_AUDIENCE="$callout_audience"
  )
}

# callout_after_first_broker, with --callout, starts the second replica once the first has
# migrated the database they share, and sets BROKER2_PID; it returns once both answer the hub, so
# the listener and Dispatch, which start next, never wait out an authorization timeout. It then
# sets callout_exports, the lines the banner prints after its first block of exports: what a
# second shell needs to drive the hub behind the auth service, as this file's header lists them.
callout_answering_in() { grep -q 'the NATS auth service is answering' "$1" && echo answering; }
callout_after_first_broker() {
  [ "$CALLOUT" = true ] || return 0
  local log="$LOG_DIR/broker-2.log"
  echo "dev-stack: starting the second broker..." >&2
  start_broker "$log"
  # shellcheck disable=SC2154 # start_broker sets started_pid
  BROKER2_PID=$started_pid
  await 'the second broker' "$BROKER2_PID" "$log" "$log" 'its bound address' broker_addr_in
  echo "dev-stack: broker-2 http://$awaited" >&2
  await broker "$BROKER_PID" "$BROKER_LOG" "$BROKER_LOG" 'its NATS auth service answering' callout_answering_in
  await 'the second broker' "$BROKER2_PID" "$log" "$log" 'its NATS auth service answering' callout_answering_in
  callout_exports="
  export DEV_STACK_CALLOUT=1
  export NATS_AUTH=broker
  export DEV_STACK_NATS_SERVICE_TOKEN_FILE=$callout_listener_token
  export DEV_STACK_EXCHANGE_SESSION=$EXCHANGE_SESSION
  export DEV_STACK_BROKER_PIDS=\"$BROKER_PID $BROKER2_PID\"
  export DEV_STACK_NATS_MONITOR_URL=$callout_monitor_url"
}
