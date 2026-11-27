#!/usr/bin/env bash
# packages/envoy/scripts/legacy-subjects-gate.sh --context <kube-context> --record <file>
#   [--since <earlier record>] [--listener <machine>=<url>]... [--insecure]
#   [--bus-alias <nats-urls>]... [--token-file <path>] [--inbox-prefix <prefix>]
#   [--deployment <owner/repo> --new-form-commit <commit> [--dotfiles <dir>]]
#   [--hub-statefulset <name>] [--hub-server <name>]... [--publisher <identity>=<pin key>]...
#   [--broker <namespace>/<deployment>]
#
# Says whether any reader or writer of the bus still uses the old form of a name the rename moved
# (an issue event named by its key alone, an agent's or a role's name with no project, a kept lane
# with no project, and a failed-delivery report of one), the reading a pull request quotes before it
# refuses the old form and deletes the code that reads it.
# One reading cannot say that nothing moved, so the gate is read twice: the first reading is written
# to --record, and the second names it with --since, no sooner than a listener's counter line may be
# old (180 s, the judge's log_line_bound), so each listener's line at the second reading comes after
# the one the first read. It prints GATE OPEN only at a second reading, when both readings read
# every surface, no listener counted an old-form name in between, nothing still holds an old form,
# and the five facts below read true.
#
# The five facts, read at a second reading alone, each printed on lines that name it first and kept
# in the record's facts (true, false or unknown), which legacy-subjects-purge.sh reads:
#
#   (a1) The pins, for the clients that start later: daemon-release-gate.sh --deployment
#        <owner/repo> --clients [--dotfiles <dir>] <commit>, run beside this script, where <commit>
#        is --new-form-commit, the commit from which every client writes only new-form names. It
#        is true when that gate opens having read at least one pin; its GATE SHUT lines make it
#        false; anything else, a census that read no pin among it, is unknown.
#   (a2) The running clients: every connection of every hub server, read by the census of
#        connections, $SYS.REQ.SERVER.PING.CONNZ, which a Job of its own (lib/legacy-subjects-gate-
#        census.sh) requests page by page as the legacy-cutover user (below); each server answers
#        for the account that user sits in, the clients' own. Each server --hub-statefulset (its
#        pods, <name>-<ordinal>, as the servers name themselves) and --hub-server name must answer
#        every page, and no other may. Each connection but the census's own must end its name with
#        its build, or with its build and then privacy-required, which a listener in required
#        privacy mode keeps last (lib/legacy-subjects-gate-forms.jq, build_token): legion@<commit>,
#        judged by GitHub's compare of --new-form-commit with that commit, or
#        @sjawhar/<plugin>@<version>, judged by the commit of its release tag (pi-envoy-v<version>,
#        claude-envoy-v<version> for claude-envoy and codex-envoy), as daemon-release-gate.sh judges
#        a pin. A build behind or beside the commit makes it false; a connection that names no
#        build, a server that does not answer, a census that moved between its pages and one that
#        enumerates no connection make it unknown.
#   (a3) The publishers: every user of the hub that may publish a notification subject runs a
#        listener pin at or after --new-form-commit. The users are read from the hub's auth.conf,
#        the key of a ConfigMap the --hub-statefulset's pods mount, in the shape infra/core's
#        Authorization.Render writes it (lib/legacy-subjects-gate-publishers.jq, auth_conf): each
#        user, by the name its `# <name>` line gives it, whose publish allow list can match a
#        notification subject (notifications.>, notifications.<x>.>, *.>, >) or that has none,
#        which allows every subject, unless one deny entry matches every notification subject.
#        Behind an auth callout, each entry of the auth service's BROKER_NATS_SERVICES, read from
#        the Deployment --broker names, whose kind lib/legacy-subjects-gate-callout-publishers.json
#        lists is one too, by the name the service's answer gives its user (<kind>/<machine id>);
#        the natscallout test TestTheGateCountsEveryCalloutKindWhoseGrantPublishesANotificationSubject
#        holds that list to the grants the service answers. Nothing the gate reads links a user to
#        the stack that runs it, so the operator maps each with --publisher <identity>=<pin key>,
#        repeated for a user more than one process runs, to a pin key of daemon-release-gate.sh
#        --listeners, run beside this script: envoy, envoy-cluster, dispatch or dispatch-server.
#        It is true when every such user is mapped and every key it maps to passed; a key that
#        census shut makes it false; a user no --publisher maps, a key it printed no PASS for, a
#        server --hub-server names (whose users cannot be read), an auth.conf of another shape, a
#        callout whose auth service is not named or cannot be read, and no such user at all make
#        it unknown. The record keeps each --publisher as given, with its key's verdict.
#   (b)  The consumers: each consumer of the stream has acknowledged (its ack floor's stream
#        sequence) the newest message of the old-form subjects its filters match: a floor equal to
#        that message's sequence holds, since the floor's own message is acknowledged, and a floor
#        below it shuts. A stream that holds no old-form message hands none. A consumer list that
#        lists none is unknown.
#   (c)  The stream: it holds no old-form message. Once (a1), (a2), (a3) and (b) read true, the
#        committed driver legacy-subjects-purge.sh, given that reading, purges each old-form subject
#        and reads back that none is left, in place of waiting out the stream's retention.
#
# The legacy-cutover user: the one NATS user that reads the census and purges, a temporary static
# user of the account the hub's clients connect in, the global account $G on a hub that renders no
# accounts block (no private project), which the deployment adds for the cutover and removes
# before any project is private. It publishes exactly $SYS.REQ.SERVER.PING.CONNZ,
# $JS.API.STREAM.PURGE.ENVOY_NOTIFICATIONS, $JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS,
# $JS.API.CONSUMER.LIST.ENVOY_NOTIFICATIONS and $JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS, and
# subscribes its own inbox prefix alone, _INBOX_legacy_cutover.>. Its Jobs, this gate's census and
# legacy-subjects-purge.sh's purge, run in namespace envoy as the ServiceAccount legacy-cutover and
# mount its nkey seed through the SecretProviderClass legacy-cutover-nats-seed, as the file
# nats-seed; the deployment grants that ServiceAccount alone the read of that seed, which no
# developer box's own identity reads, and no other workload names the class. Any principal that
# may create pods in envoy can still start one as that ServiceAccount with that class mounted, as
# these Jobs do, and read the seed (this gate's operator holds that role), so while the deployment
# keeps the user the boundary around its grant is who may create pods in envoy, as it is for the
# listener's own seed.
#
# What it reads, all of it read-only:
#
#   - Each listener's envoy_listener_legacy_subjects_total samples (every label set: an interest a
#     registration named, a role a claim named, and a valid role a publish, the role lane or a
#     holder lookup named) and its start-time gauge envoy_listener_start_time_seconds, whose one
#     label, machine, is its ENVOY_MACHINE_ID. The listeners in the cluster are found by what they
#     run, not by name: every container, in any namespace, of an image whose path ends legion/envoy
#     that runs the image's own entrypoint (envoy-listener; a command of envoy-listener counts too,
#     and any other command is the Dispatch server, the broker or a helper), on the bus the reading
#     is about: the NATS_URLS of the envoy-listener Deployment, or another NATS_URLS the operator
#     names with --bus-alias as an address of the same NATS servers (a hub in the cluster that the
#     Service envoy-listener dials and a Service of the hub's own both select, so a listener that
#     dials the second is on the bus). A listener container on another bus (a load-test stack's, on
#     its own NATS) is named (SKIP), with its machine and its NATS_URLS, any credential in it
#     masked, and not read. Each is read from its pod's log: the listener logs the two metrics, as
#     /metrics serves them, on a line "listener legacy subjects" every minute (cmd/listener
#     startLegacySubjectsLog), and the gate takes the newest from the last five minutes, which must
#     be at most three minutes older than the
#     reading. The log and not /metrics, because the operator may read every listener pod's log and
#     may not reach Dispatch's listener's /metrics: the API server's pod proxy is refused in that
#     namespace, and the listener's NetworkPolicy admits its port from Dispatch's own pods alone. A
#     listener outside the cluster, such as the VM hub's, is named with --listener <machine>=<url>
#     and read at <url>/metrics, over https unless --insecure is given. Each reading is held to the
#     machine it was read as: the start-time gauge of a listener outside the cluster must name the
#     machine the operator gave, and a pod's must name its container's ENVOY_MACHINE_ID when it names
#     one, so a URL serving another listener is UNKNOWN, never that machine's count. Every listener
#     with its durable on the bus (listener-<machine>, bus.ListenerDurable), bound now or not, must
#     be one of those: one that is not is UNKNOWN by name, so a listener nobody named, or one offline
#     at the reading, is never left out of the count while its durable lasts.
#   - Over NATS, from a one-off Job in namespace envoy that connects as the in-cluster listener
#     does (lib/legacy-subjects-gate-nats.sh): who the server says the connection is, the stream's
#     info, every consumer of the stream, the stream's subjects under notifications.dispatch.issue.>,
#     notifications.agent.>, notifications.envoy.exceptions.notifications.agent.> and
#     notifications.envoy.kept.>, the last message (subject, stream sequence and time) of each of
#     those subjects in an old form, and every record of the interest store envoy_interests.
#
# The first reading shuts on no old-form name: the stream still holds the old-form messages
# published before every producer moved, and interest records and consumer filters may still be in
# an old form. It names each one it finds (FOUND), and records the stream's last sequence, the
# newest old-form message and every old-form counter. What holds the gate shut at the second
# reading:
#
#   - any old-form message the stream still holds, fact (c); one published after the first
#     reading's last stream sequence (its subject's last message, read by last_by_subj, is later)
#     is named apart;
#   - a listener whose count rose since the first reading, under any kind;
#   - a consumer one of whose filters only an old-form name matches, and whose new form the same
#     consumer does not also filter under every project its names carry (a session consumer filters
#     both forms of each topic until the old form is refused, so its old filter loses nothing; a
#     filter that is the old form alone, a Legion daemon's legion-go-<project>-<kind> or any other,
#     would go deaf once publishers stop, and so would one beside a new form under one project of
#     names that carry any); an issue filter is one that reaches the issue family, through a
#     wildcard in the family's own tokens too (notifications.dispatch.*.*.comment.created);
#   - an interest record holding an old-form topic other than its session's own agent subject,
#     which the listener writes into every record itself until the old form is refused;
#   - a reading named with --since that shut the gate, less than 180 s between the two readings,
#     or a listener whose counter line is no later than the one the first reading read;
#   - a fact read false: a pin, a running client or a publisher's listener pin behind the commit, a
#     consumer that has not acknowledged an old-form message it takes.
#
# The old form is the gate's own rule (lib/legacy-subjects-gate-forms.jq, old_form), so the gate
# stays right once the code that reads the old form is deleted.
#
# What it cannot see is UNKNOWN, by name, and UNKNOWN is never GATE OPEN: a surface it could not
# read or that answered nothing (no listener, no Legion daemon consumer, no issue or agent subject
# on the stream, no interest record: a production bus always holds them), a refused read named by
# its request, a list that changed between the pages it was read in, an answer that is no JSON
# object, a listener that is not running, a listener build that logs or serves no old-form counter,
# a reading whose start-time gauge names another machine than the one read, or none for a listener
# outside the cluster, a listener that restarted between the readings (its pod's restart count or
# its start-time gauge moved, or it is a new pod), a listener of the first reading that is gone, a
# listener on another bus that serves as the machine of a listener read (that machine's durable can
# be either's) or whose ENVOY_MACHINE_ID is no literal value (set through valueFrom or envFrom,
# holding a $(VAR) reference, or not set: its machine cannot be read; a literal one is read trimmed,
# as the listener reads it), a listener container that lists ENVOY_MACHINE_ID or NATS_URLS more than
# once (the kubelet takes the last entry, the gate would read the first), a start-time gauge served
# at one reading only, a record that cannot be written, and at a second reading: a first reading
# that did not read every surface, a stream that is not the first reading's (its envoy_stream_id
# changed), readings further apart than a listener's durable outlives its listener, and a fact it
# could not read (above).
#
# The Job: namespace envoy, the ServiceAccount, NATS URL and credential of the listener Deployment
# envoy/envoy-listener as it is deployed (read at each run, so the Job connects with whatever
# credential the listener holds): its NATS seed file (NATS_NKEY_SEED_FILE) and the volume holding
# it, or, with --token-file <path>, the listener's projected NATS token at that path in the
# Deployment and the volume holding it, which a hub that is not static takes in place of a seed;
# with --inbox-prefix <prefix>, every nats call of the Job takes that inbox prefix, the only one
# such a hub's grant lets the identity subscribe. It runs the nats-box image the deployment pins
# for its own NATS Jobs, as nobody (65534) with every capability dropped, no Kubernetes API token
# mounted, a scratch directory in memory, backoffLimit 0, activeDeadlineSeconds 300 and
# ttlSecondsAfterFinished 600; labels app.kubernetes.io/name and created-by legacy-subjects-gate
# and owner <your login> (unknown for a uid with no name). The gate waits for it to finish, reads
# its log, deletes it and reads back that no Job or pod of its name is left; one it could not
# delete is UNKNOWN and named with the command that deletes it, and one it could not create is
# never deleted. The Job's log holds what the gate judges and no more: of an interest record, its
# session id and its topics; of a stored message, its subject, stream sequence and time; of a seed
# nsc could not read, a fixed line. The cluster keeps a container's log in its logging backend as
# it keeps the listener's own, which logs every topic it delivers, so the Job adds no kind of text
# the namespace does not already log.
#
# The census Job, at a second reading given --new-form-commit: one more Job of the same shape and
# limits, labelled created-by legacy-subjects-gate-census, that runs lib/legacy-subjects-gate-
# census.sh as the ServiceAccount legacy-cutover with the legacy-cutover user's seed mounted (above)
# and nothing of the listener's, under a connection name this reading draws at random and leaves
# out of the census. Its log holds, of each connection the census lists, its server, id, name,
# account, client library and NATS user. It is created, read, deleted and read back as the
# listener-identity Job is.
#
# It prints the identity the NATS reads ran as (`IDENTITY`), one line per surface, `READ`, `SKIP` (a
# listener on another bus, with its machine), `PASS`, `FOUND` (an old-form name a first reading
# found), `GATE SHUT` or `UNKNOWN`, the Job's creation and
# deletion, and the verdict alone on the last line: `GATE OPEN` (exit 0) at a clean second reading,
# `GATE SHUT` (exit 1) when anything shut it, `UNKNOWN` (exit 3) otherwise when anything was not
# read, and at a first reading that read every surface `FIRST READING`, naming how many old-form
# names it found, the stream sequence the second reading reads after, and when to read again (exit
# 4). It writes the reading to --record in every case, as a new file in a directory that exists
# when the gate starts: a file made at that path meanwhile is left as it is, and the reading is
# UNKNOWN. A second reading that opened is a first reading for the next one. A usage error exits
# 2 and reads nothing. No line of a reading prints a URL's user, password or token (user:***@,
# ***@ in its place), whether a NATS_URLS or a nats error naming its server holds it.
#
# It needs bash, kubectl (a context for the cluster that runs the listeners and the hub bridge,
# signed in as an account that may list pods in every namespace, read the logs of the listener
# pods in each namespace that runs one, read the envoy-listener Deployment, and create, read, read
# the logs of and delete Jobs and pods in envoy: a developer box's account holds list through its
# container.viewer role, and the rest through its edit role in envoy and its pods/log read in
# dispatch), jq, curl, base64 and date; at a second reading, gh too (signed in with read access to
# cybermonklabs/legion, for each build a connection names), read of the hub StatefulSet and of the
# ConfigMaps its pods mount in envoy, with --broker read of that Deployment in its namespace, and
# what daemon-release-gate.sh needs.
set -euo pipefail

