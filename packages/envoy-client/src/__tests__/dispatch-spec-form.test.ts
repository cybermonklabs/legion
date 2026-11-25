import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import type { SpecFormFinding, SpecFormRead } from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { bearerCredential } from "../bearer";
import { runDispatchCli } from "../dispatch-cli";
import { executeDispatchTool, resetAdviceMemory } from "../dispatch-execute";
import { DispatchServiceError } from "../dispatch-http";

// `dispatch spec-form` and the form refusal `dispatch request-approval` prints, against a stub
// Dispatch that answers `GET /api/v1/artifacts/{id}/spec-form` and the approval request route as
// the server answers them (`getArtifactSpecForm` in
// packages/envoy/internal/dispatch/api/spec_form_route.go, and the 409 SPEC_FORM body of
// `specFormRefusal.body` in spec_form.go). The shape is `dispatch-review.test.ts`'s: one stub
// server driven through `runDispatchCli`, with a temporary state directory per row, and the one
// row that reads the thrown error's fields drives the executor the CLI runs.
//
// The tree the stub holds: ACME-1 is the root, with the children ACME-2 (a spec) and ACME-3 (only
// notes.md); ACME-4, a child of ACME-3, holds a spec. ACME-5 holds spec.md, verification.md and
// notes.md, ACME-6 a spec with no version yet beside a Verification document, and ACME-9 is a root
// with no child. Any other key is an issue that does not exist, which the stub answers as the
// server does: the issue's read is 404 NOT_FOUND (`writeHandlerError` in
// packages/envoy/internal/dispatch/api/handler_errors.go), and the list of its documents is 200 and
// empty, since `listArtifacts` reads no issue.

const makeDirectory = temporaryDirectories("dispatch-spec-form");

/** The Dispatch bearer each command, and the executor, is handed. */
const TOKEN = fixtureSecret("dsp");

/** The person the stub's issues are assigned to. */
const PERSON = "ana@example.com";

interface Recorded {
  readonly method: string;
  readonly path: string;
  readonly search: string;
  readonly body: unknown;
}

const requests: Recorded[] = [];

interface StubArtifact {
  readonly id: string;
  readonly slug: string;
  readonly name: string;
  readonly primary: boolean;
  readonly role?: string;
  readonly latest?: number;
}

interface StubIssue {
  readonly key: string;
  readonly title: string;
  readonly status: string;
  readonly parent: string | null;
  readonly artifacts: readonly StubArtifact[];
}

const spec = (key: string, latest = 3): StubArtifact => ({
  id: `${key}-spec`,
  slug: "spec",
  name: "spec.md",
  primary: true,
  latest,
});
const verification = (key: string): StubArtifact => ({
  id: `${key}-verification`,
  slug: "verification-md",
  name: "verification.md",
  primary: false,
  role: "verification",
  latest: 1,
});
const notes = (key: string): StubArtifact => ({
  id: `${key}-notes`,
  slug: "notes-md",
  name: "notes.md",
  primary: false,
  latest: 1,
});

const ISSUES: readonly StubIssue[] = [
  { key: "ACME-1", title: "The Spec", status: "in_progress", parent: null, artifacts: [] },
  {
    key: "ACME-2",
    title: "The queue",
    status: "todo",
    parent: "ACME-1",
    artifacts: [spec("ACME-2")],
  },
  { key: "ACME-3", title: "Notes", status: "todo", parent: "ACME-1", artifacts: [notes("ACME-3")] },
  {
    key: "ACME-4",
    title: "The lease",
    status: "in_progress",
    parent: "ACME-3",
    artifacts: [spec("ACME-4", 1)],
  },
  {
    key: "ACME-5",
    title: "Three documents",
    status: "todo",
    parent: null,
    artifacts: [spec("ACME-5"), verification("ACME-5"), notes("ACME-5")],
  },
  {
    key: "ACME-6",
    title: "A new spec",
    status: "todo",
    parent: null,
    artifacts: [spec("ACME-6", 0), verification("ACME-6")],
  },
  { key: "ACME-9", title: "An empty root", status: "todo", parent: null, artifacts: [] },
];

