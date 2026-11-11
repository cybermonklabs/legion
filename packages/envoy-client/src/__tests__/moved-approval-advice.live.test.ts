import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  type DispatchStack,
  dispatchStackEnabled,
  startDispatchStack,
  useDispatchStackDirectory,
} from "../../test/dispatch-stack";

// The moved-approval-request line of a document write's advice, over the whole path it is printed
// through: the real `dispatch` CLI of this checkout, run as a child process against a real
// Dispatch server of this checkout, on a database of its own. The unit rows
// (dispatch-advice.test.ts) prove the line this CLI builds from an advice it is handed, and the
// server's own Go rows (write_advice_moved_approval_test.go) prove the advice Dispatch sends;
// neither proves that the field the server fills is the field the CLI reads, which is what the
// rows below write a real document to find out.
//
// Everything a row needs is made over the routes its author reaches: the project, the issue and
// its Verification document over a person's own writes, and the approval request over both
// documents as the asking agent's own session (POST /api/v1/artifacts/{id}/approval-requests with
// include_verification). The write that takes that request back from the person is the real CLI,
// run in that same session - which is the only thing here no unit row can stand in for.

const ENTRY = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");

/** The person who owns the project and every document below, as the server's header identity
 *  names them, and the person each approval request is asked of. */
const PERSON = "alice";
/** The session the CLI acts as, which is also the session that asked for the approval. */
const SESSION = "session-moved-approval-live";
/** The project whose issues each row makes one of its own in. */
const PROJECT = "MOVED";

/** An issue as its creation answers it. */
interface CreatedIssue {
  readonly key: string;
  readonly primary_artifact_id: string;
}

/** A document as its upload answers it. */
interface UploadedDocument {
  readonly artifact: { readonly id: string };
  readonly version: { readonly number: number };
}

/** An approval request as the route answers it: the ask the person was handed. */
interface OpenedApprovalRequest {
  readonly ask: { readonly id: string } | null;
  readonly version: number;
}

// Reserved here, at the file's module scope: the file's own `afterAll` removes each once the
// `describe` below has stopped the server writing into it.
const stackDirectory = useDispatchStackDirectory();
const stateDirectory = temporaryDirectory("dispatch-cli-live-state");

