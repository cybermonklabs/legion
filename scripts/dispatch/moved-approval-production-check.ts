import { parseArgs } from "node:util";
import { z } from "zod";
import {
  DISPATCH_TOOL_DEADLINE_MS,
  DispatchClient,
} from "../../packages/envoy-client/src/dispatch-http";
import {
  type Category,
  type DispatchConfig,
  EXIT_CODE,
  errorCategory,
  Health,
  readJson,
  resolveDispatch,
  UsageError,
  type Verdict,
} from "./production-check-runtime.ts";

/** A production check of the report a document version write carries when it takes an open
 * approval request back from the person it was waiting on (`moved_approval_request`), and anyone
 * can run it again. It writes in production: it makes a scratch issue of its own in the project
 * `--project` names, uploads its documents, requests approval of them, writes the versions that
 * move that request, and closes the scratch issue again in a `finally`.
 *
 * Everything it prints is from an allowlist: the issue key it made (validated, and of the project
 * the flag named), the names of the documents it uploaded, version numbers, the ask id of the
 * request it opened (validated as a uuid), a verdict word, a fixed failure category and the
 * validated build commit. A value the server answers that is none of its own is printed as
 * `other`. It never prints a URL, the token, a response body, an exception's text, an issue
 * title, the output of the `dispatch` command it runs, or anything of an issue it did not itself
 * create, since this repository is public and those carry private hosts and names. */

const PROJECT_KEY = /^[A-Z][A-Z0-9]*$/;
const ISSUE_KEY = /^[A-Z][A-Z0-9]*-[1-9][0-9]*$/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** What a key, a name or an id the server answered is printed as when it is none of the check's
 *  own: the allowlist admits the check's own values and this one word for everything else. */
const OTHER = "other";

/** The documents of the scratch issue: its spec, the Verification document the approval request
 *  is asked about beside it, and the third document no request names. */
const SPEC = "spec.md";
const VERIFICATION = "verification.md";
const NOTES = "notes.md";
const DOCUMENT_NAMES: readonly string[] = [SPEC, VERIFICATION, NOTES];

/** The assertions a whole run makes: the report of the write that moved the request, the
 *  negative control, and the close of the scratch issue. */
const ASSERTIONS = 3;

/** Everything the check writes into production. The texts are its own, the title and the closing
 *  reason name INFRA-3096 so a person who meets the issue knows what made it, and each inserted
 *  sentence is plain prose: a typed block would settle into a second version of its own and move
 *  the request again under the step's feet. */
const WRITTEN = {
  title: "Scratch issue of the INFRA-3096 production check of the moved approval request report",
  spec: "# Design\n\nThe design, as it stands.\n",
  verification: "# Verification\n\nOne check.\n",
  notes: "# Notes\n\nOne note.\n",
  summary: "The INFRA-3096 production check asks for approval of its own scratch documents.",
  sentence: "One more check the person has not seen.",
  note: "One more note.",
  reason:
    "The INFRA-3096 production check of the moved approval request report made this scratch " +
    "issue, and its run is over, so it closes it again.",
} as const;

/** The two parts of the line `dispatch doc-edit` prints from the report (`renderAdvice` in
 *  `packages/envoy-client/src/dispatch-execute.ts`). They stand in one sentence of one line and
 *  nowhere else, so output that holds both printed that line. */
const CLI_LINE: readonly string[] = ["moved the open approval request", "Not yours to hand back?"];

const USAGE = {
  arguments: "unknown option, missing option value or stray argument",
  project: "--project takes the key of the project the check writes its scratch issue in",
} as const;

/** `POST /api/v1/issues` as the client answers it, with `created` for the 201 it read. */
const CreatedIssue = z.looseObject({
  key: z.string(),
  primary_artifact_id: z.string(),
  created: z.boolean(),
});

/** `POST /api/v1/issues/{key}/artifacts`: the document and the version the upload stored. */
const Uploaded = z.looseObject({
  artifact: z.looseObject({ id: z.string() }),
  version: z.looseObject({ number: z.int().min(1) }),
});

/** `POST /api/v1/artifacts/{id}/approval-requests`: the ask the person was handed. */
const OpenedRequest = z.looseObject({
  ask: z.looseObject({ id: z.string() }).nullable(),
  version: z.int().min(1),
});