function artifactBody(issue: StubIssue, artifact: StubArtifact) {
  return {
    id: artifact.id,
    slug: artifact.slug,
    name: artifact.name,
    kind: "doc",
    primary: artifact.primary,
    ...(artifact.role === undefined ? {} : { role: artifact.role }),
    issue_key: issue.key,
    project: "ACME",
    ref_key: `${issue.key}/${artifact.slug}`,
    session_id: null,
    created_at: "2026-11-16T08:00:00Z",
    created_by: { kind: "user", id: PERSON },
    versions:
      artifact.latest === undefined || artifact.latest === 0
        ? []
        : [{ number: artifact.latest, created_at: "2026-11-16T08:00:00Z" }],
    approval: { state: "draft", latest_version: artifact.latest ?? 0 },
  };
}

function issueBody(issue: StubIssue) {
  return {
    key: issue.key,
    project: "ACME",
    title: issue.title,
    status: issue.status,
    labels: [],
    priority: null,
    assignee: PERSON,
    route: null,
    parent: issue.parent,
    blocked_by: [],
    components: [],
    repo: null,
    resolved_repo: null,
    closed_at: null,
    external_links: [],
    primary_artifact_id: issue.artifacts.find((artifact) => artifact.primary)?.id ?? "",
    artifacts: issue.artifacts.map((artifact) => artifactBody(issue, artifact)),
    open_asks: [],
  };
}

function summaryBody(issue: StubIssue) {
  return {
    key: issue.key,
    title: issue.title,
    status: issue.status,
    parent: issue.parent,
    priority: null,
    labels: [],
    open_asks: 0,
    claim: null,
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    updated_at: "2026-11-16T08:00:00Z",
    primary_artifact_id: issue.artifacts.find((artifact) => artifact.primary)?.id ?? "",
    progress: { tasks: null, children: null },
  };
}

/** One finding as the read route and the refusal carry it, with the ways out it gives (`ways`). */
function finding(
  artifactID: string,
  document: string,
  version: number,
  node: string,
  kind: string,
  detail: string,
  ways = ""
): SpecFormFinding {
  return {
    document,
    artifact_id: artifactID,
    version,
    node,
    kind,
    where: `${document} version ${version}`,
    rule: "sentences",
    detail,
    ways,
    excerpt: "Dispatch stores each comment. Dispatch indexes it. Dispatch shows it.",
  };
}

const THREE_SENTENCES = "it has 3 sentences, and a statement has one or two";
const LONG_SENTENCE =
  'a sentence has 26 words, and a sentence has at most 25: "Dispatch refuses a write of the whole text"';
/** An approved document under the root whose approved version Dispatch cannot read, as the server writes it. */
const UNREAD = {
  document: "spec.md",
  artifact_id: "ACME-3-spec",
  issue: "ACME-3",
  version: 1,
  line: "Dispatch cannot read spec.md version 1 of ACME-3, an approved document under the root, so no rule compared its texts. To change that, write a new version of that document and request its approval.",
};

/** What `GET /api/v1/artifacts/{id}/spec-form` answers for one document. */
function formRead(
  artifactID: string,
  name: string,
  version: number,
  scope: string,
  texts: Record<string, number>,
  findings: readonly SpecFormFinding[] = []
): SpecFormRead {
  const inScope = scope === "covered" || scope === "list";
  return {
    artifact_id: artifactID,
    name,
    version,
    in_scope: inScope,
    scope,
    plays: name === "spec.md" ? "spec" : name === "verification.md" ? "verification" : "",
    root: scope === "no_root" || scope === "outside" ? null : "ACME-1",
    texts: {
      statement: 0,
      claim: 0,
      procedure: 0,
      failing_example: 0,
      name: 0,
      purpose: 0,
      definition: 0,
      ...texts,
    },
    findings: [...findings],
    findings_total: findings.length,
    terms: [],
    unread: [],
    refuses_approval: inScope && findings.length > 0,
  };
}