describe.skipIf(!dispatchStackEnabled)(
  "the moved-approval-request line the real CLI prints against a real Dispatch",
  () => {
    let stack: DispatchStack | undefined;

    /** The running server, or the error that says why no row can run. */
    function server(): DispatchStack {
      if (stack === undefined) throw new Error("the Dispatch stack did not start");
      return stack;
    }

    async function request<T>(
      method: string,
      route: string,
      body: unknown,
      headers: Readonly<Record<string, string>>
    ): Promise<T> {
      const response = await fetch(`${server().url}${route}`, {
        method,
        headers: { "Content-Type": "application/json", ...headers },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
      const text = await response.text();
      if (!response.ok) throw new Error(`${method} ${route}: ${response.status} ${text}`);
      return JSON.parse(text) as T;
    }

    /** A person's own write, which the server's header identity takes as that person. */
    const asPerson = <T>(method: string, route: string, body?: unknown): Promise<T> =>
      request<T>(method, route, body, { "X-Dispatch-User": PERSON });

    /** The asking agent's own write, over the credential the deployment configured, as the CLI
     *  itself reaches the server. */
    const asAgent = <T>(method: string, route: string, body?: unknown): Promise<T> =>
      request<T>(method, route, body, { Authorization: `Bearer ${server().token}` });

    /** An issue in the workflow, with the spec every approval request below is opened on: an
     *  issue Dispatch still shows in `triage` is out of the workflow. */
    const createIssue = async (title: string): Promise<CreatedIssue> => {
      const issue = await asPerson<CreatedIssue>("POST", "/api/v1/issues", {
        project: PROJECT,
        title,
        force: true,
        spec: "# Design\n\nThe design, as it stands.\n",
      });
      await asPerson("PATCH", `/api/v1/issues/${issue.key}`, { status: "in_progress" });
      return issue;
    };

    /** A document of the issue, uploaded as the person who owns it. */
    const upload = (issue: string, name: string, content: string): Promise<UploadedDocument> =>
      asPerson<UploadedDocument>("POST", `/api/v1/issues/${issue}/artifacts`, { name, content });

    /** The approval request the asking agent opens over the issue's spec and its Verification
     *  document, and the id of the ask it handed the person. */
    async function requestApproval(specArtifactID: string): Promise<string> {
      const opened = await asAgent<OpenedApprovalRequest>(
        "POST",
        `/api/v1/artifacts/${specArtifactID}/approval-requests`,
        { actor: { kind: "session", id: SESSION }, include_verification: true }
      );
      if (opened.ask === null) throw new Error("the approval request opened no ask");
      return opened.ask.id;
    }

    /**
     * One insert at the end of a document, run through the real CLI entry as an agent's shell runs
     * it, and the output it printed. A refusal, or anything on standard error, fails the row here:
     * every row below is about the line a write that went through printed.
     *
     * The sentence inserted is plain prose with no typed block, as the sibling file's rows insert
     * one (design-gate-advice.live.test.ts), and here for a reason of this file's own: the
     * settlement that follows a write stores a version of its own when the write leaves a typed
     * block to settle, and that second version would move the request again under the row's feet
     * and leave its version numbers at the mercy of a settlement that may or may not have landed.
     */
    async function docEdit(issue: string, artifact: string, sentence: string): Promise<string> {
      const child = Bun.spawn(
        [
          process.execPath,
          ENTRY,
          "doc-edit",
          "--issue",
          issue,
          "--artifact",
          artifact,
          "--ops-json",
          JSON.stringify([{ op: "insert", markdown: sentence, after: "end" }]),
        ],
        {
          env: {
            PATH: process.env.PATH,
            HOME: stateDirectory,
            DISPATCH_STATE_DIR: stateDirectory,
            DISPATCH_HOST: "omp",
            DISPATCH_SESSION_ID: SESSION,
            DISPATCH_URL: server().url,
            DISPATCH_TOKEN: server().token,
          },
          stdout: "pipe",
          stderr: "pipe",
        }
      );
      const [stdout, stderr, code] = await Promise.all([
        new Response(child.stdout).text(),
        new Response(child.stderr).text(),
        child.exited,
      ]);
      if (code !== 0 || stderr !== "") {
        throw new Error(`dispatch doc-edit on ${issue} exited ${code}: ${stderr}${stdout}`);
      }
      return stdout.trimEnd();
    }

    beforeAll(async () => {
      stack = await startDispatchStack({ work: stackDirectory });
      await asPerson("POST", "/api/v1/projects", { key: PROJECT, name: `${PROJECT} project` });
    }, 600_000);
    afterAll(async () => {
      await stack?.stop();
    }, 60_000);

    test("a write of a document the open request names prints the ask and the hand-back call", async () => {
      const issue = await createIssue("Approve the design and its checks");
      const verification = await upload(
        issue.key,
        "verification.md",
        "# Verification\n\nOne check.\n"
      );
      const ask = await requestApproval(issue.primary_artifact_id);

      const printed = await docEdit(
        issue.key,
        verification.artifact.id,
        "One more check the person has not seen."
      );

      // The request asks about version 1 of both documents; this write stored version 2 of the
      // Verification document and left the spec alone, so the line names that document and both
      // its versions, and the call it ends with reopens the request on the issue's spec with the
      // Verification document beside it, from the version the request was handed over at.
      const clause = "verification.md asked about version 1 and is now version 2";
      const handBack =
        `dispatch request-approval --issue ${issue.key} --include-verification ` +
        "--summary '<what changed since version 1>'";
      expect(printed).toContain(
        `This write moved the open approval request (ask ${ask}): it waits on an agent now, not ` +
          `on the person, and nothing tells the person. ${clause}. Once the human has agreed to ` +
          `every point in it, hand the same request back: ${handBack}. Not yours to hand back? ` +
          "Tell the issue's architect."
      );
    }, 120_000);

    test("a write of a document no open request names prints no such line", async () => {
      const issue = await createIssue("Approve the design, and keep notes beside it");
      await upload(issue.key, "verification.md", "# Verification\n\nOne check.\n");
      await requestApproval(issue.primary_artifact_id);
      // A third document of the same issue, which the request above does not name: the request is
      // open and it is this session's own, and still this write takes nothing back from anyone.
      const notes = await upload(issue.key, "notes.md", "# Notes\n\nOne note.\n");

      const printed = await docEdit(issue.key, notes.artifact.id, "One more note.");

      expect(printed).not.toContain("moved the open approval request");
    }, 120_000);
  }
);
