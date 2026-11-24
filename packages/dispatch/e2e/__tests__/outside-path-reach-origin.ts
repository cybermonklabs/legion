/** The host of one fake Dispatch of the tests of `e2e/acceptance/outside-path-reach.sh`, a
 *  process beside the test process. `fakeDispatch` in `outside-path-reach-fixtures.ts` starts one,
 *  as `bun outside-path-reach-origin.ts`, each time it is called, and says why the fakes answer
 *  from a process of their own; `fakeHost` there says why each has a host of its own. Its control
 *  server, which the script never reads, is `serveFakeHost` of `fake-host-server.ts`.
 *
 *  `POST /` with `{ health, projects, refusal }` starts the fake Dispatch, a `Bun.serve` on
 *  127.0.0.1 on a port the kernel picks. That fake answers `/healthz` with the status `health` and
 *  the body `{ ok }`, true only when `health` is 200. It answers `/api/v1/projects` with 200 and an
 *  empty list when the request carries a credential (an `Authorization` or a `Cookie`), as
 *  Dispatch would. When the request carries none, it answers with the status `projects`: for 302,
 *  a redirect to a sign-in page; for 200, an empty list; for any other status, `refusal` as the
 *  body. Every other path answers 404. Like any HTTP server, it holds a connection that sends
 *  nothing, so the script's probe of its port reads REACHED through curl's exit 28; it holds one
 *  for as long as its client waits (`idleTimeout` 0, where Bun's own 10 s would drop a read this
 *  host was kept from answering under load, before the script's own bound for it ends). It records
 *  every request it answers, which `GET /seen` answers.
 *
 *  Imported, it serves nothing: `fakeDispatch` takes only the types `Answers` and `Seen` from it.
 */

import { type Serving, serveFakeHost } from "./fake-host-server";

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

/** Starts one fake Dispatch that answers as `answers` says. */
function serveFake({ health, projects, refusal }: Answers): Serving<Seen> {
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
  return { port: Number(server.url.port), seen };
}

if (import.meta.main) await serveFakeHost(serveFake);
