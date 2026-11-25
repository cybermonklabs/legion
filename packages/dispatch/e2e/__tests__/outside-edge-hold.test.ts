/** The outside driver's `hold` and `release` (`e2e/acceptance/outside-edge.ts`): the session `run`
 *  kept, registered at the listener again and held live while a check acts as the person's
 *  session, then ended. The rows run them against a stand-in of the edge: envoy-client's relay
 *  socket stand-in (`@legion/envoy-client/test/relay-socket-stand-in`), its broker's trade and its
 *  socket, with the device login's grants, the registration and its end served through its
 *  `routes`, each taking only the session's own credential (`outside-edge-credentials.ts`). Each
 *  row first makes a device login with the real broker client and keeps it, and a session, where
 *  `run` keeps them, then runs the driver with the variables a check gives it and no other: `hold`
 *  with OUTSIDE_PROJECT, OUTSIDE_BROKER_URL, OUTSIDE_LISTENER_URL and OUTSIDE_EDGE_STATE (and
 *  OUTSIDE_EDGE_ALLOW_HTTP, for the stand-in's http), `release` with OUTSIDE_EDGE_STATE alone.
 *
 *  Each way a hold ends is a row, and so is each refusal: a hold time outside 60 to 3600 s, a
 *  listener that refuses the registration or the relay subject, a listener that closes the held
 *  socket for good, an unregister that fails, no hold to release, a holder that ignores SIGTERM,
 *  and a held pid whose process started at another time, which release does not signal. So is
 *  each way a hold stays held, against a stand-in whose tokens last 3 s, closing the socket 4401
 *  at each lapse, and one whose registrations lapse after 5 s unrenewed, as the real listener's
 *  last at most 300 s and 5 minutes: two token lapses and the listener's own stop (1012), each
 *  registered again on the new socket, and a hold renewed past the lapse; a renewal the listener
 *  refuses ends it, a socket that does not open again within 30 s of a routine close ends it, a
 *  SIGTERM while a closed socket opens again sends no registration after it, and a SIGTERM while
 *  a renewal the listener does not answer is on the wire sends no second attempt of it. An ended
 *  hold leaves no `held`, and a release spends the `holder` it ended, so a release after it reads
 *  no hold; once the next hold of a directory has started, a release reads its end, never that of
 *  a hold that ended by itself unreleased, and a third hold leaves a live hold's records to it.
 *  So is each way a hold ends before it holds anything: a flush the listener never
 *  answers, which fails at its bound; a SIGTERM while the socket opens, which ends the hold well
 *  inside the open's 30 s, while the flush waits, and while the listener answers the
 *  registration, after which no registration stands; a `release` that
 *  comes before `held`; and files an earlier hold left, which a refused hold must not leave for
 *  `release` to read. So is an OUTSIDE_EDGE_STATE the hold cannot write, before it registers and
 *  once the listener took the session, which is UNKNOWN, exit 2, and leaves no session
 *  registered. For the flush rows a mute listener (`startMuteListener`) takes the socket
 *  and answers nothing on it. The rows start no browser and no stack, so they run wherever
 *  `e2e/__tests__` runs; they read `/proc`, as `hold` and `release` do, and skip where there is
 *  none.
 *
 *  The rows that wait out a span of real time - the 60 s hold, the 15 s release, the 10 s flush,
 *  a renewal's 5 s attempt, the 30 s for a socket to open again, and the rows on short tokens and
 *  a short lapse, each some seconds - skip under
 *  DISPATCH_UNIT_REPEAT, which `bun run test:repeat` (`e2e/tools/unit-repeat.ts`) sets: its
 *  harness lane runs this directory 20 times under one deadline, and these rows would spend
 *  minutes of it each run waiting, finding no fault a single run does not. Each `hold` is a
 *  process of its own on the real clock, so no fake timer can drive these waits. */

import { describe, expect, test } from "bun:test";
import { createHash, randomBytes } from "node:crypto";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { createServer, type Socket } from "node:net";
import { hostname, tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  deviceLoginHost,
  finishDeviceLogin,
  startDeviceLogin,
} from "@legion/cipher-client/device-login";
import { legacyAgentSubject } from "@legion/contracts";
import {
  type RelaySocketStandIn,
  standInDeliverSubject,
  startRelaySocketStandIn,
} from "@legion/envoy-client/test/relay-socket-stand-in";
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer";
import {
  type BoundedRun,
  beforeDeadline,
  bounds,
  eventually,
  pastDeadline,
  rowLimit,
  runBounded,
  watch,
  whether,
} from "./bounded-subprocess";
import { sessionRefusal, topicRefusal, tradeRefusal } from "./outside-edge-credentials";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const driver = join(dispatchPackage, "e2e/acceptance/outside-edge.ts");
/** The machine name every registration of the driver names (its DEVICE_HOST). */
const deviceHost = deviceLoginHost("outside-edge-e2e");
/** A row that waits out a span of real time, skipped in the repeat lane (this file's head). */
const realTimeRow = process.env.DISPATCH_UNIT_REPEAT === "1" ? test.skip : test;

/** The edge stand-in of these rows, and what it was asked. */
interface HoldEdge {
  readonly relay: RelaySocketStandIn;
  /** The session each registration named, in order, taken or refused; when each registration it
   *  took was taken, and the `host` each named; each unregister; and each request a listener route
   *  or the exchange refused for its credential or its topics, with why. */
  readonly registrations: string[];
  readonly taken: number[];
  readonly hosts: string[];
  readonly unregisters: string[];
  readonly refusals: string[];
}

/** Starts the edge: the relay socket stand-in, which trades and serves the socket, with the
 *  device login's two grants, approved at once, the registration and its end, each listener route
 *  taking only the session's own token and proof and a registration only the session's own topic
 *  (`outside-edge-credentials.ts`), and the exchange only a subject token the device grant
 *  issued. `registration` and `unregister` are the statuses those two answer, 200 where unset;
 *  with `refuseAfter`, every registration after that many is refused 403; with `answerAfter`,
 *  the registration is answered only once that settles; with `stallAfter`, every registration
 *  after that many is never answered at all, so its attempt runs out its deadline. */
