// The TLS front door of e2e/served-addresses.e2e.ts: one listener on a loopback port the OS
// chooses, answering every host it is given with one throwaway certificate, and forwarding each
// request to one Dispatch server with the `Host` and `Origin` the browser sent, as a load balancer
// in front of a server that answers at several addresses does. The server picks the address a
// request is at by its `Host` alone, so a page the browser opened at https://b.example.test is at
// that address for the server too. Chromium reaches this listener by mapping each host to its port
// (`--host-resolver-rules`), so no address the page names carries a port. The relay is
// e2e/http-relay.ts, which the harness's two proxies forward through too.
//
//   bun e2e/served-hosts-proxy.ts <upstream port> <host>...
//
// It forwards to http://127.0.0.1:<upstream port> and, once it serves, prints
//   served-hosts proxy listening port=<port> upstream=http://127.0.0.1:<upstream port>
// on stdout, the line its caller reads the port from. It runs until it is stopped.
//
// The certificate is made with `openssl` in a directory of its own under the system's temporary
// directory, read, and the directory removed before the listener starts, so the key is held by this
// process alone. Nothing trusts it: the spec's browser context ignores HTTPS errors.
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  forwardedHeaders,
  isWebSocketUpgrade,
  type RelaySocket,
  relayHttp,
  relayWebSocket,
  requestBody,
  upstreamUrl,
} from "./http-relay";

const [upstreamPort, ...hosts] = process.argv.slice(2);
if (upstreamPort === undefined || !/^[1-9][0-9]{0,4}$/.test(upstreamPort) || hosts.length === 0) {
  console.error("usage: bun e2e/served-hosts-proxy.ts <upstream port> <host>...");
  process.exit(2);
}
const upstream = `http://127.0.0.1:${upstreamPort}`;

/** A certificate for `names` and its key, made by `openssl` in a directory removed once both are
 *  read. */
function throwawayCertificate(names: readonly string[]): { cert: string; key: string } {
  const dir = mkdtempSync(join(tmpdir(), "served-hosts-proxy-"));
  try {
    const made = Bun.spawnSync(
      [
        "openssl",
        "req",
        "-x509",
        "-newkey",
        "ec",
        "-pkeyopt",
        "ec_paramgen_curve:prime256v1",
        "-nodes",
        "-days",
        "1",
        "-subj",
        `/CN=${names[0]}`,
        "-addext",
        `subjectAltName=${names.map((name) => `DNS:${name}`).join(",")}`,
        "-keyout",
        join(dir, "key.pem"),
        "-out",
        join(dir, "cert.pem"),
      ],
      { stderr: "pipe", stdout: "ignore" }
    );
    if (made.exitCode !== 0) {
      throw new Error(`openssl made no certificate: ${made.stderr.toString().trim()}`);
    }
    return {
      cert: readFileSync(join(dir, "cert.pem"), "utf8"),
      key: readFileSync(join(dir, "key.pem"), "utf8"),
    };
  } finally {
    rmSync(dir, { force: true, recursive: true });
  }
}

const listener = Bun.serve<RelaySocket>({
  hostname: "127.0.0.1",
  // A request the server is slow to answer sends nothing while it waits, and Bun's default 10 s
  // idle timeout would cut it; the browser and the server keep their own deadlines.
  idleTimeout: 0,
  port: 0,
  tls: throwawayCertificate(hosts),
  async fetch(request, server) {
    const websocket = isWebSocketUpgrade(request);
    const url = upstreamUrl(request, upstream, websocket);
    const headers = forwardedHeaders(request.headers, websocket);
    const host = request.headers.get("host");
    if (host !== null) headers.set("host", host);
    if (websocket) {
      const upgraded = server.upgrade(request, {
        data: { headers: Object.fromEntries(headers), pending: [], url: url.href },
      });
      return upgraded ? undefined : new Response("WebSocket upgrade failed", { status: 400 });
    }
    return relayHttp(request, url, headers, await requestBody(request));
  },
  websocket: relayWebSocket(),
});

console.log(`served-hosts proxy listening port=${listener.port} upstream=${upstream}`);
