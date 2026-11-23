// packages/envoy-client/test/relay-socket-stand-in.ts
//
// A stand-in of the listener's relay socket (`src/listener-relay.ts` says the wire), with the
// secrets broker's token exchange on the same origin, so a test's session trades for its token
// and opens its socket at one address: `AGENT_SECRETS_URL` and `ENVOY_URL` both name `url`.
//
// It serves the wire as a listener in edge mode does. The upgrade takes `Authorization: DPoP
// <token>` and a proof of the token's key whose `htm` is GET, whose `htu` is the socket's own
// http(s) URL and whose `ath` names the token (401 otherwise); the token must be one this broker
// minted, live and not revoked, and must name the session the path names (403 otherwise); it asks
// for the subprotocol `envoy-relay.v1` (400 otherwise); and a fifth socket of one session is 429.
// It reads a socket's frames one at a time and handles each in full before the next, a ping
// included, whose pong Bun sends as it reads it: so a `sub` is held, and every frame before a ping
// handled, by the time that ping's pong goes out, and a successful `sub` has no answer of its own.
// A `sub` names exactly the session's relay subject, else `subject_not_granted`, and a socket
// holds one, a second answered `subscription_limit` with the socket left open; a `pub`
// (`op`, `subject`, `data` alone) names the reply of a frame this socket forwarded in the last 10
// minutes, else `publish_not_granted`. A frame it cannot read (no JSON, an unknown `op`, a `sub` or
// `unsub` naming no sid, a `sub` of a sid already held) is answered `bad_frame` and the socket
// closed 1008 with reason `bad_frame`. A `msg` always carries `headers`: each name as NATS carries
// it, case kept, to the list of its values in order, `{}` for none (Go's `nats.Header` as JSON),
// and goes to every subscription of the relay subject, as a NATS message does. A frame over 1 MiB
// closes the socket 1009. The server pings every 20 s.
//
// A test that drives the listener's HTTP routes beside the socket, as a session registers and
// sends at the listener it opens its socket at, or the broker's other grants, as a device login
// renews at the broker it trades at, serves them through `routes` (`RelaySocketStandInOptions`):
// it is asked first for every request but the relay upgrade, the token endpoint included, and a
// request it does not answer goes on to the trade, else 404.
//
// Nothing here registers a hook: a test file pushes the stand-in's `stop` onto its own cleanup.

import {
  createHash,
  createPublicKey,
  type JsonWebKeyInput,
  randomBytes,
  verify,
} from "node:crypto";
import { thumbprintOf } from "@legion/cipher-client/test/sign-ins";
import type { ServerWebSocket } from "bun";

/** The subprotocol a relay socket speaks. */
const SUBPROTOCOL = "envoy-relay.v1";
/** The prefix of a session's relay subject. */
const DELIVER_PREFIX = "notifications.envoy.deliver.";
/** How long a forwarded frame's reply may be answered on the socket that forwarded it. */
const REPLY_WINDOW_MS = 10 * 60_000;
/** The most sockets one session may hold open. */
const MOST_SOCKETS = 4;
/** The most subscriptions one socket may hold. */
const MOST_SUBSCRIPTIONS = 1;
/** The largest frame a socket takes. */
const MOST_FRAME_BYTES = 1 << 20;
/** How often the server pings an open socket. */
const PING_EVERY_MS = 20_000;

/** One token the stand-in's broker minted. */
export interface StandInToken {
  readonly value: string;
  readonly session: string;
  readonly jkt: string;
  readonly expiresAt: number;
}

/** One upgrade the stand-in was asked for, and how it answered. */
export interface StandInUpgrade {
  readonly session: string;
  readonly token: string;
  readonly at: number;
  /** 101 for a socket it opened, else the status it refused with. */
  readonly status: number;
  readonly why: string;
  /** The DPoP proof a socket it opened was upgraded with; "" for an upgrade it refused. */
  readonly proof: string;
}

/** A frame the stand-in delivers: the headers a listener's relay frame carries, each name to the
 *  list of its values, and its body. */
export interface StandInFrame {
  readonly headers: Readonly<Record<string, readonly string[]>>;
  readonly data: string;
}

