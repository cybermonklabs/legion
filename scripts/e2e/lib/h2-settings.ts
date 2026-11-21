// Whether a Dispatch front proxy advertises HTTP/2 extended CONNECT, which the live socket's design
// rests on it not doing (dispatch://INFRA-258).
//
//   bun scripts/e2e/lib/h2-settings.ts <https-url>
//
// It offers `h2` and `http/1.1` over TLS, as a browser does, and prints one JSON line: the URL, the
// protocol the server chose (`alpn`), and, when that is `h2`, the SETTINGS the server sent as Node's
// HTTP/2 client reads them (`settings`). A server advertising SETTINGS_ENABLE_CONNECT_PROTOCOL
// lets a browser open the live socket as a stream of a shared HTTP/2 connection rather than a
// connection of its own (RFC 8441), and the never-ending request INFRA-258 fixed could come back:
// exit 1. One that does not, or that speaks no HTTP/2 at all: exit 0. A usage error, or a server
// the probe could not read the answer from: exit 2.
//
// Certificates are verified, so a proxy behind a private CA needs `NODE_EXTRA_CA_CERTS`.
import http2 from "node:http2";
import { isIP } from "node:net";
import tls from "node:tls";
// The exit comes once what the probe printed has reached the pipe (`exitProcess`): an operator
// reads it as it comes and h2-settings.test.ts reads it whole, so nothing bounds the flush.
import { exitProcess } from "../../../packages/envoy-client/src/process-exit";

const USAGE = "usage: bun scripts/e2e/lib/h2-settings.ts <https-url>";
/** How long the TLS handshake and the server's first SETTINGS may take together. */
const DEADLINE_MS = 10_000;

/** What the probe prints. */
interface Answer {
  readonly url: string;
  /** The protocol the server chose from the two offered, false when it chose none. */
  readonly alpn: string | false;
  /** The server's SETTINGS, null when it chose no HTTP/2. */
  readonly settings: http2.Settings | null;
}

/** Why the probe read no answer from the server: exit 2. */
interface NoAnswer {
  readonly refused: string;
}

/** Writes `message` on stderr and gives the status to exit with, `code`. */
function refuse(message: string, code: number): number {
  process.stderr.write(`h2-settings: ${message}\n`);
  return code;
}

/** The https URL the one argument names, or the usage error to print. */
function target(argv: readonly string[]): URL | string {
  const [raw, ...extra] = argv;
  if (raw === undefined || extra.length > 0) return USAGE;
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    return `not a URL: ${raw}\n${USAGE}`;
  }
  if (parsed.protocol !== "https:") return `not an https URL: ${raw}\n${USAGE}`;
  return parsed;
}

/** Reads the server the arguments name, prints what it read, and gives the status to exit with. */
async function probe(argv: readonly string[]): Promise<number> {
  const url = target(argv);
  if (typeof url === "string") return refuse(url, 2);
  const host = url.hostname.replace(/^\[(.*)\]$/u, "$1");

  // The first of the server's answer, the deadline and a connection error settles the read; what
  // comes after it is dropped.
  const answer = Promise.withResolvers<Answer | NoAnswer>();
  // The line the deadline prints is the probe's own record of its wait: the delay the timer was
  // armed with and how long after arming it fired, read from this process's monotonic clock in
  // whole milliseconds. Only this timer writes that part, so a reader can tell the deadline's own
  // exit from any other path that might say the same first words.
  const armedAt = performance.now();
  const deadline = setTimeout(() => {
    const fired = Math.floor(performance.now() - armedAt);
    answer.resolve({
      refused: `${url.origin} sent no SETTINGS within ${DEADLINE_MS / 1_000}s (the deadline timer, armed for ${DEADLINE_MS} ms, fired ${fired} ms after it was armed)`,
    });
  }, DEADLINE_MS);
  const socket = tls.connect({
    ALPNProtocols: ["h2", "http/1.1"],
    host,
    port: url.port === "" ? 443 : Number(url.port),
    // SNI takes a host name, never an address.
    ...(isIP(host) === 0 ? { servername: host } : {}),
  });
  socket.on("error", (error) => answer.resolve({ refused: `${url.origin}: ${error.message}` }));
  socket.once("secureConnect", () => {
    const alpn = socket.alpnProtocol ?? false;
    if (alpn !== "h2") {
      socket.destroy();
      answer.resolve({ alpn, settings: null, url: url.href });
      return;
    }
    // The session takes over the socket the handshake chose h2 on.
    const session = http2.connect(url, { createConnection: () => socket });
    session.on("error", (error) => answer.resolve({ refused: `${url.origin}: ${error.message}` }));
    session.once("remoteSettings", (settings) => {
      session.close();
      answer.resolve({ alpn, settings, url: url.href });
    });
  });

  const read = await answer.promise;
  clearTimeout(deadline);
  if ("refused" in read) {
    socket.destroy();
    return refuse(read.refused, 2);
  }
  process.stdout.write(`${JSON.stringify(read)}\n`);
  if (read.settings?.enableConnectProtocol === true) {
    return refuse(
      `${url.origin} advertises SETTINGS_ENABLE_CONNECT_PROTOCOL: a browser would run Dispatch's live socket over HTTP/2, where the INFRA-258 hang can return`,
      1
    );
  }
  return 0;
}

await exitProcess(await probe(process.argv.slice(2)));
