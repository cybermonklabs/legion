import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { CLAIM_RUN_VARIABLE, RESERVE_VARIABLE, type Registration, runDigestOf } from "../reserve";
import { INPUTS_VERSION } from "../s10-inputs-io";
import { admissionOf, JUDGE_COMMAND, main } from "../s10-judge-admit";

/**
 * The claim's judging stage. Its driver, `s10_judge.py`, and the program its sandbox runs,
 * `s10_sandbox.py`, are tested by `s10_judge_test.py` under `python3 -m unittest`, which the first
 * test here runs, so this directory's one `bun test` runs them. The rest test the admission the
 * driver asks before it reads anything (`s10-judge-admit.ts`), over an inputs stage written here
 * in the shapes `s10-inputs-io.ts` reads back, with a registration made for this test's own digest
 * over a bundle root of its own, as `s10-inputs.test.ts` makes one.
 */

const DIRECTORY = join(import.meta.dir, "..");
const REFERENCE = "registration-comment-1";
const START = "start-1";
const ASK = "00000000-0000-4000-8000-0000000000a1";
const DEVIATION = "00000000-0000-4000-8000-0000000000b1";

const sha256 = (text: string): string => createHash("sha256").update(text, "utf8").digest("hex");

let scratch: string;
let root: string;
let inputs: string;
let files: { code: string; skill: string; never: string };
const env: Record<string, string | undefined> = {};

/** The inputs stage's files: one ready request, one deviation, and the page the request's search used. */
function inputsFiles(changes: { state?: string; claimRun?: string } = {}): Record<string, string> {
  const markdown = "# The spec\n";
  const page = {
    sha256: "f".repeat(64),
    hits: [{ id: "ACME-9", kind: "issue", rank: 0.5 }],
    reads: [{ query: "The work of ACME-1", offset: 0, readAt: "2026-11-21T00:00:00Z" }],
  };
  const query = (text: string) => ({
    query: text,
    pages: [{ sha256: page.sha256, offset: 0 }],
    hits: [],
  });
  const counts = {
    askHits: 0,
    candidates: 0,
    held: 0,
    reserveErrors: 0,
    kept: 0,
    droppedLaterEvent: { log: 0, "no-event-through-pin": 0, "after-pin": 0 },
  };
  const request = {
    version: INPUTS_VERSION,
    askId: ASK,
    issueKey: "ACME-1",
    group: "report",
    cases: 1,
    judged: { eventId: 50, type: "ask.opened", createdAt: "2026-11-17T10:00:00.000001Z" },
    ask: {
      id: ASK,
      question: "Approve the spec?",
      approval: { artifact_id: "doc-a", name: "spec.md", version: 2 },
    },
    issue: {
      key: "ACME-1",
      title: "The work of ACME-1",
      status: "todo",
      labels: [],
      parent: null,
      primary_artifact_id: null,
    },
    issueAsOfEventId: 10,
    documents: [
      { artifact_id: "doc-a", name: "spec.md", version: 2, markdown, sha256: sha256(markdown) },
    ],
    fact: { holds: false, legion: false, primary: false, root: true, status: "todo" },
    factHold: false,
    search: { title: query("The work of ACME-1"), question: query("Approve the spec?") },
    candidates: {},
    counts,
    dropScanThrough: 120,
  };
  const requestText = `${JSON.stringify(request, null, 2)}\n`;
  const pageText = `${JSON.stringify(page, null, 2)}\n`;
  const line = (askId: string, ready: boolean) => ({
    askId,
    issueKey: ready ? "ACME-1" : "ACME-5",
    group: "report",
    cases: 1,
    judged: ready ? request.judged : null,
    state: ready ? "ready" : "deviation",
    reason: ready ? null : "the log gives no title of ACME-5 as of event 7",
    requestSha256: ready ? sha256(requestText) : null,
    counts: ready ? counts : null,
    dropScanThrough: ready ? 120 : null,
  });
  const inventory = [line(ASK, true), line(DEVIATION, false)]
    .map((one) => `${JSON.stringify(one)}\n`)
    .join("");
  const record = {
    stage: "s10-inputs",
    version: INPUTS_VERSION,
    state: changes.state ?? "written",
    claimRun: {
      id: changes.claimRun ?? START,
      snapshotSha256: "1".repeat(64),
      corpusSha256: "2".repeat(64),
      treesSha256: "3".repeat(64),
    },
    reference: REFERENCE,
    runDigest: "4".repeat(64),
    person: "person@example.test",
    throughEventId: 100,
    draw: { drawSha256: "5".repeat(64), slicesSha256: "6".repeat(64) },
    reserveAsk: "7".repeat(64),
    directories: { build: "/claim/build", draw: "/claim/draw" },
    startedAt: "2026-11-21T00:00:00Z",
    finishedAt: "2026-11-21T00:10:00Z",
    dropScanThrough: 120,
    dropScanPages: { [page.sha256]: sha256(pageText) },
    counts: { ...counts, requests: 2, ready: 1, deviations: 1, cases: 2, pages: 1 },
    inventorySha256: sha256(inventory),
    requests: { [ASK]: sha256(requestText) },
    pages: { [page.sha256]: sha256(pageText) },
  };
  return {
    "s10-inputs.json": `${JSON.stringify(record, null, 2)}\n`,
    "inventory.jsonl": inventory,
    [`requests/${ASK}.json`]: requestText,
    [`pages/${page.sha256}.json`]: pageText,
  };
}

