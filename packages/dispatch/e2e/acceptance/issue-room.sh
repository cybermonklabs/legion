#!/usr/bin/env bash
# The driver of the check c-one-room of the issue tree's Verification document: a person edits one
# room per issue, and the server, not the browser, decides which room a socket reaches.
#
# It empties the database DATABASE_URL names (a database on this machine whose name ends
# `_upgrade`, which it drops and creates again), boots the release BASELINE_SHA names on it through
# lib-baseline.sh, signs DEV_SIGNIN_LOGIN in, creates PROJECT, and writes through that release's API
# three issues: P, in flight, with its spec, verification-md and plan-md and a comment of that
# person; C, a child of P whose spec holds a requirement of its own, with its three documents; and
# D, closed (`done`), with its three documents, which the conversion passes over. It stops that
# server, boots this head's over the same rows with the hold settings lib-tree.sh gives every
# upgrade boot, waits for the backfill as the Verification preamble states it, waits for this
# head's build announcement, and runs `envoy-dispatch convert-issues --project PROJECT`, reading P
# and C converted and D passed over.
#
# The one room. On P, as the person, it writes a comment and a criterion under its first block
# (POST /api/v1/artifacts/{tree}/edits on P's tree artifact). It opens the document socket for P's
# tree node and writes through it a requirement whose serves names that criterion, into the spec's
# fragment of the room, and reads that block back through GET /api/v1/issues/{key}/tree under the
# criterion, with no second socket opened. It opens the socket for an issue tree node it invented
# and reads the 404 the room check gives.
#
# The server decides the room. It opens the socket for P's spec document by its own uuid and reads
# the admission to P's room: the room's Yjs document holds a fragment for each of P's three
# documents. The paragraph it then writes there reads back in P's tree. P's spec's document_rooms
# row names P's tree node as its room. The socket for D's spec, whose issue has not converted,
# is admitted to D's spec alone (one fragment, no document_rooms row, the artifact read's room its
# own id).
#
# Two overlapping views. It opens two sockets on P's room and holds both, one through the spec's
# uuid and one through the Verification document's, and brings both to one state vector. The
# second stops taking the room's updates. The first writes a paragraph into the Verification
# document's fragment, the part the second draws, and the driver waits until the issue's version
# count rises by one. The second then writes a paragraph into the spec's fragment, the part the
# first draws, from the state vector it held before that first write. Both writes stand: both
# views read both, each document's text read holds its paragraph, and the version count rose by
# the two writes and no more. Where one is missing it prints the one that disappeared.
#
# The stored tables keep their shape. It reads the columns of doc_updates, doc_snapshots and
# doc_checkpoints out of the catalog of the upgraded database and out of a database this head's
# migrations build from zero (a database of its own beside it, migrated by a boot of this head and
# dropped again), finds them equal, and finds each table's foreign key to artifacts (id) in both.
#
# A node whose parent is in an issue above. The person writes, in C's tree, a criterion whose
# parent names P's comment. A write naming it through P's tree artifact is refused 409
# NODE_IN_OTHER_ISSUE naming C, and the write in C's own tree takes; both trees read the new text,
# P's marking it read-only and C's. C's ?view=spec answers it after the nodes of C's own walk, C's
# spec digest moves for the write, and P's stays as it was.
#
# The socket speaks the Hocuspocus framing the browser's provider speaks, through a Bun program
# this driver writes into its own work directory (issue_room_peer_program) and drives line by line.
#
# Inputs: DATABASE_URL, PROJECT, DEV_SIGNIN_LOGIN, BASELINE_SHA (or BASELINE_BINARY),
# DISPATCH_TOKEN (the agent bearer every server here accepts), envoy-dispatch on PATH, and bun with
# this package's node_modules installed (yjs, y-protocols, lib0). Each passing check prints
# `PASS: <section> <what>`; the first failing one prints `FAIL: <section> <what>: <observed>` and
# the script exits 1; a missing input exits 2. It prints no SKIP line, and its last line is
# `PASS: issue-room.sh complete <N> sections`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<name>_upgrade PROJECT=CORE \
         DISPATCH_TOKEN=e2e-token DEV_SIGNIN_LOGIN=alice BASELINE_SHA=<commit> \
         bash e2e/acceptance/issue-room.sh
       (run from packages/dispatch, with the stamped envoy-dispatch on PATH and bun install run;
       DATABASE_URL names a database on this machine whose name ends _upgrade, which this run
       drops and creates again)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-baseline.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-baseline.sh"
# shellcheck source-path=SCRIPTDIR source=lib-tree.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-tree.sh"
# shellcheck source-path=SCRIPTDIR source=seed-corpus.sh
. "$(dirname "${BASH_SOURCE[0]}")/seed-corpus.sh"

require_env DATABASE_URL PROJECT DEV_SIGNIN_LOGIN
require_commands bun curl diff head jq ln openssl psql tail
readonly driver=issue-room.sh
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
project="$PROJECT"
readonly project
# Not readonly: lib-baseline.sh's baseline_boot takes a local of that name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
database_url="$DATABASE_URL"
readonly person_login="$DEV_SIGNIN_LOGIN"
dispatch_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly dispatch_dir
[[ -d "$dispatch_dir/node_modules/yjs" && -d "$dispatch_dir/node_modules/y-protocols" && -d "$dispatch_dir/node_modules/lib0" ]] || {
  printf 'the socket peer imports yjs, y-protocols and lib0 from %s/node_modules: run bun install first\n' "$dispatch_dir" >&2
  exit 2
}
readonly tables="'doc_updates', 'doc_snapshots', 'doc_checkpoints'"
# How long the peer waits for a socket's answer, and how long the driver waits for one line of it.
readonly peer_answer_seconds=60