/** One frame a socket's reader handled, in the order it handled them: `sub <sid> <subject>`,
 *  `unsub <sid>`, `pub <subject>`, or `ping <payload>`. */
export interface StandInHandled {
  readonly session: string;
  readonly frame: string;
}

interface SocketData {
  readonly id: number;
  readonly session: string;
  readonly token: string;
  readonly subs: Map<number, string>;
  /** Each reply this socket forwarded, and when. */
  readonly replies: Map<string, number>;
  ping?: ReturnType<typeof setInterval>;
}

/** What the stand-in heard and did, for a test to read. */
export interface RelaySocketStandIn {
  /** The origin of both the broker and the listener: `AGENT_SECRETS_URL` and `ENVOY_URL`. */
  readonly url: string;
  /** Every token the broker minted, in order. */
  readonly minted: StandInToken[];
  /** Every upgrade asked for, opened or refused, in order. */
  readonly upgrades: StandInUpgrade[];
  /** Every `sub` refused, `subject_not_granted` or `subscription_limit`, by the session its socket
   *  was for. */
  readonly refusedSubs: {
    readonly session: string;
    readonly subject: string;
    readonly code: string;
  }[];
  /** Every `sub` granted. */
  readonly subs: { readonly session: string; readonly subject: string }[];
  /** Every `pub` the stand-in took: a receipt on a reply it forwarded. */
  readonly receipts: { readonly reply: string; readonly body: string }[];
  /** Every `pub` refused `publish_not_granted`. */
  readonly refusedPublishes: string[];
  /** Every frame each socket's reader handled, in order (`StandInHandled`). */
  readonly handled: StandInHandled[];
  /** Every client frame whose members are not the wire's for its `op` (the file's header), as it
   *  was sent. */
  readonly malformed: string[];
  /** How the broker answers a trade: `mint` (the default), or `refuse`, as a broker that says no
   *  to the identity it was shown. */
  trades: "mint" | "refuse";
  /** How many trades the broker was asked, answered or refused. */
  tradesAsked: number;
  /** How long each token the broker mints lives: an hour by default, past the token client's
   *  five-minute renewal window, so a token in hand is presented again rather than traded anew. */
  tokenSeconds: number;
  /** A status every upgrade is refused with while set, as an edge whose broker is away (503). */
  refuseUpgrades: number | undefined;
  /** While true, a `sub` of the session's own relay subject is refused `subject_not_granted` too:
   *  the wire allows it, though #4658 answers so only another subject (it closes the socket 1013
   *  when NATS refuses a subscribe), for a row on what a client does with such a refusal. */
  refuseOwnSubject: boolean;
  /** The open sockets of `session`. */
  sockets(session: string): number;
  /** Sends `frame` to every subscription of `session`'s relay subject, on every socket of the
   *  session, and answers the reply it named; undefined when no socket of the session holds that
   *  subject. */
  deliver(session: string, frame: StandInFrame): string | undefined;
  /** Answers every open socket of `session` as the listener answers a frame it cannot read: the
   *  err frame `bad_frame`, then a close 1008 with reason `bad_frame`. */
  refuseFrame(session: string): void;
  /** Sends `text` as one text frame to every open socket of `session`, as it is: a frame no
   *  listener sends, for a row on what the client does with one. */
  sendRaw(session: string, text: string): void;
  /** Ends every open socket of `session` with no close frame, as a path that drops the
   *  connection does: the client sees 1006. */
  drop(session: string): void;
  /** Closes every open socket of `session`, or of every session, with `code`. */
  close(code: number, reason: string, session?: string): void;
  /** Ends a token before it lapses, as a revoke does. */
  revoke(token: string): void;
  stop(): void;
}

/** The relay subject of `session`. */
export function standInDeliverSubject(session: string): string {
  return `${DELIVER_PREFIX}${session}`;
}

/** A proof's header and claims as sent: the members the stand-in reads, any of which may be
 *  missing or of the wrong type. */
interface ProofPart {
  readonly jwk?: unknown;
  readonly htm?: unknown;
  readonly htu?: unknown;
  readonly ath?: unknown;
}