function startHoldEdge(
  answers: {
    registration?: number;
    refuseAfter?: number;
    unregister?: number;
    answerAfter?: Promise<unknown>;
    stallAfter?: number;
  } = {}
): HoldEdge {
  const registrations: string[] = [];
  const taken: number[] = [];
  const hosts: string[] = [];
  const unregisters: string[] = [];
  const refusals: string[] = [];
  const issued = new Set<string>();
  const refused = async (what: string, refusal: Response | undefined) => {
    if (refusal !== undefined) refusals.push(`${what}: ${await refusal.clone().text()}`);
    return refusal;
  };
  const pair = () => {
    const accessToken = `hold-login-${randomBytes(16).toString("hex")}`;
    issued.add(accessToken);
    return Response.json({
      access_token: accessToken,
      token_type: "DPoP",
      expires_in: 900,
      refresh_token: `hold-refresh-${randomBytes(16).toString("hex")}`,
      scope: "machine",
      credential_id: "hold-credential",
      credential_expires_at: new Date(Date.now() + 86_400_000)
        .toISOString()
        .replace(/\.\d{3}Z$/, "Z"),
    });
  };
  const relay = startRelaySocketStandIn({
    routes: async (request, standIn) => {
      const { pathname } = new URL(request.url);
      if (request.method === "POST" && pathname === "/v1/oauth/device_authorization") {
        return Response.json({
          device_code: `hold-device-${randomBytes(8).toString("hex")}`,
          user_code: "HOLD-CODE",
          verification_uri: `${standIn.url}/credentials/machine`,
          expires_in: 600,
          interval: 1,
        });
      }
      if (request.method === "POST" && pathname === "/v1/oauth/token") {
        const form = new URLSearchParams(await request.text());
        const grant = form.get("grant_type");
        if (grant === "urn:ietf:params:oauth:grant-type:device_code" || grant === "refresh_token")
          return pair();
        // The exchange: the stand-in's own trade mints the session's token, for a subject token
        // the device grant issued.
        return refused("the exchange", tradeRefusal(form, issued));
      }
      if (request.method === "POST" && pathname === "/v1/interests/subscribe") {
        const body: unknown = await request.json();
        const field = (name: string) =>
          typeof body === "object" && body !== null ? Reflect.get(body, name) : undefined;
        const session = String(field("session_id"));
        const refusal =
          (await refused("a registration", await sessionRefusal(request, standIn, session))) ??
          (await refused("a registration", topicRefusal(session, field("topics"))));
        if (refusal !== undefined) return refusal;
        registrations.push(session);
        if (answers.stallAfter !== undefined && registrations.length > answers.stallAfter) {
          return await new Promise<Response>(() => undefined);
        }
        await answers.answerAfter;
        const status =
          answers.refuseAfter !== undefined && registrations.length > answers.refuseAfter
            ? 403
            : (answers.registration ?? 200);
        if (status !== 200) {
          return Response.json(
            { error: "the stand-in refuses this registration", code: "registration_refused" },
            { status }
          );
        }
        taken.push(Date.now());
        hosts.push(String(field("host")));
        return Response.json({
          session_id: session,
          machine_id: "outside-edge-hold-stand-in",
          dir: "/outside-edge-e2e",
          topics: [legacyAgentSubject(session)],
          delivery: consumerDelivery(session),
        });
      }
      const ended = /^\/v1\/sessions\/([^/]+)$/.exec(pathname);
      if (request.method === "DELETE" && ended !== null) {
        const session = decodeURIComponent(ended[1] ?? "");
        const refusal = await refused(
          "an unregister",
          await sessionRefusal(request, standIn, session)
        );
        if (refusal !== undefined) return refusal;
        unregisters.push(session);
        const status = answers.unregister ?? 200;
        if (status !== 200) {
          return Response.json({ error: "the stand-in refuses this unregister" }, { status });
        }
        return Response.json({ deleted: true });
      }
      return undefined;
    },
  });
  return { relay, registrations, taken, hosts, unregisters, refusals };
}

/** Closes the held socket of `session` 4401 each time the token it was opened with has lapsed, as
 *  the listener closes an open socket at its next check after its token's expiry (the listener
 *  checks every 20 s; this every 100 ms, on the real clock the token lapses on, since the hold
 *  that trades for it is another process). Answers how many sockets it closed, and its stop. */
function closeAtTokenLapse(edge: HoldEdge, session: string): { closes(): number; stop(): void } {
  const closed = new Set<string>();
  const timer = setInterval(() => {
    const opened = edge.relay.upgrades
      .filter((one) => one.session === session && one.status === 101)
      .at(-1);
    if (opened === undefined || closed.has(opened.token)) return;
    const token = edge.relay.minted.find((one) => one.value === opened.token);
    if (token === undefined || token.expiresAt > Date.now()) return;
    closed.add(opened.token);
    edge.relay.close(4401, "the token lapsed", session);
  }, 100);
  return { closes: () => closed.size, stop: () => clearInterval(timer) };
}

/** A state directory as `run` leaves it for `hold`: a device login made with the real broker
 *  client at `edge`, and a session. */
async function keptState(edge: HoldEdge): Promise<{ state: string; session: string }> {
  const state = mkdtempSync(join(tmpdir(), "outside-edge-hold-"));
  const started = await startDeviceLogin({
    brokerUrl: edge.relay.url,
    person: "erin@outside.example",
    host: deviceLoginHost("outside-edge-e2e"),
  });
  await finishDeviceLogin(started, {
    path: join(state, "device-login.json"),
    sleep: async () => undefined,
  });
  const session = `outside-e2e-${randomBytes(6).toString("hex")}`;
  writeFileSync(join(state, "session"), session, { mode: 0o600 });
  return { state, session };
}

/** The variables a check gives `hold`: the edge's two URLs, the project and the state. */
function holdEnv(broker: string, listener: string, state: string): Record<string, string> {
  return {
    PATH: process.env.PATH ?? "",
    HOME: process.env.HOME ?? "",
    OUTSIDE_EDGE_ALLOW_HTTP: "1",
    OUTSIDE_PROJECT: "OWN",
    OUTSIDE_BROKER_URL: broker,
    OUTSIDE_LISTENER_URL: listener,
    OUTSIDE_EDGE_STATE: state,
  };
}

/** `hold`, with the variables a check gives it and these beside them; its listener the edge's
 *  own unless `listener` names another. */
function hold(
  edge: HoldEdge,
  state: string,
  extra: Record<string, string> = {},
  listener = edge.relay.url
) {
  return runBounded({
    argv: ["bun", driver, "hold"],
    bound: "outsideEdgeHold",
    cwd: dispatchPackage,
    env: { ...holdEnv(edge.relay.url, listener, state), ...extra },
    from: import.meta.url,
  });
}

/** `hold` as a process the row drives rather than waits for: the row signals it, so it needs its
 *  pid, and reads what it printed as it comes. `ended` waits for its exit for the hold's bound at
 *  most; `kill` ends one still running, which the row's `finally` calls. */
interface DrivenHold {
  readonly pid: number;
  stdout(): string;
  stderr(): string;
  ended(): Promise<number>;
  kill(): void;
}

function driveHold(
  broker: string,
  listener: string,
  state: string,
  extra: Record<string, string> = {}
): DrivenHold {
  const child = Bun.spawn(["bun", driver, "hold"], {
    cwd: dispatchPackage,
    env: { ...holdEnv(broker, listener, state), ...extra },
    stdout: "pipe",
    stderr: "pipe",
  });
  let out = "";
  let err = "";
  const read = async (stream: ReadableStream<Uint8Array>, add: (text: string) => void) => {
    const decoder = new TextDecoder();
    const reader = stream.getReader();
    for (;;) {
      const { done, value } = await reader.read();
      if (done) return;
      add(decoder.decode(value, { stream: true }));
    }
  };
  const reads = Promise.all([
    read(child.stdout, (text) => {
      out += text;
    }),
    read(child.stderr, (text) => {
      err += text;
    }),
  ]);
  return {
    pid: child.pid,
    stdout: () => out,
    stderr: () => err,
    async ended() {
      const code = await beforeDeadline(child.exited, performance.now() + bounds.outsideEdgeHold);
      if (code === pastDeadline) {
        throw new Error(`the hold did not end within ${bounds.outsideEdgeHold} ms:\n${out}${err}`);
      }
      await reads;
      return code;
    },
    kill() {
      if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
    },
  };
}