work_dir
stamp="$(date +%s)-$$"
readonly stamp
sections=0
catalog_url=""
catalog_pid=""
peer_pid=""
cleanup() {
  local code=$?
  if [[ -n "$peer_pid" ]]; then
    kill "$peer_pid" 2>/dev/null || true
    wait "$peer_pid" 2>/dev/null || true
  fi
  baseline_stop_pid "$catalog_pid"
  tree_stop_head
  [[ -z "$catalog_url" ]] || issue_room_drop_catalog
  baseline_stop
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# issue_room_database_parts: database_url split into the server it names (database_server, its
# URL with the `postgres` database in place of its own) and its database's name (database_name),
# for the drop and the create of a database beside it. It refuses a URL whose database is not on
# this machine or whose name does not end `_upgrade`, the database the Verification preamble gives
# the upgrade drivers.
issue_room_database_parts() {
  loopback_database_url "$database_url" ||
    tree_refuse "DATABASE_URL names no single loopback postgres database: this driver drops and creates it"
  [[ "$database_url" =~ ^(postgres(ql)?://)([^/?]*)/([^/?]+)(\?.*)?$ ]] ||
    tree_refuse "DATABASE_URL is no postgres:// URL naming a database"
  database_server="${BASH_REMATCH[1]}${BASH_REMATCH[3]}/postgres${BASH_REMATCH[5]}"
  database_name="${BASH_REMATCH[4]}"
  database_query="${BASH_REMATCH[5]}"
  database_prefix="${BASH_REMATCH[1]}${BASH_REMATCH[3]}"
  [[ "$database_name" =~ ^[a-z0-9_]+_upgrade$ ]] ||
    tree_refuse "DATABASE_URL names the database $database_name: this driver drops and creates only a database whose name ends _upgrade"
}

# issue_room_empty_database: drops the database database_url names and creates it empty, so the
# earlier release's own Store.Migrate writes its schema and the run's rows are its only content.
issue_room_empty_database() {
  baseline_psql "$database_server" -q \
    -c "drop database if exists \"$database_name\" with (force)" -c "create database \"$database_name\"" >"$work/reset" 2>&1 ||
    fail setup "empty the database $database_name" "psql: $(<"$work/reset")"
  pass setup "dropped the database $database_name and created it empty"
}

# issue_room_drop_catalog: drops the database the catalog comparison migrated from zero, from the
# cleanup too; a refusal is printed and stops nothing.
issue_room_drop_catalog() {
  baseline_psql "$database_server" -q -c "drop database if exists \"${database_name}_catalog\" with (force)" \
    >"$work/catalog-drop" 2>&1 ||
    printf '%s: could not drop the database %s_catalog: %s\n' "$driver" "$database_name" "$(<"$work/catalog-drop")" >&2
  catalog_url=""
}

# issue_room_key SECTION WHAT VALUE and issue_room_uuid SECTION WHAT VALUE: VALUE is an issue key,
# or a uuid as Dispatch writes one, which the SQL below names.
issue_room_key() {
  [[ "$3" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail "$1" "$2" "'$3' is no issue key"
}
issue_room_uuid() {
  [[ "$3" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] || fail "$1" "$2" "'$3' is no uuid"
}

# issue_room_outcome KEY: what the last conversion's output in $work/out printed for the issue
# KEY, in outcome: `converted`, `passed over`, `deferred` or `none`, read off the line that names
# the key as a word.
issue_room_outcome() {
  local pattern="(^|[^A-Za-z0-9-])${1}([^0-9]|\$)" line
  outcome=none
  while IFS= read -r line; do
    [[ "$line" =~ $pattern ]] || continue
    case "$line" in
      *deferred*) outcome=deferred ;;
      *"passed over"*) outcome="passed over" ;;
      *converted*) outcome=converted ;;
    esac
  done <"$work/out"
}

# issue_room_tree SECTION ISSUE: ISSUE's tree artifact, read off GET /api/v1/issues/{key}/tree,
# in tree_value, and whether the issue converted in converted.
issue_room_tree() {
  tree_read agent "$2"
  tree_want 200 "$1" "read $2's tree"
  converted="$(jq -r '.converted' "$work/body")"
  tree_value="$(jq -r '.tree // ""' "$work/body")"
}

# issue_room_versions ISSUE: how many versions the documents of ISSUE hold, in versions.
issue_room_versions() {
  query versions "count the versions of $1's documents" "
    select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id where a.issue_key = '$1'"
  versions="$answer"
}

# issue_room_digest SECTION ISSUE: the digest of the newest version of ISSUE's spec, as GET
# /api/v1/issues/{key}/artifacts/spec answers it, in digest; empty while it answers none.
issue_room_digest() {
  tree_send GET "/api/v1/issues/$2/artifacts/spec"
  tree_want 200 "$1" "read $2's spec and its versions"
  digest="$(jq -r '(.versions | max_by(.number) | .digest) // ""' "$work/body")"
}

# issue_room_node SECTION ISSUE JQ-ARGS… PREDICATE: the first node of ISSUE's tree read with no
# view of which the jq PREDICATE holds (JQ-ARGS bind its variables), as compact JSON in node,
# empty where the read holds none. It reads every page.
issue_room_node() {
  local section="$1" issue="$2" cursor=""
  shift 2
  local predicate="${*: -1}"
  local -a options=("${@:1:$#-1}")
  node=""
  while true; do
    tree_read agent "$issue" "" "" "$cursor"
    tree_want 200 "$section" "read $issue's tree"
    node="$(jq -c "${options[@]}" "[.nodes[] | select($predicate)][0] // empty" "$work/body")" ||
      fail "$section" "read $issue's tree" "jq could not read it: $(body)"
    [[ -z "$node" ]] || return 0
    cursor="$(jq -r '.cursor // ""' "$work/body")"
    [[ -n "$cursor" ]] || return 0
  done
}

# issue_room_wait_node SECTION ISSUE WHAT JQ-ARGS… PREDICATE: waits, every 2 s for at most 300 s,
# until ISSUE's tree read answers a node PREDICATE holds of (issue_room_node), and fails naming
# WHAT with the last page it read.
issue_room_wait_node() {
  local section="$1" issue="$2" what="$3" deadline=$((SECONDS + tree_wait_seconds))
  shift 3
  while true; do
    issue_room_node "$section" "$issue" "$@"
    [[ -z "$node" ]] || return 0
    ((SECONDS < deadline)) ||
      fail "$section" "$what" "after ${tree_wait_seconds} s $issue's tree holds no such node: $(body)"
    sleep "$tree_poll_seconds"
  done
}

# issue_room_cookie: the person's session cookie, out of the jar tree_sign_in wrote, as the Cookie
# header's value, in $work/cookie, which the socket peer reads.
issue_room_cookie() {
  local domain flag path secure expiry name value
  : >"$work/cookie"
  while IFS=$'\t' read -r domain flag path secure expiry name value; do
    [[ "$name" == dsession ]] || continue
    printf 'dsession=%s' "$value" >"$work/cookie"
  done <"$work/jar"
  [[ -s "$work/cookie" ]] || fail setup "read the person's session cookie" "the jar holds no dsession cookie"
}

# issue_room_socket_status DOCUMENT: the HTTP status a document socket's upgrade request for
# DOCUMENT is answered, as the person, in socket_status: the room check answers it before any
# upgrade.
issue_room_socket_status() {
  socket_status="$(curl -sS -o "$work/socket-body" -w '%{http_code}' -b "$work/jar" -H "Origin: $(tree_origin)" \
    -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' \
    -H "Sec-WebSocket-Key: $(openssl rand -base64 16)" \
    "$base/ws/doc/$1?schema_version=$schema" 2>"$work/curl")" || socket_status="curl failed: $(<"$work/curl")"
}

# issue_room_peer_program: writes the socket peer, a Bun program, to $work/issue-room-peer.ts,
# beside a link to this package's node_modules, which it imports from.
#
# `bun issue-room-peer.ts <server> <cookie file> <schema version>` holds document sockets, each
# by a name, as the person whose Cookie header the file holds, and answers each command it reads
# on stdin with one line on stdout. A socket speaks the Hocuspocus framing the browser's provider
# speaks: each frame starts with the document name the socket asked for, the token frame carries
# the schema version, a sync step 1 follows the server's Authenticated answer, and the server
# answers each update it is sent with one SyncStatus frame.
#   open NAME DOCUMENT       opens NAME on DOCUMENT (a closed NAME reconnects, keeping its state)
#                            and syncs it: `opened NAME <scope> <fragment,...>`, or
#                            `refused NAME <why>`
#   freeze NAME / thaw NAME  stops taking the room's updates into NAME, or applies those held
#   sv NAME                  `sv NAME <base64 state vector>`
#   fragments NAME           `fragments NAME <fragment,...>`, every shared type of NAME's document
#                            but the marks map; `open` answers the same list
#   append NAME FRAGMENT J   appends the block the JSON J describes - {type, id, text, attrs?} - at
#                            the end of FRAGMENT and waits for the server's answer:
#                            `appended NAME <1|0>`, or `unsent NAME` on a closed socket
#   blocks NAME FRAGMENT     `blocks NAME <json>`: each element of FRAGMENT with a blockId, {id,
#                            type, text}, in document order
#   wait-close NAME SECONDS  `closed NAME <code> <reason>` once the socket closes, else `open NAME`
#   close NAME               `closed NAME`
#   quit
issue_room_peer_program() {
  ln -s "$dispatch_dir/node_modules" "$work/node_modules"
  cat >"$work/issue-room-peer.ts" <<'PEER'
import * as Y from "yjs";
import * as encoding from "lib0/encoding";
import * as decoding from "lib0/decoding";

// Bun's WebSocket client takes request headers; the DOM WebSocket type does not.
const BunWebSocket = WebSocket as unknown as new (
  url: string,
  options: { readonly headers: Record<string, string> }
) => WebSocket;

const messageSync = 0;
const messageAuth = 2;
const messageSyncStatus = 8;
const syncStep1 = 0;
const syncStep2 = 1;
const syncUpdate = 2;
const authToken = 0;
const authDenied = 1;
const authAuthenticated = 2;
const answerWithinMs = Number(process.env.PEER_ANSWER_MS ?? "60000");

const [server, cookieFile, schemaVersion] = Bun.argv.slice(2);
if (server === undefined || cookieFile === undefined || schemaVersion === undefined) {
  console.error("usage: bun issue-room-peer.ts <server> <cookie file> <schema version>");
  process.exit(2);
}
const origin = new URL(server).origin;
const cookie = (await Bun.file(cookieFile).text()).trim();

type Peer = {
  doc: Y.Doc;
  asked: string;
  socket?: WebSocket;
  scope?: string;
  denied?: string;
  synced: boolean;
  frozen: boolean;
  held: Uint8Array[];
  sent: number;
  statuses: number[];
  closed?: string;
};
const peers = new Map<string, Peer>();

function frame(name: string, write: (encoder: encoding.Encoder) => void): Uint8Array {
  const encoder = encoding.createEncoder();
  encoding.writeVarString(encoder, name);
  write(encoder);
  return encoding.toUint8Array(encoder);
}

function sendSync(peer: Peer, subtype: number, payload: Uint8Array): boolean {
  if (peer.socket === undefined || peer.socket.readyState !== WebSocket.OPEN) return false;
  peer.socket.send(
    frame(peer.asked, (encoder) => {
      encoding.writeVarUint(encoder, messageSync);
      encoding.writeVarUint(encoder, subtype);
      encoding.writeVarUint8Array(encoder, payload);
    })
  );
  if (subtype !== syncStep1) peer.sent += 1;
  return true;
}

async function until(test: () => boolean, ms = answerWithinMs): Promise<boolean> {
  const deadline = Date.now() + ms;
  while (!test()) {
    if (Date.now() > deadline) return false;
    await Bun.sleep(50);
  }
  return true;
}

function receive(peer: Peer, data: ArrayBuffer): void {
  const decoder = decoding.createDecoder(new Uint8Array(data));
  decoding.readVarString(decoder);
  const type = decoding.readVarUint(decoder);
  if (type === messageAuth) {
    const subtype = decoding.readVarUint(decoder);
    const text = decoding.readVarString(decoder);
    if (subtype === authAuthenticated) {
      peer.scope = text;
      sendSync(peer, syncStep1, Y.encodeStateVector(peer.doc));
    } else if (subtype === authDenied) {
      peer.denied = text;
    }
    return;
  }
  if (type === messageSyncStatus) {
    peer.statuses.push(decoding.readVarUint(decoder));
    return;
  }
  if (type !== messageSync) return;
  const subtype = decoding.readVarUint(decoder);
  const payload = decoding.readVarUint8Array(decoder);
  if (subtype === syncStep1) {
    sendSync(peer, syncStep2, Y.encodeStateAsUpdate(peer.doc, payload));
    return;
  }
  if (subtype === syncStep2 || subtype === syncUpdate) {
    if (peer.frozen) peer.held.push(payload);
    else Y.applyUpdate(peer.doc, payload, "server");
    if (subtype === syncStep2) peer.synced = true;
  }
}

async function open(name: string, asked: string): Promise<string> {
  let peer = peers.get(name);
  if (peer !== undefined && peer.closed === undefined) return `refused ${name} it is open already`;
  if (peer === undefined) {
    const doc = new Y.Doc();
    peer = { doc, asked, synced: false, frozen: false, held: [], sent: 0, statuses: [] };
    peers.set(name, peer);
    const held = peer;
    doc.on("update", (update: Uint8Array, source: unknown) => {
      if (source !== "server") sendSync(held, syncUpdate, update);
    });
  }
  Object.assign(peer, { asked, scope: undefined, denied: undefined, closed: undefined, synced: false, sent: 0, statuses: [] });
  const url = `${origin.replace(/^http/, "ws")}/ws/doc/${encodeURIComponent(asked)}?schema_version=${encodeURIComponent(schemaVersion)}`;
  const socket = new BunWebSocket(url, { headers: { Cookie: cookie, Origin: origin } });
  socket.binaryType = "arraybuffer";
  peer.socket = socket;
  const held = peer;
  socket.addEventListener("open", () =>
    socket.send(
      frame(asked, (encoder) => {
        encoding.writeVarUint(encoder, messageAuth);
        encoding.writeVarUint(encoder, authToken);
        encoding.writeVarString(encoder, schemaVersion);
      })
    )
  );
  socket.addEventListener("message", (event) => {
    if (event.data instanceof ArrayBuffer) receive(held, event.data);
  });
  socket.addEventListener("close", (event) => {
    held.closed = `${event.code} ${event.reason || "-"}`;
  });
  socket.addEventListener("error", () => {
    held.closed ??= "error -";
  });
  const settled = await until(() => held.synced || held.denied !== undefined || held.closed !== undefined);
  if (held.denied !== undefined) return `refused ${name} denied: ${held.denied}`;
  if (held.closed !== undefined) return `refused ${name} closed: ${held.closed}`;
  if (!settled) return `refused ${name} no sync within ${answerWithinMs / 1000} s`;
  await until(() => held.statuses.length >= held.sent);
  return `opened ${name} ${held.scope} ${fragments(held)}`;
}

// The fragments a socket's document holds: every shared type but the marks map (docs' marksMapName).
function fragments(peer: Peer): string {
  return [...peer.doc.share.keys()].filter((name) => name !== "marks").sort().join(",");
}

function build(spec: { type: string; id: string; text: string; attrs?: Record<string, unknown> }): Y.XmlElement {
  const element = new Y.XmlElement(spec.type);
  element.setAttribute("blockId", spec.id);
  for (const [key, value] of Object.entries(spec.attrs ?? {})) {
    element.setAttribute(key, value as string);
  }
  if (spec.type === "paragraph" || spec.type === "heading") {
    element.insert(0, [new Y.XmlText(spec.text)]);
  } else {
    const paragraph = new Y.XmlElement("paragraph");
    paragraph.setAttribute("blockId", `${spec.id}-text`);
    paragraph.insert(0, [new Y.XmlText(spec.text)]);
    element.insert(0, [paragraph]);
  }
  return element;
}

function blocks(fragment: Y.XmlFragment): { id: string; type: string; text: string }[] {
  const found: { id: string; type: string; text: string }[] = [];
  const walk = (node: Y.XmlElement | Y.XmlFragment) => {
    for (const child of node.toArray()) {
      if (!(child instanceof Y.XmlElement)) continue;
      const id = child.getAttribute("blockId");
      if (typeof id === "string" && id !== "") {
        const text = child
          .toArray()
          .map((part) => (part instanceof Y.XmlText ? part.toString() : part instanceof Y.XmlElement ? part.toString() : ""))
          .join("");
        found.push({ id, type: child.nodeName, text: text.replace(/<[^>]*>/g, "") });
      }
      walk(child);
    }
  };
  walk(fragment);
  return found;
}

async function run(line: string): Promise<string> {
  const [command = "", name = "", ...rest] = line.trim().split(" ");
  const peer = peers.get(name);
  switch (command) {
    case "open":
      return open(name, rest[0] ?? "");
    case "quit":
      for (const held of peers.values()) held.socket?.close();
      process.exit(0);
  }
  if (peer === undefined) return `error ${name} no socket of that name`;
  switch (command) {
    case "freeze":
      peer.frozen = true;
      return `frozen ${name}`;
    case "thaw": {
      peer.frozen = false;
      for (const update of peer.held.splice(0)) Y.applyUpdate(peer.doc, update, "server");
      return `thawed ${name}`;
    }
    case "sv":
      return `sv ${name} ${Buffer.from(Y.encodeStateVector(peer.doc)).toString("base64")}`;
    case "fragments":
      return `fragments ${name} ${fragments(peer)}`;
    case "append": {
      const fragment = rest[0] ?? "";
      const spec = JSON.parse(rest.slice(1).join(" "));
      if (peer.closed !== undefined) return `unsent ${name}`;
      const before = peer.sent;
      peer.doc.transact(() => {
        const target = peer.doc.getXmlFragment(fragment);
        target.insert(target.length, [build(spec)]);
      }, "local");
      if (peer.sent === before) return `unsent ${name}`;
      const answered = await until(() => peer.statuses.length >= peer.sent || peer.closed !== undefined);
      if (!answered || peer.statuses.length < peer.sent) return `unsent ${name}`;
      return `appended ${name} ${peer.statuses[peer.statuses.length - 1]}`;
    }
    case "blocks":
      return `blocks ${name} ${JSON.stringify(blocks(peer.doc.getXmlFragment(rest[0] ?? "")))}`;
    case "wait-close": {
      await until(() => peer.closed !== undefined, Number(rest[0] ?? "10") * 1000);
      return peer.closed === undefined ? `open ${name}` : `closed ${name} ${peer.closed}`;
    }
    case "close":
      peer.socket?.close();
      await until(() => peer.closed !== undefined, 10_000);
      return `closed ${name}`;
    default:
      return `error ${name} unknown command ${command}`;
  }
}

for await (const line of console) {
  if (line.trim() === "") continue;
  try {
    console.log(await run(line));
  } catch (error) {
    console.log(`error - ${error instanceof Error ? error.message : String(error)}`);
  }
}
PEER
}

# issue_room_peer_start: starts the socket peer against $base as the person, on schema version
# $schema, as a coprocess whose stdin and stdout the driver holds.
issue_room_peer_start() {
  issue_room_cookie
  coproc ISSUE_ROOM_PEER {
    PEER_ANSWER_MS=$((peer_answer_seconds * 1000)) exec bun "$work/issue-room-peer.ts" "$base" "$work/cookie" "$schema" 2>"$work/peer.err"
  }
  peer_pid="$ISSUE_ROOM_PEER_PID"
  exec {peer_in}>&"${ISSUE_ROOM_PEER[1]}" {peer_out}<&"${ISSUE_ROOM_PEER[0]}"
}

# issue_room_peer SECTION WHAT COMMAND…: sends COMMAND to the peer and reads its one line into
# reply; a peer that answers nothing within its bound, or answers `error`, fails naming WHAT.
issue_room_peer() {
  local section="$1" what="$2"
  shift 2
  printf '%s\n' "$*" >&"$peer_in" ||
    fail "$section" "$what" "the socket peer stopped: $(tail -n 20 "$work/peer.err" 2>/dev/null)"
  IFS= read -r -t $((peer_answer_seconds + 30)) reply <&"$peer_out" ||
    fail "$section" "$what" "the socket peer answered nothing to '$1': $(tail -n 20 "$work/peer.err" 2>/dev/null)"
  [[ "$reply" != error* ]] || fail "$section" "$what" "the socket peer answered: $reply"
  printf 'peer: %s -> %s\n' "$1 ${2:-}" "$reply"
}

# issue_room_open SECTION NAME DOCUMENT: opens the socket NAME on DOCUMENT, which must be admitted
# writable; its fragments in fragments.
issue_room_open() {
  issue_room_peer "$1" "open a document socket on $3" open "$2" "$3"
  [[ "$reply" =~ ^opened\ $2\ read-write\ (.*)$ ]] ||
    fail "$1" "a document socket on $3 is admitted writable" "the peer answered: $reply"
  fragments="${BASH_REMATCH[1]}"
}

# issue_room_append SECTION NAME FRAGMENT JSON: NAME appends the block JSON describes to FRAGMENT,
# and the room applies it.
issue_room_append() {
  issue_room_peer "$1" "write a block through the socket $2" append "$2" "$3" "$4"
  [[ "$reply" == "appended $2 1" ]] ||
    fail "$1" "the room applies the block the socket $2 writes into the fragment $3" "the peer answered: $reply"
}

# issue_room_holds NAME FRAGMENT ID: whether the socket NAME's document holds the block ID in
# FRAGMENT.
issue_room_holds() {
  issue_room_peer views "read the blocks the socket $1 draws" blocks "$1" "$2"
  jq -e --arg id "$3" 'any(.[]; .id == $id)' <<<"${reply#blocks "$1" }" >/dev/null
}

# --- setup: the earlier release over an emptied database --------------------------------------

section=setup
issue_room_database_parts
issue_room_empty_database
baseline_start
token="$baseline_token"
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
session="issue-room-$stamp"
readonly session
base="$baseline_url"
tree_init
tree_sign_in "$section" "$person_login"
tree_create_project "$section" "$project" "The one room of an issue"
sections=$((sections + 1))

# --- base: three issues the earlier release writes ---------------------------------------------

section=base
# issue_room_write_issue TITLE LABEL [PARENT]: an issue of the run, through the server at $base,
# with a spec holding a requirement, a Verification document of a check serving it and a plan of
# a task serving it, each holding the shapes; sets written, written_spec, written_verification and
# written_plan.
issue_room_write_issue() {
  local title="$1" label="$2" parent="${3:-}"
  {
    printf 'The spec of the %s issue.\n\n:::requirement{#r-%s}\nThe %s issue holds one requirement.\n:::\n\n' "$label" "$label" "$label"
    corpus_shapes spec "$label-spec"
  } >"$work/$label-spec.md"
  corpus_issue "$section" "$title" "$work/$label-spec.md" "$parent"
  written="$corpus_key"
  written_spec="$corpus_spec"
  issue_room_key "$section" "the key of $title" "$written"
  issue_room_uuid "$section" "the spec of $written" "$written_spec"
  {
    printf ':::check{#c-%s serves="dispatch://%s/spec#b-r-%s" surface="cli" run="bash e2e/acceptance/issue-room.sh" negative_control="Key the room on the spec again and the write lands elsewhere."}\nThe %s issue is one room.\n:::\n\n' \
      "$label" "$written" "$label" "$label"
    corpus_shapes verification.md "$label-verification"
  } >"$work/$label-verification.md"
  corpus_upload "$section" "$written" verification.md "$work/$label-verification.md"
  written_verification="$corpus_doc"
  {
    printf ':::plan_task{#t-%s serves="dispatch://%s/spec#b-r-%s"}\nBuild the %s issue.\n:::\n\n' "$label" "$written" "$label" "$label"
    corpus_shapes plan.md "$label-plan"
  } >"$work/$label-plan.md"
  corpus_upload "$section" "$written" plan.md "$work/$label-plan.md"
  written_plan="$corpus_doc"
  issue_room_uuid "$section" "the verification-md of $written" "$written_verification"
  issue_room_uuid "$section" "the plan-md of $written" "$written_plan"
}
issue_room_write_issue "The parent the one room is proved on" parent
parent="$written" parent_spec="$written_spec" parent_verification="$written_verification" parent_plan="$written_plan"
readonly parent parent_spec parent_verification parent_plan
corpus_comment person "$section" "$parent" "The person's comment on the parent."$'\n\n'"Its second block."
parent_comment="$corpus_item"
readonly parent_comment
issue_room_uuid "$section" "the person's comment on $parent" "$parent_comment"
issue_room_write_issue "A child of the parent, with a requirement of its own" child "$parent"
child="$written" child_spec="$written_spec"
readonly child child_spec
issue_room_write_issue "A closed issue the conversion passes over" closed
closed="$written" closed_spec="$written_spec"
readonly closed closed_spec
tree_send PATCH "/api/v1/issues/$closed" "$(jq -cn --argjson a "$(tree_actor)" '{status: "done", actor: $a}')"
tree_want 200 "$section" "close $closed"
expect "$section" "$closed is done" '"done"' .status
corpus_wait_settled "$section"
pass "$section" "the earlier release wrote $parent (in flight), $child (its child) and $closed (done), each with three documents"
baseline_stop_server
sections=$((sections + 1))

# --- upgrade: this head over those rows, and the conversion --------------------------------------

section=upgrade
tree_boot_head head
base="$tree_head_url"
tree_wait_backfill "$section"
tree_wait_announcement "$section"
tree_convert "$section"
printf '%s\n' "$(<"$work/out")"
for issue in "$parent" "$child"; do
  issue_room_outcome "$issue"
  [[ "$outcome" == converted ]] ||
    fail "$section" "convert-issues prints $issue converted" "it prints $outcome for it: $(tail -n 20 "$work/out")"
  issue_room_tree "$section" "$issue"
  [[ "$converted" == true ]] || fail "$section" "$issue reads converted: true" "it reads converted: $converted"
done
issue_room_outcome "$closed"
[[ "$outcome" == "passed over" ]] ||
  fail "$section" "convert-issues passes over the done issue $closed" "it prints $outcome for it: $(tail -n 20 "$work/out")"
issue_room_tree "$section" "$closed"
[[ "$converted" == false ]] || fail "$section" "the done issue $closed reads converted: false" "it reads converted: $converted"
pass "$section" "$parent and $child converted, and $closed, done, was passed over"
issue_room_tree "$section" "$parent"
parent_tree="$tree_value"
issue_room_tree "$section" "$child"
child_tree="$tree_value"
readonly parent_tree child_tree
issue_room_uuid "$section" "$parent's tree node" "$parent_tree"
issue_room_uuid "$section" "$child's tree node" "$child_tree"
tree_send GET "/api/v1/artifacts/$parent_tree"
tree_want 200 "$section" "read $parent's tree artifact"
tree_field "$section" "the slug of $parent's tree artifact" .slug
parent_tree_slug="$tree_value"
tree_field "$section" "the address of $parent's tree artifact" .ref_key
parent_tree_ref="$tree_value"
tree_send GET "/api/v1/artifacts/$child_tree"
tree_want 200 "$section" "read $child's tree artifact"
tree_field "$section" "the address of $child's tree artifact" .ref_key
child_tree_ref="$tree_value"
readonly parent_tree_slug parent_tree_ref child_tree_ref
tree_send GET /api/v1/schema/blocks
tree_want 200 "$section" "read the block schema this head serves"
tree_field "$section" "the block schema's version" .version
schema="$tree_value"
readonly schema
issue_room_peer_program
issue_room_peer_start
sections=$((sections + 1))

# --- room: one room per issue, and a write under a criterion through it -----------------------

section=room
tree_person_send POST "/api/v1/issues/$parent/comments" \
  "$(jq -cn '{body: "The person wants the tree read in one room.\n\nThe second block of that comment."}')"
tree_want 201 "$section" "$person_login comments on $parent"
tree_field "$section" "the comment's id" .id
room_comment="$tree_value"
readonly room_comment
issue_room_uuid "$section" "the person's comment" "$room_comment"
readonly criterion="k-room-${stamp//-/}"
tree_person_send POST "/api/v1/artifacts/$parent_tree/edits" "$(jq -cn --arg id "$criterion" \
  --arg p "dispatch://$parent/comment/$room_comment#b-b1" \
  '{ops: [{op: "insert", after: "end", markdown: (":::criterion{#" + $id + " parent=\"" + $p + "\"}\nThe person wants one room.\n:::\n")}]}')"
tree_want 200 "$section" "$person_login writes a criterion under the comment into $parent's tree"
readonly criterion_node="$parent_tree_ref#$criterion"
issue_room_wait_node "$section" "$parent" "the criterion hangs under the person's comment" \
  --arg id "$criterion_node" --arg c "$room_comment" '.id == $id and ((.parent.id // "") | contains($c))'
pass "$section" "$person_login wrote the criterion $criterion_node under the comment $room_comment"
issue_room_open "$section" tree "$parent_tree"
[[ ",$fragments," == *",$parent_spec,"* ]] ||
  fail "$section" "the socket on $parent's tree node holds the spec's fragment of the issue's room" "its fragments: $fragments"
readonly under="r-under-${stamp//-/}" under_words="A requirement written through the issue room under the criterion."
issue_room_append "$section" tree "$parent_spec" "$(jq -cn --arg id "$under" --arg w "$under_words" \
  --arg s "dispatch://$parent/artifact/$parent_tree_slug#b-$criterion" \
  '{type: "requirement", id: $id, text: $w, attrs: {serves: [$s]}}')"
issue_room_wait_node "$section" "$parent" "the block the socket wrote reads back under the criterion in $parent's tree" \
  --arg w "$under_words" --arg c "$criterion_node" '((.text // "") | contains($w)) and .parent.id == $c'
pass "$section" "the requirement written through the socket on $parent's tree node reads back under $criterion_node ($(jq -r '.id' <<<"$node")), through one socket"
issue_room_peer "$section" "close the socket on $parent's tree node" close tree
invented="$(printf '%08x-0000-4000-8000-%012x' "$(date +%s)" "$$")"
issue_room_uuid "$section" "the invented tree node" "$invented"
issue_room_socket_status "$invented"
[[ "$socket_status" == 404 ]] ||
  fail "$section" "the room check answers 404 for a tree node nobody holds" "the socket for $invented answered HTTP $socket_status: $(head -c 300 "$work/socket-body")"
pass "$section" "the socket for the invented tree node $invented is answered 404 by the room check"
sections=$((sections + 1))

# --- server: the server decides the room ---------------------------------------------------------

section=server
issue_room_open "$section" spec "$parent_spec"
for document in "$parent_spec" "$parent_verification" "$parent_plan"; do
  [[ ",$fragments," == *",$document,"* ]] ||
    fail "$section" "the socket on $parent's spec uuid is admitted to $parent's room" "its document holds the fragments $fragments, not $document"
done
pass "$section" "the socket on $parent's spec ($parent_spec) is admitted to $parent's room: it holds the fragments $fragments"
readonly payload="p-payload-${stamp//-/}" payload_words="The payload written through the spec uuid lands in the tree."
issue_room_append "$section" spec "$parent_spec" "$(jq -cn --arg id "$payload" --arg w "$payload_words" \
  '{type: "paragraph", id: $id, text: $w}')"
issue_room_wait_node "$section" "$parent" "the payload written through the spec uuid reads in $parent's tree" \
  --arg w "$payload_words" '(.text // "") | contains($w)'
pass "$section" "the paragraph written through the spec's uuid reads in $parent's tree as $(jq -r '.id' <<<"$node")"
issue_room_peer "$section" "close the socket on $parent's spec" close spec
want_value "$section" "$parent's spec's document_rooms row names $parent's tree node as its room" "$parent_tree" \
  "select coalesce((select room::text from document_rooms where artifact_id = '$parent_spec'), '(no row)')"
issue_room_open "$section" closed "$closed_spec"
[[ "$fragments" == prosemirror ]] ||
  fail "$section" "the socket on the spec of $closed, which has not converted, is admitted to that document alone" "its document holds the fragments $fragments"
want_value "$section" "the spec of $closed holds no document_rooms row" 0 \
  "select count(*) from document_rooms where artifact_id = '$closed_spec'"
tree_send GET "/api/v1/artifacts/$closed_spec"
tree_want 200 "$section" "read the spec of $closed"
expect "$section" "the spec of $closed is its own room" "\"$closed_spec\"" '.room'
issue_room_peer "$section" "close the socket on $closed's spec" close closed
sections=$((sections + 1))

# --- views: two overlapping views of one issue --------------------------------------------------

section=views
issue_room_open "$section" first "$parent_spec"
issue_room_open "$section" second "$parent_verification"
deadline=$((SECONDS + 60))
while true; do
  issue_room_peer "$section" "read the first socket's state vector" sv first
  first_sv="${reply#sv first }"
  issue_room_peer "$section" "read the second socket's state vector" sv second
  [[ "${reply#sv second }" != "$first_sv" ]] || break
  ((SECONDS < deadline)) || fail "$section" "both sockets reach one state vector" "they hold $first_sv and ${reply#sv second }"
  sleep 1
done
pass "$section" "both sockets on $parent's room hold one state vector"
issue_room_peer "$section" "hold the second socket at that state vector" freeze second
issue_room_versions "$parent"
versions_before="$versions"
readonly from_first="p-first-${stamp//-/}" from_second="p-second-${stamp//-/}"
issue_room_append "$section" first "$parent_verification" "$(jq -cn --arg id "$from_first" \
  '{type: "paragraph", id: $id, text: "The first view writes into the part the second draws."}')"
deadline=$((SECONDS + tree_wait_seconds))
while true; do
  issue_room_versions "$parent"
  ((versions < versions_before + 1)) || break
  ((SECONDS < deadline)) ||
    fail "$section" "the first write raises $parent's version count by one" "after ${tree_wait_seconds} s it reads $versions, from $versions_before"
  sleep "$tree_poll_seconds"
done
pass "$section" "the first write raised $parent's version count from $versions_before to $versions"
issue_room_append "$section" second "$parent_spec" "$(jq -cn --arg id "$from_second" \
  '{type: "paragraph", id: $id, text: "The second view writes into the part the first draws."}')"
issue_room_peer "$section" "let the second socket take the room's updates again" thaw second
deadline=$((SECONDS + tree_wait_seconds))
while true; do
  missing=""
  for view in first second; do
    issue_room_holds "$view" "$parent_verification" "$from_first" || missing+=" $from_first from the $view view"
    issue_room_holds "$view" "$parent_spec" "$from_second" || missing+=" $from_second from the $view view"
  done
  tree_send GET "/api/v1/artifacts/$parent_verification/text"
  tree_want 200 "$section" "read $parent's verification-md"
  grep -qF "The first view writes into the part the second draws." <(jq -r '.markdown' "$work/body") ||
    missing+=" $from_first from verification-md's text"
  tree_send GET "/api/v1/artifacts/$parent_spec/text"
  tree_want 200 "$section" "read $parent's spec"
  grep -qF "The second view writes into the part the first draws." <(jq -r '.markdown' "$work/body") ||
    missing+=" $from_second from the spec's text"
  issue_room_versions "$parent"
  [[ -n "$missing" || "$versions" -lt $((versions_before + 2)) ]] || break
  ((SECONDS < deadline)) ||
    fail "$section" "both writes stand, and both views read both" \
      "after ${tree_wait_seconds} s the block that disappeared:${missing:- none, and the version count reads $versions, from $versions_before}"
  sleep "$tree_poll_seconds"
done
((versions == versions_before + 2)) ||
  fail "$section" "$parent's version count rose by the two writes and no more" "it reads $versions, from $versions_before"
pass "$section" "both writes stand, both views read both, and $parent's version count rose by two, from $versions_before to $versions"
issue_room_peer "$section" "close the first view" close first
issue_room_peer "$section" "close the second view" close second
sections=$((sections + 1))

# --- shape: the three stored tables keep their shape -------------------------------------------

section=shape
# issue_room_catalog URL NAME: the columns of the three stored tables and their foreign keys, out
# of the catalog of the database URL names, in $work/NAME.columns and $work/NAME.keys.
issue_room_catalog() {
  baseline_psql "$1" -X -q -At -c "
    select table_name || '.' || column_name || ' ' || ordinal_position || ' ' || data_type || ' '
           || is_nullable || ' ' || coalesce(column_default, '')
    from information_schema.columns
    where table_schema = 'public' and table_name in ($tables)
    order by table_name, ordinal_position" >"$work/$2.columns" 2>"$work/psql" ||
    fail "$section" "read the columns of the stored tables in the $2 database" "psql: $(<"$work/psql")"
  baseline_psql "$1" -X -q -At -c "
    select conrelid::regclass::text || ' ' || pg_get_constraintdef(oid)
    from pg_constraint
    where contype = 'f' and conrelid::regclass::text in ($tables)
    order by 1" >"$work/$2.keys" 2>"$work/psql" ||
    fail "$section" "read the foreign keys of the stored tables in the $2 database" "psql: $(<"$work/psql")"
}
issue_room_catalog "$database_url" upgraded
catalog_url="${database_prefix}/${database_name}_catalog${database_query}"
baseline_psql "$database_server" -q -c "drop database if exists \"${database_name}_catalog\" with (force)" \
  -c "create database \"${database_name}_catalog\"" >"$work/catalog-create" 2>&1 ||
  fail "$section" "create a database this head migrates from zero" "psql: $(<"$work/catalog-create")"
baseline_boot catalog "$(command -v envoy-dispatch)" "$catalog_url"
catalog_pid="$baseline_boot_pid"
baseline_stop_pid "$catalog_pid"
catalog_pid=""
issue_room_catalog "$catalog_url" fresh
issue_room_drop_catalog
cat "$work/upgraded.columns"
diff "$work/fresh.columns" "$work/upgraded.columns" >"$work/columns.diff" ||
  fail "$section" "doc_updates, doc_snapshots and doc_checkpoints hold the columns this head's migrations build from zero" \
    "$(head -n 20 "$work/columns.diff")"
[[ -s "$work/upgraded.columns" ]] || fail "$section" "read the stored tables' columns" "the catalog lists none"
pass "$section" "doc_updates, doc_snapshots and doc_checkpoints hold the $(wc -l <"$work/upgraded.columns") columns this head's migrations build from zero"
cat "$work/upgraded.keys"
diff "$work/fresh.keys" "$work/upgraded.keys" >"$work/keys.diff" ||
  fail "$section" "the stored tables keep the foreign keys this head's migrations build" "$(head -n 20 "$work/keys.diff")"
for table in doc_updates doc_snapshots doc_checkpoints; do
  grep -qE "^$table FOREIGN KEY \(artifact_id\) REFERENCES artifacts\(id\)" "$work/upgraded.keys" ||
    fail "$section" "$table keeps its foreign key to artifacts (id)" "its keys: $(paste -sd ';' "$work/upgraded.keys")"
done
pass "$section" "each of doc_updates, doc_snapshots and doc_checkpoints keeps its foreign key to artifacts (id)"
sections=$((sections + 1))

# --- above: a node whose parent is in an issue above -------------------------------------------

section=above
readonly above_node="k-above-${stamp//-/}"
issue_room_digest "$section" "$parent"
parent_digest="$digest"
tree_person_send POST "/api/v1/artifacts/$child_tree/edits" "$(jq -cn --arg id "$above_node" \
  --arg p "dispatch://$parent/comment/$parent_comment#b-b1" \
  '{ops: [{op: "insert", after: "end", markdown: (":::criterion{#" + $id + " parent=\"" + $p + "\"}\nThe child wants its own words.\n:::\n")}]}')"
tree_want 200 "$section" "$person_login writes, in $child's tree, a criterion under $parent's comment"
readonly above_address="$child_tree_ref#$above_node"
issue_room_wait_node "$section" "$parent" "$parent's tree answers $child's criterion, read-only, under its comment" \
  --arg id "$above_address" --arg c "$child" '.id == $id and .issue == $c and .read_only == true'
issue_room_digest "$section" "$child"
child_digest="$digest"
issue_room_digest "$section" "$parent"
[[ "$digest" == "$parent_digest" ]] ||
  fail "$section" "$child's criterion leaves $parent's spec digest as it was" "it moved from $parent_digest to $digest"
parent_digest="$digest"
tree_person_send POST "/api/v1/artifacts/$parent_tree/edits" "$(jq -cn --arg b "$above_address" '{ops: [{op: "delete", block: $b}]}')"
tree_want 409 "$section" "a write naming $above_address through $parent's tree is refused"
expect "$section" "the refusal is NODE_IN_OTHER_ISSUE and names $child" true \
  --arg c "$child" '.code == "NODE_IN_OTHER_ISSUE" and ((.error // "") | contains($c))'
tree_person_send POST "/api/v1/artifacts/$child_tree/edits" \
  "$(jq -cn '{ops: [{op: "replace", find: "The child wants its own words.", with: "The child wants its own words, written in its own room."}]}')"
tree_want 200 "$section" "$person_login rewrites the criterion in $child's own tree"
readonly reworded="The child wants its own words, written in its own room."
issue_room_wait_node "$section" "$child" "$child's tree reads the criterion's new words" \
  --arg id "$above_address" --arg w "$reworded" '.id == $id and ((.text // "") | contains($w))'
issue_room_wait_node "$section" "$parent" "$parent's tree reads the criterion's new words, read-only" \
  --arg id "$above_address" --arg w "$reworded" --arg c "$child" \
  '.id == $id and ((.text // "") | contains($w)) and .issue == $c and .read_only == true'
pass "$section" "the write in $child's own room takes, and both trees read the new words"
tree_read agent "$child" spec
tree_want 200 "$section" "read $child's ?view=spec"
expect "$section" "$child's ?view=spec answers the criterion after the nodes of $child's own walk" true \
  --arg a "$above_address" '[.nodes[] | select(.role == "query")] as $q
    | ([$q[] | .id] | index($a)) as $at
    | $at != null and ([$q[($at + 1):][] | select((.issue // "") == "" and .id != $a)] | length == 0)'
deadline=$((SECONDS + tree_wait_seconds))
while true; do
  issue_room_digest "$section" "$child"
  if [[ "$digest" =~ ^[0-9a-f]{64}$ && "$digest" != "$child_digest" ]]; then
    break
  fi
  ((SECONDS < deadline)) ||
    fail "$section" "the write moves $child's spec digest" "after ${tree_wait_seconds} s it reads '$digest', from '$child_digest'"
  sleep "$tree_poll_seconds"
done
pass "$section" "$child's spec digest moved for the write, from $child_digest to $digest"
issue_room_digest "$section" "$parent"
[[ "$digest" == "$parent_digest" ]] ||
  fail "$section" "the write in $child's room leaves $parent's spec digest as it was" "it moved from $parent_digest to $digest"
pass "$section" "$parent's spec digest reads $parent_digest still"
sections=$((sections + 1))

printf 'quit\n' >&"$peer_in" || true
wait "$peer_pid" 2>/dev/null || true
peer_pid=""
printf 'PASS: %s complete %d sections\n' "$driver" "$sections"