async function writeInputs(written: Record<string, string>): Promise<void> {
  await rm(inputs, { recursive: true, force: true });
  for (const [name, text] of Object.entries(written)) {
    await mkdir(dirname(join(inputs, name)), { recursive: true });
    await writeFile(join(inputs, name), text, "utf8");
  }
}

/** The driver's own arguments, as the claim's command names them. */
function args(): string[] {
  return [
    "--inputs",
    inputs,
    "--out",
    join(scratch, "judged"),
    "--code",
    files.code,
    "--skill",
    files.skill,
    "--never-decide",
    files.never,
    "--image",
    `registry.example.test/sandbox@sha256:${"a".repeat(64)}`,
  ];
}

/** The registration of this test's run: its digest, over its own bundle root, under the read. */
async function registration(): Promise<Registration> {
  const run = await runDigestOf({ argv: ["python3", JUDGE_COMMAND, ...args()], env }, root);
  return { reads: [{ reference: REFERENCE, run }], ended: null };
}

beforeAll(async () => {
  scratch = await mkdtemp(join(tmpdir(), "s10-judge-"));
  root = join(scratch, "bundle");
  inputs = join(scratch, "inputs");
  await mkdir(join(root, "scripts/approval-eval"), { recursive: true });
  await writeFile(join(root, "scripts/approval-eval/s10_judge.py"), "the driver\n", "utf8");
  files = {
    code: join(scratch, "code.txt"),
    skill: join(scratch, "SKILL.md"),
    never: join(scratch, "never.md"),
  };
  for (const [name, path] of Object.entries(files)) await writeFile(path, `the ${name}\n`, "utf8");
  env[RESERVE_VARIABLE] = `read:${REFERENCE}`;
  env[CLAIM_RUN_VARIABLE] = START;
});

afterAll(async () => {
  await rm(scratch, { recursive: true, force: true });
});

describe("s10_judge.py and s10_sandbox.py", () => {
  test("pass their own tests under python3 -m unittest", () => {
    const run = Bun.spawnSync(
      [
        "python3",
        "-m",
        "unittest",
        "discover",
        "-s",
        join(DIRECTORY, "__tests__"),
        "-p",
        "s10_judge_test.py",
      ],
      {
        cwd: DIRECTORY,
        env: { ...process.env, PYTHONDONTWRITEBYTECODE: "1" },
        stdout: "pipe",
        stderr: "pipe",
      }
    );
    const report = run.stderr.toString();
    if (run.exitCode !== 0) console.error(report);
    expect(run.exitCode).toBe(0);
    expect(report).toMatch(/^Ran \d+ tests/m);
  }, 120_000);
});

