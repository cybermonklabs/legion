import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import {
  type DispatchStack,
  dispatchStackEnabled,
  startDispatchStack,
  useDispatchStackDirectory,
} from "../../test/dispatch-stack";
import type { RequestCredential } from "../bearer";
import { DispatchClient } from "../dispatch-http";

// A keyed write whose first attempt the server refused because it could not resolve the caller,
// end to end: the real `DispatchClient` of this checkout against a real Dispatch server of this
// checkout, on a database of its own. The server's own Go rows walk every write route with a
// handler of the test's own (`TestAKeyedWriteNeverRunsUnkeyed`), and the client's unit rows drive
// a stub Dispatch (`dispatch-keyed-resend.test.ts`); neither proves that a real client's resend
// under its own key against a real server lands the write once, which is what the row below
// writes a real comment to find out.
//
// The refusal is a real one, met by the server's own caller lookup: the stack is started with
// `DISPATCH_OIDC_ISSUER` at a port nothing serves, so a JWT-shaped bearer is read as a projected
// service-account token, its issuer is never discovered, and `serviceTokenActor` answers `503
// OIDC_ISSUER_UNAVAILABLE`. Nothing is stored under the key, and the client's next attempt - with
// the stack's own token - runs the write once under that same key. The row proves the client's
// end: it resends under its key and asks nothing first. It cannot tell whether the keyed layer or
// the route's own handler refused the first attempt, since an issuer that stays down fails both
// lookups; that no handler runs is the Go walk's to prove.

/** The person who owns the project and the issue, as the server's header identity names them. */
const PERSON = "alice";
/** The session the client's writes name, as a bearer caller names its own. */
const SESSION = "session-keyed-resend-live";
/** The project whose issue each row comments on. */
const PROJECT = "KEYED";
/** The comment body each row writes, which it then counts on the issue. */
const BODY = "The write whose first attempt the caller lookup refused.";

/**
 * A bearer shaped like a JWT, which a server with an OIDC issuer configured reads as a projected
 * service-account token (`oidc.LooksLikeJWT`): three base64url segments. Its issuer is the dead
 * port the stack is started with, and nothing of it is ever verified.
 */
const SERVICE_TOKEN = [
  "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9",
  "eyJpc3MiOiJodHRwOi8vMTI3LjAuMC4xOjEiLCJzdWIiOiJsaXZlLXJvdyJ9",
  "c2lnbmF0dXJlLW5vdGhpbmctdmVyaWZpZXM",
].join(".");

/** One attempt the client made, as the row's own fetch saw it. */
interface Attempt {
  readonly key: string | null;
  /** Whether the attempt presented the JWT-shaped bearer rather than the stack's own token. */
  readonly service: boolean;
  readonly status: number;
  /** The error code of a refusal's JSON body, read from a copy so the client reads its own; null
   *  for an answer that is no refusal, or whose body carries no code. */
  readonly code: string | null;
}

/** A refusal's `code`, where its body is JSON that carries one as a string. */
async function refusalCode(response: Response): Promise<string | null> {
  try {
    const body = (await response.json()) as { code?: unknown };
    return typeof body.code === "string" ? body.code : null;
  } catch {
    return null;
  }
}

// Reserved at the file's module scope: the file's own `afterAll` removes it once the `describe`
// below has stopped the server writing into it.
const stackDirectory = useDispatchStackDirectory();