function decoded(part: string): ProofPart {
  return JSON.parse(Buffer.from(part, "base64url").toString("utf8")) as ProofPart;
}

/** A frame a client sent, before the handler checks it: the wire's members (the file's header),
 *  any of which may be missing or of the wrong type, and nothing else read. */
interface ClientFrame {
  readonly op?: unknown;
  readonly sid?: unknown;
  readonly subject?: unknown;
  readonly data?: unknown;
}

/** A proof's header and claims, verified with the key its own header embeds; undefined when it
 *  is not a compact JWS or its signature does not verify. */
function verifiedProof(
  proof: string
): { readonly jwk: JsonWebKeyInput["key"]; readonly claims: ProofPart } | undefined {
  const [header = "", claims = "", signature = ""] = proof.split(".");
  try {
    const read = { header: decoded(header), claims: decoded(claims) };
    // The header's key as it was sent: `createPublicKey` refuses any that is no JWK.
    const jwk = read.header.jwk as JsonWebKeyInput["key"];
    const key = createPublicKey({ key: jwk, format: "jwk" });
    const signed = Buffer.from(`${header}.${claims}`);
    const raw = Buffer.from(signature, "base64url");
    if (!verify("sha256", signed, { key, dsaEncoding: "ieee-p1363" }, raw)) return undefined;
    return { jwk, claims: read.claims };
  } catch {
    return undefined;
  }
}

/** How a test starts the stand-in. */
export interface RelaySocketStandInOptions {
  /**
   * Asked first for every request but the relay upgrade (`GET /v1/sessions/<session>/relay`), the
   * broker's token endpoint included, on the one origin, with the stand-in itself to read and drive
   * (`deliver`). It is handed a copy of the request, so one it reads and does not answer still
   * reaches the stand-in whole: undefined goes on to the trade (`POST /v1/oauth/token`), else 404.
   */
  readonly routes?: (
    request: Request,
    standIn: RelaySocketStandIn
  ) => Response | undefined | Promise<Response | undefined>;
}

