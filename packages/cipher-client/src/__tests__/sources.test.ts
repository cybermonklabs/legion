// packages/cipher-client/src/__tests__/sources.test.ts
//
// Two of the identities a TypeScript caller holds, the two this file tests: a pod's projected
// service-account token, read from the file its runtime mounts, and a Compute Engine machine's
// own Google identity token, asked of its metadata server with format=full and fetched through
// no proxy. A pool machine's identity and a person's sign-in have tests of their own
// (pool-identity.test.ts, person-identity.test.ts).

import { afterAll, expect, test } from "bun:test";
import { randomBytes } from "node:crypto";
import { writeFile } from "node:fs/promises";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { METADATA_HOST_VAR, metadataIdentity } from "../metadata-identity";
import { projectedToken } from "../projected-token";
import { SUBJECT_TOKEN_TYPE_GOOGLE, SUBJECT_TOKEN_TYPE_KUBERNETES } from "../wire";

const makeDirectory = temporaryDirectories("broker-client");

const servers: { stop: (closeActive?: boolean) => void }[] = [];
afterAll(() => {
  for (const server of servers) server.stop(true);
});

/** A file in a directory of this test's own, holding `contents`. */
async function tokenFile(contents: string): Promise<string> {
  const path = join(makeDirectory(), "token");
  await writeFile(path, contents, { mode: 0o600 });
  return path;
}

/** One request the stub metadata server was asked, as it saw it. */
interface MetadataRequest {
  readonly path: string;
  readonly query: string;
  readonly flavor: string | null;
}

/** A stand-in metadata server answering `token`, and what it was asked. */
function stubMetadata(token: string): { host: string; asked: MetadataRequest[] } {
  const asked: MetadataRequest[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      const url = new URL(request.url);
      asked.push({
        path: url.pathname,
        query: url.search,
        flavor: request.headers.get("Metadata-Flavor"),
      });
      return new Response(`${token}\n`);
    },
  });
  servers.push(server);
  return { host: `127.0.0.1:${server.port}`, asked };
}

/** The machine's identity token the stand-in metadata server answers: random, as a real one is,
 *  since the process holds it (`@legion/contracts/held-secrets`). */
const MACHINE_IDENTITY_TOKEN = fixtureSecret("machine");

test("a projected token is read from its file at every read, trimmed", async () => {
  const first = fixtureSecret("first");
  const second = fixtureSecret("second");
  const path = await tokenFile(`${first}\n`);
  const source = projectedToken(path, "AGENT_SECRETS_MACHINE_TOKEN_FILE");
  expect(await source.read()).toEqual({
    token: first,
    tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES,
  });
  // The kubelet rewrites the file in place, so a renewal must see the new token.
  await writeFile(path, `${second}\n`);
  expect((await source.read()).token).toBe(second);
  expect(source.describe()).toBe(
    "the projected service-account token AGENT_SECRETS_MACHINE_TOKEN_FILE names"
  );
});

test("a projected token file that is missing or empty is an error naming its variable, never an empty token", async () => {
  const missing = join(makeDirectory(), "absent");
  const variable = "AGENT_SECRETS_MACHINE_TOKEN_FILE";
  expect(projectedToken(missing, variable).read()).rejects.toThrow(
    `read the projected service-account token ${variable} names: ENOENT`
  );
  const blank = await tokenFile("  \n");
  expect(projectedToken(blank, variable).read()).rejects.toThrow(
    `the projected service-account token ${variable} names is empty`
  );
});

test("a token pasted into the token file's variable is named in no error", async () => {
  // The variable should name a path; a token pasted there is the path every read error would quote.
  const pasted = `eyJhbGciOiJSUzI1NiJ9.${randomBytes(12).toString("hex")}.sig`;
  const source = projectedToken(pasted, "AGENT_SECRETS_MACHINE_TOKEN_FILE");
  const refusal = await source.read().then(
    () => undefined,
    (error: unknown) => (error instanceof Error ? error.message : String(error))
  );
  expect(refusal).toBe(
    "read the projected service-account token AGENT_SECRETS_MACHINE_TOKEN_FILE names: ENOENT"
  );
  expect(source.describe()).not.toContain(pasted);
});