usage() {
  [ -z "${1:-}" ] || echo "legacy-subjects-gate.sh: $1" >&2
  echo "usage: legacy-subjects-gate.sh --context <kube-context> --record <file> [--since <earlier record>] [--listener <machine>=<url>]... [--insecure] [--bus-alias <nats-urls>]... [--token-file <path>] [--inbox-prefix <prefix>] [--deployment <owner/repo> --new-form-commit <commit> [--dotfiles <dir>]] [--hub-statefulset <name>] [--hub-server <name>]... [--publisher <identity>=<pin key>]... [--broker <namespace>/<deployment>]" >&2
  echo "  <kube-context> is the kubectl context of the cluster that runs the listeners and the hub bridge" >&2
  echo "  <file> is where this reading is written; it must not exist" >&2
  echo "  <earlier record> is the first reading's --record, taken at least 180 s before" >&2
  echo "  <machine>=<url> names a listener outside the cluster: its ENVOY_MACHINE_ID and its https://host[:port]" >&2
  echo "  --insecure takes an http:// listener URL, whose answer anyone on the way could read or change" >&2
  echo "  <nats-urls> is a NATS_URLS that reaches the NATS servers the listener Deployment's NATS_URLS reaches, by another name: a listener container that dials it is read as one of the bus's" >&2
  echo "  <path> is where the listener Deployment mounts its projected NATS token, for a hub that is not static" >&2
  echo "  <prefix> is the inbox prefix the listener identity's grant subscribes, for a hub that is not static" >&2
  echo "  at a second reading, for the five facts:" >&2
  echo "  <owner/repo> is the deployment repository daemon-release-gate.sh reads the pins of: --clients for (a1), --listeners for (a3)" >&2
  echo "  <commit> is the full commit from which every client writes only new-form names (a1, a2, a3)" >&2
  echo "  --hub-statefulset names the hub's StatefulSet in envoy, and --hub-server a hub server outside it, each of which must answer the census (a2); the StatefulSet's auth.conf names the hub's users (a3)" >&2
  echo "  <identity>=<pin key> maps a hub user that may publish a notification subject, by its auth.conf name or, behind an auth callout, <kind>/<machine id>, to the daemon-release-gate.sh --listeners pin it runs: envoy, envoy-cluster, dispatch or dispatch-server (a3)" >&2
  echo "  <namespace>/<deployment> is the auth service's Deployment, whose BROKER_NATS_SERVICES names the services a hub behind an auth callout admits (a3)" >&2
  exit 2
}

