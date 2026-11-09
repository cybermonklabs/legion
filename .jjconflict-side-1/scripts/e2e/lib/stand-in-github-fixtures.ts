// The stand-in GitHub the tests of lib/app-logins.ts and lib/github-apps.sh prove their Apps
// against, with the Apps on it, their keys, their releases and the assertion that no message
// quotes a token. It answers GraphQL's viewer and GET /apps/<slug> under an installation token of
// one of its Apps, and the two App-JWT routes a key-configured App is proved through — GET
// /app/installations and POST /app/installations/<id>/access_tokens — to a request signed with
// that App's own key; it refuses every other request, the rest of /app (GET /app among them)
// included. The implement App holds checks: read alone, as the real one does.
import { afterAll, expect } from "bun:test";
import { createVerify, generateKeyPairSync } from "node:crypto";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

interface StandInApp {
  id: number;
  slug: string;
  /** The App id GET /apps/<slug> answers, when it is not `id`. */
  answers?: number;
  /** The login GraphQL's viewer answers, when it is not `<slug>[bot]`. */
  login?: string;
  permissions: Record<string, string>;
}
const implementPermissions = { contents: "write", checks: "read", pull_requests: "write" };
const reviewPermissions = { contents: "write", checks: "write", pull_requests: "write" };
/** Each stand-in App by its installation token. */
const tokens: Record<string, StandInApp> = {
  ghs_implement_101_secret: {
    id: 101,
    slug: "example-implement",
    permissions: implementPermissions,
  },
  ghs_review_202_secret: { id: 202, slug: "example-review", permissions: reviewPermissions },
  ghs_other_404_secret: {
    id: 404,
    slug: "example-other",
    answers: 405,
    permissions: reviewPermissions,
  },
  ghs_person_303_secret: {
    id: 303,
    slug: "example-person",
    login: "a-person",
    permissions: reviewPermissions,
  },
};
const allTokens = Object.keys(tokens);

/** The installation token of the stand-in App whose id is ID. */
export const tokenOf = (id: number) => allTokens.find((token) => tokens[token].id === id) ?? "";

/** The stand-in App whose app id is ID, by the string of that id, so a JWT that issues itself
 * from `"202"` and one that issues itself from `202` name the same App. */
const appOf = (id: number | string) =>
  Object.values(tokens).find((app) => String(app.id) === String(id));

/** Each stand-in App's keypair, made at the first request that needs it and kept afterwards:
 * generating an RSA 2048-bit key costs real milliseconds, every test file of this directory
 * imports this module, and most of them sign nothing, so a file that proves no key pays for no
 * key. */
const keypairs = new Map<number, { publicKey: string; privateKey: string }>();
const keypairOf = (id: number) => {
  const held = keypairs.get(id);
  if (held !== undefined) return held;
  const made = generateKeyPairSync("rsa", {
    modulusLength: 2048,
    publicKeyEncoding: { type: "spki", format: "pem" },
    privateKeyEncoding: { type: "pkcs8", format: "pem" },
  });
  keypairs.set(id, made);
  return made;
};

/** The PKCS#8 PEM private key of the stand-in App whose id is ID, the key a test puts behind a
 * key command, or a throw naming the id when no stand-in App has it. */
export function appPrivateKey(id: number): string {
  if (appOf(id) === undefined) throw new Error(`no stand-in App has app id ${id}`);
  return keypairOf(id).privateKey;
}

/** The installation of the stand-in App whose app id is ID: one apiece, its id derived from the
 * App's own, so no App's installation is another's and the pair holds across runs. */
const installationOf = (id: number) => 10 * id;

/** The account every installation of this GitHub is on, the login the releases of `release()` are
 * keyed by, so the key path and the token path name one account. */
const accountLogin = "sjawhar";

/** When a minted installation token expires, and when the installations of `release()` do, since
 * no proof here is about a token running out. */
const expiry = "2099-01-01T00:00:00Z";

/** The JSON object a base64url part of a JWT holds, or undefined when the part holds no such
 * thing. */
function partOf(part: string): Record<string, unknown> | undefined {
  try {
    const held: unknown = JSON.parse(Buffer.from(part, "base64url").toString("utf8"));
    return typeof held === "object" && held !== null
      ? (held as Record<string, unknown>)
      : undefined;
  } catch {
    // Not base64url JSON at all, and what the decoder tripped over says nothing an operator of
    // the test needs: the caller refuses the JWT for being malformed.
    return undefined;
  }
}

