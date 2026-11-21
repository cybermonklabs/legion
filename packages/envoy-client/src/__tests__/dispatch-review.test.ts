import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { NO_ROUTE_ROLLING_RELEASE_MESSAGE } from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { keyedLayer } from "../../test/keyed-layer";
import { bearerCredential } from "../bearer";
import { runDispatchCli } from "../dispatch-cli";
import { executeDispatchTool, resetAdviceMemory } from "../dispatch-execute";
import { DispatchServiceError } from "../dispatch-http";
import { messageFor } from "../errors";
import { ToolInputError } from "../tool-input-errors";

// `dispatch review` against a stub Dispatch: the issue read its document resolves through, and
// `POST /api/v1/artifacts/{id}/versions/{number}/reviews` answered as
// `createArtifactVersionReview` (packages/envoy/internal/dispatch/api/reviews.go) answers it, so
// each row pins what goes on the wire and what the command prints of Dispatch's answer. The shape
// is `dispatch-who-waits.test.ts`'s: one stub server driven through `runDispatchCli`, with a
// temporary state directory per row; the row of a build without the route drives the executor
// the CLI runs instead, which takes the wait its retries pause with.

const makeDirectory = temporaryDirectories("dispatch-review");

/** The Dispatch bearer each command, and the executor, is handed. */
const TOKEN = fixtureSecret("dsp");

/** The person the token names, whom Dispatch records every review under. */
const PERSON = "alice@example.com";

interface Recorded {
  readonly method: string;
  readonly path: string;
  readonly body: unknown;
}

const requests: Recorded[] = [];

/** DSP-1 as `GET /api/v1/issues/DSP-1` reads it, with its spec listed after another document, so
 *  the spec a review defaults to is the one the issue names as its primary document, never the
 *  first one listed. */
const ISSUE = {
  key: "DSP-1",
  title: "Native workspace",
  status: "in_progress",
  labels: [],
  priority: null,
  assignee: PERSON,
  route: null,
  parent: null,
  blocked_by: [],
  components: [],
  repo: null,
  resolved_repo: null,
  closed_at: null,
  external_links: [],
  primary_artifact_id: "artifact-spec",
  artifacts: [
    { id: "artifact-design", slug: "design-md", name: "design.md", kind: "doc", primary: false },
    { id: "artifact-spec", slug: "spec-md", name: "spec.md", kind: "doc", primary: true },
  ],
  open_asks: [],
};

/** The refusal the review route answers next, or null for a recorded review. */
let refusal: {
  readonly status: number;
  readonly code: string;
  readonly error: string;
  readonly headers?: Readonly<Record<string, string>>;
} | null = null;
/** The approval request the next recorded review answered, or null when none was open. */
let answeredAsk: string | null = null;
/** Whether the stub answers the versioned review route as a build without it does: no route. */
let noRoute = false;

// Behind Dispatch's keyed layer: each 2xx to a write names the write's key back.
const server = Bun.serve({
  port: 0,
  fetch: keyedLayer(async (request: Request) => {
    const { pathname } = new URL(request.url);
    const text = await request.text();
    const body: unknown = text === "" ? undefined : JSON.parse(text);
    requests.push({ method: request.method, path: pathname, body });
    if (pathname === "/api/v1/issues/DSP-1" && request.method === "GET") {
      return Response.json(ISSUE);
    }
    const review = /^\/api\/v1\/artifacts\/([^/]+)\/versions\/([^/]+)\/reviews$/.exec(pathname);
    if (review !== null && request.method === "POST") {
      if (noRoute) {
        // A build older than the route, answering as its router answers a path it has no route
        // for (`staticHandler` in packages/envoy/internal/dispatch/routes/router.go).
        return Response.json(
          {
            code: "NOT_FOUND",
            error: `no route for ${request.method} ${pathname}`,
            hint: "GET /api/v1 lists every route",
          },
          { status: 404 }
        );
      }
      if (refusal !== null) {
        return Response.json(
          { code: refusal.code, error: refusal.error },
          { status: refusal.status, headers: refusal.headers ?? {} }
        );
      }
      // The review as Dispatch records it: at the version the path named, in the name of the
      // person the token names, with the session the token proved as its act, and the reason
      // trimmed.
      const sent = body as { state: string; reason?: string };
      return Response.json(
        {
          id: "review-1",
          artifact_id: review[1],
          version: Number(review[2]),
          state: sent.state,
          kind: "person",
          actor: { kind: "user", id: PERSON, act: { session: "ses-1" } },
          reason: sent.reason?.trim() || null,
          ask_id: answeredAsk,
          created_at: "2026-11-14T09:00:00Z",
          content_sha256: null,
          content_sha256_source: null,
        },
        { status: 201 }
      );
    }
    return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
  }),
});

