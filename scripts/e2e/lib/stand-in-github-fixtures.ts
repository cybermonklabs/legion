// The stand-in GitHub the tests of lib/app-logins.ts and lib/github-apps.sh prove their Apps
// against, with the Apps on it, their releases and the assertion that no message quotes a token.
// It answers GraphQL's viewer and GET /apps/<slug> under an installation token of one of its Apps,
// and refuses every other request, the JWT routes (GET /app, the installation routes) among them.
// The implement App holds checks: read alone, as the real one does.
import { afterAll, expect } from "bun:test";
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

/** Asserts that TEXT quotes no stand-in App's token. */
export const holdsNoToken = (text: string) => {
  for (const token of allTokens) expect(text).not.toContain(token);
};

/** A release as the secrets broker gives one: one entry per installation, keyed by login. */
export function release(installations: Record<string, string>, expires = "2099-01-01T00:00:00Z") {
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
  const github = Bun.serve({
    port: 0,
    async fetch(request) {
      const path = new URL(request.url).pathname;
      seen.push(`${request.method} ${path}`);
      const token = (request.headers.get("authorization") ?? "").replace(/^token /, "");
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
  return { url: `http://127.0.0.1:${github.port}/`, seen, dir };
}
