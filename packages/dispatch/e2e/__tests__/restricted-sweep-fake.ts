/** A fake Dispatch the restricted sweep's judging rows run `e2e/acceptance/restricted-sweep.sh`
 *  against, and the one run of the sweep each row makes (`sweep`). The fake keeps every route
 *  from the restricted member but the one a row plants a leak in, so the leak that row plants is
 *  the only reason the run can fail. A run against a real stack (`restricted-sweep.test.ts`) fails
 *  on many routes at once, and a rule that stopped working there would hide behind the others.
 *
 *  The fake answers the full member's lookup with one object of each kind the sweep fills a
 *  wildcard with, all of the project TGT. To the full member, any other path or query that names
 *  one of those objects answers a row of TGT, a bare list answers one too, and a path naming an
 *  object that does not exist answers 404. To the restricted member, every path that names an
 *  object of TGT or a missing one answers 404 and a bare list answers `[]`, as a server holding
 *  them to their own projects does. The GitHub proxy answers a repository its caller may not see,
 *  and one nobody holds, as the real proxy answers a repository the App is not installed on: 503
 *  GITHUB_TOKEN_UNAVAILABLE. Its sockets refuse every upgrade unless a row serves them
 *  (FeedShape), so a run with no leak and no socket served is UNKNOWN, never PASS.
 *
 *  It also records every path the restricted member is sent, so a row can hold the sweep to
 *  calling every GET row of the route index, read from that record rather than from the script's
 *  own lines. */

import { writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { ServerWebSocket } from "bun";
import { createEncoder, toUint8Array, writeVarString, writeVarUint } from "lib0/encoding";

import { type BoundName, runBounded } from "./bounded-subprocess";
import { dispatchPackage, getRows, sweepScript } from "./restricted-sweep-index";

export const fullToken = "the-full-members-token";
export const memberCookie = "dsession=the-restricted-members-cookie";
export const fullCookie = "dsession=the-full-members-cookie";

/** Both sockets served, and the full member's browser for the GitHub proxy and the agent view:
 *  what a run needs to pass where its rows do. */
export const served = { env: { SWEEP_FULL_COOKIE: fullCookie }, feed: { doc: true, live: true } };
/** The verdict of a run that passes, of one run (a runner's goes on to name its live window). */
export const passed = "PASS: no route and neither socket gave the member a row of TGT";

/** `text` as a RegExp source that matches it literally. */
export const literally = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

/** TGT's objects, as the fake's lookup answers them, and the reference scheme it writes: the sweep
 *  reads the scheme from the server's own answer rather than spelling one. */
export const tgt = {
  issue: "TGT-1",
  title: "Chart the target reef",
  document: "11111111-1111-4111-8111-111111111111",
  block: "b-1111111111",
  comment: "22222222-2222-4222-8222-222222222222",
  ask: "33333333-3333-4333-8333-333333333333",
  message: "44444444-4444-4444-8444-444444444444",
  handoff: "55555555-5555-4555-8555-555555555555",
  projectDocument: "66666666-6666-4666-8666-666666666666",
  nearCopy: "77777777-7777-4777-8777-777777777777",
  goingRun: "88888888-8888-4888-8888-888888888888",
  session: "ses-tgt-worker",
  conversationFile: "shot-png",
  directMessage: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
  repository: "acme/tgt-repo",
  /** A person who wrote in TGT alone: the author of its issue's message. */
  person: "dave@tgt.example",
};
/** The member's own session, in OWN, which the live check's control message is sent to. */
export const ownSession = "ses-own-worker";
/** The restricted member's login, which whoami answers the member's cookie and OWN's access list
 *  names. */
export const memberLogin = "bob@outside.example";
/** A repository both TGT's first issue and OWN's resolve to, under `sharedRepo`. */
const sharedRepository = "acme/shared-repo";
const scheme = "fake-ref://";
const tgtValues = Object.values(tgt);
/** The values the sweep fills a path with on its missing side, which name nothing. */
const missingValues = [
  "ZZNONE",
  "00000000-0000-4000-8000-",
  "no-such-",
  "nobody@example.com",
  "ses-nosuchsession",
  "b-nosuchblock",
  "999999999999",
  "zz no such words zz",
];
/** The route that answers a repository its caller may not see as one the App is not installed
 *  on, which the sweep fills with the project's repository. */
export const appInstallation = "/api/v1/github/app-installation/{owner}/{repo}";

/** What a planted answer answers, for the requests it matches: the member's (a leak) or, among a
 *  shape's `full`, the full member's. */
export interface Leak {
  readonly matches: (target: string) => boolean;
  readonly status: number;
  readonly body: unknown;
}

/** A planted answer at the one request whose decoded path and query is `target`. */
export function at(target: string, status: number, body: unknown): Leak {
  return { matches: (seen) => seen === target, status, body };
}

/** How a fake serves its sockets. The live feed: not at all (every upgrade refused 404), or, with
 *  `live`, a feed that starts for both callers and sends each caller every write it may read.
 *  `closes` closes the member's feed when the comment is written, which the sweep writes only
 *  once both feeds have started, so it closes long before any window could end; `ends` answers
 *  the member's events subscription `ended` right after its `events_start`, the socket left open,
 *  as the server ends a subscription whose read failed; `refuses` answers it an `error` frame in
 *  place of its `events_start`. `memberRefusal` refuses the member's live upgrade 404 with that
 *  body. `noise` sends the full member, in place of the comment, an event of TGT whose first field
 *  holds that text. `dmLeak` sends the member's feed the full member's direct message to TGT's
 *  session; `ownDmLost` leaves the member's own direct message out of their feed. An agent view
 *  of TGT's session answers the full member that the session responds, and its replay, and the
 *  member as no one answers for it, unless `agentLeak` sends the member the replay too. The
 *  document socket: not at all, or, with `doc`, a room for the member's own document that admits
 *  them, as Hocuspocus answers a token frame, and a 404 for TGT's, whose body is `docRefusal`
 *  when a row sets it; `ownDocSilent` leaves the member's own room unanswered, so the control's
 *  probe gives no answer. */
export interface FeedShape {
  readonly live?: boolean;
  readonly closes?: boolean;
  readonly ends?: boolean;
  readonly refuses?: boolean;
  readonly memberRefusal?: unknown;
  readonly noise?: string;
  readonly dmLeak?: boolean;
  readonly ownDmLost?: boolean;
  readonly agentLeak?: boolean;
  readonly doc?: boolean;
  readonly docRefusal?: unknown;
  readonly ownDocSilent?: boolean;
}

/** What a fake changes of its answers. `omit` leaves a kind out of the lookup; `issues` is how many
 *  issues TGT lists (1 by default, 2 with `sharedRepo`), and `handoffOn` the one whose handoff the
 *  lookup finds; `conversationGone` answers the conversation file as missing to the full member
 *  too; `proxyReadsCookies` makes the GitHub proxy read as the real one does, as the browser's
 *  person alone, so the full member's token is answered there as a repository the App is not
 *  installed on; `notInstalled` answers that the App is installed on no repository, TGT's
 *  included; `sharedRepo` puts TGT's first issue and OWN's on one repository the App is installed
 *  on, which both callers may see; `full` plants answers for the full member's requests;
 *  `createdAt` is when OWN was created, which GET /api/v1/projects answers either caller, so
 *  fills made at one fake and a runner at another whose OWN was created otherwise stand for two
 *  deployments; null lists OWN with no creation time. */
export interface FakeShape {
  readonly omit?: ReadonlySet<string>;
  readonly issues?: number;
  readonly handoffOn?: string;
  readonly conversationGone?: boolean;
  readonly proxyReadsCookies?: boolean;
  readonly notInstalled?: boolean;
  readonly sharedRepo?: boolean;
  readonly full?: readonly Leak[];
  readonly feed?: FeedShape;
  readonly createdAt?: string | null;
  /** The GET patterns of the route index a run reads, which the fake's own index (GET /api/v1)
   *  lists too, as a deployment's lists the routes its route table holds; the whole index when
   *  unset. */
  readonly rows?: readonly string[];
}

/** The fake's own route index, GET /api/v1, as the server answers it: every GET route the run's
 *  route index holds. */
function routeIndexOf(fake: FakeShape): unknown {
  return { routes: (fake.rows ?? getRows).map((path) => ({ method: "GET", path })) };
}

/** OWN, the member's own project, as GET /api/v1/projects lists it to either caller. */
function ownProject(fake: FakeShape): unknown {
  if (fake.createdAt === null) return { key: "OWN" };
  return { key: "OWN", created_at: fake.createdAt ?? "2026-01-01T00:00:00.123456Z" };
}

/** The lookup the full member is answered, by exact path and query: TGT's objects, and OWN's, the
 *  member's own project, whose issue names carol and no session. TGT's message is dave's, a
 *  person of TGT alone. */
function lookup(target: string, fake: FakeShape): unknown {
  const author = { kind: "user", id: "carol@example.com" };
  const agent = { kind: "session", id: tgt.session };
  const handoffOn = fake.handoffOn ?? tgt.issue;
  const issues = Array.from(
    { length: fake.issues ?? (fake.sharedRepo === true ? 2 : 1) },
    (_, index) => ({
      key: `TGT-${index + 1}`,
      title: index === 0 ? tgt.title : `Issue ${index + 1} of the target reef`,
      repo: index === 0 && fake.sharedRepo === true ? sharedRepository : tgt.repository,
    })
  );
  const ownIssue = {
    key: "OWN-1",
    created_by: author,
    assignee: "carol@example.com",
    ...(fake.sharedRepo === true ? { repo: sharedRepository } : {}),
  };
  const answers: Record<string, unknown> = {
    "/api/v1": routeIndexOf(fake),
    "/api/v1/whoami": { kind: "agent", owner: "carol@example.com" },
    "/api/v1/projects": [ownProject(fake), { key: "TGT", created_at: "2026-01-02T00:00:00Z" }],
    "/api/v1/projects/TGT/access": { project: "TGT", people: [] },
    "/api/v1/projects/OWN/access": { project: "OWN", people: [memberLogin] },
    "/api/v1/issues?project=TGT&limit=50": { issues },
    "/api/v1/issues?project=OWN&limit=50": { issues: [ownIssue] },
    "/api/v1/issues/OWN-1/comments": [],
    "/api/v1/issues/OWN-1/asks": [],
    "/api/v1/issues/OWN-1/messages": [],
    "/api/v1/issues/OWN-1/handoffs": [],
    [`/api/v1/issues/${tgt.issue}/summary`]: {
      documents: [{ id: tgt.document, slug: "spec", primary: true }],
    },
    [`/api/v1/artifacts/${tgt.document}/blocks`]: [{ id: tgt.block }],
    [`/api/v1/issues/${tgt.issue}/comments`]: [{ id: tgt.comment, author }],
    [`/api/v1/comments/${tgt.comment}/blocks`]: {
      item: { issue_key: tgt.issue, ref: `${scheme}${tgt.issue}/comment/${tgt.comment}` },
    },
    [`/api/v1/issues/${tgt.issue}/asks`]: [{ id: tgt.ask, author: agent }],
    [`/api/v1/issues/${tgt.issue}/messages`]: [
      { message: { id: tgt.message, author: { kind: "user", id: tgt.person } } },
    ],
    [`/api/v1/agents/${tgt.session}/messages`]: [
      { message: { id: tgt.directMessage, author, body: "A direct message to the worker." } },
    ],
    [`/api/v1/issues/${tgt.issue}/handoffs`]: [],
    [`/api/v1/issues/${handoffOn}/handoffs`]: fake.omit?.has("handoff")
      ? []
      : [{ id: tgt.handoff, phase: "plan", author: agent }],
    "/api/v1/projects/TGT/artifacts": [
      { id: tgt.projectDocument, issue_key: null, slug: "playbook-md" },
    ],
    "/api/v1/near-copy-checks?project=TGT": { checks: [{ id: tgt.nearCopy }] },
    "/api/v1/priorities": { priorities: [{ id: "tgt-work" }] },
    "/api/v1/broadcasts": [{ id: "99999999-9999-4999-8999-999999999999" }],
    "/api/v1/delivery/timeline": { runs: [{ id: 900 }] },
  };
  if (fake.conversationGone === true) {
    answers[`/api/v1/agents/${tgt.session}/artifacts/${tgt.conversationFile}`] = null;
  }
  return answers[target];
}

/** Whether `target`, decoded, names an object of TGT: one of its ids or keys, or the project key
 *  standing as a path segment, a query value or a reference's project. */
function namesTgt(target: string): boolean {
  return (
    tgtValues.some((value) => target.includes(value)) ||
    /(^|[/=])TGT([-/&?]|$)/.test(target) ||
    target.includes(`${scheme}TGT`)
  );
}

/** The member's own document, in OWN, which the document socket's control opens. */
const ownDocument = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";

/** Hocuspocus's answer to a token frame that admits the caller: the room, the authentication
 *  message (2), its authenticated subtype (2) and the scope (socket-access.ts reads it). */
function admitted(room: string): Uint8Array {
  const frame = createEncoder();
  writeVarString(frame, room);
  writeVarUint(frame, 2);
  writeVarUint(frame, 2);
  writeVarString(frame, "read-write");
  return toUint8Array(frame);
}

/** What a socket of the fake is: the member's or the full member's, and, for the document
 *  socket, the room. */
interface SocketData {
  readonly member: boolean;
  readonly room?: string;
}

/** One events subscription a socket holds, by its `sub`. */
interface Feed {
  readonly socket: ServerWebSocket<SocketData>;
  readonly sub: string;
}

/** The answer of the real proxy for a repository its caller may not see, or nobody holds. */
function notInstalledOn(repository: string): Response {
  return Response.json(
    {
      code: "GITHUB_TOKEN_UNAVAILABLE",
      error: `the GitHub App is not installed on ${repository}`,
    },
    { status: 503 }
  );
}

/** A fake Dispatch on a port of its own, with `leaks` planted for the restricted member. */
function fakeDispatch(leaks: readonly Leak[], fake: FakeShape) {
  const feed = fake.feed ?? {};
  const memberPaths: string[] = [];
  // Every request the full member's credential made: a runner must make none.
  let fullRequests = 0;
  const fullFeeds: Feed[] = [];
  const memberFeeds: Feed[] = [];
  // Every direct message written to the fake, by id: whose it is, and the session it went to.
  const directMessages = new Map<string, { readonly member: boolean; readonly session: string }>();
  let events = 0;
  const publish = (feeds: readonly Feed[], event: Record<string, unknown>) => {
    for (const { socket, sub } of feeds) {
      socket.send(JSON.stringify({ type: "event", sub, id: String(events), event }));
    }
  };
  const server = Bun.serve<SocketData>({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request, served) {
      const url = new URL(request.url);
      const target = decodeURIComponent(url.pathname + url.search);
      const member = request.headers.get("cookie") === memberCookie;
      const fullBrowser = request.headers.get("cookie") === fullCookie;
      const full = fullBrowser || request.headers.get("authorization") === `Bearer ${fullToken}`;
      if (full) fullRequests += 1;
      if (request.headers.get("upgrade") !== null) {
        if (member && feed.memberRefusal !== undefined && url.pathname === "/ws/live") {
          return Response.json(feed.memberRefusal, { status: 404 });
        }
        if (feed.live === true && url.pathname === "/ws/live" && (member || full)) {
          if (served.upgrade(request, { data: { member } })) return undefined;
        }
        if (feed.doc === true && member && url.pathname === `/ws/doc/${ownDocument}`) {
          if (served.upgrade(request, { data: { member, room: ownDocument } })) return undefined;
        }
        if (feed.doc === true && member && url.pathname === `/ws/doc/${tgt.document}`) {
          return feed.docRefusal === undefined
            ? new Response("document not found", { status: 404 })
            : Response.json(feed.docRefusal, { status: 404 });
        }
        return new Response("no socket here", { status: 404 });
      }
      if (!member && !full) return Response.json({ code: "NO_IDENTITY" }, { status: 401 });
      // Whether the App is installed on a repository, which the server answers a caller that may
      // not see the repository as for one the App is not installed on (the GitHub proxy's rule):
      // to the full member, installed on TGT's repository, unless `notInstalled`, and on the
      // shared one; to the member, who sees no repository of TGT's alone, on the shared one alone.
      // A leak planted at it answers the member otherwise.
      const installation = /^\/api\/v1\/github\/app-installation\/([^/]+\/[^/]+)$/.exec(target);
      const installed =
        installation !== null &&
        fake.notInstalled !== true &&
        (installation[1] === tgt.repository
          ? !member
          : fake.sharedRepo === true && installation[1] === sharedRepository);
      const proxied = /^\/api\/github\/rest\/repos\/([^/]+\/[^/]+)\//.exec(target);
      const direct = /^\/api\/v1\/agents\/([^/]+)\/messages$/.exec(url.pathname);
      const read = /^\/api\/v1\/messages\/([^/]+)(\/blocks)?$/.exec(url.pathname);
      if (request.method === "POST" && direct !== null) {
        events += 1;
        const id = crypto.randomUUID();
        const session = direct[1] ?? "";
        directMessages.set(id, { member, session });
        const event = {
          id: events,
          type: "message.created",
          payload: { id, target: `session:${session}` },
        };
        // The full member sees every conversation; the member, only their own.
        publish(fullFeeds, event);
        if (member ? feed.ownDmLost !== true : feed.dmLeak === true) publish(memberFeeds, event);
        return Response.json({ id, target: `session:${session}` }, { status: 201 });
      }
      if (read !== null && directMessages.has(read[1] ?? "")) {
        const written = directMessages.get(read[1] ?? "");
        if (member && written?.member !== true) {
          return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
        }
        return Response.json({ message: { id: read[1], target: `session:${written?.session}` } });
      }
      if (member) {
        memberPaths.push(url.pathname);
        if (target === "/api/v1/whoami") {
          return Response.json({
            kind: "user",
            login: memberLogin,
            member_kind: "restricted",
          });
        }
        if (target === "/api/v1/projects") return Response.json([ownProject(fake)]);
        if (feed.doc === true && target === "/api/v1/schema/blocks") {
          return Response.json({ version: 1 });
        }
        if (feed.doc === true && target === "/api/v1/projects/OWN/artifacts") {
          return Response.json([{ id: ownDocument, issue_key: null, slug: "guide-md" }]);
        }
        const leak = leaks.find((planted) => planted.matches(target));
        if (leak !== undefined) return Response.json(leak.body, { status: leak.status });
        if (target === "/api/v1") return Response.json(routeIndexOf(fake));
        if (installation !== null) return Response.json({ installed });
        if (proxied !== null) return notInstalledOn(proxied[1] ?? "");
        if (namesTgt(target) || missingValues.some((value) => target.includes(value))) {
          return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
        }
        return Response.json([]);
      }
      const planted = fake.full?.find((answer) => answer.matches(target));
      if (planted !== undefined) return Response.json(planted.body, { status: planted.status });
      if (installation !== null) return Response.json({ installed });
      if (request.method === "POST" && target === "/api/v1/going/requests") {
        return Response.json({ id: tgt.goingRun, project: "TGT" }, { status: 201 });
      }
      if (request.method === "POST" && target === `/api/v1/issues/${tgt.issue}/comments`) {
        events += 1;
        const id = crypto.randomUUID();
        // The noise leads its event, so a line that quotes the feed's first bytes quotes it.
        const event =
          feed.noise === undefined
            ? { id: events, issue_key: tgt.issue, project: "TGT", payload: { id } }
            : { body: feed.noise, id: events, issue_key: tgt.issue, project: "TGT" };
        publish(fullFeeds, event);
        if (feed.closes === true) {
          for (const { socket } of memberFeeds) socket.close(1011, "the feed went away");
        }
        return Response.json({ id }, { status: 201 });
      }
      if (proxied !== null) {
        const repository = proxied[1] ?? "";
        if ((fake.proxyReadsCookies === true && !fullBrowser) || repository.includes("no-such-")) {
          return notInstalledOn(repository);
        }
      }
      const answer = lookup(target, fake);
      if (answer === null) return Response.json({ code: "NOT_FOUND" }, { status: 404 });
      if (answer !== undefined) return Response.json(answer);
      if (missingValues.some((value) => target.includes(value))) {
        return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
      }
      return Response.json([{ project: "TGT", issue_key: tgt.issue, path: target }]);
    },
    websocket: {
      message(socket, raw) {
        if (socket.data.room !== undefined) {
          // The one frame the probe sends is its token frame; a fake admits the member to it,
          // unless it leaves the member's own room unanswered.
          if (feed.ownDocSilent !== true) socket.send(admitted(socket.data.room));
          return;
        }
        const frame = JSON.parse(String(raw)) as {
          type?: string;
          sub?: string;
          view?: { kind?: string; session_id?: string };
        };
        if (frame.type !== "subscribe" || frame.sub === undefined) return;
        const sub = frame.sub;
        const { member } = socket.data;
        if (frame.view?.kind === "agent") {
          const agent = (event: string, data: unknown) =>
            socket.send(JSON.stringify({ type: "agent", sub, event, data }));
          if (frame.view.session_id !== tgt.session) {
            agent("responder", { v: 1, responding: false });
            return;
          }
          agent("responder", { v: 1, responding: !member || feed.agentLeak === true });
          if (!member || feed.agentLeak === true)
            agent("replay", { v: 1, turns: ["the worker's turn"] });
          return;
        }
        if (member && feed.refuses === true) {
          socket.send(
            JSON.stringify({ type: "error", sub, code: "EVENTS_UNAVAILABLE", message: "refused" })
          );
          return;
        }
        socket.send(JSON.stringify({ type: "events_start", sub, since: String(events) }));
        if (member && feed.ends === true) {
          socket.send(JSON.stringify({ type: "ended", sub, reason: "behind" }));
          return;
        }
        (member ? memberFeeds : fullFeeds).push({ socket, sub });
      },
    },
  });
  return {
    memberPaths,
    fullRequests: () => fullRequests,
    origin: `http://127.0.0.1:${server.port}`,
    server,
  };
}