context=""
record=""
since=""
insecure=0
token_file=""
inbox_prefix=""
deployment=""
new_form_commit=""
dotfiles=""
hub_statefulset=""
broker=""
outside=()
aliases=()
hub_servers=()
publishers=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --context) [ "$#" -ge 2 ] || usage "--context takes a value"; context=$2; shift 2 ;;
    --record) [ "$#" -ge 2 ] || usage "--record takes a value"; record=$2; shift 2 ;;
    --since) [ "$#" -ge 2 ] || usage "--since takes a value"; since=$2; shift 2 ;;
    --listener) [ "$#" -ge 2 ] || usage "--listener takes a value"; outside+=("$2"); shift 2 ;;
    --bus-alias) [ "$#" -ge 2 ] || usage "--bus-alias takes a value"; aliases+=("$2"); shift 2 ;;
    --insecure) insecure=1; shift ;;
    --token-file) [ "$#" -ge 2 ] || usage "--token-file takes a value"; token_file=$2; shift 2 ;;
    --inbox-prefix) [ "$#" -ge 2 ] || usage "--inbox-prefix takes a value"; inbox_prefix=$2; shift 2 ;;
    --deployment) [ "$#" -ge 2 ] || usage "--deployment takes a value"; deployment=$2; shift 2 ;;
    --new-form-commit) [ "$#" -ge 2 ] || usage "--new-form-commit takes a value"; new_form_commit=$2; shift 2 ;;
    --dotfiles) [ "$#" -ge 2 ] || usage "--dotfiles takes a value"; dotfiles=$2; shift 2 ;;
    --hub-statefulset) [ "$#" -ge 2 ] || usage "--hub-statefulset takes a value"; hub_statefulset=$2; shift 2 ;;
    --hub-server) [ "$#" -ge 2 ] || usage "--hub-server takes a value"; hub_servers+=("$2"); shift 2 ;;
    --publisher) [ "$#" -ge 2 ] || usage "--publisher takes a value"; publishers+=("$2"); shift 2 ;;
    --broker) [ "$#" -ge 2 ] || usage "--broker takes a value"; broker=$2; shift 2 ;;
    *) usage "unknown argument $1" ;;
  esac
done
[ -n "$context" ] || usage "--context <kube-context> is required: the gate reads no cluster it was not named"
[ -n "$record" ] || usage "--record <file> is required: a reading the gate does not keep cannot be compared"
[ ! -e "$record" ] || usage "--record $record exists; the gate writes a new file and overwrites none"
[ -d "$(dirname -- "$record")" ] && [ -w "$(dirname -- "$record")" ] ||
  usage "--record $record: its directory $(dirname -- "$record") is no directory the gate can write"
