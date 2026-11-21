import type { Socket } from "bun";

/**
 * What the stub proxy answers one CONNECT: a refusal with `status`, `reason` and the airlock's
 * `code`, as the secrets broker's airlock proxy refuses a tunnel (a 407 for a proxy password that
 * names no live grant, a 503 for a grant it could not read), or `open-then-close`: the tunnel is
 * opened, and closed at the first byte the client sends through it, as a connection that breaks
 * after the client may have sent its request.
 */
export type StubProxyAnswer =
  | { readonly status: number; readonly reason: string; readonly code: string }
  | "open-then-close";

/** A loopback HTTP proxy that answers each CONNECT as it is told, and counts them. */
export interface StubProxy {
  /** `http://127.0.0.1:<port>`, the proxy's URL, for `HTTPS_PROXY` or fetch's `proxy`. */
  readonly url: string;
  /** The target of each CONNECT so far, oldest first, such as `dispatch.test:443`. */
  readonly connects: readonly string[];
  stop(): void;
}

/** One client connection: the bytes of its CONNECT read so far, and whether a tunnel is open. */
interface Link {
  head: string;
  tunnel: boolean;
}

/**
 * Starts the stub proxy on a free 127.0.0.1 port. `answer(n)` decides the nth CONNECT, from 1.
 * The refusal carries the headers the airlock writes on its own answers (`Airlock-Forwarded:
 * false`, `Airlock-Code`) and a text body, though a client of the CONNECT reads its status line
 * alone. A request that is not a CONNECT is answered 405, and nothing is ever forwarded.
 */
export function startStubProxy(answer: (connect: number) => StubProxyAnswer): StubProxy {
  const connects: string[] = [];
  const answerConnect = (socket: Socket<Link>, target: string): void => {
    connects.push(target);
    const given = answer(connects.length);
    if (given === "open-then-close") {
      socket.data.tunnel = true;
      socket.write("HTTP/1.1 200 Connection Established\r\n\r\n");
      return;
    }
    const body = `airlock: ${given.code}: the stub proxy refuses this tunnel`;
    socket.end(
      `HTTP/1.1 ${given.status} ${given.reason}\r\nAirlock-Forwarded: false\r\n` +
        `Airlock-Code: ${given.code}\r\nContent-Type: text/plain\r\n` +
        `Content-Length: ${Buffer.byteLength(body)}\r\nConnection: close\r\n\r\n${body}`
    );
  };
  const listener = Bun.listen<Link>({
    hostname: "127.0.0.1",
    port: 0,
    socket: {
      open(socket) {
        socket.data = { head: "", tunnel: false };
      },
      data(socket, chunk) {
        if (socket.data.tunnel) {
          socket.end();
          return;
        }
        socket.data.head += chunk.toString("latin1");
        const end = socket.data.head.indexOf("\r\n\r\n");
        if (end === -1) return;
        const [method = "", target = ""] = (socket.data.head.split("\r\n")[0] ?? "").split(" ");
        if (method === "CONNECT") {
          answerConnect(socket, target);
          return;
        }
        socket.end(
          "HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
        );
      },
    },
  });
  return { url: `http://127.0.0.1:${listener.port}`, connects, stop: () => listener.stop(true) };
}
