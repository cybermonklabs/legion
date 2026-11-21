// Red test of the acceptance round at fc72c17788 (place it at
// packages/envoy-client/src/__tests__/spec-form-ref-version.test.ts).
//
// `dispatch spec-form --version N` with a --ref that names an issue and no document reads the latest
// versions of the issue's spec and Verification document and drops N, as `--issue K --version N`
// did before this round. The README of envoy-client says that `--version` needs `--artifact` "or a
// `--ref` that names the document", and that the argument check refuses it beside an issue alone
// before any request. A ref of an issue names no document, so the same check must refuse it.
// Control rows: a ref that names a document (`/spec`, `/artifact/spec.md`) with --version is taken.
import { afterAll, beforeEach, expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";
import { forgetKeyedLayerMemory } from "../dispatch-http";

const makeDirectory = temporaryDirectories("spec-form-ref-version");

/** The Dispatch bearer each command is handed. */
const TOKEN = fixtureSecret("dsp");
const requests: string[] = [];

const ARTIFACTS = [
  {
    id: "ACME-5-spec",
    slug: "spec",
    name: "spec.md",
    kind: "doc",
    primary: true,
    issue_key: "ACME-5",
  },
  {
    id: "ACME-5-verification",
    slug: "verification-md",
    name: "verification.md",
    kind: "doc",
    primary: false,
    role: "verification",
    issue_key: "ACME-5",
  },
];

function formRead(id: string, name: string, version: number) {
  return {
    artifact_id: id,
    name,
    version,
    scope: "covered",
    refuses_approval: false,
    texts: { statement: 1 },
    terms: 0,
    findings: [],
    findings_total: 0,
  };
}

const server = Bun.serve({
  port: 0,
  fetch(request) {
    const url = new URL(request.url);
    requests.push(`${request.method} ${url.pathname}${url.search}`);
    if (url.pathname === "/api/v1/issues/ACME-5") {
      return Response.json({
        key: "ACME-5",
        title: "Two versions",
        status: "todo",
        artifacts: ARTIFACTS,
      });
    }
    if (url.pathname === "/api/v1/issues/ACME-5/artifacts") return Response.json(ARTIFACTS);
    const form = url.pathname.match(/^\/api\/v1\/artifacts\/([^/]+)\/spec-form$/);
    if (form?.[1] !== undefined) {
      const artifact = ARTIFACTS.find((each) => each.id === form[1]);
      if (artifact === undefined)
        return Response.json({ code: "NOT_FOUND", error: "no" }, { status: 404 });
      return Response.json(
        formRead(artifact.id, artifact.name, Number(url.searchParams.get("version") ?? 3))
      );
    }
    return Response.json(
      { code: "NOT_FOUND", error: `no stub for ${url.pathname}` },
      { status: 404 }
    );
  },
});
afterAll(() => server.stop(true));

let stateDir = "";
beforeEach(() => {
  requests.length = 0;
  stateDir = makeDirectory();
  resetAdviceMemory();
  forgetKeyedLayerMemory();
});

async function run(argv: readonly string[]): Promise<{ code: number; stdout: string }> {
  let stdout = "";
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
        stdout += text;
      },
      readText: () => "",
      cwd: stateDir,
    }
  );
  return { code, stdout };
}

test("--version with a --ref that names an issue and no document is refused before any request", async () => {
  const { code, stdout } = await run(["spec-form", "--ref", "dispatch://ACME-5", "--version", "1"]);
  expect(stdout).toContain("with version, name the document with artifact (--artifact)");
  expect(code).toBe(2);
  expect(requests).toEqual([]);
});

for (const ref of ["dispatch://ACME-5/spec", "dispatch://ACME-5/artifact/spec.md"]) {
  test(`control: --version with a --ref that names a document (${ref}) reads that version`, async () => {
    const { code } = await run(["spec-form", "--ref", ref, "--version", "1"]);
    expect(code).toBe(0);
    expect(requests.filter((each) => each.includes("/spec-form"))).toEqual([
      "GET /api/v1/artifacts/ACME-5-spec/spec-form?version=1",
    ]);
  });
}