/** One document of the report, as `MovedApprovalDocument` has it. */
const MovedDocument = z.looseObject({
  name: z.string(),
  requested_version: z.int().min(1),
  version: z.int().min(1),
});

/** The report itself, as `MovedApprovalRequest` has it: the ask, the place it stands in, and
 *  every document it names. `documents` is null on a request the server cannot describe. */
const MovedReport = z.looseObject({
  ask_id: z.string(),
  issue: z.string().optional(),
  project: z.string().optional(),
  documents: z.array(MovedDocument).nullable(),
});

/** `POST /api/v1/artifacts/{id}/edits`: the version it stored, and its advice with the report. */
const EditAnswer = z.looseObject({
  version: z.looseObject({ number: z.int().min(1) }).nullable(),
  advice: z.looseObject({ moved_approval_request: MovedReport.optional() }).optional(),
});

/** `PATCH /api/v1/issues/{key}`: the status the close left. */
const ClosedIssue = z.looseObject({ status: z.string() });

interface Options {
  readonly project: string;
}

/** The state of the request the check opened, as the write that moved it should report it: the
 *  ask the request stands on, the issue it was opened in, and the document the write stored with
 *  the version the request asks about beside the version it stands at now. */
export interface ExpectedMove {
  readonly askID: string;
  readonly issueKey: string;
  readonly name: string;
  readonly requestedVersion: number;
  readonly version: number;
}

/** One assertion's verdict and the line that says what it saw. */
interface Judgement {
  readonly verdict: "pass" | "fail";
  readonly line: string;
}

/** The writes and reads the steps make, each answering the JSON it received. */
interface DispatchWrites {
  /** `GET /healthz`, whose body is read on 200 and on 503. */
  healthz(): Promise<unknown>;
  /** `POST /api/v1/issues`: the scratch issue, with its spec, forced past any near-duplicate. */
  createIssue(project: string): Promise<unknown>;
  /** `PATCH /api/v1/issues/{key}`: the status a write names. */
  setStatus(key: string, status: string): Promise<unknown>;
  /** `POST /api/v1/issues/{key}/artifacts`: one document of the scratch issue. */
  upload(key: string, name: string, content: string): Promise<unknown>;
  /** `POST /api/v1/artifacts/{id}/approval-requests`, with `include_verification`. */
  requestApproval(artifactID: string): Promise<unknown>;
  /** `POST /api/v1/artifacts/{id}/edits`: one insert at the end of a document. */
  edit(artifactID: string, sentence: string): Promise<unknown>;
  /** The closing reason as a message, then the close itself. */
  close(key: string): Promise<unknown>;
  /** The same edit run through the pod's own `dispatch doc-edit`. */
  docEditCommand(key: string, artifactID: string, sentence: string): Promise<CommandRun>;
}

/** A run of the `dispatch` command: its exit status, or null when it could not be run, and
 *  whether the moved-approval line was among the lines it printed. Its output stays inside the
 *  run, so nothing of it can reach the check's own output. */
interface CommandRun {
  readonly code: number | null;
  readonly line: boolean;
}

/** The scratch issue, and its documents once every one of them was uploaded. The key stands on
 *  its own, because an issue the check made is an issue the check closes even when the upload
 *  after it failed. */
interface Scratch {
  readonly key: string;
  readonly documents?: ScratchDocuments;
}

interface ScratchDocuments {
  readonly spec: string;
  readonly verification: string;
  readonly notes: string;
  /** The version of the Verification document the approval request is opened at. */
  readonly verificationVersion: number;
}

type Out = (line: string) => void;

/** The flags as `parseArgs` reads them, before their rules are checked. */
function readFlags(argv: readonly string[]) {
  try {
    return parseArgs({ args: [...argv], options: { project: { type: "string" } }, strict: true })
      .values;
  } catch {
    // parseArgs names what it refused, so its message is never repeated.
    throw new UsageError(USAGE.arguments);
  }
}

export function parseArguments(argv: readonly string[]): Options {
  const project = readFlags(argv).project;
  if (project === undefined || !PROJECT_KEY.test(project)) throw new UsageError(USAGE.project);
  return { project };
}

/** A name the check uploaded itself, or `other`. */
function documentName(name: string): string {
  return DOCUMENT_NAMES.includes(name) ? name : OTHER;
}