/** The read the stub answers for each document id, set by each row. */
let forms: Record<string, SpecFormRead> = {};

/** What the approval request route answers next: a 201 with `spec_form`, or the 409 body. */
let approval:
  | { readonly kind: "recorded"; readonly specForm?: unknown }
  | { readonly kind: "refused"; readonly body: unknown } = { kind: "recorded" };

const server = Bun.serve({
  port: 0,
  async fetch(request) {
    const url = new URL(request.url);
    const { pathname, search } = url;
    const text = await request.text();
    const body: unknown = text === "" ? undefined : JSON.parse(text);
    requests.push({ method: request.method, path: pathname, search, body });
    if (pathname === "/api/v1/issues" && request.method === "GET") {
      const parent = url.searchParams.get("parent");
      return Response.json(ISSUES.filter((issue) => issue.parent === parent).map(summaryBody));
    }
    const issue = /^\/api\/v1\/issues\/([^/]+)(\/artifacts)?$/.exec(pathname);
    if (issue !== null && request.method === "GET") {
      const held = ISSUES.find((candidate) => candidate.key === issue[1]);
      if (issue[2] !== undefined) {
        return Response.json(
          held === undefined ? [] : held.artifacts.map((artifact) => artifactBody(held, artifact))
        );
      }
      if (held === undefined) {
        return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
      }
      return Response.json(issueBody(held));
    }
    const artifact = /^\/api\/v1\/artifacts\/([^/]+)\/(spec-form|blocks|approval-requests)$/.exec(
      pathname
    );
    if (artifact !== null && artifact[2] === "spec-form" && request.method === "GET") {
      const read = forms[artifact[1] ?? ""];
      if (read === undefined) {
        return Response.json(
          { code: "NO_VERSION", error: "the document has no settled version to read yet" },
          { status: 409 }
        );
      }
      return Response.json(read);
    }
    if (artifact !== null && artifact[2] === "blocks" && request.method === "GET") {
      return Response.json([]);
    }
    if (artifact !== null && artifact[2] === "approval-requests" && request.method === "POST") {
      if (approval.kind === "refused") return Response.json(approval.body, { status: 409 });
      const sent = body as { summary: string };
      return Response.json(
        {
          ask: {
            id: "ask-9",
            issue_key: "ACME-2",
            artifact_id: artifact[1],
            kind: "approval",
            question: `Approve spec.md (version 12)? ${sent.summary}`,
            waiting_on_person: PERSON,
          },
          approval: { state: "awaiting", latest_version: 12, version: 12 },
          artifact_id: artifact[1],
          coverage: null,
          version: 12,
          ...(approval.specForm === undefined ? {} : { spec_form: approval.specForm }),
        },
        { status: 201 }
      );
    }
    return Response.json(
      { error: `unexpected ${request.method} ${pathname}${search}` },
      { status: 500 }
    );
  },
});

afterAll(() => server.stop(true));

let stateDir = "";
beforeEach(() => {
  requests.length = 0;
  forms = {};
  approval = { kind: "recorded" };
  stateDir = makeDirectory();
  resetAdviceMemory();
});