describe("s10-judge-admit.ts", () => {
  test("admits the claim run's judging over its written inputs, each file by its sha256", async () => {
    const written = inputsFiles();
    await writeInputs(written);
    const read = await registration();
    const admission = await admissionOf(args(), { registration: read, root, env });
    expect(admission.reference).toBe(REFERENCE);
    expect(admission.claimRun).toBe(START);
    expect(admission.runDigest).toBe(read.reads[0]?.run as string);
    expect(admission.out).toBe(join(scratch, "judged"));
    expect(admission.inputs.stageSha256).toBe(sha256(written["s10-inputs.json"] as string));
    const named = Object.fromEntries(
      Object.entries(written)
        .filter(([name]) => name !== "s10-inputs.json")
        .map(([name, text]) => [name, sha256(text)])
    );
    expect(admission.inputs.files).toEqual(named);
  });

  test("admits nothing outside the registered read", async () => {
    await writeInputs(inputsFiles());
    const read = await registration();
    const held = { ...env, [RESERVE_VARIABLE]: undefined };
    await expect(admissionOf(args(), { registration: read, root, env: held })).rejects.toThrow(
      "runs only under its registered read"
    );
    await expect(
      admissionOf(args(), { registration: { ...read, ended: "the claim's result" }, root, env })
    ).rejects.toThrow("the reserve ended at the claim's result");
  });

  test("covers the frozen code's bytes: a changed --code is another run", async () => {
    await writeInputs(inputsFiles());
    const read = await registration();
    await writeFile(files.code, "other code\n", "utf8");
    try {
      await expect(admissionOf(args(), { registration: read, root, env })).rejects.toThrow(
        "registers registration-comment-1 for other runs"
      );
    } finally {
      await writeFile(files.code, "the code\n", "utf8");
    }
  });

  test("takes only its own claim run's finished inputs, each file as the record names it", async () => {
    const read = await registration();
    await writeInputs(inputsFiles({ claimRun: "start-2" }));
    await expect(admissionOf(args(), { registration: read, root, env })).rejects.toThrow(
      "written by the claim run start-2"
    );
    await writeInputs(inputsFiles({ state: "started" }));
    await expect(admissionOf(args(), { registration: read, root, env })).rejects.toThrow(
      "never finished"
    );
    const changed = inputsFiles();
    await writeInputs(changed);
    await writeFile(
      join(inputs, `requests/${ASK}.json`),
      `${changed[`requests/${ASK}.json`]} `,
      "utf8"
    );
    await expect(admissionOf(args(), { registration: read, root, env })).rejects.toThrow(
      "is not the file the inputs stage's record names"
    );
  });

  test("refuses an --out that is relative or inside the repository, and a run claim.ts does not register", async () => {
    await writeInputs(inputsFiles());
    const read = await registration();
    const out = join(scratch, "judged");
    const relative = args().map((one) => (one === out ? "relative/out" : one));
    await expect(admissionOf(relative, { registration: read, root, env })).rejects.toThrow(
      "absolute path"
    );
    const inside = args().map((one) => (one === out ? join(DIRECTORY, "judged") : one));
    await expect(admissionOf(inside, { registration: read, root, env })).rejects.toThrow(
      "outside the repository workspace"
    );
    const saved = process.env[RESERVE_VARIABLE];
    process.env[RESERVE_VARIABLE] = `read:${REFERENCE}`;
    const errors: string[] = [];
    const error = console.error;
    console.error = (...parts: unknown[]) => errors.push(parts.join(" "));
    try {
      expect(await main(args())).toBe(2);
    } finally {
      console.error = error;
      if (saved === undefined) delete process.env[RESERVE_VARIABLE];
      else process.env[RESERVE_VARIABLE] = saved;
    }
    expect(errors.join("\n")).toContain("This run's digest is");
  });
});
