# shellcheck shell=bash
# packages/envoy/scripts/lib/airlock-stack.sh
# The airlock's own half of scripts/dev-stack.sh --project-access, in one place: the stand-ins
# that mint the certificate authority a proxy decrypts with, run in a container, the profiles file
# whose proxy blocks say which secret may go to which host, and the BROKER_PROFILES_FILE and
# BROKER_AIRLOCK_* settings a broker is started with. It is sourced, never run, and dev-stack.sh is
# its one caller.
# scripts/dev-airlock.sh does not use it: that script starts its own stands as a process of this
# machine, four of them on loopback addresses' port 443, each with options of its own, beside two
# brokers behind two fronts, six kinds of secret and the model judge.
#
# A bound host listens on port 443. That is not a choice: the proxy compares a request's Host
# header against its CONNECT target, and proxy.hostHeaderOf trims ":443" alone, so a bound
# host on any other port is refused "host header mismatch" before anything else it could prove.
# No unprivileged process binds port 443 on a machine whose
# /proc/sys/net/ipv4/ip_unprivileged_port_start is above it, or whose 443 another listener holds,
# so the stands run in a container, which has a port 443 of its own.

# airlock_stands_start starts cmd/dev-proxy-stands in a container and sets, for the caller:
#   AIRLOCK_STANDS_CONTAINER  the container to stop, set once docker has started it
#   AIRLOCK_CA_CERT_FILE      the authority the proxy decrypts with
#   AIRLOCK_CA_KEY_FILE       its key
#   AIRLOCK_S_URL             https://<the bound host's name>
#   AIRLOCK_DIAL_ALLOW        what the proxy may dial although it is not a public address
#   AIRLOCK_RESOLVE           where that name is
#
# $1 is the stands binary, $2 the work directory, $3 the bound host's name, $4 the image the
# container runs, and $5 a name for the container.
#
# How a bound host gets port 443 (the measurements are in the pull request of
# dispatch://INFRA-3862): the stands run in an ordinary bridge container, binding 0.0.0.0:443
# inside a network namespace of their own, and the broker on this machine dials the container's
# own address, which a bridge network routes to. The proxy refuses every private address it is not
# told about, so that address is named in both BROKER_AIRLOCK_TEST_RESOLVE and
# BROKER_AIRLOCK_TEST_DIAL_ALLOW, which exist for exactly this. The work directory is bind-mounted
# writable, since the broker on this machine reads the certificate authority the stands write into
# it.
airlock_stands_start() {
  local binary="$1" work_dir="$2" host="$3" image="$4" container="$5"
  local stands_dir="$work_dir/stands" stands_json="$work_dir/stands.json" stands_log="$work_dir/stands.log"
  mkdir -p "$stands_dir"
  : >"$stands_log"
  AIRLOCK_STANDS_CONTAINER=""
  docker rm -f "$container" >/dev/null 2>&1 || true
  if ! docker run -d --name "$container" \
    -v "$binary:/dev-airlock-stands:ro" -v "$stands_dir:/stands" \
    "$image" /dev-airlock-stands --dir /stands --address "s=0.0.0.0:443" --host "s=$host" \
    >"$work_dir/stands-container" 2>"$stands_log"; then
    echo "airlock-stack: docker refused to start the stands; see $stands_log" >&2
    return 1
  fi
  AIRLOCK_STANDS_CONTAINER="$container"
  local waited=0 listening=""
  while [ "$waited" -lt 120 ]; do
    # The stands write their JSON to stdout and their lines to stderr; docker keeps the two
    # apart, so each is read on its own and neither is parsed as the other.
    docker logs "$container" >"$stands_json" 2>"$stands_log" || true
    if grep -q 'stands listening records=' "$stands_log" 2>/dev/null; then
      listening=yes
      break
    fi
    sleep 0.5
    waited=$((waited + 1))
  done
  if [ -z "$listening" ]; then
    echo "airlock-stack: dev-airlock-stands never logged its addresses within 60s; see $stands_log" >&2
    return 1
  fi
  # The stands name each file by the path they wrote it at, which is the container's. The bind
  # mount makes /stands this run's own directory, so the paths the broker is given are this
  # machine's.
  AIRLOCK_CA_CERT_FILE="$(jq -r .ca_cert_file "$stands_json")"
  AIRLOCK_CA_KEY_FILE="$(jq -r .ca_key_file "$stands_json")"
  AIRLOCK_CA_CERT_FILE="${stands_dir}${AIRLOCK_CA_CERT_FILE#/stands}"
  AIRLOCK_CA_KEY_FILE="${stands_dir}${AIRLOCK_CA_KEY_FILE#/stands}"
  # The stands run as the container's root and write the key readable by its owner alone, so
  # the broker on this machine, which is another user, could not read it. The container's own
  # root is what can change that, so it is asked to.
  docker exec "$container" chmod 0644 /stands/ca.crt /stands/ca.key >/dev/null 2>&1 || true
  if [ ! -s "$AIRLOCK_CA_CERT_FILE" ] || [ ! -s "$AIRLOCK_CA_KEY_FILE" ]; then
    echo "airlock-stack: the stands wrote no certificate authority this machine can read ($AIRLOCK_CA_CERT_FILE)" >&2
    return 1
  fi
  local address
  address="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$container" 2>/dev/null | head -n1)"
  if [ -z "$address" ]; then
    echo "airlock-stack: the stands container $container has no address of its own" >&2
    return 1
  fi
  # shellcheck disable=SC2034 # the caller's: dev-stack.sh prints and exports it
  AIRLOCK_S_URL="https://$host"
  AIRLOCK_DIAL_ALLOW="$host:443"
  AIRLOCK_RESOLVE="$host=$address"
}