async function run(
  argv: readonly string[],
  env: Readonly<Record<string, string>> = {}
): Promise<{ code: number; stdout: string; stderr: string }> {
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
      ...env,
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

/** The form reads this run sent, each as its path and query. */
function formReads(): string[] {
  return requests
    .filter((recorded) => recorded.path.endsWith("/spec-form"))
    .map(({ path, search }) => `${path}${search}`);
}

/** The 409 SPEC_FORM body the approval request route answers for one broken statement. */
const REFUSAL = {
  code: "SPEC_FORM",
  error:
    "Dispatch refused this approval request, because 1 text breaks the form of The Spec.\n" +
    `- statement ship-at-head in spec.md version 12: ${THREE_SENTENCES}.\n` +
    "Fix each text, then request approval again.",
  findings: [finding("ACME-2-spec", "spec.md", 12, "ship-at-head", "statement", THREE_SENTENCES)],
  findings_total: 1,
};

describe("dispatch spec-form", () => {
  test("a covered document with no finding prints what Dispatch read and that no text breaks the form", async () => {
    forms["ACME-2-spec"] = formRead("ACME-2-spec", "spec.md", 3, "covered", { statement: 1 });

    const { code, stdout, stderr } = await run(["spec-form", "ACME-2", "--artifact", "spec.md"]);

    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toBe(
      "Dispatch read the form of 1 statement, 0 claims, 0 purposes and 0 definitions: no text breaks it.\n"
    );
    expect(formReads()).toEqual(["/api/v1/artifacts/ACME-2-spec/spec-form"]);
  });

  test("a document that holds no text of The Spec says that Dispatch decided nothing about its form", async () => {
    forms["ACME-2-spec"] = formRead("ACME-2-spec", "spec.md", 2, "covered", {});

    const { code, stdout } = await run(["spec-form", "ACME-2", "--artifact", "spec.md"]);

    expect(code).toBe(0);
    expect(stdout).toBe(
      "Dispatch found no text of The Spec in spec.md version 2, so it decided nothing about its form.\n"
    );
  });

  test("an approved document under the root that Dispatch could not read gets its line after what Dispatch read", async () => {
    forms["ACME-2-spec"] = {
      ...formRead("ACME-2-spec", "spec.md", 3, "covered", { statement: 1 }),
      unread: [UNREAD],
    };

    const { code, stdout } = await run(["spec-form", "ACME-2", "--artifact", "spec.md"]);

    expect(code).toBe(0);
    expect(stdout.split("\n")).toEqual([
      "Dispatch read the form of 1 statement, 0 claims, 0 purposes and 0 definitions: no text breaks it.",
      UNREAD.line,
      "",
    ]);
  });

  test("a covered document with findings prints that Dispatch refuses its approval, then each finding as a statement line", async () => {
    forms["ACME-2-spec"] = formRead("ACME-2-spec", "spec.md", 2, "covered", { statement: 2 }, [
      finding("ACME-2-spec", "spec.md", 2, "ship-at-head", "statement", THREE_SENTENCES),
      finding("ACME-2-spec", "spec.md", 2, "one-write", "statement", LONG_SENTENCE),
    ]);

    const { code, stdout } = await run(["spec-form", "ACME-2", "--artifact", "spec.md"]);

    expect(code).toBe(0);
    expect(stdout.split("\n")).toEqual([
      "Dispatch refuses an approval request of spec.md version 2, because 2 texts break the form of The Spec.",
      `- statement ship-at-head: ${THREE_SENTENCES}.`,
      `- statement one-write: ${LONG_SENTENCE}.`,
      "",
    ]);
  });

  test("a statement with two faults is one text that breaks the form, with a line for each fault", async () => {
    forms["ACME-2-spec"] = formRead("ACME-2-spec", "spec.md", 2, "covered", { statement: 1 }, [
      finding("ACME-2-spec", "spec.md", 2, "ship-at-head", "statement", THREE_SENTENCES),
      finding("ACME-2-spec", "spec.md", 2, "ship-at-head", "statement", LONG_SENTENCE),
    ]);

    const { code, stdout } = await run(["spec-form", "ACME-2", "--artifact", "spec.md"]);

    expect(code).toBe(0);
    expect(stdout.split("\n")).toEqual([
      "Dispatch refuses an approval request of spec.md version 2, because 1 text breaks the form of The Spec.",
      `- statement ship-at-head: ${THREE_SENTENCES}.`,
      `- statement ship-at-head: ${LONG_SENTENCE}.`,
      "",
    ]);
  });

  test("a finding that gives ways out prints its detail, then its ways out, as the server's refusal writes them", async () => {
    const stop =
      'a stop in "Web.Config" has no space after it, so Dispatch cannot find the end of the sentence';
    const ways = "Put a space after the stop, or put the name in code font";
    forms["ACME-2-spec"] = formRead("ACME-2-spec", "spec.md", 2, "covered", { statement: 1 }, [
      finding("ACME-2-spec", "spec.md", 2, "ship-at-head", "statement", stop, ways),
    ]);

    const { code, stdout } = await run(["spec-form", "ACME-2", "--artifact", "spec.md"]);

    expect(code).toBe(0);
    expect(stdout.split("\n")).toEqual([
      "Dispatch refuses an approval request of spec.md version 2, because 1 text breaks the form of The Spec.",
      `- statement ship-at-head: ${stop}. ${ways}.`,
      "",
    ]);
  });

  test("a read that lists fewer findings than it counts says how many it lists, and that more texts can break the form", async () => {
    // The read route lists at most 500 findings and counts them all in findings_total.
    const listed = [
      finding("ACME-2-spec", "spec.md", 2, "ship-at-head", "statement", THREE_SENTENCES),
      finding("ACME-2-spec", "spec.md", 2, "one-write", "statement", LONG_SENTENCE),
    ];
    forms["ACME-2-spec"] = {
      ...formRead("ACME-2-spec", "spec.md", 2, "covered", { statement: 3 }, listed),
      findings_total: 3,
    };
    forms["ACME-3-notes"] = {
      ...formRead("ACME-3-notes", "notes.md", 2, "not_spec", { statement: 3 }, listed),
      findings_total: 3,
    };

    const covered = await run(["spec-form", "ACME-2", "--artifact", "spec.md"]);
    const outside = await run(["spec-form", "ACME-3", "--artifact", "notes.md"]);

    expect(covered.code).toBe(0);
    expect(covered.stdout.split("\n")).toEqual([
      "Dispatch refuses an approval request of spec.md version 2, because 2 texts or more break the form of The Spec.",
      `- statement ship-at-head: ${THREE_SENTENCES}.`,
      `- statement one-write: ${LONG_SENTENCE}.`,
      "Dispatch lists the first 2 of the 3 findings.",
      "",
    ]);
    expect(outside.code).toBe(0);
    expect(outside.stdout.split("\n")).toEqual([
      "notes.md is not the spec or the Verification document of its issue, so Dispatch refuses no approval request of it for its form.",
      "The findings below are for you to read.",
      `- statement ship-at-head: ${THREE_SENTENCES}.`,
      `- statement one-write: ${LONG_SENTENCE}.`,
      "Dispatch lists the first 2 of the 3 findings.",
      "",
    ]);
  });

  test.each([
    [
      "no_root",
      "This server names no root of The Spec, so Dispatch refuses no approval request for its form.",
    ],
    [
      "outside",
      "spec.md is not under a root of The Spec, so Dispatch refuses no approval request of it for its form.",
    ],
    [
      "not_spec",
      "spec.md is not the spec or the Verification document of its issue, so Dispatch refuses no approval request of it for its form.",
    ],
    [
      "root",
      "spec.md belongs to a root of The Spec, so Dispatch refuses no approval request of it for its form.",
    ],
  ])("a document of scope %s says why Dispatch refuses nothing for it, and still lists its findings", async (scope, line) => {
    forms["ACME-2-spec"] = formRead("ACME-2-spec", "spec.md", 2, scope, { statement: 1 }, [
      finding("ACME-2-spec", "spec.md", 2, "ship-at-head", "statement", THREE_SENTENCES),
    ]);

    const { code, stdout } = await run(["spec-form", "ACME-2", "--artifact", "spec.md"]);

    expect(code).toBe(0);
    expect(stdout.split("\n")).toEqual([
      line,
      "The findings below are for you to read.",
      `- statement ship-at-head: ${THREE_SENTENCES}.`,
      "",
    ]);
  });

  test("a document out of scope with no finding says why, then what Dispatch read", async () => {
    forms["ACME-2-spec"] = formRead("ACME-2-spec", "spec.md", 2, "no_root", { statement: 2 });

    const { code, stdout } = await run(["spec-form", "ACME-2", "--artifact", "spec.md"]);

    expect(code).toBe(0);
    expect(stdout.split("\n")).toEqual([
      "This server names no root of The Spec, so Dispatch refuses no approval request for its form.",
      "Dispatch read the form of 2 statements, 0 claims, 0 purposes and 0 definitions: no text breaks it.",
      "",
    ]);
  });

  test("--version reads that version of the document", async () => {
    forms["ACME-2-spec"] = formRead("ACME-2-spec", "spec.md", 2, "covered", { statement: 1 });

    const { code } = await run(["spec-form", "ACME-2", "--artifact", "spec.md", "--version", "2"]);

    expect(code).toBe(0);
    expect(formReads()).toEqual(["/api/v1/artifacts/ACME-2-spec/spec-form?version=2"]);
  });

  test("--version with an issue, or a ref of an issue, and no --artifact is refused before any request, since a version belongs to one document", async () => {
    forms["ACME-5-spec"] = formRead("ACME-5-spec", "spec.md", 3, "covered", { statement: 1 });
    forms["ACME-5-verification"] = formRead(
      "ACME-5-verification",
      "verification.md",
      3,
      "covered",
      {
        claim: 1,
      }
    );

    for (const argv of [
      ["spec-form", "ACME-5", "--version", "1"],
      ["spec-form", "--issue", "ACME-5", "--version", "1"],
      ["spec-form", "--ref", "dispatch://ACME-5", "--version", "1"],
      ["spec-form", "--issue", "ACME-5", "--ref", "dispatch://ACME-5", "--version", "1"],
    ]) {
      requests.length = 0;
      const { code, stdout } = await run(argv);

      expect(code, argv.join(" ")).toBe(2);
      expect(stdout, argv.join(" ")).toContain(
        "A version number belongs to one document, and an issue has several: with version, name the document with artifact (--artifact)."
      );
      expect(requests, argv.join(" ")).toEqual([]);
    }
    requests.length = 0;
    const error: unknown = await executeDispatchTool({
      tool: "dispatch_spec_form",
      args: { issue: "ACME-5", version: 1 },
      cwd: stateDir,
      host: "omp",
      sessionId: "ses-1",
      config: {
        url: `http://127.0.0.1:${server.port}`,
        credential: bearerCredential(TOKEN),
      },
      env: {},
    }).catch((thrown: unknown) => thrown);
    expect(String(error)).toContain("with version, name the document with artifact (--artifact)");
    expect(requests).toEqual([]);
  });

  test("with no --artifact, it prints the spec and the Verification document of the issue and leaves notes.md out", async () => {
    forms["ACME-5-spec"] = formRead("ACME-5-spec", "spec.md", 3, "covered", {
      statement: 2,
      purpose: 1,
    });
    forms["ACME-5-verification"] = formRead(
      "ACME-5-verification",
      "verification.md",
      1,
      "covered",
      { claim: 1, procedure: 1 },
      [finding("ACME-5-verification", "verification.md", 1, "v-one", "claim", LONG_SENTENCE)]
    );
    forms["ACME-5-notes"] = formRead("ACME-5-notes", "notes.md", 1, "not_spec", { statement: 1 });

    const { code, stdout } = await run(["spec-form", "ACME-5"]);

    expect(code).toBe(0);
    expect(stdout.split("\n")).toEqual([
      "spec.md version 3:",
      "Dispatch read the form of 2 statements, 0 claims, 1 purpose and 0 definitions: no text breaks it.",
      "verification.md version 1:",
      "Dispatch refuses an approval request of verification.md version 1, because 1 text breaks the form of The Spec.",
      `- claim v-one: ${LONG_SENTENCE}.`,
      "",
    ]);
    expect(formReads()).toEqual([
      "/api/v1/artifacts/ACME-5-spec/spec-form",
      "/api/v1/artifacts/ACME-5-verification/spec-form",
    ]);
  });

  test("with no --artifact, a document with no version yet is named, and the next one is read", async () => {
    forms["ACME-6-verification"] = formRead(
      "ACME-6-verification",
      "verification.md",
      1,
      "covered",
      { claim: 1 }
    );

    const { code, stdout } = await run(["spec-form", "ACME-6"]);

    expect(code).toBe(0);
    expect(stdout.split("\n")).toEqual([
      "spec.md has no version yet, so Dispatch read no text in it.",
      "verification.md version 1:",
      "Dispatch read the form of 0 statements, 1 claim, 0 purposes and 0 definitions: no text breaks it.",
      "",
    ]);
  });

  test("an issue that does not exist is refused as dispatch read refuses it, and no document is read", async () => {
    const read = await run(["read", "--issue", "ACME-999"]);
    expect(read).toEqual({ code: 1, stdout: "NOT_FOUND: not found\n", stderr: "" });

    for (const argv of [
      ["spec-form", "--issue", "ACME-999"],
      ["spec-form", "ACME-999"],
      ["spec-form", "--root", "ACME-999"],
    ]) {
      requests.length = 0;

      const answered = await run(argv);

      expect(answered, argv.join(" ")).toEqual(read);
      expect(formReads(), argv.join(" ")).toEqual([]);
    }
  });

  test("--root lists each issue under the root and each document the root covers, then the counts", async () => {
    forms["ACME-2-spec"] = formRead("ACME-2-spec", "spec.md", 3, "covered", { statement: 2 });
    forms["ACME-4-spec"] = formRead("ACME-4-spec", "spec.md", 1, "covered", { statement: 1 }, [
      finding("ACME-4-spec", "spec.md", 1, "ship-at-head", "statement", THREE_SENTENCES),
    ]);

    const { code, stdout, stderr } = await run(["spec-form", "--root", "ACME-1"]);

    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout.split("\n")).toEqual([
      "ACME-2 [todo] The queue: 2 statements, 0 claims, 0 purposes and 0 definitions, with 0 findings.",
      "  spec.md version 3 (scope covered): 2 statements, 0 claims, 0 purposes and 0 definitions, with 0 findings.",
      "ACME-3 [todo] Notes: no spec and no Verification document.",
      "  ACME-4 [in_progress] The lease: 1 statement, 0 claims, 0 purposes and 0 definitions, with 1 finding.",
      "    spec.md version 1 (scope covered): 1 statement, 0 claims, 0 purposes and 0 definitions, with 1 finding.",
      "Dispatch read 3 issues and 2 documents under ACME-1.",
      "",
    ]);
    expect(formReads()).toEqual([
      "/api/v1/artifacts/ACME-2-spec/spec-form",
      "/api/v1/artifacts/ACME-4-spec/spec-form",
    ]);
  });

  test("--root over a root with no child says it found no issue", async () => {
    const { code, stdout } = await run(["spec-form", "--root", "ACME-9"]);

    expect(code).toBe(0);
    expect(stdout).toBe("Dispatch found no issue under ACME-9, so it read no document.\n");
    expect(formReads()).toEqual([]);
  });

  test("--root walks the root it names when LEGION_ISSUE names another issue", async () => {
    const { code, stdout } = await run(["spec-form", "--root", "ACME-9"], {
      LEGION_ISSUE: "ACME-5",
    });

    expect(code).toBe(0);
    expect(stdout).toBe("Dispatch found no issue under ACME-9, so it read no document.\n");
  });

  test("--root beside --issue, --artifact or --version is refused before any request", async () => {
    for (const extra of [
      ["--issue", "ACME-2"],
      ["--artifact", "spec.md"],
      ["--version", "2"],
    ]) {
      requests.length = 0;
      const { code, stdout } = await run(["spec-form", "--root", "ACME-1", ...extra]);

      expect(code, extra.join(" ")).toBe(2);
      expect(stdout, extra.join(" ")).toContain(
        "root walks every issue under that root: name no issue, project, artifact, version or ref with it."
      );
      expect(requests, extra.join(" ")).toEqual([]);
    }
  });
});