test("a machine's identity token is asked for with format=full, the audience and the header", async () => {
  const metadata = stubMetadata(MACHINE_IDENTITY_TOKEN);
  const source = metadataIdentity("example-audience", {
    [METADATA_HOST_VAR]: metadata.host,
  } as NodeJS.ProcessEnv);
  expect(await source.read()).toEqual({
    token: MACHINE_IDENTITY_TOKEN,
    tokenType: SUBJECT_TOKEN_TYPE_GOOGLE,
  });
  expect(metadata.asked).toEqual([
    {
      path: "/computeMetadata/v1/instance/service-accounts/default/identity",
      query: "?audience=example-audience&format=full",
      flavor: "Google",
    },
  ]);
});

test("a metadata server that refuses is an error naming the variable that names it", async () => {
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () => new Response("no identity here", { status: 403 }),
  });
  servers.push(server);
  const source = metadataIdentity("example-audience", {
    [METADATA_HOST_VAR]: `127.0.0.1:${server.port}`,
  } as NodeJS.ProcessEnv);
  expect(source.read()).rejects.toThrow(
    `the metadata server ${METADATA_HOST_VAR} names answered HTTP 403`
  );
});

// GCE_METADATA_HOST is a host, like a NATS server's setting, and a host's text cannot be told from
// a secret pasted there, so a text names the server by the variable and the failure by its class.
test.each([
  `${randomBytes(9).toString("hex")}.invalid`,
  `operator:${randomBytes(9).toString("hex")}@127.0.0.1:1`,
])("a metadata host of %p is named in no error", async (pasted) => {
  const source = metadataIdentity("example-audience", {
    [METADATA_HOST_VAR]: pasted,
  } as NodeJS.ProcessEnv);
  const refusal = await source.read().then(
    () => undefined,
    (error: unknown) => error
  );
  if (!(refusal instanceof Error)) throw new Error("the metadata server answered");
  expect(refusal.message).toStartWith(
    `the metadata server ${METADATA_HOST_VAR} names could not be reached: `
  );
  expect(Bun.inspect(refusal)).not.toContain(pasted);
  expect(Bun.inspect(refusal).toLowerCase()).not.toContain(pasted.split(/[.@]/)[0]?.toLowerCase());
});

test("no proxy variable can take the identity token", async () => {
  // A proxy that would take the token if anything here honoured one. It answers every request
  // with a token of its own, so a reader that went through it would succeed with the wrong
  // token rather than fail, which is the failure this guards against.
  const taken: string[] = [];
  const proxy = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      taken.push(request.url);
      return new Response("the.proxy.took.it");
    },
  });
  servers.push(proxy);
  const metadata = stubMetadata(MACHINE_IDENTITY_TOKEN);
  const proxyUrl = `http://127.0.0.1:${proxy.port}`;
  const source = metadataIdentity("example-audience", {
    [METADATA_HOST_VAR]: metadata.host,
    HTTP_PROXY: proxyUrl,
    http_proxy: proxyUrl,
    HTTPS_PROXY: proxyUrl,
    ALL_PROXY: proxyUrl,
    NODE_USE_ENV_PROXY: "1",
  } as NodeJS.ProcessEnv);

  // The reader takes its environment from the caller, so set the process's variables too: a
  // runtime that reads them reads them from there.
  const before = { ...process.env };
  process.env.HTTP_PROXY = proxyUrl;
  process.env.http_proxy = proxyUrl;
  process.env.HTTPS_PROXY = proxyUrl;
  process.env.ALL_PROXY = proxyUrl;
  process.env.NODE_USE_ENV_PROXY = "1";
  try {
    expect((await source.read()).token).toBe(MACHINE_IDENTITY_TOKEN);
  } finally {
    for (const name of [
      "HTTP_PROXY",
      "http_proxy",
      "HTTPS_PROXY",
      "ALL_PROXY",
      "NODE_USE_ENV_PROXY",
    ]) {
      if (before[name] === undefined) delete process.env[name];
      else process.env[name] = before[name];
    }
  }
  expect(taken).toEqual([]);
  expect(metadata.asked.length).toBe(1);
});