/**
 * The report of the write that should have moved the open approval request: it is there, it
 * stands on the ask the request opened, it names the issue the check made, and its entry for the
 * document the write stored carries the version the request asks about beside the version that
 * document stands at now.
 *
 * The line prints what the report holds, which is the check's own making throughout: the ask id,
 * the names of the documents it uploaded and the two version numbers of each. Anything else the
 * report names is printed as `other`.
 */
export function judgeMoved(answer: unknown, expected: ExpectedMove): Judgement {
  const report = EditAnswer.parse(answer).advice?.moved_approval_request;
  if (report === undefined) return { verdict: "fail", line: "moved report=none fail" };
  const ask = report.ask_id === expected.askID ? expected.askID : OTHER;
  const place =
    report.issue === expected.issueKey && (report.project === undefined || report.project === "")
      ? expected.issueKey
      : OTHER;
  const documents = report.documents ?? [];
  const printed =
    documents.length === 0
      ? "none"
      : documents
          .map(
            (document) =>
              `${documentName(document.name)}:${document.requested_version}/${document.version}`
          )
          .join(",");
  const moved = documents.some(
    (document) =>
      document.name === expected.name &&
      document.requested_version === expected.requestedVersion &&
      document.version === expected.version
  );
  const verdict = ask !== OTHER && place !== OTHER && moved ? "pass" : "fail";
  return {
    verdict,
    line: `moved ask=${ask} issue=${place} documents=${printed} ${verdict}`,
  };
}

/** The negative control: a write of a document no open approval request names took nothing back
 *  from anybody, so its answer carries no report at all. */
export function judgeControl(answer: unknown, name: string): Judgement {
  const present = EditAnswer.parse(answer).advice?.moved_approval_request !== undefined;
  const verdict = present ? "fail" : "pass";
  return {
    verdict,
    line: `control document=${documentName(name)} report=${present ? "present" : "none"} ${verdict}`,
  };
}

/** The running build, which is not an assertion of its own: whether its commit carries a given
 *  merge is decided outside the script, as it is for the meaning search check. */
async function printBuild(writes: DispatchWrites, out: Out): Promise<Verdict> {
  try {
    const health = Health.parse(await writes.healthz());
    out(
      `build commit=${health.commit ?? "null"} schema=${health.schema_version ?? "null"} ` +
        `ok=${health.ok}`
    );
    return "pass";
  } catch (error) {
    out(`build unavailable: ${errorCategory(error)}`);
    return "unavailable";
  }
}

/**
 * The scratch issue, its spec and the two documents beside it. An answer that is not the 201 of
 * an issue of the named project is refused without closing anything: the check closes the issues
 * it made and no other.
 */
async function makeScratch(
  project: string,
  writes: DispatchWrites,
  out: Out
): Promise<Scratch | undefined> {
  let key: string;
  let spec: string;
  try {
    const created = CreatedIssue.parse(await writes.createIssue(project));
    if (
      !created.created ||
      !ISSUE_KEY.test(created.key) ||
      !created.key.startsWith(`${project}-`)
    ) {
      out("scratch unavailable: refused");
      return undefined;
    }
    key = created.key;
    spec = created.primary_artifact_id;
  } catch (error) {
    out(`scratch unavailable: ${errorCategory(error)}`);
    return undefined;
  }
  try {
    // An issue Dispatch still shows in triage is out of the workflow an approval request stands in.
    await writes.setStatus(key, "in_progress");
    const verification = Uploaded.parse(
      await writes.upload(key, VERIFICATION, WRITTEN.verification)
    );
    const notes = Uploaded.parse(await writes.upload(key, NOTES, WRITTEN.notes));
    out(`scratch issue=${key} documents=${SPEC},${VERIFICATION},${NOTES} pass`);
    return {
      key,
      documents: {
        spec,
        verification: verification.artifact.id,
        notes: notes.artifact.id,
        verificationVersion: verification.version.number,
      },
    };
  } catch (error) {
    out(`scratch unavailable: ${errorCategory(error)}`);
    return { key };
  }
}

/** The approval request over the spec and the Verification document, and the id of the ask it
 *  handed the person. */