const servers: { stop: (closeActiveConnections?: boolean) => unknown }[] = [];
/** Stops every fake a file's rows started; each file runs it after its rows. */
export function stopFakes(): void {
  for (const server of servers) server.stop(true);
}

const indexes = temporaryDirectories("restricted-sweep-index");

/** The route index a run over the GET patterns `rows` reads, as `sweep` writes it. */
export function routesFileOf(rows: readonly string[]): string {
  return rows.map((pattern) => `GET ${pattern} any\n`).join("");
}

/** What a row changes of a run: the fake's answers (FakeShape), inputs (a variable set undefined
 *  is left out), and `rows`, the GET patterns of the route index the run reads in place of the
 *  whole of it. A sweep of the whole index makes about 400 calls; a row that holds one rule reads
 *  only the rows that rule needs. */
export interface RunShape extends FakeShape {
  readonly env?: Readonly<Record<string, string | undefined>>;
  /** The bound the run is stopped at: `short` for a run over the rows a rule needs, and
   *  `wholeIndexSweep` for the one run over the whole route index. */
  readonly bound?: BoundName;
}

/** What one run of the sweep answered, the paths of every request the member sent, how many
 *  requests the full member's credential made, and the fake's origin. */
export interface SweptRun {
  readonly code: number;
  readonly stdout: string;
  readonly memberPaths: readonly string[];
  readonly fullRequests: number;
  readonly origin: string;
}