describe("dispatch request-approval and the form of The Spec", () => {
  test("a 409 SPEC_FORM exits 1 and prints the code, each text and the next step", async () => {
    approval = { kind: "refused", body: REFUSAL };

    const { code, stdout } = await run([
      "request-approval",
      "ACME-2",
      "--summary",
      "The queue keeps each message for 72 hours.",
    ]);

    expect(code).toBe(1);
    expect(stdout).toBe(`SPEC_FORM: ${REFUSAL.error}\n`);
  });

  test("the refusal it throws starts with SPEC_FORM and carries the findings", async () => {
    approval = { kind: "refused", body: REFUSAL };

    const error: unknown = await executeDispatchTool({
      tool: "dispatch_request_approval",
      args: { issue: "ACME-2", summary: "The queue keeps each message for 72 hours." },
      cwd: stateDir,
      host: "omp",
      sessionId: "ses-1",
      config: {
        url: `http://127.0.0.1:${server.port}`,
        credential: bearerCredential(TOKEN),
      },
      env: {},
    }).catch((thrown: unknown) => thrown);

    expect(error).toBeInstanceOf(DispatchServiceError);
    const refusal = error as DispatchServiceError;
    expect(refusal.code).toBe("SPEC_FORM");
    expect(refusal.message).toStartWith("SPEC_FORM: Dispatch refused this approval request");
    expect(refusal.findings).toEqual(REFUSAL.findings);
    expect(refusal.changesReason).toBeUndefined();
  });

  test("a request Dispatch recorded prints the line on the form that its answer carries", async () => {
    approval = {
      kind: "recorded",
      specForm: {
        root: "ACME-1",
        texts: { statement: 59, claim: 0, procedure: 0, purpose: 40, name: 40, definition: 0 },
      },
    };

    const { code, stdout } = await run([
      "request-approval",
      "ACME-2",
      "--summary",
      "The queue keeps each message for 72 hours.",
    ]);

    expect(code).toBe(0);
    expect(stdout.split("\n")).toContain(
      "Dispatch read the form of 59 statements, 0 claims, 40 purposes and 0 definitions: no text breaks it."
    );
  });

  test("a recorded request prints the line of each approved document that Dispatch could not read", async () => {
    approval = {
      kind: "recorded",
      specForm: { root: "ACME-1", texts: { statement: 1 }, unread: [UNREAD] },
    };

    const { code, stdout } = await run([
      "request-approval",
      "ACME-2",
      "--summary",
      "The queue keeps each message for 72 hours.",
    ]);

    expect(code).toBe(0);
    expect(stdout.split("\n")).toContain(UNREAD.line);
  });

  test("a recorded request whose documents hold no text of The Spec says Dispatch decided nothing", async () => {
    approval = { kind: "recorded", specForm: { root: "ACME-1", texts: {} } };

    const { code, stdout } = await run([
      "request-approval",
      "ACME-2",
      "--summary",
      "The queue keeps each message for 72 hours.",
    ]);

    expect(code).toBe(0);
    expect(stdout.split("\n")).toContain(
      "Dispatch found no text of The Spec in spec.md version 12, so it decided nothing about its form."
    );
  });

  test("a request of a document no root covers prints no line on the form", async () => {
    const { code, stdout } = await run([
      "request-approval",
      "ACME-2",
      "--summary",
      "The queue keeps each message for 72 hours.",
    ]);

    expect(code).toBe(0);
    expect(stdout).not.toContain("the form of");
    expect(stdout).not.toContain("about its form");
  });
});