/** A listener that answers nothing on the relay socket: it takes the upgrade (unless `upgrade` is
 *  false, when the socket never opens) and then reads the client's frames and answers none, so a
 *  flush's ping gets no pong. Any other request is answered 404. It records each request line,
 *  and the opcode of each frame the client sent on an upgraded socket (RFC 6455 section 5.2). */
interface MuteListener {
  readonly url: string;
  readonly requests: string[];
  readonly opcodes: number[];
  stop(): void;
}

/** The opcode of each whole frame at the head of `buffer`, pushed onto `opcodes`; answers what is
 *  left of it, a frame not yet whole. */
function framesRead(buffer: Buffer, opcodes: number[]): Buffer {
  let at = buffer;
  for (;;) {
    if (at.length < 2) return at;
    const short = at.readUInt8(1) & 0x7f;
    const masked = (at.readUInt8(1) & 0x80) !== 0;
    let head = 2;
    let length = short;
    if (short === 126) {
      if (at.length < 4) return at;
      head = 4;
      length = at.readUInt16BE(2);
    } else if (short === 127) {
      if (at.length < 10) return at;
      head = 10;
      length = Number(at.readBigUInt64BE(2));
    }
    const whole = head + (masked ? 4 : 0) + length;
    if (at.length < whole) return at;
    opcodes.push(at.readUInt8(0) & 0x0f);
    at = at.subarray(whole);
  }
}