describe.skipIf(!dispatchStackEnabled)(
  "a keyed write a real Dispatch refused because it could not resolve the caller",
  () => {
    let stack: DispatchStack | undefined;

    /** The running server, or the error that says why no row can run. */
    function server(): DispatchStack {
      const running = stack;
      if (running === undefined) throw new Error("the Dispatch stack did not start");
      return running;
    }

    /** One request as the person the header identity names. */
    async function asPerson<T>(method: string, route: string, body?: unknown): Promise<T> {
      const response = await fetch(`${server().url}${route}`, {
        method,
        headers: { "Content-Type": "application/json", "X-Dispatch-User": PERSON },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
      const text = await response.text();
      if (!response.ok) throw new Error(`${method} ${route}: ${response.status} ${text}`);
      return JSON.parse(text) as T;
    }

    /** The issue's comments as the server holds them now, by body. */
    async function commentBodies(issue: string): Promise<string[]> {
      const listed = await asPerson<{ body: string }[]>("GET", `/api/v1/issues/${issue}/comments`);
      return listed.map((comment) => comment.body);
    }

    /** An issue of the row's own, so each row counts only its own comments. */
    async function issueOfItsOwn(title: string): Promise<string> {
      const issue = await asPerson<{ key: string }>("POST", "/api/v1/issues", {
        project: PROJECT,
        title,
        force: true,
      });
      return issue.key;
    }

    /**
     * A client whose credential presents the JWT-shaped bearer on its first request when
     * `refusedFirst`, and the stack's own token on every other, and whose fetch records every
     * attempt.
     */
    function clientPresenting(refusedFirst: boolean): { client: DispatchClient; made: Attempt[] } {
      const made: Attempt[] = [];
      let service = refusedFirst;
      const credential: RequestCredential = {
        scheme: "Bearer",
        headers: () => {
          const token = service ? SERVICE_TOKEN : server().token;
          service = false;
          return { Authorization: `Bearer ${token}` };
        },
        secret: () => server().token,
      };
      const recording = (async (input: URL | RequestInfo, init?: RequestInit) => {
        const response = await fetch(input, init);
        const headers = new Headers(init?.headers);
        made.push({
          key: headers.get("Idempotency-Key"),
          service: headers.get("Authorization") === `Bearer ${SERVICE_TOKEN}`,
          status: response.status,
          code: response.ok ? null : await refusalCode(response.clone()),
        });
        return response;
      }) as typeof fetch;
      return { client: new DispatchClient(server().url, credential, recording), made };
    }

    beforeAll(async () => {
      stack = await startDispatchStack({
        work: stackDirectory,
        // A port nothing serves: the issuer is never discovered, so every projected
        // service-account token is answered 503 OIDC_ISSUER_UNAVAILABLE rather than verified.
        env: {
          DISPATCH_OIDC_ISSUER: "http://127.0.0.1:1",
          DISPATCH_OIDC_AUDIENCE: "dispatch-keyed-resend-live",
        },
      });
      await asPerson("POST", "/api/v1/projects", { key: PROJECT, name: "Keyed resend" });
    }, 600_000);
    afterAll(async () => {
      await stack?.stop();
    }, 60_000);

    // The case: one call of the client. Its first attempt is refused 503 OIDC_ISSUER_UNAVAILABLE by
    // the server's caller lookup, which stores nothing; the client sends the same write again under
    // the same key, and the issue holds the comment once. The code is asserted beside the status,
    // since a 503 of any other cause would show the same two statuses.
    test("case: the refused attempt stores nothing, and the resend under its key lands once", async () => {
      const issue = await issueOfItsOwn("The refused attempt");
      const { client, made } = clientPresenting(true);

      const written = await client.comment(issue, {
        body: BODY,
        actor: { kind: "session", id: SESSION },
      });

      const keys = new Set(made.map((attempt) => attempt.key));
      expect({
        statuses: made.map((attempt) => attempt.status),
        codes: made.map((attempt) => attempt.code),
        keyed: made.every((attempt) => attempt.key !== null),
        keys: keys.size,
        presented: made.map((attempt) => attempt.service),
        bodies: await commentBodies(issue),
        written: written.id !== undefined,
      }).toEqual({
        statuses: [503, 201],
        codes: ["OIDC_ISSUER_UNAVAILABLE", null],
        keyed: true,
        keys: 1,
        presented: [true, false],
        bodies: [BODY],
        written: true,
      });
    }, 120_000);

    // The control: the same write made by two calls, each of which mints a key of its own, with no
    // refusal in either. The issue holds two comments, which shows the row counts what the real
    // server stored rather than what the client believes it sent.
    test("control: the same write by two calls, each under a fresh key, lands twice", async () => {
      const issue = await issueOfItsOwn("Two calls, two keys");
      const { client, made } = clientPresenting(false);
      const actor = { kind: "session", id: SESSION } as const;

      await client.comment(issue, { body: BODY, actor });
      await client.comment(issue, { body: BODY, actor });

      expect({
        statuses: made.map((attempt) => attempt.status),
        keys: new Set(made.map((attempt) => attempt.key)).size,
        bodies: await commentBodies(issue),
      }).toEqual({ statuses: [201, 201], keys: 2, bodies: [BODY, BODY] });
    }, 120_000);
  }
);
