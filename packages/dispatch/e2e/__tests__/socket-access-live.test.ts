/** How `e2e/acceptance/socket-access.ts live` decides it has read what the event feed sends a
 *  caller of the driver's writes. The feed sends an event only once the server's event horizon has
 *  passed it, and any transaction open on the Postgres instance holds the horizon back, so on a
 *  busy instance the feed can be silent for seconds after the writes have committed. A probe that
 *  took a silence as the end printed no event where the twin's printed one (seen on CI), so the
 *  probe reads what GET /api/v1/events/log lets the caller read, by the feed's own rule and capped
 *  at the same horizon, and waits until the feed has sent every event of it.
 *
 *  Each row starts a stand-in Dispatch: its `/ws/live` takes the probe's subscription and answers
 *  its `events_start`, and its `/api/v1/events/log` answers the caller's log. A row holds the
 *  horizon as an open transaction does: the log answers without the driver's comment and the feed
 *  sends nothing, until the stand-in has answered the log once while held, which only a probe that
 *  waits on the log ever asks. Then the log lists the comment, and the feed sends its events only
 *  once the log has said so, as the relay publishes behind the horizon the log reads. So no timer
 *  in a row decides what the probe prints: a probe that ends on a silence, or once the log has
 *  answered, ends before the feed has sent anything and prints no event. */

import { afterAll, beforeAll, expect, test } from "bun:test";
import { existsSync, renameSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Server, ServerWebSocket } from "bun";
import { eventually, rowLimit, runBounded } from "./bounded-subprocess";

const probe = fileURLToPath(new URL("../acceptance/socket-access.ts", import.meta.url));
const scratch = temporaryDirectories("socket-access-live");

/** The id the stand-in's feed starts after; the event of a comment the caller may not read, which
 *  a leaking feed sends; and the event of the driver's last comment, the one the caller may read,
 *  written after it and so above it. */
const since = "10";
const leakEvent = { id: 11, type: "comment.created", payload: { id: "hidden-comment" } };
const commentEvent = { id: 12, type: "comment.created", payload: { id: "comment-1" } };

/** What one row's stand-in holds: whether the horizon is held below the comments' events, how
 *  often the log was read, the sockets the probe opened, whether the comment is the caller's to
 *  read at all, and whether the feed leaks the hidden comment's event. */
interface Stand {
  held: boolean;
  logReads: number;
  readable: boolean;
  leaks: boolean;
  readonly sockets: Set<ServerWebSocket<undefined>>;
}

let stand: Stand = { held: false, logReads: 0, readable: true, leaks: false, sockets: new Set() };
let server: Server<undefined> | undefined;

/** The feed's events, sent in id order as the relay sends them once the horizon passes them: the
 *  hidden comment's event where the feed leaks it and the caller's comment's event where the
 *  caller may read it. The log never lists the hidden one. */
function publish(): void {
  const sent = [...(stand.leaks ? [leakEvent] : []), ...(stand.readable ? [commentEvent] : [])];
  for (const socket of stand.sockets) {
    for (const event of sent) {
      socket.send(JSON.stringify({ type: "event", sub: "e", id: String(event.id), event }));
    }
  }
}

beforeAll(() => {
  server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request, served) {
      const url = new URL(request.url);
      if (url.pathname === "/ws/live") {
        if (served.upgrade(request)) return undefined;
        return new Response("not a websocket", { status: 400 });
      }
      if (url.pathname === "/api/v1/events/log") {
        stand.logReads++;
        const after = Number(url.searchParams.get("after"));
        const listed =
          stand.held || !stand.readable || commentEvent.id <= after ? [] : [commentEvent];
        // The hold is lifted once a read is answered without the comment, and the feed sends its
        // events once a read has listed it, on a later turn of this server's loop than the one
        // that writes that read's answer: a zero-delay turn orders the two writes and waits for
        // nothing, and the probe under test prints the same whatever comes between them.
        if (stand.held) stand.held = false;
        else if (listed.length > 0) setTimeout(publish, 0);
        return Response.json(listed);
      }
      return new Response("not found", { status: 404 });
    },
    websocket: {
      open(socket) {
        stand.sockets.add(socket);
      },
      message(socket, message) {
        const frame = JSON.parse(String(message)) as { type?: string; sub?: string };
        if (frame.type === "subscribe" && frame.sub === "e") {
          socket.send(
            JSON.stringify({ type: "events_start", sub: "e", since, replay_complete: false })
          );
        }
      },
      close(socket) {
        stand.sockets.delete(socket);
      },
    },
  });
});