async function openRequest(
  documents: ScratchDocuments,
  writes: DispatchWrites,
  out: Out
): Promise<string | undefined> {
  try {
    const opened = OpenedRequest.parse(await writes.requestApproval(documents.spec));
    const ask = opened.ask;
    if (ask === null || !UUID.test(ask.id)) {
      out("request unavailable: refused");
      return undefined;
    }
    out(`request ask=${ask.id} include-verification=yes pass`);
    return ask.id;
  } catch (error) {
    out(`request unavailable: ${errorCategory(error)}`);
    return undefined;
  }
}

/** The write that takes the request back from the person, and the report its own answer carries. */
async function checkMoved(
  scratch: Scratch & { documents: ScratchDocuments },
  askID: string,
  writes: DispatchWrites,
  out: Out
): Promise<Verdict> {
  try {
    const answer = await writes.edit(scratch.documents.verification, WRITTEN.sentence);
    const stored = EditAnswer.parse(answer).version;
    if (stored === null) {
      out("moved unavailable: refused");
      return "unavailable";
    }
    const judgement = judgeMoved(answer, {
      askID,
      issueKey: scratch.key,
      name: VERIFICATION,
      requestedVersion: scratch.documents.verificationVersion,
      version: stored.number,
    });
    out(judgement.line);
    return judgement.verdict;
  } catch (error) {
    out(`moved unavailable: ${errorCategory(error)}`);
    return "unavailable";
  }
}

/** The same edit through the pod's own `dispatch doc-edit`. This is an observation and not an
 *  assertion: a pod prints the line only once the `@sjawhar/pi-envoy` pin of the worker image
 *  carries this change. */
async function observeCommand(
  scratch: Scratch & { documents: ScratchDocuments },
  writes: DispatchWrites,
  out: Out
): Promise<void> {
  const run = await writes.docEditCommand(
    scratch.key,
    scratch.documents.verification,
    WRITTEN.sentence
  );
  out(`observed cli-line=${run.line ? "yes" : "no"} exit=${run.code ?? "none"}`);
}

/** The negative control: the same edit on the document no open approval request names. */
async function checkControl(
  documents: ScratchDocuments,
  writes: DispatchWrites,
  out: Out
): Promise<Verdict> {
  try {
    const judgement = judgeControl(await writes.edit(documents.notes, WRITTEN.note), NOTES);
    out(judgement.line);
    return judgement.verdict;
  } catch (error) {
    out(`control unavailable: ${errorCategory(error)}`);
    return "unavailable";
  }
}

/** The close, which withdraws the approval request the check opened, so a step that failed
 *  leaves no open request in production. */
async function closeScratch(key: string, writes: DispatchWrites, out: Out): Promise<Verdict> {
  try {
    const closed = ClosedIssue.parse(await writes.close(key));
    const done = closed.status === "done";
    out(`close issue=${key} status=${done ? "done" : OTHER} ${done ? "pass" : "fail"}`);
    return done ? "pass" : "fail";
  } catch (error) {
    out(`close issue=${key} unavailable: ${errorCategory(error)}`);
    return "unavailable";
  }
}

/** Runs every step in order, prints their lines, and answers the exit code. The scratch issue is
 *  closed in a `finally`, so a step that threw still gives its issue back. */
async function runCheck(options: Options, writes: DispatchWrites, out: Out): Promise<number> {
  const assertions: Verdict[] = [];
  let blocked = (await printBuild(writes, out)) === "unavailable";
  let scratch: Scratch | undefined;
  try {
    scratch = await makeScratch(options.project, writes, out);
    const documents = scratch?.documents;
    if (scratch === undefined || documents === undefined) {
      blocked = true;
    } else {
      const askID = await openRequest(documents, writes, out);
      if (askID === undefined) {
        blocked = true;
      } else {
        const opened = { ...scratch, documents };
        assertions.push(await checkMoved(opened, askID, writes, out));
        await observeCommand(opened, writes, out);
        assertions.push(await checkControl(documents, writes, out));
      }
    }
  } finally {
    if (scratch !== undefined) assertions.push(await closeScratch(scratch.key, writes, out));
  }
  const result: Verdict =
    blocked || assertions.includes("unavailable")
      ? "unavailable"
      : assertions.includes("fail")
        ? "fail"
        : "pass";
  const code = EXIT_CODE[result];
  out(`result ${result} exit=${code}`);
  const passed = assertions.filter((verdict) => verdict === "pass").length;
  out(
    passed === ASSERTIONS
      ? `all ${ASSERTIONS} assertions passed`
      : `assertions passed ${passed} of ${ASSERTIONS}`
  );
  return code;
}

