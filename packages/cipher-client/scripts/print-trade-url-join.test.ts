// packages/cipher-client/scripts/print-trade-url-join.test.ts
//
// print-trade.ts, run as scripts/project-secrets-check.sh runs it, asks the broker's
// introspection route joined onto AGENT_SECRETS_URL's path, the URL's query kept as a query
// (`joinUrl`), as its trade asks the token route. Appended to the URL's text, the route sat
// inside the query (`/prefix?key=v/v1/oauth/introspect`) and the broker was asked the URL's own
// path. A broker URL that is a plain origin is asked as before. The stand-in broker records the
// path and the query of each request, and tells a trade from an introspection by its form, so it
// answers both wherever they arrive.

import { afterEach, describe, expect, test } from "bun:test";
import { writeFile } from "node:fs/promises";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

const makeDirectory = temporaryDirectories("print-trade-url-join");
const SCRIPT = join(import.meta.dir, "print-trade.ts");

const stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** A broker on an origin of its own: it records each request, mints a token for a trade and
 *  answers an introspection, at whatever path it was asked. */
function standIn(): { readonly origin: string; readonly asked: string[][] } {
  const asked: string[][] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const url = new URL(request.url);
      const form = new URLSearchParams(await request.text());
      asked.push([url.pathname, url.search]);
      if (!form.has("grant_type")) {
        return Response.json({ active: true, session: "s", project_scope: "named", projects: [] });
      }
      return Response.json({
        access_token: fixtureSecret("access"),
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 600,
      });
    },
  });
  stops.push(() => server.stop(true));
  return { origin: `http://127.0.0.1:${server.port}`, asked };
}

/** Runs print-trade.ts as a pod would, against the broker at `url`. */
async function run(url: string) {
  const dir = makeDirectory();
  const tokenFile = join(dir, "exchange-token");
  await writeFile(tokenFile, `${fixtureSecret("pod")}\n`);
  const accountFile = join(dir, "account-token");
  await writeFile(accountFile, `${fixtureSecret("account")}\n`);
  const child = Bun.spawn(["bun", SCRIPT], {
    env: {
      PATH: process.env.PATH ?? "",
      HOME: dir,
      AGENT_SECRETS_URL: url,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
      AGENT_SECRETS_SESSION: "legion-priv-worker-session",
      AGENT_SECRETS_SIGNIN_DIR: join(dir, "signins"),
      AGENT_SECRETS_ACCOUNT_TOKEN_FILE: accountFile,
    },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, exit] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { stdout, stderr, exit };
}

/** The broker URLs of the rows, each a path under the stand-in's origin, the path each route
 *  must reach the broker under, and the query it must reach it with. */
const CASES = [
  ["query", "/prefix?key=k3y-made-up", "/prefix", "?key=k3y-made-up"],
  ["query-trailing-slash", "/prefix/?key=k3y-made-up", "/prefix", "?key=k3y-made-up"],
  ["no-query", "", "", ""],
] as const;

describe("print-trade asks the introspection route joined onto the broker URL's path", () => {
  for (const [name, at, under, query] of CASES) {
    test(name, async () => {
      const broker = standIn();

      const { stderr, exit } = await run(`${broker.origin}${at}`);

      expect({ asked: broker.asked, exit, stderr }).toEqual({
        asked: [
          [`${under}/v1/oauth/token`, query],
          [`${under}/v1/oauth/introspect`, query],
        ],
        exit: 0,
        stderr: "",
      });
    });
  }
});