async function startMuteListener(upgrade: boolean): Promise<MuteListener> {
  const requests: string[] = [];
  const opcodes: number[] = [];
  const sockets = new Set<Socket>();
  const server = createServer((socket) => {
    sockets.add(socket);
    socket.on("close", () => sockets.delete(socket));
    socket.on("error", () => undefined);
    let head = Buffer.alloc(0);
    let frames: Buffer | undefined;
    socket.on("data", (chunk: Buffer) => {
      if (frames !== undefined) {
        frames = framesRead(Buffer.concat([frames, chunk]), opcodes);
        return;
      }
      head = Buffer.concat([head, chunk]);
      const end = head.indexOf("\r\n\r\n");
      if (end < 0) return;
      const lines = head.subarray(0, end).toString("latin1").split("\r\n");
      requests.push(lines[0] ?? "");
      const key = lines
        .find((line) => /^sec-websocket-key:/i.test(line))
        ?.slice("sec-websocket-key:".length)
        .trim();
      if (key === undefined) {
        socket.end("HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
        return;
      }
      if (!upgrade) return;
      const accept = createHash("sha1")
        .update(`${key}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`)
        .digest("base64");
      socket.write(
        [
          "HTTP/1.1 101 Switching Protocols",
          "Upgrade: websocket",
          "Connection: Upgrade",
          `Sec-WebSocket-Accept: ${accept}`,
          "Sec-WebSocket-Protocol: envoy-relay.v1",
          "",
          "",
        ].join("\r\n")
      );
      frames = framesRead(head.subarray(end + 4), opcodes);
    });
  });
  const listening = Promise.withResolvers<void>();
  server.listen(0, "127.0.0.1", () => listening.resolve());
  await listening.promise;
  const address = server.address();
  if (address === null || typeof address === "string")
    throw new Error("the mute listener bound no port");
  return {
    url: `http://127.0.0.1:${address.port}`,
    requests,
    opcodes,
    stop() {
      for (const socket of sockets) socket.destroy();
      server.close();
    },
  };
}

/** `release`, with OUTSIDE_EDGE_STATE alone. */
function release(state: string) {
  return runBounded({
    argv: ["bun", driver, "release"],
    bound: "short",
    cwd: dispatchPackage,
    env: { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "", OUTSIDE_EDGE_STATE: state },
    from: import.meta.url,
  });
}

/** Waits for the hold's `held`, as a check polls for it; where the hold ends first, or the short
 *  bound passes, throws with what the hold printed. */
async function heldBy(state: string, held: Promise<BoundedRun>): Promise<void> {
  const ended = watch(held);
  if (await whether(() => existsSync(join(state, "held")), ended)) return;
  const ran = ended() ? await held : undefined;
  throw new Error(
    ran === undefined
      ? "the hold wrote no held within the short bound"
      : `the hold ended, exit ${ran.code}, and wrote no held:\n${ran.stdout}${ran.stderr}`
  );
}

/** The mode bits of a file. */
const modeOf = (path: string) => statSync(path).mode & 0o777;

/** The start of process `pid`, in clock ticks since boot: field 22 of `/proc/<pid>/stat`, the
 *  fields after the name counted from field 3, since the name may hold spaces. */
function startOf(pid: number): number {
  const stat = readFileSync(`/proc/${pid}/stat`, "utf8");
  return Number(stat.slice(stat.lastIndexOf(")") + 2).split(" ")[19]);
}

/** The process a file names as `held` does, `<pid> <start>`; undefined until it is written. */
function namedBy(path: string): { readonly pid: number; readonly start: number } | undefined {
  const named = existsSync(path)
    ? /^([1-9][0-9]*) ([0-9]+)$/.exec(readFileSync(path, "utf8"))
    : null;
  return named === null ? undefined : { pid: Number(named[1]), start: Number(named[2]) };
}

/** A holder that takes SIGTERM and goes on, printing that it took it. Once its handler is in
 *  place it writes `<pid> <start>` to `at`, as `hold` names itself in `holder`, so a SIGTERM sent
 *  after that reaches the handler. */
function stubbornHolder(at: string): Promise<BoundedRun> {
  const holder = runBounded({
    argv: [
      "bun",
      "--eval",
      `process.on("SIGTERM", () => console.log("took SIGTERM"));
const fs = require("node:fs");
const stat = fs.readFileSync("/proc/self/stat", "utf8");
const start = stat.slice(stat.lastIndexOf(")") + 2).split(" ")[19];
fs.writeFileSync(${JSON.stringify(at)}, \`\${process.pid} \${start}\`);
setInterval(() => undefined, 1_000);`,
    ],
    bound: "short",
    env: { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "" },
    from: import.meta.url,
  });
  // A row that fails before it awaits the holder leaves it to end at its bound, and that end is
  // this row's own failure, not an error between rows: a row that awaits it still sees it.
  holder.catch(() => undefined);
  return holder;
}

/** The times of `edge`'s 101 upgrades of `session`: each socket the hold opened. */
function socketsOpened(edge: HoldEdge, session: string): number[] {
  return edge.relay.upgrades
    .filter((one) => one.session === session && one.status === 101)
    .map((one) => one.at);
}

describe.skipIf(!existsSync("/proc/self/stat"))(
  "hold and release, against an edge stand-in",
  () => {
    test(
      "hold registers the kept session under the device login's machine name and holds its socket until release ends it, unregistered",
      async () => {
        const edge = startHoldEdge();
        const { state, session } = await keptState(edge);
        try {
          const held = hold(edge, state);
          await heldBy(state, held);
          // While held: the session registered once, its socket open, its relay subject taken.
          expect(edge.registrations).toEqual([session]);
          expect(edge.relay.sockets(session)).toBe(1);
          expect(edge.relay.subs).toContainEqual({
            session,
            subject: standInDeliverSubject(session),
          });
          // The registration names the device login's machine name, never this machine's, and
          // carried the session's own token and topic alone.
          expect(edge.hosts).toEqual([deviceHost]);
          expect(edge.hosts).not.toContain(hostname());
          expect(edge.refusals).toEqual([]);
          const { pid, start } = namedBy(join(state, "held")) ?? { pid: 0, start: 0 };
          expect(pid).toBeGreaterThan(0);
          expect(start).toBe(startOf(pid));
          expect(namedBy(join(state, "holder"))).toEqual({ pid, start });
          expect(modeOf(join(state, "held"))).toBe(0o600);
          expect(modeOf(join(state, "holder"))).toBe(0o600);
          expect(edge.unregisters).toEqual([]);
          const released = await release(state);
          expect(released.stdout).toBe(`RELEASED ${session}\n`);
          expect(released.code).toBe(0);
          const ran = await held;
          expect(ran.stdout).toBe(`HELD ${session}\nRELEASED ${session}\n`);
          expect(ran.code).toBe(0);
          expect(ran.stderr).toContain(`the hold of ${session} ends: SIGTERM`);
          expect(readFileSync(join(state, "released"), "utf8")).toBe("unregistered");
          expect(modeOf(join(state, "released"))).toBe(0o600);
          expect(edge.unregisters).toEqual([session]);
          expect(edge.registrations).toEqual([session]);
          expect(edge.refusals).toEqual([]);
          await eventually("the held socket's close", () => edge.relay.sockets(session) === 0);
        } finally {
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("outsideEdgeHold", "short", "short")
    );

    test(
      "an ended hold leaves no held, and release spends its holder, so a release after it, as one before a later hold of the directory starts, reads no hold",
      async () => {
        const edge = startHoldEdge();
        const { state, session } = await keptState(edge);
        try {
          const first = hold(edge, state);
          await heldBy(state, first);
          expect((await release(state)).stdout).toBe(`RELEASED ${session}\n`);
          expect((await first).code).toBe(0);
          // What it left: how it ended, and nothing a later check reads as a hold.
          expect(existsSync(join(state, "held"))).toBe(false);
          expect(existsSync(join(state, "holder"))).toBe(false);
          expect(readFileSync(join(state, "released"), "utf8")).toBe("unregistered");
          const again = await release(state);
          expect(again.stdout).toBe("UNKNOWN: no session is held\n");
          expect(again.code).toBe(2);
          // A later hold of the same directory, which ends by itself at the listener's close for
          // good: it leaves no held either, and release reads its end once, then no hold.
          const second = hold(edge, state, { OUTSIDE_EDGE_HOLD_SECONDS: "60" });
          await heldBy(state, second);
          edge.relay.close(4403, "the token was revoked", session);
          expect((await second).code).toBe(2);
          expect(existsSync(join(state, "held"))).toBe(false);
          expect((await release(state)).stdout).toBe(
            `UNKNOWN: ${session} was not released: the listener closed the held socket (4403 the token was revoked); unregistered\n`
          );
          expect((await release(state)).stdout).toBe("UNKNOWN: no session is held\n");
          expect(edge.unregisters).toEqual([session, session]);
        } finally {
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit(
        "outsideEdgeHold",
        "short",
        "short",
        "short",
        "outsideEdgeHold",
        "short",
        "short",
        "short"
      )
    );

    test(
      "once the next hold of a directory has started, release reads its end, never that of a hold that ended by itself unreleased, and a third hold leaves a live one's records",
      async () => {
        const edge = startHoldEdge();
        // A listener that never opens the socket: the next hold stays in its start, not held.
        const mute = await startMuteListener(false);
        const { state, session } = await keptState(edge);
        let next: DrivenHold | undefined;
        try {
          // The first hold ends by itself, at the listener's close for good, and is never released.
          const first = hold(edge, state, { OUTSIDE_EDGE_HOLD_SECONDS: "60" });
          await heldBy(state, first);
          edge.relay.close(4403, "the token was revoked", session);
          expect((await first).code).toBe(2);
          const ended = readFileSync(join(state, "holder"), "utf8");
          expect(readFileSync(join(state, "released"), "utf8")).toStartWith(
            "the listener closed the held socket (4403"
          );
          next = driveHold(edge.relay.url, mute.url, state);
          const started = next;
          await eventually("the next hold's holder", () => {
            const now = namedBy(join(state, "holder"));
            return now !== undefined && now.pid === started.pid;
          });
          expect(readFileSync(join(state, "holder"), "utf8")).not.toBe(ended);
          // A third hold while that one runs holds nothing and leaves its records.
          const third = await hold(edge, state);
          expect(third.stdout).toBe(
            `UNKNOWN: no session held: the hold ${started.pid} still holds OUTSIDE_EDGE_STATE; release it first\n`
          );
          expect(third.code).toBe(2);
          expect(namedBy(join(state, "holder"))?.pid).toBe(started.pid);
          // The release ends the hold that is starting and reads its end, not the first one's.
          const why = `no session held: SIGTERM came before ${session} was registered, and no registration was sent`;
          const released = await release(state);
          expect(released.stdout).toBe(`UNKNOWN: ${session} was not released: ${why}\n`);
          expect(released.code).toBe(2);
          expect(await started.ended()).toBe(2);
          expect(started.stdout()).toBe(`UNKNOWN: ${why}\n`);
        } finally {
          next?.kill();
          mute.stop();
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("outsideEdgeHold", "short", "short", "outsideEdgeHold", "short", "outsideEdgeHold")
    );

    realTimeRow(
      "hold ends itself once OUTSIDE_EDGE_HOLD_SECONDS have passed since the listener took the session, the least of them, 60",
      async () => {
        const edge = startHoldEdge();
        const { state, session } = await keptState(edge);
        const holding = driveHold(edge.relay.url, edge.relay.url, state, {
          OUTSIDE_EDGE_HOLD_SECONDS: "60",
        });
        try {
          // The hold is another process on the real clock, and 60 s is the least it takes, so this
          // row spends that minute, counted from when the listener took the registration, before
          // which the hold's time had not begun.
          const code = await holding.ended();
          const endedAt = Date.now();
          expect(edge.taken).toHaveLength(1);
          expect(endedAt - (edge.taken[0] ?? endedAt)).toBeGreaterThanOrEqual(60_000);
          expect(holding.stdout(), holding.stderr()).toBe(`HELD ${session}\nRELEASED ${session}\n`);
          expect(code).toBe(0);
          expect(holding.stderr()).toContain(`the hold of ${session} ends: 60 s passed`);
          expect(edge.unregisters).toEqual([session]);
          // The hold ended on its own: release reads what it wrote and signals no process.
          const released = await release(state);
          expect(released.stdout).toBe(`RELEASED ${session}\n`);
          expect(released.code).toBe(0);
        } finally {
          holding.kill();
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("outsideEdgeHold", "short")
    );

    test(
      "a hold time outside 60 to 3600 s, or a renewal outside 1 to 90 s, is refused, naming the variable, and registers nothing",
      async () => {
        const edge = startHoldEdge();
        const { state } = await keptState(edge);
        try {
          const cases = [
            ...["59", "3601", "1e3", "-60"].map((seconds) => ({
              env: { OUTSIDE_EDGE_HOLD_SECONDS: seconds },
              said: "OUTSIDE_EDGE_HOLD_SECONDS must be a whole number of seconds from 60 to 3600",
            })),
            ...["0", "91"].map((seconds) => ({
              env: { OUTSIDE_EDGE_RENEW_SECONDS: seconds },
              said: "OUTSIDE_EDGE_RENEW_SECONDS must be a whole number of seconds from 1 to 90",
            })),
          ];
          const runs = await Promise.all(cases.map(({ env }) => hold(edge, state, env)));
          for (const [index, ran] of runs.entries()) {
            expect(ran.stdout).toBe(`UNKNOWN: no session held: ${cases[index]?.said}\n`);
            expect(ran.code).toBe(2);
          }
          expect(existsSync(join(state, "held"))).toBe(false);
          expect(existsSync(join(state, "holder"))).toBe(false);
          expect(edge.registrations).toEqual([]);
          expect(edge.relay.upgrades).toEqual([]);
        } finally {
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit({
        atOnce: [
          "outsideEdgeHold",
          "outsideEdgeHold",
          "outsideEdgeHold",
          "outsideEdgeHold",
          "outsideEdgeHold",
          "outsideEdgeHold",
        ],
      })
    );

    test(
      "a listener that refuses the registration: UNKNOWN naming it, no held, and release reads why",
      async () => {
        const edge = startHoldEdge({ registration: 403 });
        const { state, session } = await keptState(edge);
        try {
          const ran = await hold(edge, state);
          const why = `the listener did not register ${session}: `;
          expect(ran.stdout).toStartWith(`UNKNOWN: ${why}`);
          expect(ran.stdout).toContain("the stand-in refuses this registration");
          expect(ran.stdout).not.toContain("HELD");
          expect(ran.code).toBe(2);
          expect(edge.registrations).toEqual([session]);
          expect(existsSync(join(state, "held"))).toBe(false);
          expect(readFileSync(join(state, "released"), "utf8")).toStartWith(why);
          const released = await release(state);
          expect(released.stdout).toStartWith(`UNKNOWN: ${session} was not released: ${why}`);
          expect(released.code).toBe(2);
        } finally {
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("outsideEdgeHold", "short")
    );

    test(
      "a listener that refuses the relay subject: UNKNOWN naming it, no registration sent, no held",
      async () => {
        const edge = startHoldEdge();
        edge.relay.refuseOwnSubject = true;
        const { state, session } = await keptState(edge);
        try {
          const ran = await hold(edge, state);
          expect(ran.stdout, ran.stderr).toBe(
            `UNKNOWN: the listener did not register ${session}: the listener's relay socket refused the subject ${standInDeliverSubject(session)} (subject_not_granted), so no registration was sent\n`
          );
          expect(ran.code).toBe(2);
          expect(edge.registrations).toEqual([]);
          expect(existsSync(join(state, "held"))).toBe(false);
        } finally {
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("outsideEdgeHold")
    );

    test(
      "a listener that closes the held socket for good (4403): the close and its code printed, no second registration, exit 2",
      async () => {
        const edge = startHoldEdge();
        const { state, session } = await keptState(edge);
        try {
          // Its least time, so a hold that took this close for a routine one ends in a minute.
          const held = hold(edge, state, { OUTSIDE_EDGE_HOLD_SECONDS: "60" });
          await heldBy(state, held);
          edge.relay.close(4403, "the token was revoked", session);
          const ran = await held;
          const closed = "4403 the token was revoked";
          expect(ran.stdout).toBe(
            `HELD ${session}\nUNKNOWN: the listener closed the held socket of ${session} (${closed}); it was not registered again\n`
          );
          expect(ran.code).toBe(2);
          expect(edge.registrations).toEqual([session]);
          expect(edge.unregisters).toEqual([session]);
          expect(readFileSync(join(state, "released"), "utf8")).toBe(
            `the listener closed the held socket (${closed}); unregistered`
          );
          const released = await release(state);
          expect(released.stdout).toBe(
            `UNKNOWN: ${session} was not released: the listener closed the held socket (${closed}); unregistered\n`
          );
          expect(released.code).toBe(2);
        } finally {
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("outsideEdgeHold", "short", "short")
    );

    test(
      "an unregister that fails: RELEASED and exit 2, released naming the failure, and release UNKNOWN",
      async () => {
        const edge = startHoldEdge({ unregister: 500 });
        const { state, session } = await keptState(edge);
        try {
          const held = hold(edge, state);
          await heldBy(state, held);
          const released = await release(state);
          expect(released.stdout).toStartWith(
            `UNKNOWN: ${session} was not released: the unregister failed: `
          );
          expect(released.code).toBe(2);
          const ran = await held;
          expect(ran.stdout).toBe(`HELD ${session}\nRELEASED ${session}\n`);
          expect(ran.code).toBe(2);
          expect(edge.unregisters).toEqual([session]);
          expect(readFileSync(join(state, "released"), "utf8")).toStartWith(
            "the unregister failed: "
          );
        } finally {
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("outsideEdgeHold", "short", "short")
    );

    test(
      "release with no hold: UNKNOWN: no session is held",
      async () => {
        const state = mkdtempSync(join(tmpdir(), "outside-edge-hold-"));
        try {
          const released = await release(state);
          expect(released.stdout).toBe("UNKNOWN: no session is held\n");
          expect(released.code).toBe(2);
        } finally {
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("short")
    );

    test(
      "a hold refused at its inputs leaves no holder, held or released an earlier hold wrote, so release reads no hold",
      async () => {
        const edge = startHoldEdge();
        const { state } = await keptState(edge);
        try {
          // What an earlier hold left once it ended released, naming a pid no process has.
          for (const [file, text] of [
            ["holder", `${2 ** 22 + 1} 1`],
            ["held", `${2 ** 22 + 1} 1`],
            ["released", "unregistered"],
          ] as const) {
            writeFileSync(join(state, file), text, { mode: 0o600 });
          }
          const ran = await hold(edge, state, { OUTSIDE_EDGE_HOLD_SECONDS: "59" });
          expect(ran.stdout).toStartWith("UNKNOWN: no session held: OUTSIDE_EDGE_HOLD_SECONDS ");
          expect(ran.code).toBe(2);
          for (const file of ["holder", "held", "released"]) {
            expect(existsSync(join(state, file)), file).toBe(false);
          }
          const released = await release(state);
          expect(released.stdout).toBe("UNKNOWN: no session is held\n");
          expect(released.code).toBe(2);
        } finally {
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("outsideEdgeHold", "short")
    );

    // An error of the hold's own judged nothing of the edge, so it is UNKNOWN, exit 2, never 1,
    // which is a FAIL. These two make the error as root too, which a directory's mode does not
    // stop: a `holder` that is a directory with a file in it, which the hold's start cannot remove,
    // and a `held` made a directory while the listener answers the registration, so the hold
    // cannot write it once the listener took the session, and must unregister it.
    test(
      "a hold that cannot write OUTSIDE_EDGE_STATE is UNKNOWN, exit 2, naming the file, and leaves no session registered",
      async () => {
        const answer = Promise.withResolvers<void>();
        const edge = startHoldEdge({ answerAfter: answer.promise });
        const { state, session } = await keptState(edge);
        const blocked = mkdtempSync(join(tmpdir(), "outside-edge-hold-"));
        writeFileSync(join(blocked, "session"), session, { mode: 0o600 });
        copyFileSync(join(state, "device-login.json"), join(blocked, "device-login.json"));
        mkdirSync(join(blocked, "holder"));
        writeFileSync(join(blocked, "holder", "kept"), "");
        const holding = driveHold(edge.relay.url, edge.relay.url, state);
        try {
          const before = await hold(edge, blocked);
          expect(before.stdout, before.stderr).toBe(
            "UNKNOWN: the hold failed on an error of its own: rm ERR_FS_EISDIR on OUTSIDE_EDGE_STATE's holder\n"
          );
          expect(before.code).toBe(2);
          // It sent nothing: the one registration below is the other hold's.
          await eventually("the registration at the listener", () => edge.registrations.length > 0);
          expect(edge.registrations).toEqual([session]);
          mkdirSync(join(state, "held"));
          answer.resolve();
          const code = await holding.ended();
          expect(holding.stdout(), holding.stderr()).toBe(
            "UNKNOWN: the hold failed on an error of its own: open EISDIR on OUTSIDE_EDGE_STATE's held; unregistered\n"
          );
          expect(code).toBe(2);
          expect(edge.registrations).toEqual([session]);
          expect(edge.unregisters).toEqual([session]);
        } finally {
          answer.resolve();
          holding.kill();
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
          rmSync(blocked, { force: true, recursive: true });
        }
      },
      rowLimit("outsideEdgeHold", "outsideEdgeHold")
    );

    // The brief's own case, a state directory no write reaches: its mode stops every user but root,
    // which writes through it, so this row runs as any other user, and says it skipped as root.
    test.skipIf(process.getuid?.() === 0)(
      "a state directory of mode 0500 is UNKNOWN, exit 2, naming the holder it could not write, and registers nothing",
      async () => {
        const edge = startHoldEdge();
        const { state } = await keptState(edge);
        chmodSync(state, 0o500);
        try {
          const ran = await hold(edge, state);
          expect(ran.stdout, ran.stderr).toBe(
            "UNKNOWN: the hold failed on an error of its own: open EACCES on OUTSIDE_EDGE_STATE's holder\n"
          );
          expect(ran.code).toBe(2);
          expect(edge.relay.upgrades).toEqual([]);
          expect(edge.registrations).toEqual([]);
        } finally {
          chmodSync(state, 0o700);
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("outsideEdgeHold")
    );

    realTimeRow(
      "release sends SIGKILL to a holder still running 15 s after its SIGTERM, and exits 2",
      async () => {
        const state = mkdtempSync(join(tmpdir(), "outside-edge-hold-"));
        const session = `outside-e2e-${randomBytes(6).toString("hex")}`;
        writeFileSync(join(state, "session"), session, { mode: 0o600 });
        try {
          const holder = stubbornHolder(join(state, "holder"));
          await eventually(
            "the holder's holder file",
            () => namedBy(join(state, "holder")) !== undefined
          );
          const { pid } = namedBy(join(state, "holder")) ?? { pid: 0 };
          expect(pid).toBeGreaterThan(0);
          const started = performance.now();
          const released = await release(state);
          expect(performance.now() - started).toBeGreaterThanOrEqual(15_000);
          expect(released.stdout).toBe(
            `UNKNOWN: the holder ${pid} of ${session} still ran 15 s after its SIGTERM, so it was sent SIGKILL; ${session} may still be registered\n`
          );
          expect(released.code).toBe(2);
          const ran = await holder;
          expect(ran.stdout).toBe("took SIGTERM\n");
          expect(ran.code).not.toBe(0);
          expect(() => process.kill(pid, 0)).toThrow();
        } finally {
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("short", "short", "short")
    );

    test(
      "release signals no process at the held pid that started at another time: the held process is gone",
      async () => {
        // The row above is this one's control: the same holder, named by its own start, takes the
        // SIGTERM. Here `holder` names its pid with another start, as a pid a later process took.
        const state = mkdtempSync(join(tmpdir(), "outside-edge-hold-"));
        const session = `outside-e2e-${randomBytes(6).toString("hex")}`;
        writeFileSync(join(state, "session"), session, { mode: 0o600 });
        const ready = join(state, "ready");
        const holder = stubbornHolder(ready);
        let alive = 0;
        try {
          await eventually("the holder's ready file", () => namedBy(ready) !== undefined);
          const { pid, start } = namedBy(ready) ?? { pid: 0, start: 0 };
          expect(pid).toBeGreaterThan(0);
          alive = pid;
          writeFileSync(join(state, "holder"), `${pid} ${start + 1}`, { mode: 0o600 });
          const released = await release(state);
          expect(() => process.kill(pid, 0)).not.toThrow();
          process.kill(pid, "SIGKILL");
          alive = 0;
          const ran = await holder;
          expect(released.stdout).toBe("UNKNOWN: the held process is gone\n");
          expect(released.code).toBe(2);
          // It took no SIGTERM, which it would have printed.
          expect(ran.stdout).toBe("");
        } finally {
          // A row that failed before the holder was ended ends it here, rather than at its bound;
          // one that ended already throws, and is left so.
          try {
            if (alive > 0) process.kill(alive, "SIGKILL");
          } catch {}
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("short", "short", "short")
    );

    realTimeRow(
      "a flush the listener never answers fails at its bound, naming it, and sends no registration",
      async () => {
        const edge = startHoldEdge();
        const mute = await startMuteListener(true);
        const { state, session } = await keptState(edge);
        try {
          const ran = await hold(edge, state, {}, mute.url);
          expect(ran.stdout, ran.stderr).toBe(
            `UNKNOWN: the listener did not register ${session}: the relay socket's flush got no answer within 10 s, so no registration was sent\n`
          );
          expect(ran.code).toBe(2);
          expect(existsSync(join(state, "held"))).toBe(false);
          // The flush's ping reached the listener, and nothing but the upgrade did: no
          // registration, so no unregister either.
          expect(mute.opcodes).toContain(0x9);
          expect(mute.requests).toHaveLength(1);
          expect(mute.requests[0]).toStartWith(`GET /v1/sessions/${session}/relay `);
        } finally {
          mute.stop();
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("outsideEdgeHold")
    );

    test(
      "a SIGTERM while the relay socket opens ends the hold, which sends no registration and holds nothing",
      async () => {
        const edge = startHoldEdge();
        const mute = await startMuteListener(false);
        const { state, session } = await keptState(edge);
        const holding = driveHold(edge.relay.url, mute.url, state);
        try {
          await eventually(
            "the relay socket's upgrade at the listener",
            () => mute.requests.length > 0
          );
          const terminatedAt = performance.now();
          process.kill(holding.pid, "SIGTERM");
          const code = await holding.ended();
          // The SIGTERM cuts the open short. A hold that waited out the open's own 30 s after it
          // prints the same line, so the row holds its end well inside that bound: 10 s, a third of
          // it, and not less, since the hold's exit took 1 s unloaded and a box at a load average
          // of 700 or more stretches a process's exit by seconds.
          const took = Math.round(performance.now() - terminatedAt);
          expect(
            took,
            `the hold ended ${took} ms after its SIGTERM, not inside 10000 ms`
          ).toBeLessThan(10_000);
          expect(holding.stdout(), holding.stderr()).toBe(
            `UNKNOWN: no session held: SIGTERM came before ${session} was registered, and no registration was sent\n`
          );
          expect(code).toBe(2);
          expect(existsSync(join(state, "held"))).toBe(false);
          for (const line of mute.requests) {
            expect(line).toStartWith(`GET /v1/sessions/${session}/relay `);
          }
          expect(edge.registrations).toEqual([]);
        } finally {
          holding.kill();
          mute.stop();
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("short", "outsideEdgeHold")
    );

    test(
      "a SIGTERM while the flush waits for its pong ends the hold, which sends no registration",
      async () => {
        const edge = startHoldEdge();
        const mute = await startMuteListener(true);
        const { state, session } = await keptState(edge);
        const holding = driveHold(edge.relay.url, mute.url, state);
        try {
          await eventually("the flush's ping at the listener", () => mute.opcodes.includes(0x9));
          process.kill(holding.pid, "SIGTERM");
          const code = await holding.ended();
          expect(holding.stdout(), holding.stderr()).toBe(
            `UNKNOWN: no session held: SIGTERM came before ${session} was registered, and no registration was sent\n`
          );
          expect(code).toBe(2);
          expect(existsSync(join(state, "held"))).toBe(false);
          expect(mute.requests).toHaveLength(1);
          expect(mute.requests[0]).toStartWith(`GET /v1/sessions/${session}/relay `);
        } finally {
          holding.kill();
          mute.stop();
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("short", "outsideEdgeHold")
    );

    test(
      "a SIGTERM while the listener answers the registration leaves no session held and none registered",
      async () => {
        const answer = Promise.withResolvers<void>();
        const edge = startHoldEdge({ answerAfter: answer.promise });
        const { state, session } = await keptState(edge);
        const holding = driveHold(edge.relay.url, edge.relay.url, state);
        try {
          await eventually("the registration at the listener", () => edge.registrations.length > 0);
          process.kill(holding.pid, "SIGTERM");
          // The listener answers once the hold has taken the SIGTERM, which it says on stderr; a
          // hold that never says so is answered at the short bound, and is judged by what it did.
          await whether(() => holding.stderr().includes("outside-edge: hold got SIGTERM\n"));
          answer.resolve();
          const code = await holding.ended();
          expect(holding.stdout(), holding.stderr()).toBe(
            `UNKNOWN: no session held: SIGTERM came while the listener answered the registration of ${session}; unregistered\n`
          );
          expect(code).toBe(2);
          expect(existsSync(join(state, "held"))).toBe(false);
          expect(edge.registrations).toEqual([session]);
          expect(edge.unregisters).toEqual([session]);
        } finally {
          answer.resolve();
          holding.kill();
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("short", "short", "outsideEdgeHold")
    );

    test(
      "a release before held stops the hold, which sends no registration, and reads why it held nothing",
      async () => {
        const edge = startHoldEdge();
        // A listener that never opens the socket: the hold is opening it, and holds nothing yet.
        const mute = await startMuteListener(false);
        const { state, session } = await keptState(edge);
        const holding = driveHold(edge.relay.url, mute.url, state);
        try {
          await eventually(
            "the relay socket's upgrade at the listener",
            () => mute.requests.length > 0
          );
          expect(existsSync(join(state, "held"))).toBe(false);
          const why = `no session held: SIGTERM came before ${session} was registered, and no registration was sent`;
          const released = await release(state);
          expect(released.stdout).toBe(`UNKNOWN: ${session} was not released: ${why}\n`);
          expect(released.code).toBe(2);
          const code = await holding.ended();
          expect(holding.stdout(), holding.stderr()).toBe(`UNKNOWN: ${why}\n`);
          expect(code).toBe(2);
          expect(edge.registrations).toEqual([]);
        } finally {
          holding.kill();
          mute.stop();
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("short", "short", "outsideEdgeHold")
    );

    realTimeRow(
      "a hold outlasts two token lapses (4401) and the listener's stop (1012), registering again on each new socket, and ends RELEASED",
      async () => {
        const edge = startHoldEdge();
        const { state, session } = await keptState(edge);
        // Each token the broker mints from now lasts 3 s, as an edge relay token lasts 300 s at most.
        edge.relay.tokenSeconds = 3;
        try {
          const held = hold(edge, state);
          await heldBy(state, held);
          const lapses = closeAtTokenLapse(edge, session);
          try {
            await eventually(
              "two token lapses, each followed by a registration on the socket opened after it",
              () =>
                lapses.closes() >= 2 &&
                socketsOpened(edge, session).length >= 3 &&
                edge.taken.length >= socketsOpened(edge, session).length
            );
          } finally {
            lapses.stop();
          }
          // The listener stops (1012): the socket opens again at once, its token its own.
          edge.relay.tokenSeconds = 3600;
          const sockets = socketsOpened(edge, session).length;
          const registered = edge.taken.length;
          edge.relay.close(1012, "the listener is stopping", session);
          await eventually(
            "a registration on the socket opened after the listener's stop",
            () => socketsOpened(edge, session).length > sockets && edge.taken.length > registered
          );
          // Every socket the hold opened was registered on after it opened, before the next.
          const opened = socketsOpened(edge, session);
          for (const [index, at] of opened.entries()) {
            const next = opened[index + 1] ?? Number.POSITIVE_INFINITY;
            expect(
              edge.taken.some((one) => one >= at && one < next),
              `socket ${index}: ${JSON.stringify({ opened, taken: edge.taken })}`
            ).toBe(true);
          }
          const released = await release(state);
          expect(released.stdout).toBe(`RELEASED ${session}\n`);
          expect(released.code).toBe(0);
          const ran = await held;
          expect(ran.stdout, ran.stderr).toBe(`HELD ${session}\nRELEASED ${session}\n`);
          expect(ran.code).toBe(0);
          expect(ran.stderr).toContain("(4401 the token lapsed), a routine close");
          expect(ran.stderr).toContain("(1012 the listener is stopping), a routine close");
          expect(edge.unregisters).toEqual([session]);
          expect(edge.refusals).toEqual([]);
        } finally {
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("outsideEdgeHold", "short", "short", "short", "short")
    );

    realTimeRow(
      "a hold renews its registration, so a listener whose record of it lapses 5 s after the last still holds it past that, and ends RELEASED",
      async () => {
        // The listener's record lapses 5 minutes after the session's last registration; here 5 s,
        // and the hold renews every 1 s, as a host renews every 90 s.
        const lapseMs = 5_000;
        const edge = startHoldEdge();
        const { state, session } = await keptState(edge);
        try {
          const held = hold(edge, state, { OUTSIDE_EDGE_RENEW_SECONDS: "1" });
          await heldBy(state, held);
          const heldAt = Date.now();
          await eventually(
            "the lapse passed since the hold took the session, and 2 s more",
            () => Date.now() - heldAt > lapseMs + 2_000
          );
          // Registered all along: no gap between two registrations, or since the last, reached the
          // lapse.
          const times = [...edge.taken, Date.now()];
          const gaps = times.slice(1).map((at, index) => at - (times[index] ?? at));
          expect(Math.max(...gaps), JSON.stringify(times)).toBeLessThan(lapseMs);
          const released = await release(state);
          expect(released.stdout).toBe(`RELEASED ${session}\n`);
          expect(released.code).toBe(0);
          const ran = await held;
          expect(ran.stdout, ran.stderr).toBe(`HELD ${session}\nRELEASED ${session}\n`);
          expect(ran.code).toBe(0);
          expect(new Set(edge.registrations)).toEqual(new Set([session]));
          expect(edge.refusals).toEqual([]);
        } finally {
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("outsideEdgeHold", "short", "short")
    );

    test(
      "a renewal the listener refuses ends the hold UNKNOWN, unregistered, and release reads why",
      async () => {
        const edge = startHoldEdge({ refuseAfter: 1 });
        const { state, session } = await keptState(edge);
        try {
          // Its least time, so a hold that went on past the refusal ends in a minute.
          const ran = await hold(edge, state, {
            OUTSIDE_EDGE_RENEW_SECONDS: "1",
            OUTSIDE_EDGE_HOLD_SECONDS: "60",
          });
          const why = `the hold of ${session} lost its registration: the renewal failed: `;
          expect(ran.stdout, ran.stderr).toStartWith(`HELD ${session}\nUNKNOWN: ${why}`);
          expect(ran.stdout).toContain("the stand-in refuses this registration");
          expect(ran.stdout).toEndWith("; unregistered\n");
          expect(ran.code).toBe(2);
          expect(edge.registrations).toEqual([session, session]);
          expect(edge.unregisters).toEqual([session]);
          expect(readFileSync(join(state, "released"), "utf8")).toStartWith(why);
          const released = await release(state);
          expect(released.stdout).toStartWith(`UNKNOWN: ${session} was not released: ${why}`);
          expect(released.code).toBe(2);
        } finally {
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("outsideEdgeHold", "short")
    );

    test(
      "a SIGTERM while a routinely closed socket opens again sends no registration after it, and the hold ends RELEASED",
      async () => {
        const edge = startHoldEdge();
        const { state, session } = await keptState(edge);
        const holding = driveHold(edge.relay.url, edge.relay.url, state);
        try {
          await eventually("the hold's held", () => existsSync(join(state, "held")));
          // The socket closes as at its token's lapse, and the listener refuses every open from
          // now, so the hold is opening it again when its SIGTERM comes.
          edge.relay.refuseUpgrades = 503;
          const closedAt = Date.now();
          edge.relay.close(4401, "the token lapsed", session);
          await eventually("an open of the socket refused after its close", () =>
            edge.relay.upgrades.some(
              (one) => one.session === session && one.status === 503 && one.at >= closedAt
            )
          );
          const terminatedAt = Date.now();
          process.kill(holding.pid, "SIGTERM");
          // The listener takes opens again: a hold still opening would register on the new socket.
          edge.relay.refuseUpgrades = undefined;
          const code = await holding.ended();
          expect(holding.stdout(), holding.stderr()).toBe(`HELD ${session}\nRELEASED ${session}\n`);
          expect(code).toBe(0);
          expect(edge.taken.filter((at) => at >= terminatedAt)).toEqual([]);
          expect(edge.registrations).toEqual([session]);
          expect(edge.unregisters).toEqual([session]);
        } finally {
          holding.kill();
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("short", "short", "outsideEdgeHold")
    );

    realTimeRow(
      "a held socket that does not open again within 30 s of a routine close ends the hold UNKNOWN, unregistered, and never RELEASED",
      async () => {
        // The socket closes as at its token's lapse (4401), and the listener refuses every open
        // from then on, so the hold waits out its 30 s for the socket to open again and then ends
        // lost. Its time is the least, 60 s: a hold that went on past the 30 s would end at it
        // RELEASED, exit 0, holding a registration on no socket.
        const edge = startHoldEdge();
        const { state, session } = await keptState(edge);
        try {
          const held = hold(edge, state, { OUTSIDE_EDGE_HOLD_SECONDS: "60" });
          await heldBy(state, held);
          edge.relay.refuseUpgrades = 503;
          const closedAt = Date.now();
          edge.relay.close(4401, "the token lapsed", session);
          const ran = await held;
          const endedAfter = Date.now() - closedAt;
          const why = `the hold of ${session} lost its registration: its relay socket did not open again within 30 s of the close (4401 the token lapsed); unregistered`;
          expect(ran.stdout, ran.stderr).toBe(`HELD ${session}\nUNKNOWN: ${why}\n`);
          expect(ran.code).toBe(2);
          // At that bound, and well before the hold's own 60 s.
          expect(endedAfter).toBeGreaterThanOrEqual(30_000);
          expect(endedAfter).toBeLessThan(55_000);
          expect(edge.relay.upgrades.some((one) => one.status === 503)).toBe(true);
          expect(edge.unregisters).toEqual([session]);
          expect(readFileSync(join(state, "released"), "utf8")).toBe(why);
          const released = await release(state);
          expect(released.stdout).toBe(`UNKNOWN: ${session} was not released: ${why}\n`);
          expect(released.code).toBe(2);
        } finally {
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("outsideEdgeHold", "short", "short")
    );

    realTimeRow(
      "a renewal on the wire at a SIGTERM is not sent again after it, and the hold ends RELEASED",
      async () => {
        // The first registration is taken, and every one after it is never answered: the renewal
        // that comes 1 s later waits out its attempt's 5 s, after which the Envoy client sends a
        // second attempt of a call that got no answer, unless the hold has ended by then.
        const edge = startHoldEdge({ stallAfter: 1 });
        const { state, session } = await keptState(edge);
        const holding = driveHold(edge.relay.url, edge.relay.url, state, {
          OUTSIDE_EDGE_RENEW_SECONDS: "1",
        });
        try {
          await eventually("the renewal at the listener", () => edge.registrations.length >= 2);
          const asked = edge.registrations.length;
          process.kill(holding.pid, "SIGTERM");
          const code = await holding.ended();
          expect(holding.stdout(), holding.stderr()).toBe(`HELD ${session}\nRELEASED ${session}\n`);
          expect(code).toBe(0);
          // No attempt of the renewal, nor any registration, reached the listener after it.
          expect(edge.registrations, holding.stderr()).toHaveLength(asked);
          expect(edge.unregisters).toEqual([session]);
        } finally {
          holding.kill();
          edge.relay.stop();
          rmSync(state, { force: true, recursive: true });
        }
      },
      rowLimit("short", "outsideEdgeHold")
    );
  }
);