afterAll(() => server.stop(true));

let stateDir = "";
beforeEach(() => {
  requests.length = 0;
  refusal = null;
  answeredAsk = null;
  noRoute = false;
  stateDir = makeDirectory();
  resetAdviceMemory();
});

async function run(argv: readonly string[]): Promise<{
  code: number;
  stdout: string;
  stderr: string;
}> {
  let stdout = "";
  let stderr = "";
  const code = await runDispatchCli(
    argv,
    {
      HOME: stateDir,
      DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      DISPATCH_TOKEN: TOKEN,
      DISPATCH_STATE_DIR: stateDir,
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: "ses-1",
    },
    {
      stdout: (text) => {
        stdout += text;
      },
      stderr: (text) => {
        stderr += text;
      },
      readText: () => "",
      cwd: stateDir,
    }
  );
  return { code, stdout, stderr };
}

/** The reviews this run posted, each as its path and the body it carried. */
function posted(): { readonly path: string; readonly body: unknown }[] {
  return requests
    .filter((recorded) => recorded.method === "POST")
    .map(({ path, body }) => ({ path, body }));
}

/** What the command refuses a request for changes without a reason with (`REVIEW_REASON_RULE` in
 *  packages/contracts/src/dispatch-tools.ts), read as literal words. */
const REASON_RULE =
  "verdict changes_requested requires reason, a non-empty note saying what must change before the document can be approved.";

