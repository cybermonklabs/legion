/** The host of one stand-in edge of the tests of `e2e/acceptance/outside-edge-refusals.sh`, a
 *  process beside the test process. `fakeHost` in `outside-path-reach-fixtures.ts` starts one, as
 *  `bun outside-edge-refusals-edge.ts`, for each stand-in a row asks for, and says why each
 *  stand-in has a host of its own. Its control server, which the script never reads, is
 *  `serveFakeHost` of `fake-host-server.ts`.
 *
 *  `POST /` with `{ routes, notFound }` starts the stand-in edge on 127.0.0.1, on a port the kernel
 *  picks. The stand-in routes a request as the load balancer's HTTP routes would: by the first
 *  route whose `host` is the request's Host header, whose `method`, where the route names one, is
 *  the request's method, and whose `path` is the request's path, or a prefix of it for a `prefix`
 *  route. It answers with that route's `status` and `body`, and with
 *  `X-Outside-Edge-Backend: <marker>` where the route names a marker, as each production route sets
 *  it. A request no route takes is answered 404 with `notFound` as its body and no marker, as the
 *  Gateway's own 404 is. It records every request it answers, which `GET /seen` answers.
 *
 *  The path a route matches is the request's path as it came on the wire, up to any `?`: curl
 *  sends a path such as `/v1/%2e%2e/healthz` as written, and a Gateway route names a path as
 *  written too. So the stand-in reads raw HTTP/1.1 from its socket (`node:net`) rather than through
 *  `Bun.serve`, whose `request.url` has already decoded `%2e%2e` and removed the dot segment it
 *  makes (`/v1/%2e%2e/healthz` reads `/healthz` there), so a planted route could be matched, or
 *  missed, for a path no probe sent. Each answer closes its connection, as the script makes one
 *  request on each.
 *
 *  Imported, it serves nothing: the tests take only the types `Route`, `Edge` and `Seen` from it.
 */

import { type AddressInfo, createServer, type Socket } from "node:net";
import { type Serving, serveFakeHost } from "./fake-host-server";

/** One route of a stand-in edge. */
export interface Route {
  host: string;
  /** The one method the route takes; every method where it names none. */
  method?: string;
  /** The path as a request writes it, encoded bytes and all. */
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
  /** The path as it came on the wire, up to any `?`, never decoded. */
  path: string;
  contentLength: string | null;
}

/** The end of an HTTP/1.1 request's head. */
const headEnd = Buffer.from("\r\n\r\n");

/** The answer of `routes` to `request`, the head of a request read whole. */
function route(
  { routes, notFound }: Edge,
  request: Seen
): Pick<Route, "status" | "marker" | "body"> {
  const taken = routes.find(
    (each) =>
      each.host === request.host &&
      (each.method === undefined || each.method === request.method) &&
      (each.prefix === true ? request.path.startsWith(each.path) : request.path === each.path)
  );
  return taken ?? { status: 404, body: notFound };
}

/** Reads one request from `socket`, records it in `seen` and answers it as `edge` routes it. */
function answerOn(socket: Socket, edge: Edge, seen: Seen[]): void {
  let read = Buffer.alloc(0);
  let answered = false;
  // A connection the client resets is no error of the stand-in's.
  socket.on("error", () => {});
  socket.on("data", (chunk: Buffer) => {
    if (answered) return;
    read = Buffer.concat([read, chunk]);
    const end = read.indexOf(headEnd);
    if (end < 0) return;
    const [requestLine = "", ...lines] = read.subarray(0, end).toString("latin1").split("\r\n");
    const headers = new Map(
      lines.map((line) => {
        const colon = line.indexOf(":");
        return [line.slice(0, colon).trim().toLowerCase(), line.slice(colon + 1).trim()] as const;
      })
    );
    const contentLength = headers.get("content-length") ?? null;
    // The body, which the script's probes send empty, is read whole before the answer.
    if (read.length < end + headEnd.length + Number(contentLength ?? 0)) return;
    answered = true;
    const [method = "", target = ""] = requestLine.split(" ");
    const request: Seen = {
      method,
      host: headers.get("host") ?? null,
      path: target.split("?")[0] ?? "",
      contentLength,
    };
    seen.push(request);
    const { status, marker, body } = route(edge, request);
    const content = Buffer.from(body);
    const head = [
      `HTTP/1.1 ${status} Stand-in`,
      ...(marker === undefined ? [] : [`X-Outside-Edge-Backend: ${marker}`]),
      `Content-Length: ${content.length}`,
      "Connection: close",
      "",
      "",
    ].join("\r\n");
    socket.end(Buffer.concat([Buffer.from(head, "latin1"), content]));
  });
}

/** Starts one stand-in edge that routes as `edge` says. Like any server of these tests it sets no
 *  idle timeout of its own, which would drop a request this host was kept from answering under
 *  load. */
async function serveEdge(edge: Edge): Promise<Serving<Seen>> {
  const seen: Seen[] = [];
  const server = createServer((socket) => answerOn(socket, edge, seen));
  const listening = Promise.withResolvers<void>();
  server.listen(0, "127.0.0.1", listening.resolve);
  await listening.promise;
  return { port: (server.address() as AddressInfo).port, seen };
}

if (import.meta.main) await serveFakeHost(serveEdge);
