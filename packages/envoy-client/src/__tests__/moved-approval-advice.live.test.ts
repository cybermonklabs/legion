import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { dispatchCliLive, useDispatchCliDirectories } from "../../test/dispatch-cli-live";
import {
  type DispatchStack,
  dispatchStackEnabled,
  startDispatchStack,
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

/** The person who owns the project and every document below, as the server's header identity
 *  names them, and the person each approval request is asked of. */
const PERSON = "alice";
/** The session the CLI acts as, which is also the session that asked for the approval. */
const SESSION = "session-moved-approval-live";
/** The project whose issues each row makes one of its own in. */
const PROJECT = "MOVED";

/** An approval request as the route answers it: the ask the person was handed. */
interface OpenedApprovalRequest {
  readonly ask: { readonly id: string } | null;
  readonly version: number;
}

// Reserved here, at the file's module scope: the file's own `afterAll` removes each once the
// `describe` below has stopped the server writing into it.
const directories = useDispatchCliDirectories();

describe.skipIf(!dispatchStackEnabled)(
  "the moved-approval-request line the real CLI prints against a real Dispatch",
  () => {
    let stack: DispatchStack | undefined;

    // Every row below inserts plain prose with no typed block, as the sibling file's rows insert
    // one (design-gate-advice.live.test.ts), and here for a reason of this file's own: the
    // settlement that follows a write stores a version of its own when the write leaves a typed
    // block to settle, and that second version would move the request again under the row's feet
    // and leave its version numbers at the mercy of a settlement that may or may not have landed.
    const { asPerson, asMachine, createIssue, upload, docEdit } = dispatchCliLive({
      stack: () => stack,
      session: SESSION,
      person: PERSON,
      spec: "# Design\n\nThe design, as it stands.\n",
      directories,
    });

    /** The approval request the asking agent opens over the issue's spec and its Verification
     *  document, and the id of the ask it handed the person. */
    async function requestApproval(specArtifactID: string): Promise<string> {
      const opened = await asMachine<OpenedApprovalRequest>(
        "POST",
        `/api/v1/artifacts/${specArtifactID}/approval-requests`,
        { actor: { kind: "session", id: SESSION }, include_verification: true }
      );
      if (opened.ask === null) throw new Error("the approval request opened no ask");
      return opened.ask.id;
    }

    beforeAll(async () => {
      stack = await startDispatchStack({ work: directories.stack });
      await asPerson("POST", "/api/v1/projects", { key: PROJECT, name: `${PROJECT} project` });
    }, 600_000);
    afterAll(async () => {
      await stack?.stop();
    }, 60_000);

    test("a write of a document the open request names prints the ask and the hand-back call", async () => {
      const issue = await createIssue(PROJECT, "Approve the design and its checks");
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
      const issue = await createIssue(PROJECT, "Approve the design, and keep notes beside it");
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