afterAll(() => {
  void server?.stop(true);
});

/** Runs the probe as a caller against the stand-in as it is set, and, once it is ready, writes
 *  the written file naming `comments` as the driver does once its writes have committed. */
async function live(comments: readonly string[]) {
  const work = scratch();
  const header = join(work, "header");
  const ready = join(work, "ready");
  const written = join(work, "written");
  writeFileSync(header, "Cookie: dsession=carols-session\n");
  const run = runBounded({
    argv: ["bun", probe, "live", `http://127.0.0.1:${server?.port}`, header, ready, written],
    bound: "short",
    from: import.meta.url,
  });
  await eventually("the probe's ready file", () => existsSync(ready));
  writeFileSync(`${written}.part`, comments.map((comment) => `${comment}\n`).join(""));
  renameSync(`${written}.part`, written);
  return await run;
}

test(
  "a feed held back by an open transaction is waited out, and the comment's event is printed",
  async () => {
    stand = { held: true, logReads: 0, readable: true, leaks: false, sockets: new Set() };
    const run = await live([commentEvent.payload.id]);
    expect(run.stderr).toBe("");
    expect(run.code).toBe(0);
    // The probe printed the comment's event, which the feed sent only once the hold was lifted:
    // a probe that ended on the silence printed the events_start alone.
    expect(run.stdout.trim().split("\n")).toEqual([
      JSON.stringify({ type: "events_start", sub: "e", since, replay_complete: false }),
      JSON.stringify({
        type: "event",
        sub: "e",
        id: "<event>",
        event: { ...commentEvent, id: "<event>" },
      }),
    ]);
    // The log was read while held and again once the hold was lifted.
    expect(stand.logReads).toBeGreaterThanOrEqual(2);
  },
  rowLimit("short")
);

test(
  "a hidden event the feed leaks late, before the comment the probe waits for, is printed",
  async () => {
    // The driver writes the caller's comment last, so it stands above every event the writes
    // before it made: the feed sends in id order, so a leak of one of those has come by the time
    // the comment's event does, however late both are. The compare then sees the leak.
    stand = { held: true, logReads: 0, readable: true, leaks: true, sockets: new Set() };
    const run = await live([commentEvent.payload.id]);
    expect(run.stderr).toBe("");
    expect(run.code).toBe(0);
    expect(run.stdout.trim().split("\n")).toEqual([
      JSON.stringify({ type: "events_start", sub: "e", since, replay_complete: false }),
      JSON.stringify({
        type: "event",
        sub: "e",
        id: "<event>",
        event: { ...leakEvent, id: "<event>" },
      }),
      JSON.stringify({
        type: "event",
        sub: "e",
        id: "<event>",
        event: { ...commentEvent, id: "<event>" },
      }),
    ]);
  },
  rowLimit("short")
);

test(
  "a caller the log owes nothing is answered its events_start alone, once the log has said so",
  async () => {
    // A caller that may read none of the driver's writes has no comment of them to wait for, so
    // its probe ends once the log says it owes nothing: a leak the feed sent it later still is
    // not waited for. The walk names no comment for a caller only where it sees no project.
    stand = { held: false, logReads: 0, readable: false, leaks: false, sockets: new Set() };
    const run = await live([]);
    expect(run.stderr).toBe("");
    expect(run.code).toBe(0);
    expect(stand.logReads).toBe(1);
    expect(run.stdout.trim()).toBe(
      JSON.stringify({ type: "events_start", sub: "e", since, replay_complete: false })
    );
  },
  rowLimit("short")
);
