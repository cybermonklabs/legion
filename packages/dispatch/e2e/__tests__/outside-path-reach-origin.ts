/** The host of one fake Dispatch of the tests of `e2e/acceptance/outside-path-reach.sh`, a
 *  process beside the test process. `fakeDispatch` in `outside-path-reach-fixtures.ts` starts one,
 *  as `bun outside-path-reach-origin.ts`, each time it is called, and says why the fakes answer
 *  from a process of their own; `fakeHost` there says why each has a host of its own. It serves a
 *  control server on 127.0.0.1, on a port the kernel picks, and prints that port as its first
 *  line on stdout. The script never reads that port.
 *
 *  `POST /` with `{ health, projects, refusal }` starts the fake Dispatch, a `Bun.serve` on
 *  127.0.0.1 on a port the kernel picks, and answers `{ port }`; a second `POST /` is refused.
 *  That fake answers `/healthz` with the status `health` and the body `{ ok }`, true only when
 *  `health` is 200. It answers `/api/v1/projects` with 200 and an empty list when the request
 *  carries a credential (an `Authorization` or a `Cookie`), as Dispatch would. When the request
 *  carries none, it answers with the status `projects`: for 302, a redirect to a sign-in page; for
 *  200, an empty list; for any other status, `refusal` as the body. Every other path answers 404.
 *  Like any HTTP server, it holds a connection that sends nothing, so the script's probe of its
 *  port reads REACHED through curl's exit 28; it holds one for as long as its client waits
 *  (`idleTimeout` 0, where Bun's own 10 s would drop a read this host was kept from answering
 *  under load, before the script's own bound for it ends). It records every request it answers,
 *  and `GET /seen` answers that record, in the order it answered them.
 *
 *  The host exits once its stdin closes: the test process holds the other end of that pipe, so
 *  the host never outlives the test process. A row stops it by killing it, which closes the fake's
 *  connections. Imported, it serves nothing: `fakeDispatch` takes only the types `Answers` and
 *  `Seen` from it.
 */

import type { Server } from "bun";

/** What one fake Dispatch answers: `fakeDispatch`'s arguments. */
export interface Answers {
  health: number;
  projects: number;
  refusal: unknown;
}

/** One request a fake Dispatch answered. */
export interface Seen {
  method: string;
  path: string;
  credential: string | null;
}

/** The fake Dispatch the host serves, and its record of every request it answered. */
interface Fake {
  server: Server<undefined>;
  seen: Seen[];
}

/** Starts one fake Dispatch that answers as `answers` says. */
function serveFake({ health, projects, refusal }: Answers): Fake {
  const seen: Seen[] = [];
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    idleTimeout: 0,
    fetch(request) {
      const { pathname } = new URL(request.url);
      const credential = request.headers.get("authorization") ?? request.headers.get("cookie");
      seen.push({ method: request.method, path: pathname, credential });
      if (pathname === "/healthz") return Response.json({ ok: health === 200 }, { status: health });
      if (pathname === "/api/v1/projects") {
        const status = credential === null ? projects : 200;
        if (status === 302) return Response.redirect("https://accounts.example/signin", 302);
        return Response.json(status === 200 ? [] : refusal, { status });
      }
      return Response.json({ code: "NOT_FOUND" }, { status: 404 });
    },
  });
  return { server, seen };
}

if (import.meta.main) {
  /** The one fake this host serves, once `POST /` has started it. */
  let fake: Fake | undefined;
  const control = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    routes: {
      "/": {
        async POST(request) {
          const answers = (await request.json()) as Answers;
          if (fake !== undefined) {
            return Response.json({ code: "ALREADY_STARTED" }, { status: 409 });
          }
          fake = serveFake(answers);
          return Response.json({ port: Number(fake.server.url.port) });
        },
      },
      "/seen": {
        GET() {
          return fake === undefined
            ? Response.json({ code: "NOT_STARTED" }, { status: 404 })
            : Response.json(fake.seen);
        },
      },
    },
  });
  console.log(control.port);
  // The test process writes nothing here: this only waits for the pipe to close.
  await Bun.stdin.text();
  process.exit(0);
}