describe("dispatch review", () => {
  test("an approval goes to the versioned route of the issue's spec by default, with the version read in its path and a body of the state alone, and prints the state, version and actor Dispatch answered", async () => {
    const { code, stdout, stderr } = await run([
      "review",
      "DSP-1",
      "--version",
      "3",
      "--verdict",
      "approved",
    ]);

    expect(stderr).toBe("");
    expect(code).toBe(0);
    // The version is in the path and the body carries nothing else; it names no actor either:
    // Dispatch records the person the token names.
    expect(posted()).toEqual([
      { path: "/api/v1/artifacts/artifact-spec/versions/3/reviews", body: { state: "approved" } },
    ]);
    expect(stdout).toBe(
      `Recorded the review of spec.md (document id artifact-spec): approved at version 3, by user ${PERSON}, acting through session ses-1.\n`
    );
  });

  test("a request for changes sends its reason, with the version in the path, to the document --artifact names, and prints the reason Dispatch recorded", async () => {
    const { code, stdout, stderr } = await run([
      "review",
      "DSP-1",
      "--artifact",
      "design-md",
      "--version",
      "2",
      "--verdict",
      "changes_requested",
      "--reason",
      "Name the rollback path.",
    ]);

    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(posted()).toEqual([
      {
        path: "/api/v1/artifacts/artifact-design/versions/2/reviews",
        body: { state: "changes_requested", reason: "Name the rollback path." },
      },
    ]);
    expect(stdout.split("\n")).toEqual([
      `Recorded the review of design.md (document id artifact-design): changes_requested at version 2, by user ${PERSON}, acting through session ses-1.`,
      'Reason: "Name the rollback path."',
      "",
    ]);
  });

  test("a request for changes with no reason, or a blank one, is refused before any request", async () => {
    for (const reason of [[], ["--reason", "   "]]) {
      requests.length = 0;
      const { code, stdout } = await run([
        "review",
        "DSP-1",
        "--version",
        "3",
        "--verdict",
        "changes_requested",
        ...reason,
      ]);

      // An argument the command refuses exits 2, as every refused argument does.
      expect(code, reason.join(" ")).toBe(2);
      expect(stdout, reason.join(" ")).toContain(REASON_RULE);
      expect(requests, reason.join(" ")).toEqual([]);
    }
  });

  test.each([
    [
      409,
      "REVIEW_VERSION_MOVED",
      "the review would record version 4 of spec.md, not version 3: read version 4 and review it",
      {},
    ],
    [
      403,
      "OAUTH_SCOPE_REQUIRED",
      "this request's credential does not hold the approve scope, which this route requires",
      { "WWW-Authenticate": 'Bearer error="insufficient_scope", scope="approve"' },
    ],
    [
      403,
      "SIGN_IN_REQUIRED",
      "this request's credential acts for service system:serviceaccount:legion:legion-worker and no person, so there is nobody to record this write under; run `agent-secrets login` on this machine and try again",
      {},
    ],
  ])("a %i %s is printed with Dispatch's code and words, and exits 1", async (status, code, error, headers) => {
    refusal = { status, code, error, headers };

    const result = await run(["review", "DSP-1", "--version", "3", "--verdict", "approved"]);

    expect(result.code).toBe(1);
    expect(result.stdout).toBe(`${code}: ${error}\n`);
    // Dispatch's own refusal is final: the review was sent once, to the versioned route, and not
    // again.
    expect(posted()).toEqual([
      { path: "/api/v1/artifacts/artifact-spec/versions/3/reviews", body: { state: "approved" } },
    ]);
  });

  test("a review that answered an open approval request says so, for every document the request named, and that the answer carries this document alone", async () => {
    answeredAsk = "ask-9";

    const { code, stdout, stderr } = await run([
      "review",
      "DSP-1",
      "--version",
      "3",
      "--verdict",
      "approved",
    ]);

    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout.split("\n")).toEqual([
      `Recorded the review of spec.md (document id artifact-spec): approved at version 3, by user ${PERSON}, acting through session ses-1.`,
      "It also answered the open approval request (ask ask-9), so Dispatch recorded the same verdict on every document that request named, each at the version the request named for it. Dispatch answered with the review of spec.md alone, so no other document's review is shown here; dispatch doc-read --issue DSP-1 --artifact <document> prints the approval each of them now reads.",
      "",
    ]);
  });

  test("a Dispatch without the versioned route records nothing, and the refusal names the route and /healthz, never a release in progress, after asking no other review route", async () => {
    noRoute = true;
    const base = `http://127.0.0.1:${server.port}`;
    const waits: number[] = [];

    // runDispatchCli takes no wait of its own, and the no-route retries pause 1, 2 and 4 seconds,
    // jittered. The executor it runs takes one (`sleep`), so this row calls it directly with a
    // wait that returns at once. The CLI prints `messageFor` of what it throws, and exits 1 for
    // any error that is no ToolInputError.
    const error: unknown = await executeDispatchTool({
      tool: "dispatch_review",
      args: { issue: "DSP-1", version: 3, verdict: "approved" },
      cwd: stateDir,
      host: "omp",
      sessionId: "ses-1",
      config: { url: base, credential: bearerCredential(TOKEN) },
      env: {},
      sleep: async (ms) => {
        waits.push(ms);
      },
    }).catch((thrown: unknown) => thrown);

    expect(error).toBeInstanceOf(DispatchServiceError);
    expect(error).not.toBeInstanceOf(ToolInputError);
    expect(error).toMatchObject({ code: "NOT_FOUND", status: 404 });
    const printed = messageFor(error);
    expect(printed).toBe(
      `NOT_FOUND: Dispatch at ${base} answered no route for POST /api/v1/artifacts/artifact-spec/versions/3/reviews each time the review was sent, and an answer of no route records no review. This build is older than that route. No other review route was asked: the review route an older build serves takes no version, and it would record the review at a version that build chooses, which may not be version 3. The no-route answer does not name the build; GET ${base}/healthz names the commit of the build that answers it, when that build was stamped with one.`
    );
    expect(printed).not.toContain(NO_ROUTE_ROLLING_RELEASE_MESSAGE);
    // The first attempt and one after each of the three waits, all to the versioned route: none
    // to the review route that takes no version, and no /healthz.
    expect(waits).toHaveLength(3);
    expect(requests.map(({ method, path }) => `${method} ${path}`)).toEqual([
      "GET /api/v1/issues/DSP-1",
      ...Array(4).fill("POST /api/v1/artifacts/artifact-spec/versions/3/reviews"),
    ]);
    expect(posted().map(({ body }) => body)).toEqual(Array(4).fill({ state: "approved" }));
  });
});
