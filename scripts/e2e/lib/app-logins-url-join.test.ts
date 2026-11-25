import { afterAll, beforeAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import { generateKeyPairSync } from "node:crypto";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { HANG_GUARD_MS } from "@legion/test-fixtures/hang-guard";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { type AppCredentialSource, appLogin } from "./app-logins";

/**
 * Where `appLogin` asks GitHub: each API route joined onto the GitHub API URL's path, its query
 * kept, against a stand-in GitHub served under a path and a query that answers 404 anywhere else,
 * as a GitHub Enterprise API behind a path does. Both credential paths are driven: a token
 * command's release, and a key command's key, which mints the token through the two App-JWT
 * routes first.
 */

// The rows run credential commands and wait on a local server, which a loaded machine slows.
// Their bound is a hang guard, not a verdict.
setDefaultTimeout(HANG_GUARD_MS);

const SLUG = "made-up-app";
const TOKEN = "ghs_made_up_installation_token";
const EXPIRES = "2099-01-01T00:00:00Z";
const INSTALLATION = 7;

/** Every request the stand-in took, as `<method> <path><query>`. */
const asked: string[] = [];
/** Where the stand-in is served: under `mount`, with `query` on every request. */
let served = { mount: "", query: "" };

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  async fetch(request) {
    const url = new URL(request.url);
    asked.push(`${request.method} ${url.pathname}${url.search}`);
    await request.text();
    const route = url.pathname.startsWith(`${served.mount}/`)
      ? url.pathname.slice(served.mount.length)
      : undefined;
    const authorization = request.headers.get("authorization") ?? "";
    if (route === undefined || url.search !== served.query) {
      return Response.json({ message: "Not Found" }, { status: 404 });
    }
    if (request.method === "GET" && route === "/app/installations") {
      return Response.json([{ id: INSTALLATION, account: { login: "acme" } }]);
    }
    if (
      request.method === "POST" &&
      route === `/app/installations/${INSTALLATION}/access_tokens` &&
      authorization.startsWith("Bearer ")
    ) {
      return Response.json({ token: TOKEN, expires_at: EXPIRES }, { status: 201 });
    }
    if (authorization !== `token ${TOKEN}`) {
      return Response.json({ message: "Bad credentials" }, { status: 401 });
    }
    if (request.method === "POST" && route === "/graphql") {
      return Response.json({ data: { viewer: { login: `${SLUG}[bot]` } } });
    }
    if (request.method === "GET" && route === `/apps/${SLUG}`) {
      return Response.json({ id: 101, slug: SLUG, permissions: { contents: "write" } });
    }
    return Response.json({ message: "Not Found" }, { status: 404 });
  },
});
const origin = `http://127.0.0.1:${server.port}`;

const directory = temporaryDirectory("app-logins-url-join");
const keyFile = join(directory, "app-key.pem");

beforeAll(() => {
  const { privateKey } = generateKeyPairSync("rsa", {
    modulusLength: 2048,
    privateKeyEncoding: { type: "pkcs8", format: "pem" },
    publicKeyEncoding: { type: "spki", format: "pem" },
  });
  writeFileSync(keyFile, privateKey, { mode: 0o600 });
});

afterAll(() => server.stop(true));

const release = JSON.stringify({
  installations: { acme: { installation_id: INSTALLATION, token: TOKEN, expires_at: EXPIRES } },
});
const tokenSource: AppCredentialSource = {
  appId: "101",
  kind: "token_command",
  command: `printf %s '${release}'`,
};
const keySource = (): AppCredentialSource => ({
  appId: "101",
  kind: "private_key_command",
  command: `cat '${keyFile}'`,
});

/** What one `appLogin` of the implement App against `api` asked, the stand-in served under
 *  `mount` and `query`, and what it answered. */
async function askedBy(source: AppCredentialSource, api: string, mount: string, query: string) {
  served = { mount, query };
  asked.length = 0;
  const login = await appLogin("implement", source, api).catch((error: unknown) => error);
  return { asked: [...asked], login };
}

describe("appLogin's GitHub requests", () => {
  test("join each route onto a configured API URL's path and keep its query", async () => {
    const { asked, login } = await askedBy(
      tokenSource,
      `${origin}/prefix?key=k3y-made-up`,
      "/prefix",
      "?key=k3y-made-up"
    );
    expect(asked).toEqual([
      "POST /prefix/graphql?key=k3y-made-up",
      `GET /prefix/apps/${SLUG}?key=k3y-made-up`,
    ]);
    expect(login).toBe(`${SLUG}[bot]`);
  });

  test("join a key-configured App's two App-JWT routes the same way", async () => {
    const { asked, login } = await askedBy(
      keySource(),
      `${origin}/prefix?key=k3y-made-up`,
      "/prefix",
      "?key=k3y-made-up"
    );
    expect(asked).toEqual([
      "GET /prefix/app/installations?key=k3y-made-up",
      `POST /prefix/app/installations/${INSTALLATION}/access_tokens?key=k3y-made-up`,
      "POST /prefix/graphql?key=k3y-made-up",
      `GET /prefix/apps/${SLUG}?key=k3y-made-up`,
    ]);
    expect(login).toBe(`${SLUG}[bot]`);
  });

  test("keep an API URL query whole, a trailing slash included", async () => {
    const { asked, login } = await askedBy(
      tokenSource,
      `${origin}/prefix/?key=k3y-made-up/`,
      "/prefix",
      "?key=k3y-made-up/"
    );
    expect(asked).toEqual([
      "POST /prefix/graphql?key=k3y-made-up/",
      `GET /prefix/apps/${SLUG}?key=k3y-made-up/`,
    ]);
    expect(login).toBe(`${SLUG}[bot]`);
  });

  test("ask a plain origin, with or without its trailing slash, at each route itself", async () => {
    for (const api of [origin, `${origin}/`]) {
      const { asked, login } = await askedBy(keySource(), api, "", "");
      expect(asked, api).toEqual([
        "GET /app/installations",
        `POST /app/installations/${INSTALLATION}/access_tokens`,
        "POST /graphql",
        `GET /apps/${SLUG}`,
      ]);
      expect(login, api).toBe(`${SLUG}[bot]`);
    }
  });

  test("refuse an API URL that is no absolute URL, quoting none of it", async () => {
    const { asked, login } = await askedBy(tokenSource, "not-a-url?key=k3y-made-up", "", "");
    expect(asked).toEqual([]);
    expect(login).toBeInstanceOf(Error);
    expect((login as Error).message).toBe(
      "the GitHub API URL is not an absolute URL with a host (not printed)"
    );
  });
});