# airlock_stands_stop stops the container a start left behind.
airlock_stands_stop() {
  [ -z "${AIRLOCK_STANDS_CONTAINER:-}" ] || docker rm -f "$AIRLOCK_STANDS_CONTAINER" >/dev/null 2>&1 || true
}

# airlock_profiles_write writes the profiles file at $1 that holds, for each name after it, a
# profile of that stored secret's own name whose proxy block binds it to the host $2, and nothing
# else: the value of such a secret leaves the broker only through the proxy, and only to that
# host. A proxy block is the broker's one airlock entry (BROKER_PROFILES_FILE); a stored value's
# profile must have one.
airlock_profiles_write() {
  local file="$1" host="$2"
  shift 2
  local entries="" name
  for name in "$@"; do
    [ -z "$entries" ] || entries="$entries,"
    entries="$entries
  {\"name\": \"$name\", \"proxy\": {\"hosts\": [\"$host\"]}}"
  done
  printf '{"profiles": [%s\n]}\n' "$entries" >"$file"
  chmod 600 "$file"
}

# airlock_broker_env prints, one per line, the settings a broker reads to serve the airlock: the
# profiles file $1, whose proxy blocks are the airlock's entries, the proxy URL $2 a release hands
# to a session, the address $3 the proxy listens on, the authority read above, and where the proxy
# may dial the bound host. A caller reads them into an array (mapfile) and passes them to the
# broker it starts.
airlock_broker_env() {
  local profiles_file="$1" proxy_url="$2" listen_addr="$3"
  printf '%s\n' \
    "BROKER_PROFILES_FILE=$profiles_file" \
    "BROKER_AIRLOCK_LISTEN_ADDR=$listen_addr" \
    "BROKER_AIRLOCK_PROXY_URL=$proxy_url" \
    "BROKER_AIRLOCK_CA_CERT_FILE=$AIRLOCK_CA_CERT_FILE" \
    "BROKER_AIRLOCK_CA_KEY_FILE=$AIRLOCK_CA_KEY_FILE" \
    "BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE=$AIRLOCK_CA_CERT_FILE" \
    "BROKER_AIRLOCK_TEST_DIAL_ALLOW=$AIRLOCK_DIAL_ALLOW" \
    "BROKER_AIRLOCK_TEST_RESOLVE=$AIRLOCK_RESOLVE"
}