/** Starts the stand-in on a port the kernel picks. */
export function startRelaySocketStandIn(
  options: RelaySocketStandInOptions = {}
): RelaySocketStandIn {
  const minted: StandInToken[] = [];
  const revoked = new Set<string>();
  const upgrades: StandInUpgrade[] = [];
  const open = new Set<ServerWebSocket<SocketData>>();
  let nextSocket = 0;
  // The origin, known once the server below has bound its port.
  let origin = "";

  const refuse = (session: string, token: string, status: number, why: string): Response => {
    upgrades.push({ session, token, at: Date.now(), status, why, proof: "" });
    return new Response(why, { status });
  };

  const live = (token: string): StandInToken | undefined => {
    const held = minted.find((one) => one.value === token);
    if (held === undefined || held.expiresAt <= Date.now() || revoked.has(token)) return undefined;
    return held;
  };

  const send = (socket: ServerWebSocket<SocketData>, frame: Record<string, unknown>): void => {
    socket.send(JSON.stringify(frame));
  };

  /** The listener's answer to a frame it cannot read: `bad_frame`, under the sid it named, then
   *  the close. */
  const badFrame = (socket: ServerWebSocket<SocketData>, sid?: number): void => {
    send(socket, { op: "err", ...(sid === undefined ? {} : { sid }), code: "bad_frame" });
    socket.close(1008, "bad_frame");
  };

  const standIn: RelaySocketStandIn = {
    get url() {
      return origin;
    },
    minted,
    upgrades,
    refusedSubs: [],
    subs: [],
    receipts: [],
    refusedPublishes: [],
    handled: [],
    malformed: [],
    trades: "mint",
    tradesAsked: 0,
    tokenSeconds: 3600,
    refuseUpgrades: undefined,
    refuseOwnSubject: false,
    sockets: (session) => [...open].filter((socket) => socket.data.session === session).length,
    deliver(session, frame) {
      const subject = standInDeliverSubject(session);
      const reply = `notifications.envoy.relayreply.${randomBytes(8).toString("hex")}.${session}.${randomBytes(4).toString("hex")}`;
      let sent = false;
      for (const socket of open) {
        if (socket.data.session !== session) continue;
        for (const [sid, held] of socket.data.subs) {
          if (held !== subject) continue;
          socket.data.replies.set(reply, Date.now());
          send(socket, {
            op: "msg",
            sid,
            subject,
            reply,
            headers: frame.headers,
            data: Buffer.from(frame.data).toString("base64"),
          });
          sent = true;
        }
      }
      return sent ? reply : undefined;
    },
    refuseFrame(session) {
      for (const socket of [...open]) {
        if (socket.data.session === session) badFrame(socket);
      }
    },
    sendRaw(session, text) {
      for (const socket of open) {
        if (socket.data.session === session) socket.send(text);
      }
    },
    drop(session) {
      for (const socket of [...open]) {
        if (socket.data.session === session) socket.terminate();
      }
    },
    close(code, reason, session) {
      for (const socket of [...open]) {
        if (session === undefined || socket.data.session === session) socket.close(code, reason);
      }
    },
    revoke(token) {
      revoked.add(token);
    },
    stop() {
      for (const socket of open) clearInterval(socket.data.ping);
      server.stop(true);
    },
  };

  const trade = async (request: Request): Promise<Response> => {
    const form = new URLSearchParams(await request.text());
    standIn.tradesAsked += 1;
    if (standIn.trades === "refuse") {
      return Response.json(
        {
          error: "invalid_grant",
          error_description: "the broker refuses this identity a token",
        },
        { status: 400 }
      );
    }
    const proof = verifiedProof(request.headers.get("DPoP") ?? "");
    if (proof === undefined) {
      return Response.json(
        { error: "invalid_dpop_proof", error_description: "the request carries no DPoP proof" },
        { status: 400 }
      );
    }
    const value = `relay-standin-${randomBytes(16).toString("hex")}`;
    minted.push({
      value,
      session: form.get("session") ?? "",
      jkt: await thumbprintOf(proof.jwk as Parameters<typeof thumbprintOf>[0]),
      expiresAt: Date.now() + standIn.tokenSeconds * 1000,
    });
    return Response.json({
      access_token: value,
      issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
      token_type: "DPoP",
      expires_in: standIn.tokenSeconds,
    });
  };

  const server = Bun.serve<SocketData>({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request, srv) {
      const url = new URL(request.url);
      const matched = /^\/v1\/sessions\/([^/]+)\/relay$/.exec(url.pathname);
      if (matched === null || request.method !== "GET") {
        // One runtime class: a package whose lib loads undici's types reads `clone()` as theirs.
        const copy = request.clone() as Request;
        const answered = await options.routes?.(copy, standIn);
        if (answered !== undefined) return answered;
        if (request.method === "POST" && url.pathname === "/v1/oauth/token") return trade(request);
        return new Response("not found", { status: 404 });
      }
      const session = decodeURIComponent(matched[1] ?? "");
      const authorization = request.headers.get("Authorization") ?? "";
      const token = authorization.startsWith("DPoP ") ? authorization.slice("DPoP ".length) : "";
      if (standIn.refuseUpgrades !== undefined) {
        return refuse(session, token, standIn.refuseUpgrades, "refused while the test says so");
      }
      const proof = verifiedProof(request.headers.get("DPoP") ?? "");
      const held = live(token);
      if (held === undefined) return refuse(session, token, 401, "the token is not live");
      if (proof === undefined) return refuse(session, token, 401, "the proof does not verify");
      const jkt = await thumbprintOf(proof.jwk as Parameters<typeof thumbprintOf>[0]);
      const ath = createHash("sha256").update(token).digest("base64url");
      const htu = `${standIn.url}/v1/sessions/${matched[1]}/relay`;
      if (
        jkt !== held.jkt ||
        proof.claims.htm !== "GET" ||
        proof.claims.htu !== htu ||
        proof.claims.ath !== ath
      ) {
        return refuse(session, token, 401, "the proof does not name this request and token");
      }
      if (held.session !== session) {
        return refuse(session, token, 403, `the token names ${held.session}, not ${session}`);
      }
      const asked = (request.headers.get("Sec-WebSocket-Protocol") ?? "")
        .split(",")
        .map((one) => one.trim());
      if (!asked.includes(SUBPROTOCOL)) {
        return refuse(session, token, 400, `the upgrade asks for no ${SUBPROTOCOL}`);
      }
      if (standIn.sockets(session) >= MOST_SOCKETS) {
        return refuse(session, token, 429, `${session} holds ${MOST_SOCKETS} sockets`);
      }
      nextSocket += 1;
      const data: SocketData = {
        id: nextSocket,
        session,
        token,
        subs: new Map(),
        replies: new Map(),
      };
      if (srv.upgrade(request, { headers: { "Sec-WebSocket-Protocol": SUBPROTOCOL }, data })) {
        upgrades.push({
          session,
          token,
          at: Date.now(),
          status: 101,
          why: "",
          proof: request.headers.get("DPoP") ?? "",
        });
        return undefined;
      }
      return refuse(session, token, 400, "the request is no WebSocket upgrade");
    },
    websocket: {
      maxPayloadLength: MOST_FRAME_BYTES,
      open(socket) {
        open.add(socket);
        socket.data.ping = setInterval(() => socket.ping(), PING_EVERY_MS);
      },
      close(socket) {
        open.delete(socket);
        clearInterval(socket.data.ping);
      },
      // Bun sends the pong as it reads the ping, after every frame before it was handled here.
      ping(socket, data) {
        standIn.handled.push({
          session: socket.data.session,
          frame: `ping ${data.toString("utf8")}`,
        });
      },
      message(socket, raw) {
        // Its members as sent, each read and checked below; any other is counted in `malformed`.
        let frame: ClientFrame | null;
        try {
          frame = JSON.parse(String(raw)) as ClientFrame | null;
        } catch {
          standIn.malformed.push(String(raw));
          badFrame(socket);
          return;
        }
        const { session } = socket.data;
        const members = Object.keys(frame ?? {})
          .sort()
          .join(",");
        const shaped = { sub: "op,sid,subject", unsub: "op,sid", pub: "data,op,subject" }[
          String(frame?.op)
        ];
        if (members !== shaped) standIn.malformed.push(String(raw));
        const sid = Number.isInteger(frame?.sid) ? Number(frame?.sid) : undefined;
        if (frame?.op === "sub") {
          if (sid === undefined) {
            badFrame(socket);
            return;
          }
          const subject = String(frame.subject);
          standIn.handled.push({ session, frame: `sub ${sid} ${subject}` });
          if (subject !== standInDeliverSubject(session) || standIn.refuseOwnSubject) {
            standIn.refusedSubs.push({ session, subject, code: "subject_not_granted" });
            send(socket, { op: "err", sid, code: "subject_not_granted" });
            return;
          }
          if (socket.data.subs.has(sid)) {
            badFrame(socket, sid);
            return;
          }
          if (socket.data.subs.size >= MOST_SUBSCRIPTIONS) {
            // The socket stays open: its subscriptions up to the bound go on.
            standIn.refusedSubs.push({ session, subject, code: "subscription_limit" });
            send(socket, { op: "err", sid, code: "subscription_limit" });
            return;
          }
          socket.data.subs.set(sid, subject);
          standIn.subs.push({ session, subject });
          return;
        }
        if (frame?.op === "unsub") {
          if (sid === undefined) {
            badFrame(socket);
            return;
          }
          standIn.handled.push({ session, frame: `unsub ${sid}` });
          socket.data.subs.delete(sid);
          return;
        }
        if (frame?.op !== "pub") {
          badFrame(socket, sid);
          return;
        }
        const subject = String(frame.subject);
        standIn.handled.push({ session, frame: `pub ${subject}` });
        const at = socket.data.replies.get(subject);
        if (at === undefined || Date.now() - at > REPLY_WINDOW_MS) {
          standIn.refusedPublishes.push(subject);
          send(socket, { op: "err", code: "publish_not_granted" });
          return;
        }
        standIn.receipts.push({
          reply: subject,
          body: Buffer.from(String(frame.data), "base64").toString("utf8"),
        });
      },
    },
  });
  const { port } = server;
  if (port === undefined) throw new Error("the relay socket stand-in bound no port");
  origin = `http://127.0.0.1:${port}`;
  return standIn;
}