/**
 * The pod's own `dispatch doc-edit`, run as an agent's shell runs it: it resolves Dispatch,
 * its session and its credential from the environment this process was given, as the pod's own
 * calls do. Its output is read here and nowhere else, and only the exit status and whether the
 * line was printed leave the function.
 *
 * Both parts of the line stand in one sentence of one line, so output holding both printed it.
 *
 * The run is bounded by the deadline every other call of this check runs under
 * (DISPATCH_TOOL_DEADLINE_MS), because it is the one wait here with no deadline of its own and
 * it stands between the assertions and the `finally` that closes the scratch issue: a `dispatch`
 * that hung would leave an open approval request in production, which is the state that close
 * exists to prevent. A child the deadline kills falls out as the exit status of a process killed
 * by that signal (137 for SIGKILL) with no line, which `observed cli-line=no exit=<status>`
 * reports as it reports any other failure of the command.
 */
async function runDocEditCommand(
  key: string,
  artifactID: string,
  sentence: string
): Promise<CommandRun> {
  try {
    const child = Bun.spawn(
      [
        "dispatch",
        "doc-edit",
        "--issue",
        key,
        "--artifact",
        artifactID,
        "--ops-json",
        JSON.stringify([{ op: "insert", markdown: sentence, after: "end" }]),
      ],
      {
        stdin: "ignore",
        stdout: "pipe",
        stderr: "pipe",
        timeout: DISPATCH_TOOL_DEADLINE_MS,
        killSignal: "SIGKILL",
      }
    );
    const [stdout, stderr, code] = await Promise.all([
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
      child.exited,
    ]);
    const printed = `${stdout}\n${stderr}`;
    return { code, line: CLI_LINE.every((part) => printed.includes(part)) };
  } catch {
    return { code: null, line: false };
  }
}

function liveDispatch(config: DispatchConfig): DispatchWrites {
  // `dispatchSettings` already trimmed the URL's trailing slashes. Every write acts as the
  // session this process is, which is the session the approval request is opened by and the one
  // the writes that move it belong to.
  const actor = { kind: "session", id: process.env.DISPATCH_SESSION_ID ?? "unknown" };
  // A client's deadline runs from its construction, so each call gets a client of its own.
  const client = () => new DispatchClient(config.url, config.credential);
  const insert = (sentence: string) => ({
    actor,
    ops: [{ op: "insert", markdown: sentence, after: "end" }],
  });
  return {
    // Public: it is sent no token.
    healthz: () =>
      readJson(`${config.url}/healthz`, {}, (status) => status === 200 || status === 503),
    createIssue: (project) =>
      client().issue({
        actor,
        project,
        title: WRITTEN.title,
        spec: WRITTEN.spec,
        // The scratch issue of an earlier run is the nearest thing to it there is, and the check
        // writes its own issue every time rather than taking that one's suggestion.
        force: true,
      }),
    setStatus: (key, status) => client().updateIssue(key, { actor, status }),
    upload: (key, name, content) => client().artifact(key, { actor, name, content }),
    requestApproval: (artifactID) =>
      client().requestApproval(artifactID, {
        actor,
        summary: WRITTEN.summary,
        include_verification: true,
      }),
    edit: (artifactID, sentence) => client().docEdit(artifactID, insert(sentence)),
    close: async (key) => {
      // A closing reason is a message of its own, posted before the close, as the `dispatch
      // issue --status done --reason` call posts it.
      await client().message(key, { actor, body: WRITTEN.reason });
      return client().updateIssue(key, { actor, status: "done" });
    },
    docEditCommand: runDocEditCommand,
  };
}

async function main(argv: readonly string[]): Promise<number> {
  let options: Options;
  try {
    options = parseArguments(argv);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    console.error(`usage: ${error.message}`);
    return 2;
  }
  const config = resolveDispatch();
  if (config === null) {
    const category: Category = "config";
    console.error(`unavailable: ${category}`);
    return 2;
  }
  return runCheck(options, liveDispatch(config), (line) => {
    console.log(line);
  });
}

if (import.meta.main) {
  let code: number;
  try {
    code = await main(Bun.argv.slice(2));
  } catch (error) {
    console.error(`unavailable: ${errorCategory(error)}`);
    code = 2;
  }
  process.exit(code);
}
