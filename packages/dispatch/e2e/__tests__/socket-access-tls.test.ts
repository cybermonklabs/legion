/** `e2e/acceptance/socket-access.ts` over TLS: an `https://` origin, which is how
 *  `e2e/acceptance/restricted-sweep.sh` reaches a deployment. The probe asks for the upgrade over a
 *  connection of its own, and a request written before the TLS handshake ends is never sent: the
 *  server waits and the probe reads "closed with no answer", so both socket checks of a deployment
 *  run would be UNKNOWN. A server of this file's own answers every upgrade 404, on a certificate
 *  made for the run, which the probe trusts through NODE_EXTRA_CA_CERTS; the control is the same
 *  run without it, which the probe must refuse, so the 404 read is the server's, over a checked
 *  certificate. */

import { describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { hostTool } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { rowLimit, runBounded } from "./bounded-subprocess";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const probe = fileURLToPath(new URL("../acceptance/socket-access.ts", import.meta.url));
const scratch = temporaryDirectories("socket-access-tls");
const withoutOpenssl = hostTool("openssl") === null;

/** A certificate and key for 127.0.0.1, made for this run, and the files that hold them. */
function certificate(): { cert: string; key: string; certFile: string; directory: string } {
  const directory = scratch();
  const certFile = join(directory, "cert.pem");
  const keyFile = join(directory, "key.pem");
  const made = Bun.spawnSync(
    [
      "openssl",
      "req",
      "-x509",
      "-newkey",
      "rsa:2048",
      "-nodes",
      "-days",
      "1",
      "-subj",
      "/CN=127.0.0.1",
      "-addext",
      "subjectAltName=IP:127.0.0.1",
      "-keyout",
      keyFile,
      "-out",
      certFile,
    ],
    { stderr: "pipe", stdout: "pipe" }
  );
  if (made.exitCode !== 0) throw new Error(`openssl req: ${made.stderr.toString()}`);
  return {
    cert: readFileSync(certFile, "utf8"),
    certFile,
    directory,
    key: readFileSync(keyFile, "utf8"),
  };
}

describe.skipIf(withoutOpenssl)("the probe over TLS", () => {
  /** The probe's `live` mode against a TLS server of the row's own, which answers every upgrade
   *  404, trusting the server's certificate when `trusted`. */
  async function live(trusted: boolean) {
    const made = certificate();
    const server = Bun.serve({
      fetch: () => new Response("no socket for you", { status: 404 }),
      hostname: "127.0.0.1",
      port: 0,
      tls: { cert: made.cert, key: made.key },
    });
    const header = join(made.directory, "header");
    writeFileSync(header, "Cookie: dsession=a-cookie\n");
    try {
      return await runBounded({
        argv: [
          "bun",
          probe,
          "live",
          `https://127.0.0.1:${server.port}`,
          header,
          join(made.directory, "ready"),
          join(made.directory, "written"),
        ],
        bound: "short",
        cwd: dispatchPackage,
        env: {
          PATH: process.env.PATH ?? "",
          TMPDIR: tmpdir(),
          ...(trusted ? { NODE_EXTRA_CA_CERTS: made.certFile } : {}),
        },
        from: import.meta.url,
      });
    } finally {
      server.stop(true);
    }
  }

  test(
    "reads the server's refusal of the upgrade, over a certificate it checked",
    async () => {
      const { code, stdout } = await live(true);
      expect(stdout).toBe("http 404 no socket for you\n");
      expect(code).toBe(0);
    },
    rowLimit("short")
  );

  // The control: the same server, with its certificate untrusted, is refused, so the row above
  // read the server over a certificate the probe checked and not over one it took on trust.
  test(
    "refuses the server whose certificate it does not trust",
    async () => {
      const { code, stdout } = await live(false);
      expect(stdout).toBe("");
      expect(code).toBe(1);
    },
    rowLimit("short")
  );
});
