import { afterAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import { join } from "node:path";
import { HANG_GUARD_MS } from "@legion/test-fixtures/hang-guard";

/**
 * Where the live event probe asks Dispatch: `GET /auth/whoami` and the `/ws/live` socket, each
 * joined onto `--url`'s path with its query kept, against a stand-in Dispatch served under a path
 * and a query that answers 404 anywhere else, as a Dispatch behind a path-routed gateway would.
 */

// The rows wait on a process and a local server, which a loaded machine slows. Their bound is a
// hang guard, not a verdict.
setDefaultTimeout(HANG_GUARD_MS);

const probe = join(import.meta.dir, "dispatch-live-events.ts");
const USER = "alice@example.test";

/** Every request the stand-in took, the socket's upgrade included, as `<method> <path><query>`. */
const asked: string[] = [];
/** Where the stand-in is served: under `mount`, with `query` on every request. */
let served = { mount: "", query: "" };

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  fetch(request, srv) {
    const url = new URL(request.url);
    asked.push(`${request.method} ${url.pathname}${url.search}`);
    if (url.search === served.query && url.pathname === `${served.mount}/auth/whoami`) {
      return Response.json({ event_head: "41", kind: "user", login: USER });
    }
    if (url.search === served.query && url.pathname === `${served.mount}/ws/live`) {
      return srv.upgrade(request) ? undefined : new Response("upgrade failed", { status: 400 });
    }
    return new Response("not found", { status: 404 });
  },
  websocket: {
    message(socket) {
      socket.send(JSON.stringify({ since: "41", sub: "events", type: "events_start" }));
      socket.send(
        JSON.stringify({
          event: { id: 42, note: "the marker", type: "comment.created" },
          id: "42",
          sub: "events",
          type: "event",
        })
      );
    },
  },
});
const origin = `http://127.0.0.1:${server.port}`;

afterAll(() => server.stop(true));

/** One run of the probe from whoami against `url`, the stand-in served under `mount` and `query`:
 *  its exit code, what it printed on stderr, and the requests the stand-in took. */
async function askedBy(url: string, mount: string, query: string) {
  served = { mount, query };
  asked.length = 0;
  const child = Bun.spawn(
    [
      process.execPath,
      probe,
      "--url",
      url,
      "--user",
      USER,
      "--from-whoami",
      "--until",
      "the marker",
      "--timeout",
      "5",
    ],
    { stderr: "pipe", stdout: "pipe" }
  );
  const [code, stderr] = await Promise.all([
    child.exited,
    new Response(child.stderr).text(),
    new Response(child.stdout).text(),
  ]);
  return { asked: [...asked], code, stderr };
}

describe("dispatch-live-events' requests", () => {
  test("join whoami and the live socket onto --url's path and keep its query", async () => {
    const { asked, code, stderr } = await askedBy(
      `${origin}/prefix?key=k3y-made-up`,
      "/prefix",
      "?key=k3y-made-up"
    );
    expect(asked).toEqual([
      "GET /prefix/auth/whoami?key=k3y-made-up",
      "GET /prefix/ws/live?key=k3y-made-up",
    ]);
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
  });

  test("ask a plain origin at each route itself", async () => {
    const { asked, code, stderr } = await askedBy(origin, "", "");
    expect(asked).toEqual(["GET /auth/whoami", "GET /ws/live"]);
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
  });
});
