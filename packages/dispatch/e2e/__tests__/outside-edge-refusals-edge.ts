/** The host of one stand-in edge of `outside-edge-refusals.test.ts`, a process beside the test
 *  process. `fakeHost` in `outside-path-reach-fixtures.ts` starts one, as
 *  `bun outside-edge-refusals-edge.ts`, for each stand-in a row asks for, and says why each
 *  stand-in has a host of its own. It serves a control server on 127.0.0.1, on a port the kernel
 *  picks, and prints that port as its first line on stdout. The script never reads that port.
 *
 *  `POST /` with `{ routes, notFound }` starts the stand-in edge, a `Bun.serve` on 127.0.0.1 on a
 *  port the kernel picks, and answers `{ port }`; a second `POST /` is refused. The stand-in routes
 *  a request as the load balancer's HTTP routes would: by the first route whose `host` is the
 *  request's Host header, whose `method`, where the route names one, is the request's method, and
 *  whose `path` is the request's path, or a prefix of it for a `prefix` route. It answers with
 *  that route's `status` and `body`, and with `X-Outside-Edge-Backend: <marker>` where the route
 *  names a marker, as each production route sets it. A request no route takes is answered 404
 *  with `notFound` as its body and no marker, as the Gateway's own 404 is. It records every
 *  request it answers, and `GET /seen` answers that record, in the order it answered them.
 *
 *  The host exits once its stdin closes: the test process holds the other end of that pipe, so
 *  the host never outlives the test process. A row stops it by killing it, which closes the
 *  stand-in's connections. Imported, it serves nothing: the test file takes only the types
 *  `Route`, `Edge` and `Seen` from it.
 */

import type { Server } from "bun";

/** One route of a stand-in edge. */
export interface Route {
  host: string;
  /** The one method the route takes; every method where it names none. */
  method?: string;
  path: string;
  /** Whether `path` is a prefix of the paths the route takes rather than the one path. */
  prefix?: boolean;
  status: number;
  /** The value of `X-Outside-Edge-Backend` on the route's answers; none where it is absent. */
  marker?: string;
  body: string;
}

/** What one stand-in edge answers: the body `POST /` takes. */
export interface Edge {
  routes: Route[];
  /** The body of the 404 a request no route takes is answered with. */
  notFound: string;
}

/** One request a stand-in edge answered. */
export interface Seen {
  method: string;
  host: string | null;
  path: string;
  contentLength: string | null;
}

/** The stand-in edge the host serves, and its record of every request it answered. */
interface Standing {
  server: Server<undefined>;
  seen: Seen[];
}

/** Starts one stand-in edge that routes as `edge` says. */
function serveEdge({ routes, notFound }: Edge): Standing {
  const seen: Seen[] = [];
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    // As the fake Dispatch of `outside-path-reach-origin.ts` does: no idle timeout of Bun's own,
    // which would drop a request this host was kept from answering under load.
    idleTimeout: 0,
    fetch(request) {
      const { pathname } = new URL(request.url);
      const host = request.headers.get("host");
      seen.push({
        method: request.method,
        host,
        path: pathname,
        contentLength: request.headers.get("content-length"),
      });
      const route = routes.find(
        (each) =>
          each.host === host &&
          (each.method === undefined || each.method === request.method) &&
          (each.prefix === true ? pathname.startsWith(each.path) : pathname === each.path)
      );
      if (route === undefined) return new Response(notFound, { status: 404 });
      return new Response(route.body, {
        status: route.status,
        headers: route.marker === undefined ? {} : { "X-Outside-Edge-Backend": route.marker },
      });
    },
  });
  return { server, seen };
}

if (import.meta.main) {
  /** The one stand-in this host serves, once `POST /` has started it. */
  let edge: Standing | undefined;
  const control = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    routes: {
      "/": {
        async POST(request) {
          const config = (await request.json()) as Edge;
          if (edge !== undefined) {
            return Response.json({ code: "ALREADY_STARTED" }, { status: 409 });
          }
          edge = serveEdge(config);
          return Response.json({ port: Number(edge.server.url.port) });
        },
      },
      "/seen": {
        GET() {
          return edge === undefined
            ? Response.json({ code: "NOT_STARTED" }, { status: 404 })
            : Response.json(edge.seen);
        },
      },
    },
  });
  console.log(control.port);
  // The test process writes nothing here: this only waits for the pipe to close.
  await Bun.stdin.text();
  process.exit(0);
}