/** One run of the sweep against a fake with `leaks` planted. */
export async function sweep(leaks: readonly Leak[], shape: RunShape = {}): Promise<SweptRun> {
  const fake = fakeDispatch(leaks, shape);
  servers.push(fake.server);
  let routes: string | undefined;
  if (shape.rows !== undefined) {
    routes = join(indexes(), "routes.txt");
    writeFileSync(routes, routesFileOf(shape.rows));
  }
  const inputs: Record<string, string | undefined> = {
    PATH: process.env.PATH ?? "",
    TMPDIR: tmpdir(),
    SWEEP_ORIGIN: fake.origin,
    SWEEP_PROJECT: "TGT",
    SWEEP_WRITE_ISSUE: tgt.issue,
    SWEEP_FULL_TOKEN: fullToken,
    SWEEP_RESTRICTED_COOKIE: memberCookie,
    SWEEP_MEMBER_SESSION: ownSession,
    SWEEP_LIVE_SECONDS: "1",
    SWEEP_CONVERSATION_FILE: `${tgt.session}/${tgt.conversationFile}`,
    SWEEP_PACE_SECONDS: "0",
    SWEEP_ROUTES: routes,
    ...shape.env,
  };
  const run = await runBounded({
    argv: ["bash", sweepScript],
    bound: shape.bound ?? "short",
    cwd: dispatchPackage,
    env: Object.fromEntries(
      Object.entries(inputs).filter((entry): entry is [string, string] => entry[1] !== undefined)
    ),
    from: import.meta.url,
  });
  fake.server.stop(true);
  return {
    code: run.code,
    fullRequests: fake.fullRequests(),
    memberPaths: fake.memberPaths,
    origin: fake.origin,
    stdout: run.stdout,
  };
}

/** The `FAIL    ` lines of a run, without that prefix. */
export function failures(stdout: string): string[] {
  return stdout
    .split("\n")
    .filter((line) => line.startsWith("FAIL    "))
    .map((line) => line.slice("FAIL    ".length));
}

/** A run's verdict: its last line. */
export function verdictOf(stdout: string): string {
  return stdout.trimEnd().split("\n").at(-1) ?? "";
}

/** The tools the sweep needs, without which a file's rows are skipped. */
export const withoutTools = skipWithout("bash", "curl", "jq", "bun");