for entry in ${outside[@]+"${outside[@]}"}; do
  [[ $entry =~ ^[A-Za-z0-9._-]+=https?://[A-Za-z0-9.-]+(:[0-9]+)?/?$ ]] || usage "--listener $entry is not <machine>=https://host[:port]"
  [[ $entry =~ =https:// ]] || [ "$insecure" = 1 ] ||
    usage "--listener $entry is plain http: anyone on the way could read or change the count the gate opens on; give its https URL, or --insecure to take it as it is"
done
for alias in ${aliases[@]+"${aliases[@]}"}; do
  [[ $alias =~ ^[a-z][a-z0-9+.-]*://[A-Za-z0-9.-]+(:[0-9]+)?(,[a-z][a-z0-9+.-]*://[A-Za-z0-9.-]+(:[0-9]+)?)*$ ]] ||
    usage "--bus-alias $alias is not a NATS_URLS: one or more scheme://host[:port], comma-separated"
done
[ -z "$token_file" ] || [[ $token_file =~ ^/[A-Za-z0-9._/-]+$ ]] || usage "--token-file $token_file is not an absolute path"
case "$token_file/" in
  */../* | */./*) usage "--token-file $token_file holds a . or .. segment; name the token's own path" ;;
esac
[ -z "$inbox_prefix" ] || [[ $inbox_prefix =~ ^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$ ]] || usage "--inbox-prefix $inbox_prefix is not a NATS subject of literal tokens"
[ -z "$deployment" ] || [[ $deployment =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || usage "--deployment $deployment is not <owner>/<repo>"
[ -z "$new_form_commit" ] || [[ $new_form_commit =~ ^[0-9a-f]{40}$ ]] || usage "--new-form-commit $new_form_commit is not a full commit id (40 lowercase hex digits): GitHub's compare resolves any ref, so a branch would compare as wherever it points now"
[ -z "$hub_statefulset" ] || [[ $hub_statefulset =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || usage "--hub-statefulset $hub_statefulset is not a Kubernetes object name"
for server in ${hub_servers[@]+"${hub_servers[@]}"}; do
  [[ $server =~ ^[A-Za-z0-9._-]+$ ]] || usage "--hub-server $server is not a NATS server name"
done
for entry in ${publishers[@]+"${publishers[@]}"}; do
  [[ $entry =~ ^[^=[:space:][:cntrl:]]+=[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] ||
    usage "--publisher $entry is not <identity>=<pin key>: a hub user's name or <kind>/<machine id>, then a daemon-release-gate.sh --listeners pin key"
done
[ -z "$broker" ] || [[ $broker =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] ||
  usage "--broker $broker is not <namespace>/<deployment>"
for tool in kubectl jq curl base64 date; do
  command -v "$tool" >/dev/null 2>&1 || usage "$tool is required and is not on PATH"
done

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
readonly here context record since insecure token_file inbox_prefix deployment new_form_commit dotfiles
readonly hub_statefulset broker
readonly judge="$here/lib/legacy-subjects-gate-judge.jq"
readonly job_script="$here/lib/legacy-subjects-gate-nats.sh"
readonly census_script="$here/lib/legacy-subjects-gate-census.sh"
# The legacy-cutover user's Jobs run as this ServiceAccount, mount its seed through this
# SecretProviderClass as this file, and take this inbox prefix, the only one its grant subscribes.
# The deployment declares all four for that user alone.
readonly cutover_account=legacy-cutover
readonly cutover_seed_class=legacy-cutover-nats-seed
readonly cutover_seed_file=nats-seed
readonly cutover_inbox=_INBOX_legacy_cutover
# (a1)'s census of pinned builds, and (a3)'s of listener pins: the release gate beside this script,
# in its --clients and its --listeners mode.
readonly release_gate="$here/daemon-release-gate.sh"
# (a3)'s list of the auth service's kinds of entry, each with whether its grant may publish a
# notification subject, and whether a session's grant may, which the natscallout test
# TestTheGateCountsEveryCalloutKindWhoseGrantPublishesANotificationSubject holds to the kinds config
# declares and the grants the auth service answers.
readonly callout_publishers="$here/lib/legacy-subjects-gate-callout-publishers.json"
# The nats-box image the deployment pins for its own NATS Jobs (envoy-cluster/Pulumi.prod.yaml
# moveImage), by digest, so the Job runs the nats CLI the reads were measured with.
readonly nats_box="mirror.gcr.io/natsio/nats-box:0.19.0@sha256:00f10dd4156f28ece84bb3a52cf356ef220ebaa0494310caa90dec184d9e2b89"
readonly job_namespace=envoy
readonly listener_deployment=envoy-listener
readonly job_deadline=300
# The listener logs its counter every minute; the gate reads this much of each listener's log.
readonly log_window=5m

# listener_container selects the containers that run the listener: an image whose path ends
# legion/envoy, running the image's own entrypoint or a command of envoy-listener. The pods and the
# listener Deployment are read by the one rule.
readonly listener_container='def listener_container: select(.image | test("(^|/)legion/envoy([@:]|$)"))
  | select(.command // [] | length == 0 or (.[0] | split("/") | last) == "envoy-listener");'

earlier=null
if [ -n "$since" ]; then
  [ -r "$since" ] || usage "--since $since cannot be read"
  earlier=$(jq -c . "$since" 2>/dev/null) || usage "--since $since is no JSON"
  jq -e '.gate == "legacy-subjects-gate" and .version == 3 and (.finished_at | type) == "number"' <<<"$earlier" >/dev/null ||
    usage "--since $since is no legacy-subjects-gate reading of this version (3)"
  [ "$(jq -r .context <<<"$earlier")" = "$context" ] ||
    usage "--since $since read the cluster $(jq -r .context <<<"$earlier"), not $context"
fi

work=$(mktemp -d)
readonly work
# Each Job the gate made, and what became of it: tried while its deletion runs, deleted once
# envoy is read back holding neither it nor a pod of it, or kept when it is not shown deleted.
jobs_made=()
declare -A job_state=()

kc() {
  kubectl --context "$context" --request-timeout=60s "$@"
}

# first_line prints the first line of a file that is not blank, or nothing.
first_line() {
  awk 'NF {print; exit}' "$1"
}

# delete_job NAME deletes the gate's Job NAME and its pod, once, and reads back that envoy holds
# neither, and prints the line saying which.
delete_job() {
  local name=$1 left_job="" left_pods=""
  [ "${job_state[$name]:-}" = made ] || return 0
  job_state[$name]=tried
  kc -n "$job_namespace" delete job "$name" --cascade=foreground --wait=true --timeout=120s >/dev/null 2>"$work/delete.err" || true
  if left_job=$(kc -n "$job_namespace" get job "$name" --ignore-not-found -o name 2>"$work/get.err") &&
    left_pods=$(kc -n "$job_namespace" get pods -l "job-name=$name" -o name 2>>"$work/get.err") &&
    [ -z "$left_job$left_pods" ]; then
    job_state[$name]=deleted
    echo "JOB: deleted $job_namespace/$name, and $job_namespace holds no Job or pod of that name"
  else
    job_state[$name]=kept
    echo "JOB: $job_namespace/$name was not shown deleted${left_job:+ (still listed: $left_job)}$(first_line "$work/delete.err" | sed 's/^/ (/; s/$/)/')"
  fi
}

# run_job NAME SPEC LOG LINES creates the Job the file SPEC describes, named NAME, waits for it to
# end, keeps its log in LOG and its lines, read as the Job scripts print them, in LINES, and
# deletes it; it sets run_error to why the Job's reads are not whole, or "". A Job it could not
# create is never reported deleted.
run_job() {
  local name=$1 spec=$2 log=$3 lines=$4 deadline outcome=""
  run_error=""
  : >"$lines"
  if ! kc -n "$job_namespace" create -f "$spec" -o name >"$work/create.out" 2>"$work/create.err"; then
    run_error="the Job could not be created: $(first_line "$work/create.err")"
    return 0
  fi
  jobs_made+=("$name")
  job_state[$name]=made
  echo "JOB: created $job_namespace/$name at $(date -u +%Y-%m-%dT%H:%M:%SZ), owner $owner"
  deadline=$((SECONDS + job_deadline + 60))
  while :; do
    if kc -n "$job_namespace" get job "$name" -o json >"$work/job-status.json" 2>"$work/job-status.err"; then
      outcome=$(jq -r 'if (.status.succeeded // 0) >= 1 then "succeeded" elif (.status.failed // 0) >= 1 then "failed" else "" end' "$work/job-status.json")
    fi
    [ -z "$outcome" ] || break
    if [ "$SECONDS" -ge "$deadline" ]; then
      outcome="not finished after $((job_deadline + 60)) s"
      break
    fi
    sleep 2
  done
  if ! kc -n "$job_namespace" logs "job/$name" >"$log" 2>"$work/logs.err"; then
    run_error="the Job ($outcome) left no log that could be read: $(first_line "$work/logs.err")"
  elif ! jq -R -c 'split("\t") | select(length == 5)
      | {leg: .[0], arg: .[1], rc: (.[2] | tonumber? // -1), out: (.[3] | @base64d), err: (.[4] | @base64d)}' \
    "$log" >"$lines" 2>"$work/parse.err"; then
    run_error="the Job's log could not be read: $(first_line "$work/parse.err")"
  elif [ "$outcome" != succeeded ]; then
    run_error="the Job $outcome; its log ends: $(tail -n 1 "$log" | cut -c1-200)"
  fi
  delete_job "$name"
}

# cleanup deletes each Job when the gate stops before its own deletion ran (an error, a signal).
# shellcheck disable=SC2329 # cleanup runs through the EXIT trap below
cleanup() {
  local name
  for name in ${jobs_made[@]+"${jobs_made[@]}"}; do
    delete_job "$name" >&2 || true
  done
  rm -rf "$work"
}
trap cleanup EXIT

# Whose reading this is, for the Jobs' owner label: a uid with no passwd entry (a container run as
# an arbitrary uid) has no name, and is `unknown` rather than the end of the reading.
owner=$(id -un 2>/dev/null || echo unknown)
owner=$(printf '%s' "$owner" | tr -c 'A-Za-z0-9._-' '-' | cut -c1-63)
owner=${owner%-}
owner=${owner:-unknown}
readonly owner

started_at=$(date -u +%s)

# --- the listener Deployment, whose identity the Job takes and whose bus the reading is about -----
deployment_error=""
if ! kc -n "$job_namespace" get deployment "$listener_deployment" -o json >"$work/deployment.json" 2>"$work/deployment.err"; then
  deployment_error="the listener Deployment $job_namespace/$listener_deployment could not be read: $(first_line "$work/deployment.err")"
  echo '{}' >"$work/identity.json"
else
  jq -c --arg token "$token_file" "$listener_container"'
    .spec.template.spec as $s
    | [$s.containers[] | listener_container] as $listeners
    | ($listeners | first // {}) as $c
    | ([$c.env[]? | select(.name == "NATS_URLS") | .value // empty] | first // null) as $urls
    | (if $token != "" then $token else ([$c.env[]? | select(.name == "NATS_NKEY_SEED_FILE") | .value // empty] | first // null) end) as $file
    | (if $file == null then null else ([$c.volumeMounts[]? | . as $m | select($file | startswith(($m.mountPath | rtrimstr("/")) + "/"))] | first) end) as $mount
    | (if $mount == null then null else ([$s.volumes[]? | select(.name == $mount.name)] | first) end) as $volume
    | {listeners: ($listeners | length), sa: ($s.serviceAccountName // null), urls: $urls,
       credential: (if $token != "" then "token" else "seed" end), file: $file, mount: $mount, volume: $volume}' \
    "$work/deployment.json" >"$work/identity.json"
fi
# The bus the reading is about: the listener Deployment's NATS_URLS, and each NATS_URLS --bus-alias
# names as another address of the same NATS servers. A listener container on another bus (a
# load-test stack's listeners on their own NATS) is named and not read; one on this bus that is
# missed, by an address no --bus-alias named, is named by its durable instead (the judge's census),
# so nothing opens on it.
bus_urls=$(jq -r '.urls // ""' "$work/identity.json")
bus_aliases=$(jq -cn '$ARGS.positional' --args ${aliases[@]+"${aliases[@]}"})

# --- the listeners in the cluster -----------------------------------------------------------------
pods_error=""
if ! { kc get pods --all-namespaces -o json >"$work/pods.json" 2>"$work/pods.err" &&
  jq -c --arg urls "$bus_urls" --argjson aliases "$bus_aliases" "$listener_container"'
    def on_bus: .urls as $u | $urls == "" or $u == $urls or any($aliases[]; . == $u);
    # The machine a container runs as: its ENVOY_MACHINE_ID written as a literal value, trimmed as
    # the listener trims it (internal/config Load), or null when the gate cannot read it from the
    # pod spec: set through valueFrom, holding a $(VAR) reference, which Kubernetes expands from
    # the variables before it, empty once trimmed, or not set. machine_from says which of the
    # first two, or envFrom when the container sets none of its own and takes envFrom.
    def machine_entry: [.env[]? | select(.name == "ENVOY_MACHINE_ID")] | first;
    def reference: test("\\$\\(");
    def machine: machine_entry as $e
      | if $e == null or $e.valueFrom != null or ($e.value | type) != "string" then null
        else ($e.value | gsub("^\\s+|\\s+$"; "")) | if . == "" or reference then null else . end end;
    def machine_from: machine_entry as $e | (($e.value | strings) // "") as $v
      | if $e.valueFrom != null then "valueFrom"
        elif ($v | reference) then "reference"
        elif ($v | gsub("^\\s+|\\s+$"; "")) == "" and ((.envFrom // []) | length > 0) then "envFrom"
        else null end;
    # The names this gate reads that the container lists more than once, with every value in the
    # order listed: the kubelet takes the last entry of a name, and this reading the first.
    def duplicates: [.env[]? | select(.name == "ENVOY_MACHINE_ID" or .name == "NATS_URLS")]
      | group_by(.name) | map(select(length > 1)
        | {name: .[0].name, values: map(if .valueFrom != null then "(valueFrom)" else (.value // "") end)});
    [.items[] | . as $p | ($p.status.containerStatuses // []) as $statuses
      | $p.spec.containers[]
      | listener_container
      | . as $c
      | (($statuses | map(select(.name == $c.name)) | first) // {}) as $s
      | {ns: $p.metadata.namespace, pod: $p.metadata.name, uid: $p.metadata.uid, container: $c.name,
         running: ($s.state.running != null), state: ((($s.state // {}) | keys | first) // "not started"),
         restarts: ($s.restartCount // null),
         machine: ($c | machine), machine_from: ($c | machine_from), duplicates: ($c | duplicates),
         urls: ([$c.env[]? | select(.name == "NATS_URLS") | .value // empty] | first // null)}]
    | {bus: map(select(on_bus)), other: map(select(on_bus | not))}' \
    "$work/pods.json" >"$work/listener-sets.json" 2>"$work/pods.err" &&
  jq -c '.bus' "$work/listener-sets.json" >"$work/listeners.json" &&
  jq -c '.other' "$work/listener-sets.json" >"$work/other-bus.json"; }; then
  pods_error="the cluster's pods could not be listed: $(first_line "$work/pods.err")"
  echo '[]' >"$work/listeners.json"
  echo '[]' >"$work/other-bus.json"
fi

# Each running listener's newest "listener legacy subjects" log line: {ok, text, at}, the metrics
# text and the second it was read at, or {ok: false, error}. One jq lists the listeners' fields and
# one joins every read to its listener, so a reading starts one jq per running listener and not
# eight: a cluster runs a listener per machine.
: >"$work/reads.jsonl"
while IFS=$'\x1f' read -r ns pod container running; do
  read_json='{"ok": false, "error": "not running"}'
  if [ "$running" = true ]; then
    if kc -n "$ns" logs "$pod" -c "$container" --since="$log_window" >"$work/log" 2>"$work/log.err" </dev/null; then
      read_json=$(jq -R -n -c --arg window "$log_window" '[inputs | fromjson? | select(type == "object" and .msg == "listener legacy subjects")]
        | if length == 0
          then {ok: false, error: "its log holds no \"listener legacy subjects\" line from the last \($window): its build predates that line, or it is not logging it"}
          else last | {ok: true, text: (.metrics | strings // ""), at: (.at | numbers // null)} end' "$work/log")
    else
      read_json=$(jq -cn --arg error "$(first_line "$work/log.err")" '{ok: false, error: "its log could not be read: \($error)"}')
    fi
  fi
  printf '%s\n' "$read_json" >>"$work/reads.jsonl"
done < <(jq -r '.[] | [.ns, .pod, .container, (.running | tostring)] | join("\u001f")' "$work/listeners.json")
jq -c --slurpfile reads "$work/reads.jsonl" 'to_entries[] | .value + {read: $reads[.key]}' "$work/listeners.json" >"$work/pod-reads.jsonl"

# --- the listeners outside it ---------------------------------------------------------------------
: >"$work/outside-reads.jsonl"
for entry in ${outside[@]+"${outside[@]}"}; do
  name=${entry%%=*}
  url=${entry#*=}
  url=${url%/}
  if status=$(curl -sS --max-time 15 -o "$work/metrics" -w '%{http_code}' "$url/metrics" 2>"$work/metrics.err") && [ "$status" = 200 ]; then
    read_json=$(jq -cn --rawfile text "$work/metrics" '{ok: true, text: $text}')
  else
    read_json=$(jq -cn --arg error "${status:+HTTP $status }$(first_line "$work/metrics.err")" '{ok: false, error: "its /metrics could not be read: \($error)"}')
  fi
  jq -cn --arg name "$name" --arg url "$url" --argjson read "$read_json" '{name: $name, url: $url, read: $read}' >>"$work/outside-reads.jsonl"
done

# --- the five facts' reads, at a second reading -------------------------------------------------
# (a1): the release gate's census of pinned builds, as it prints it. (a2): the hub servers that must
# each answer the census, and the census connection's own name, drawn here so no client can name
# itself to be left out. (a3): the release gate's census of listener pins, the hub's auth.conf and,
# with --broker, the auth service's BROKER_NATS_SERVICES. A flag a fact needs and was not given is
# named, and the fact is unknown.
echo '{}' >"$work/facts.json"
census_name=""
if [ -n "$since" ]; then
  a1_missing=()
  [ -n "$deployment" ] || a1_missing+=("--deployment <owner/repo>")
  [ -n "$new_form_commit" ] || a1_missing+=("--new-form-commit <commit>")
  a1_error=""
  a1_rc=null
  : >"$work/a1.out"
  if [ "${#a1_missing[@]}" = 0 ]; then
    set +e
    "$release_gate" --deployment "$deployment" --clients ${dotfiles:+--dotfiles "$dotfiles"} "$new_form_commit" >"$work/a1.out" 2>"$work/a1.err"
    a1_rc=$?
    set -e
    if [ "$a1_rc" = 2 ]; then
      a1_error="it refused its arguments (exit 2): $(first_line "$work/a1.err")"
    fi
  fi
  census_missing=()
  [ -n "$new_form_commit" ] || census_missing+=("--new-form-commit <commit>")
  [ "${#census_missing[@]}" != 0 ] || census_name="legacy-subjects-gate-census-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
  hub_error=""
  : >"$work/hub-servers"
  if [ -n "$hub_statefulset" ]; then
    if kc -n "$job_namespace" get statefulset "$hub_statefulset" -o json >"$work/hub.json" 2>"$work/hub.err"; then
      # A StatefulSet's pods are <name>-<ordinal>, and each hub server is named after its pod
      # (SERVER_NAME, the pod's name): every ordinal the StatefulSet wants is a server that must
      # answer, and one that is coming or going makes the set no settled one.
      if ! jq -r -e '(.spec.replicas // 1) as $want | (.metadata.name // "") as $name
          | if (.status.replicas // -1) != $want or (.status.currentRevision // "") != (.status.updateRevision // "") or $name == "" then error("changing")
            else range(0; $want) | "\($name)-\(.)" end' "$work/hub.json" >"$work/hub-servers" 2>/dev/null; then
        hub_error="the StatefulSet $job_namespace/$hub_statefulset is no settled set of servers (its replicas or revisions differ between its spec and its status, or it cannot be read)"
        : >"$work/hub-servers"
      fi
    else
      hub_error="the StatefulSet $job_namespace/$hub_statefulset could not be read: $(first_line "$work/hub.err")"
    fi
  fi
  for server in ${hub_servers[@]+"${hub_servers[@]}"}; do
    echo "$server" >>"$work/hub-servers"
  done
  a3_missing=()
  [ -n "$deployment" ] || a3_missing+=("--deployment <owner/repo>")
  [ -n "$new_form_commit" ] || a3_missing+=("--new-form-commit <commit>")
  [ -n "$hub_statefulset" ] || a3_missing+=("--hub-statefulset <name>")
  a3_error=""
  a3_rc=null
  : >"$work/a3.out"
  if [ -n "$deployment" ] && [ -n "$new_form_commit" ]; then
    set +e
    "$release_gate" --deployment "$deployment" --listeners "$new_form_commit" >"$work/a3.out" 2>"$work/a3.err"
    a3_rc=$?
    set -e
    if [ "$a3_rc" = 2 ]; then
      a3_error="it refused its arguments (exit 2): $(first_line "$work/a3.err")"
    fi
  fi
  # The hub's users: the auth.conf key of the one ConfigMap the StatefulSet's pods mount that holds
  # one, read only from a StatefulSet that read settled.
  auth_error=""
  auth_configmap=""
  : >"$work/auth.conf"
  if [ -n "$hub_statefulset" ]; then
    if [ -n "$hub_error" ]; then
      auth_error=$hub_error
    else
      : >"$work/auth-holders"
      while IFS= read -r configmap; do
        if kc -n "$job_namespace" get configmap "$configmap" -o json >"$work/configmap.json" 2>"$work/configmap.err"; then
          if jq -e '.data | type == "object" and has("auth.conf")' "$work/configmap.json" >/dev/null 2>&1; then
            echo "$configmap" >>"$work/auth-holders"
            jq -j '.data["auth.conf"]' "$work/configmap.json" >"$work/auth.conf"
          fi
        else
          auth_error="the ConfigMap $job_namespace/$configmap the StatefulSet mounts could not be read: $(first_line "$work/configmap.err")"
          break
        fi
      done < <(jq -r '[.spec.template.spec.volumes[]? | (.configMap.name // empty), (.projected.sources[]? | .configMap.name // empty)] | unique[]' "$work/hub.json" 2>/dev/null)
      if [ -z "$auth_error" ]; then
        # BSD and macOS wc -l pad the count with spaces; $((holders)) reads it as the number alone.
        holders=$(wc -l <"$work/auth-holders")
        case "$((holders))" in
          1) auth_configmap=$(cat "$work/auth-holders") ;;
          0) auth_error="no ConfigMap the StatefulSet $job_namespace/$hub_statefulset mounts holds auth.conf" ;;
          *) auth_error="$((holders)) ConfigMaps the StatefulSet $job_namespace/$hub_statefulset mounts hold auth.conf: $(paste -sd' ' "$work/auth-holders")" ;;
        esac
      fi
    fi
  fi
  # The auth service's services, which a hub behind an auth callout admits: the one literal
  # BROKER_NATS_SERVICES of the Deployment --broker names. The judge reads it only for a hub whose
  # auth.conf holds the callout. Each replica admits by the list it read from its environment when
  # it started (internal/cipher/config, natscallout's decide), and every replica answers the callout
  # in one queue group, so the spec is what is admitted only once the Deployment has settled on it:
  # its controller has seen this generation, every replica it runs is of this spec and available,
  # and none is still terminating where the cluster counts those. A Deployment that is rolling out,
  # stalled included, may still admit through an older pod an entry its spec no longer lists.
  broker_read='{"given": false}'
  if [ -n "$broker" ]; then
    if kc -n "${broker%%/*}" get deployment "${broker#*/}" -o json >"$work/broker.json" 2>"$work/broker.err"; then
      broker_read=$(jq -c '(.spec.replicas // 1) as $want | (.status // {}) as $s
        | {given: true}
        + if (.metadata.generation | type) != "number" or $s.observedGeneration != .metadata.generation
            or ($s.replicas // 0) != $want or ($s.updatedReplicas // 0) != $want
            or ($s.unavailableReplicas // 0) != 0 or ($s.terminatingReplicas // 0) != 0 then
            {error: "it is rolling out, so an older pod may still admit an entry its spec no longer lists: generation \(.metadata.generation // "none"), observed \($s.observedGeneration // "none"); \($want) replica(s) wanted, \($s.replicas // 0) running, \($s.updatedReplicas // 0) updated, \($s.unavailableReplicas // 0) unavailable, \($s.terminatingReplicas // 0) terminating"}
          else
            [.spec.template.spec.containers[]? | .env[]? | select(.name == "BROKER_NATS_SERVICES")] as $e
            | if ($e | length) == 0 then {error: "its containers set no BROKER_NATS_SERVICES in their env (one set through envFrom cannot be read)"}
              elif ($e | length) > 1 then {error: "its containers set BROKER_NATS_SERVICES \($e | length) times"}
              elif ($e[0].value | type) != "string" or $e[0].valueFrom != null then {error: "its BROKER_NATS_SERVICES is no literal value (set through valueFrom)"}
              else {value: $e[0].value} end
          end' "$work/broker.json" 2>/dev/null) ||
        broker_read='{"given": true, "error": "its Deployment is no JSON object"}'
      broker_read=$(jq -c --arg broker "$broker" 'if .error then .error = "the Deployment \($broker): \(.error)" else . end' <<<"$broker_read")
    else
      broker_read=$(jq -cn --arg error "the Deployment $broker could not be read: $(first_line "$work/broker.err")" '{given: true, error: $error}')
    fi
  fi
  jq -n --arg commit "$new_form_commit" --argjson a1_rc "$a1_rc" --arg a1_error "$a1_error" \
    --rawfile a1_out "$work/a1.out" --arg census_name "$census_name" --arg hub_error "$hub_error" \
    --rawfile hub "$work/hub-servers" \
    --arg a1_missing "$(printf '%s\n' ${a1_missing[@]+"${a1_missing[@]}"})" \
    --arg census_missing "$(printf '%s\n' ${census_missing[@]+"${census_missing[@]}"})" \
    --arg a3_missing "$(printf '%s\n' ${a3_missing[@]+"${a3_missing[@]}"})" \
    --argjson a3_rc "$a3_rc" --arg a3_error "$a3_error" --rawfile a3_out "$work/a3.out" \
    --arg auth_error "$auth_error" --arg auth_configmap "$auth_configmap" --rawfile auth "$work/auth.conf" \
    --argjson broker "$broker_read" --slurpfile kinds "$callout_publishers" \
    --arg outside "$(printf '%s\n' ${hub_servers[@]+"${hub_servers[@]}"})" \
    --arg map "$(printf '%s\n' ${publishers[@]+"${publishers[@]}"})" '
    def lines: split("\n") | map(select(length > 0));
    def nullable: if . == "" then null else . end;
    {new_form_commit: ($commit | nullable),
     a1: {missing: ($a1_missing | lines), rc: $a1_rc, error: ($a1_error | nullable),
       lines: ($a1_out | lines)},
     census: {missing: ($census_missing | lines), name: ($census_name | nullable)},
     hub: {error: ($hub_error | nullable),
       servers: ($hub | split("\n") | map(select(length > 0)) | unique)},
     builds: {},
     a3: {missing: ($a3_missing | lines), rc: $a3_rc, error: ($a3_error | nullable), lines: ($a3_out | lines),
       auth: {error: ($auth_error | nullable), configmap: ($auth_configmap | nullable), text: $auth},
       broker: $broker, callout_kinds: $kinds[0].kinds, callout_session: $kinds[0].session, outside: ($outside | lines),
       map: [$map | lines | .[] | capture("^(?<identity>.+)=(?<key>[^=]+)$")]}}' >"$work/facts.json"
fi

# --- the NATS reads, from a Job that connects as the in-cluster listener --------------------------
job_ok=0
job_error=""
: >"$work/job-lines.jsonl"
if [ -n "$deployment_error" ]; then
  job_error=$deployment_error
else
  missing=$(jq -r '[if .listeners != 1 then "one listener container (it has \(.listeners))" else empty end,
      if .sa == null then "a ServiceAccount" else empty end,
      if .urls == null then "NATS_URLS" else empty end,
      if .file == null then "a NATS seed file (NATS_NKEY_SEED_FILE): name the listener'"'"'s projected NATS token with --token-file once the hub is not static" else empty end,
      if .file != null and .mount == null then "a volume mount holding \(if .credential == "token" then "the token file" else "the seed file" end) \(.file)" else empty end,
      if .mount != null and .volume == null then "the volume \(.mount.name)" else empty end] | join("; ")' "$work/identity.json")
  if [ -n "$missing" ]; then
    job_error="the listener Deployment $job_namespace/$listener_deployment lacks $missing"
  else
    name="legacy-subjects-gate-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
    # The pod mounts no Kubernetes API token: the NATS credential is the seed file's secret volume
    # (its CSI driver takes the pod's token from the kubelet, tokenRequests) or a projected volume,
    # and neither path needs the API.
    jq -n --arg name "$name" --arg namespace "$job_namespace" --arg owner "$owner" \
      --arg image "$nats_box" --rawfile script "$job_script" --argjson deadline "$job_deadline" \
      --rawfile forms "$here/lib/legacy-subjects-gate-forms.jq" \
      --arg inbox "$inbox_prefix" --slurpfile identity "$work/identity.json" '
      $identity[0] as $id
      | {"app.kubernetes.io/name": "legacy-subjects-gate", "created-by": "legacy-subjects-gate", owner: $owner} as $labels
      | "legacy-subjects-gate-work" as $scratch
      | {apiVersion: "batch/v1", kind: "Job",
         metadata: {name: $name, namespace: $namespace, labels: $labels},
         spec: {backoffLimit: 0, activeDeadlineSeconds: $deadline, ttlSecondsAfterFinished: 600,
           template: {metadata: {labels: $labels}, spec: {
             serviceAccountName: $id.sa, automountServiceAccountToken: false, restartPolicy: "Never",
             securityContext: {runAsNonRoot: true, runAsUser: 65534, runAsGroup: 65534, fsGroup: 65534,
               seccompProfile: {type: "RuntimeDefault"}},
             containers: [{name: "read", image: $image, command: ["sh", "-c", $script],
               env: ([{name: "NATS_URL", value: $id.urls},
                      (if $id.credential == "token" then {name: "NATS_TOKEN_FILE", value: $id.file} else {name: "NATS_NKEY", value: $id.file} end),
                      {name: "WORK_DIR", value: "/work"}, {name: "FORMS_JQ", value: $forms}]
                     + (if $inbox == "" then [] else [{name: "NATS_INBOX_PREFIX", value: $inbox}] end)),
               volumeMounts: [$id.mount + {readOnly: true}, {name: $scratch, mountPath: "/work"}],
               resources: {requests: {cpu: "250m", memory: "512Mi"}, limits: {cpu: "250m", memory: "512Mi"}},
               securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: ["ALL"]}}}],
             volumes: [$id.volume, {name: $scratch, emptyDir: {medium: "Memory", sizeLimit: "16Mi"}}]}}}}' >"$work/job.json"
    run_job "$name" "$work/job.json" "$work/job.log" "$work/job-lines.jsonl"
    if [ -n "$run_error" ]; then
      job_error=$run_error
    else
      job_ok=1
    fi
  fi
fi

# --- (a2)'s census of connections, from a Job of its own as the legacy-cutover user --------------
# It dials the listener Deployment's NATS_URLS, the bus this reading is about, and mounts nothing
# of the listener's.
if [ -n "$census_name" ]; then
  census_urls=$(jq -r '.urls // ""' "$work/identity.json")
  census_error=""
  : >"$work/census-lines.jsonl"
  if [ -z "$census_urls" ]; then
    census_error="the listener Deployment $job_namespace/$listener_deployment names no NATS_URLS for the census to dial${deployment_error:+: $deployment_error}"
  else
    name="legacy-subjects-census-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
    jq -n --arg name "$name" --arg namespace "$job_namespace" --arg owner "$owner" \
      --arg image "$nats_box" --rawfile script "$census_script" --argjson deadline "$job_deadline" \
      --arg url "$census_urls" --arg census_name "$census_name" --arg account "$cutover_account" \
      --arg class "$cutover_seed_class" --arg file "$cutover_seed_file" --arg inbox "$cutover_inbox" '
      {"app.kubernetes.io/name": "legacy-subjects-gate", "created-by": "legacy-subjects-gate-census", owner: $owner} as $labels
      | {apiVersion: "batch/v1", kind: "Job",
         metadata: {name: $name, namespace: $namespace, labels: $labels},
         spec: {backoffLimit: 0, activeDeadlineSeconds: $deadline, ttlSecondsAfterFinished: 600,
           template: {metadata: {labels: $labels}, spec: {
             serviceAccountName: $account, automountServiceAccountToken: false, restartPolicy: "Never",
             securityContext: {runAsNonRoot: true, runAsUser: 65534, runAsGroup: 65534, fsGroup: 65534,
               seccompProfile: {type: "RuntimeDefault"}},
             containers: [{name: "census", image: $image, command: ["sh", "-c", $script],
               env: [{name: "NATS_URL", value: $url}, {name: "CENSUS_NKEY", value: "/var/run/secrets/legacy-cutover/\($file)"},
                     {name: "CENSUS_NAME", value: $census_name}, {name: "CENSUS_INBOX_PREFIX", value: $inbox},
                     {name: "WORK_DIR", value: "/work"}],
               volumeMounts: [{name: "legacy-cutover-seed", mountPath: "/var/run/secrets/legacy-cutover", readOnly: true},
                 {name: "legacy-subjects-census-work", mountPath: "/work"}],
               resources: {requests: {cpu: "250m", memory: "256Mi"}, limits: {cpu: "250m", memory: "256Mi"}},
               securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: ["ALL"]}}}],
             volumes: [{name: "legacy-cutover-seed", csi: {driver: "secrets-store-gke.csi.k8s.io", readOnly: true, volumeAttributes: {secretProviderClass: $class}}},
               {name: "legacy-subjects-census-work", emptyDir: {medium: "Memory", sizeLimit: "16Mi"}}]}}}}' >"$work/census-job.json"
    run_job "$name" "$work/census-job.json" "$work/census.log" "$work/census-lines.jsonl"
    census_error=$run_error
    [ -n "$census_error" ] || [ "$(tail -n 1 "$work/census-lines.jsonl" | jq -r .leg 2>/dev/null)" = end ] ||
      census_error="the census Job's log ends before its last read, so what it read is not whole"
  fi
  jq --arg error "$census_error" --slurpfile lines "$work/census-lines.jsonl" \
    '.census.error = (if $error == "" then null else $error end) | .census.lines = $lines' "$work/facts.json" >"$work/facts.tmp" &&
    mv "$work/facts.tmp" "$work/facts.json"
fi

# --- (a2)'s builds: each build a connection of the census names, judged as daemon-release-gate.sh
# judges a pin: GitHub's compare of --new-form-commit with the build's commit, or with its release
# tag's, passes when the build is ahead of it or identical (behind_by 0). The builds are few (one per
# release and image in use); a census naming more than max_builds is not judged, and is unknown.
readonly max_builds=40
if [ -n "$census_name" ] && [ -s "$work/census-lines.jsonl" ]; then
  jq -r -L "$here/lib" -s 'include "legacy-subjects-gate-forms";
    [.[] | select(.leg == "census") | .out | split("\n")[] | select(length > 0) | try fromjson catch empty
      | .data.connections[]? | .name | build_token | strings] | unique[]' "$work/census-lines.jsonl" >"$work/builds" 2>/dev/null || : >"$work/builds"
  : >"$work/build-verdicts.jsonl"
  builds=$(wc -l <"$work/builds")
  if [ "$((builds))" -gt "$max_builds" ]; then
    jq -cn --argjson n "$((builds))" --argjson max "$max_builds" '{verdict: "unknown", detail: "the census names \($n) builds, more than the \($max) this gate asks GitHub about in one reading"}' >"$work/too-many"
  fi
  while IFS= read -r build; do
    case "$build" in
      legion@*) head=${build#legion@} ;;
      @sjawhar/pi-envoy@*) head="pi-envoy-v${build##*@}" ;;
      @sjawhar/claude-envoy@* | @sjawhar/codex-envoy@*) head="claude-envoy-v${build##*@}" ;;
      *) continue ;;
    esac
    if [ -s "$work/too-many" ]; then
      verdict=$(cat "$work/too-many")
    elif ! command -v gh >/dev/null 2>&1; then
      verdict='{"verdict": "unknown", "detail": "gh is not on PATH, so GitHub was not asked"}'
    elif gh api "repos/cybermonklabs/legion/compare/$new_form_commit...$head" --jq '{status, behind_by}' >"$work/compare" 2>"$work/compare.err"; then
      verdict=$(jq -c --arg head "$head" --arg commit "$new_form_commit" '
        if (.status == "ahead" or .status == "identical") and .behind_by == 0 then {verdict: "pass", detail: "\($head) is \(.status) of \($commit)"}
        elif .status == "behind" or .status == "diverged" then {verdict: "shut", detail: "\($head) is \(.status) \(if .status == "behind" then "" else "from " end)\($commit) (behind by \(.behind_by))"}
        else {verdict: "unknown", detail: "GitHub compared \($head) with \($commit) as \(.status // "nothing")"} end' "$work/compare" 2>/dev/null) ||
        verdict='{"verdict": "unknown", "detail": "GitHub'"'"'s compare answered no status"}'
    else
      verdict=$(jq -cn --arg head "$head" --arg error "$(first_line "$work/compare.err")" '{verdict: "unknown", detail: "GitHub could not compare \($head): \($error)"}')
    fi
    jq -cn --arg build "$build" --argjson verdict "$verdict" '{key: $build, value: $verdict}' >>"$work/build-verdicts.jsonl"
  done <"$work/builds"
  jq --slurpfile verdicts "$work/build-verdicts.jsonl" '.builds = ($verdicts | from_entries)' "$work/facts.json" >"$work/facts.tmp" &&
    mv "$work/facts.tmp" "$work/facts.json"
fi

finished_at=$(date -u +%s)

# --- the judgement --------------------------------------------------------------------------------
jq -n \
  --arg context "$context" \
  --argjson started "$started_at" \
  --argjson finished "$finished_at" \
  --argjson earlier "$earlier" \
  --arg pods_error "$pods_error" \
  --slurpfile pods "$work/pod-reads.jsonl" \
  --slurpfile other_bus "$work/other-bus.json" \
  --arg bus "$bus_urls" \
  --argjson bus_aliases "$bus_aliases" \
  --slurpfile outside "$work/outside-reads.jsonl" \
  --slurpfile lines "$work/job-lines.jsonl" \
  --argjson job_ok "$job_ok" \
  --arg job_error "$job_error" \
  --slurpfile facts "$work/facts.json" \
  '{context: $context, started_at: $started, finished_at: $finished, earlier: $earlier,
    pods_error: (if $pods_error == "" then null else $pods_error end),
    pods: $pods, other_bus: ($other_bus[0] // []), bus: $bus, bus_aliases: $bus_aliases, outside: $outside,
    job: {ok: ($job_ok == 1), error: (if $job_error == "" then null else $job_error end), lines: $lines},
    facts: $facts[0]}' >"$work/input.json"
# A judgement that fails on what it was given (a fault in the judge itself) is a reading that could
# not be judged: UNKNOWN, with a line and a record naming the failure, never a bare jq exit.
if ! jq -L "$here/lib" -f "$judge" "$work/input.json" >"$work/judgement.json" 2>"$work/judge.err"; then
  jq -n --arg context "$context" --argjson started "$started_at" --argjson finished "$finished_at" \
    --argjson earlier "$earlier" --arg error "$(first_line "$work/judge.err")" '
    "the judgement could not read what the gate read: \($error | gsub("[[:cntrl:]]"; "?"))" as $why
    | {verdict: "unknown", lines: ["UNKNOWN: " + $why],
       record: {gate: "legacy-subjects-gate", version: 3, context: $context, started_at: $started,
         finished_at: $finished, read_at: ($started | todate), verdict: "unknown",
         compared_with: (if $earlier == null then null else $earlier.read_at end),
         identity: null, stream: null, listeners: [], consumers_old: [], subjects_old: [], interests_old: [],
         shut: [], unknown: [$why]}}' >"$work/judgement.json"
fi

jq -r '.lines[]' "$work/judgement.json"
for name in ${jobs_made[@]+"${jobs_made[@]}"}; do
  [ "${job_state[$name]}" != deleted ] || continue
  echo "UNKNOWN: the Job $job_namespace/$name was not shown deleted; delete it: kubectl --context $context -n $job_namespace delete job $name"
  jq --arg name "$name" '.record.unknown += ["the Job \($name) was not shown deleted"] | (.record.verdict, .verdict) |= (if . == "shut" then . else "unknown" end)' \
    "$work/judgement.json" >"$work/judgement.tmp" && mv "$work/judgement.tmp" "$work/judgement.json"
done
# The record is written as a new file (noclobber): one made since the gate began is left as it is,
# and a record not kept is a reading that cannot be compared.
if ! (set -o noclobber && jq '.record' "$work/judgement.json" >"$record") 2>"$work/record.err"; then
  echo "UNKNOWN: the record $record could not be written, so this reading cannot be compared: $(first_line "$work/record.err")"
  echo "UNKNOWN"
  exit 3
fi

verdict=$(jq -r .verdict "$work/judgement.json")
case "$verdict" in
  open) echo "GATE OPEN"; exit 0 ;;
  shut) echo "GATE SHUT"; exit 1 ;;
  unknown) echo "UNKNOWN"; exit 3 ;;
  clean)
    again=$(jq -r '.again | todate' "$work/judgement.json")
    found=$(jq -r .found "$work/judgement.json")
    last_seq=$(jq -r .last_seq "$work/judgement.json")
    echo "FIRST READING: $record holds a first reading of every surface, with $found FOUND line(s) naming old-form names still in use; read again at or after $again with --since $record and the five facts' flags: that reading opens only if the stream then holds no old-form message (none after stream sequence $last_seq), no listener's count moved, no interest record or consumer filter holds an old form, and the pins, the running clients, the publishers and the consumers are all past the old form"
    exit 4
    ;;
esac
echo "UNKNOWN"
exit 3
