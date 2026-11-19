/** The redirect statuses whose `Location` fetch follows, each a case of a redirect row. */
export const REDIRECT_CODES = [301, 302, 303, 307, 308] as const;

/** One of {@link REDIRECT_CODES}. */
export type RedirectCode = (typeof REDIRECT_CODES)[number];

/** The name of every site's redirect row, in the test file beside the site. The row is one case
 * for each of {@link REDIRECT_CODES}, each named by {@link redirectCase}, so a row whose site
 * follows a redirect again fails at each code. `scripts/checks/redirect-refusals.ts` runs the rows
 * by this name. */
export const REDIRECT_ROW =
  "a redirect to another origin is refused naming its status, and that origin receives no request";

/** The name of the case of a redirect row that answers with `code`. */
export function redirectCase(code: RedirectCode): string {
  return `${REDIRECT_ROW} (${code})`;
}

/** A `Location` a refusal names no other origin for, and the words the refusal says where the
 * redirect pointed with (`@legion/envoy-client/redirect`). */
export interface UnnamedTarget {
  /** As {@link RedirectServers.location} takes it. */
  readonly location: string | null;
  readonly where: string;
  /** Whether the target is another path of the address the request went to, whose refusal says
   * what to correct in the path the client was given. */
  readonly sameAddress: boolean;
}

/** Each form of {@link UnnamedTarget}: another path of the address asked; an empty, unreadable,
 * non-http or missing `Location`, which names no target; and a host longer than any host name
 * (253 characters), whose origin is never printed. */
export const UNNAMED_TARGETS: readonly UnnamedTarget[] = [
  {
    location: "/another-path?q=1",
    where: "redirecting to another path of the same address",
    sameAddress: true,
  },
  { location: "", where: "naming no target", sameAddress: false },
  { location: "http://[not a host", where: "naming no target", sameAddress: false },
  { location: "ftp://files.example.com/x", where: "naming no target", sameAddress: false },
  { location: null, where: "naming no target", sameAddress: false },
  {
    location: `http://${"h".repeat(70_000)}.example/x`,
    where: "redirecting to another address, whose origin is 70015 characters long",
    sameAddress: false,
  },
];

/** A request the other server received: what reached the origin a redirect named. */
export interface ReceivedRequest {
  readonly method: string;
  readonly path: string;
  readonly headers: Readonly<Record<string, string>>;
  /** The body, once the server has read it whole; empty until then. */
  readonly body: string;
}

/** The two servers of {@link redirectServers}. */
export interface RedirectServers {
  /** The origin the client under test is pointed at, `http://127.0.0.1:<port>`. */
  readonly origin: string;
  /** The other server's origin: the same host on another port, so another origin. */
  readonly elsewhere: string;
  /** Every request the other server received, oldest first, each recorded as it arrives. */
  readonly received: readonly ReceivedRequest[];
  /** The status the redirects answer with, 307 until a test sets it. */
  code: RedirectCode;
  /** The `Location` the redirects name: undefined, until a test sets it, for the same path and
   * query at `elsewhere`; a string, sent as it is, relative or not; null for no `Location`. */
  location: string | null | undefined;
  /** Whether each redirect's body is one chunk followed by nothing, never ending, as from a
   * server that stalls in the middle of its answer; false, until a test sets it, for no body. */
  endlessBody: boolean;
  /** How many of those endless bodies the first server saw cancelled: a client that refuses a
   * redirect cancels the body it does not read, so a body that never ends holds no connection. */
  readonly cancelledBodies: number;
  /** Resolves once `holds` does, read again each time the other server records a request and each
   * time the first server sees a body cancelled: what a row awaits in place of a guessed wait. A
   * state that never comes leaves it pending, and the row's own limit fails the row. */
  until(holds: () => boolean): Promise<void>;
  stop(): void;
}

type Handler = (request: Request) => Response | Promise<Response>;

/**
 * Two servers on 127.0.0.1, each on a port the kernel chooses, for a test that a client which
 * carries a credential follows no redirect. The server at `origin` answers each request whose path
 * `redirects` selects (every request, unless given) with the status `code` holds and the
 * `Location` that `location` gives, and answers every other request with `direct`. The server at
 * `elsewhere` records each request as it arrives, headers included, and its body once read, and
 * answers it with `atElsewhere`, so a client that follows the redirect gets an answer it can take
 * as a success there. The test sets `code`, calls the client, and expects `received` to stay
 * empty.
 */
export function redirectServers(
  options: {
    readonly direct?: Handler;
    readonly redirects?: (path: string) => boolean;
    readonly atElsewhere?: Handler;
  } = {}
): RedirectServers {
  const received: ReceivedRequest[] = [];
  const waiters = new Set<{ readonly holds: () => boolean; readonly resolve: () => void }>();
  const changed = () => {
    for (const waiter of waiters) {
      if (waiter.holds()) {
        waiters.delete(waiter);
        waiter.resolve();
      }
    }
  };
  const atElsewhere = options.atElsewhere ?? (() => Response.json({}));
  const other = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    async fetch(request) {
      const url = new URL(request.url);
      // Recorded before its body is read, so a client that stops in the middle of sending it is
      // still seen to have reached this server.
      const record = {
        method: request.method,
        path: `${url.pathname}${url.search}`,
        headers: Object.fromEntries(request.headers),
        body: "",
      };
      received.push(record);
      changed();
      record.body = await request.text();
      return atElsewhere(request);
    },
  });
  const elsewhere = `http://127.0.0.1:${other.port}`;
  const direct = options.direct ?? (() => new Response("no direct answer", { status: 500 }));
  const redirects = options.redirects ?? (() => true);
  let code: RedirectCode = 307;
  let location: string | null | undefined;
  let endlessBody = false;
  let cancelledBodies = 0;
  const first = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    // No idle limit: Bun's own closes a connection 10 s after its last byte, and so would end an
    // endless body that no client cancelled, as a cancel would.
    idleTimeout: 0,
    fetch(request) {
      const url = new URL(request.url);
      if (!redirects(url.pathname)) return direct(request);
      const named = location === undefined ? `${elsewhere}${url.pathname}${url.search}` : location;
      const body = endlessBody
        ? new ReadableStream<Uint8Array>({
            start(controller) {
              controller.enqueue(new TextEncoder().encode("the first chunk of a body that "));
            },
            cancel() {
              cancelledBodies += 1;
              changed();
            },
          })
        : null;
      return new Response(body, {
        status: code,
        headers: named === null ? {} : { Location: named },
      });
    },
  });
  return {
    origin: `http://127.0.0.1:${first.port}`,
    elsewhere,
    received,
    get code() {
      return code;
    },
    set code(value) {
      code = value;
    },
    get location() {
      return location;
    },
    set location(value) {
      location = value;
    },
    get endlessBody() {
      return endlessBody;
    },
    set endlessBody(value) {
      endlessBody = value;
    },
    get cancelledBodies() {
      return cancelledBodies;
    },
    until(holds) {
      if (holds()) return Promise.resolve();
      const { promise, resolve } = Promise.withResolvers<void>();
      waiters.add({ holds, resolve });
      return promise;
    },
    stop() {
      first.stop(true);
      other.stop(true);
    },
  };
}