/** What a verified App JWT carried: the App that signed it, and the claims its payload held, so
 * a caller reads the claims this server accepted rather than deriving them again from a clock of
 * its own. */
export interface VerifiedAppJwt {
  app: StandInApp;
  claims: Record<string, unknown>;
}

/** The App JWT of `authorization` and the stand-in App that signed it with its own key, or the
 * sentence saying why no App did. The check is GitHub's less the clock: `Bearer` and three
 * dot-separated base64url parts, RS256, an `iss` naming an App of this GitHub, and a signature
 * over `<header>.<payload>` that verifies under that App's public key. `exp` and `iat` are
 * handed back unread, which the real GitHub reads, because a row that mints a JWT and then
 * waits — on a key command that sleeps, say — would go flaky against a stand-in that expired it,
 * and the one row that is about a JWT's lifetime reads them itself from `verified`. No refusal
 * quotes the JWT or a part of it: this server's answers are held to the rule the proofs are,
 * that no message carries a credential. */
function signerOf(authorization: string): VerifiedAppJwt | string {
  const jwt = /^Bearer (\S+)$/.exec(authorization)?.[1];
  if (jwt === undefined) return "no App JWT: Authorization must be `Bearer <JWT>`";
  const parts = jwt.split(".");
  if (parts.length !== 3) return "the App JWT is not three dot-separated base64url parts";
  const [header, payload, signature] = parts;
  const heading = partOf(header);
  if (heading === undefined) return "the App JWT's header is no base64url JSON";
  if (heading.alg !== "RS256") {
    return `the App JWT's alg is ${JSON.stringify(heading.alg ?? null)}, want RS256`;
  }
  const claims = partOf(payload);
  if (claims === undefined) return "the App JWT's payload is no base64url JSON";
  const iss = claims.iss;
  if (typeof iss !== "string" && typeof iss !== "number") {
    return "the App JWT has no iss: an App JWT names its App by its app id there";
  }
  const app = appOf(iss);
  if (app === undefined) return `the App JWT's iss ${iss} is no App on this GitHub`;
  let signed = false;
  try {
    signed = createVerify("RSA-SHA256")
      .update(`${header}.${payload}`)
      .verify(keypairOf(app.id).publicKey, Buffer.from(signature, "base64url"));
  } catch {
    // Bytes that are no signature at all: refused as one made with another App's key is.
    signed = false;
  }
  if (!signed) return `the App JWT is not signed with app id ${app.id}'s own key`;
  return { app, claims };
}

/** Asserts that TEXT quotes no stand-in App's token. */
export const holdsNoToken = (text: string) => {
  for (const token of allTokens) expect(text).not.toContain(token);
};

/** The first part of every App JWT the key path signs, `{"alg":"RS256","typ":"JWT"}` in
 * base64url: what lib/app-logins.ts writes there and what signerOf above reads back. */
const jwtHeader = Buffer.from('{"alg":"RS256","typ":"JWT"}', "utf8").toString("base64url");

/** Asserts that TEXT quotes no App JWT and no private key of the stand-in Apps IDS names, as
 * `holdsNoToken` asserts it quotes no token: the key's base64 body lines are 64 characters
 * apiece of a key this run generated, which nothing else here could print. */
export const holdsNoKey = (text: string, ids: readonly number[]) => {
  expect(text).not.toContain("BEGIN PRIVATE KEY");
  expect(text).not.toContain(jwtHeader);
  for (const id of ids) {
    for (const pem of appPrivateKey(id).split("\n")) {
      if (pem.length >= 40 && pem[0] !== "-") expect(text).not.toContain(pem);
    }
  }
};

/** A release as the secrets broker gives one: one entry per installation, keyed by login. */
export function release(installations: Record<string, string>, expires = expiry) {
  return JSON.stringify({
    installations: Object.fromEntries(
      Object.entries(installations).map(([owner, token], index) => [
        owner,
        { installation_id: 1000 + index, token, expires_at: expires },
      ])
    ),
  });
}

export interface StandInGitHub {
  /** The API URL the helpers under test are handed. */
  url: string;
  /** Each request the stand-in answered, as `<method> <path>`, in the order it took them. A row
   * that reads this empties it first, since the rows before it have written to it. */
  seen: string[];
  /** The claims of each App JWT this server verified, in the order it took them: the payload it
   * accepted, so the one row about a JWT's lifetime reads the `exp` the server was handed rather
   * than deriving it again. The server itself reads no clock, here or anywhere. A row that reads
   * this empties it first, as it empties `seen`. */
  verified: Record<string, unknown>[];
  /** A temporary directory of the caller's own, reserved at the call and made by the `beforeAll`
   * the reservation registers, so nothing is at the path until that hook has run: a caller writes
   * into it from a test body, or from a hook of its own registered below its `standInGitHub`
   * call. */
  dir: string;
}

/** A stand-in GitHub and a temporary directory for the one test file that calls this, with an
 * `afterAll` that stops that server and the pair of hooks `temporaryDirectory` registers, which
 * make that directory before the file's first row and remove it after its last. `bun test` runs
 * the files of one command in one process, one after the other: two files run together printed
 * one pid, and a value the first set on `globalThis` was there for the second to read. So what
 * keeps the files apart is this shape rather than the process: each call owns its own port-0
 * server, its own temporary directory under a prefix of its own, and its own teardown, so each
 * file calls this once and no file's teardown reaches what another file owns. A run that selects
 * no row of the file makes no directory at all, since Bun then runs neither hook. */
export function standInGitHub(prefix: string): StandInGitHub {
  const dir = temporaryDirectory(prefix);
  const seen: string[] = [];
  const verified: Record<string, unknown>[] = [];
  const github = Bun.serve({
    port: 0,
    async fetch(request) {
      const path = new URL(request.url).pathname;
      seen.push(`${request.method} ${path}`);
      // The two App-JWT routes come before the token table below: what they carry is a JWT the
      // App signed, which keys nothing there, and the refusal below would take them for a bad
      // credential. Every other route under /app, GET /app among them, is left to it.
      const authorization = request.headers.get("authorization") ?? "";
      if (request.method === "GET" && path === "/app/installations") {
        const signer = signerOf(authorization);
        if (typeof signer === "string") return Response.json({ message: signer }, { status: 401 });
        verified.push(signer.claims);
        return Response.json([
          { id: installationOf(signer.app.id), account: { login: accountLogin } },
        ]);
      }
      const minting = /^\/app\/installations\/(\d+)\/access_tokens$/.exec(path);
      if (request.method === "POST" && minting !== null) {
        const signer = signerOf(authorization);
        if (typeof signer === "string") return Response.json({ message: signer }, { status: 401 });
        verified.push(signer.claims);
        if (Number(minting[1]) !== installationOf(signer.app.id)) {
          return Response.json({ message: "Not Found" }, { status: 404 });
        }
        // The App's own installation token, the one `tokens` keys it by, answered with GitHub's
        // 201: every later request of the proof is then authorized exactly as the token path's
        // is, and `holdsNoToken` catches this token wherever a message prints it.
        return Response.json(
          { token: tokenOf(signer.app.id), expires_at: expiry },
          { status: 201 }
        );
      }
      const token = authorization.replace(/^token /, "");
      const app = tokens[token];
      if (app === undefined || path === "/app" || path.startsWith("/app/")) {
        return Response.json({ message: `Bad credentials for ${token}` }, { status: 401 });
      }
      if (request.method === "POST" && path === "/graphql") {
        const query = (await request.json()) as { query?: string };
        if (query.query !== "{viewer{login}}") return Response.json({}, { status: 400 });
        return Response.json({ data: { viewer: { login: app.login ?? `${app.slug}[bot]` } } });
      }
      const slug = /^\/apps\/([^/]+)$/.exec(path)?.[1];
      const named = Object.values(tokens).find((candidate) => candidate.slug === slug);
      if (request.method === "GET" && named !== undefined) {
        return Response.json({
          id: named.answers ?? named.id,
          slug: named.slug,
          permissions: named.permissions,
        });
      }
      return Response.json({ message: "Not Found" }, { status: 404 });
    },
  });
  afterAll(() => {
    github.stop(true);
  });
  return { url: `http://127.0.0.1:${github.port}/`, seen, verified, dir };
}
