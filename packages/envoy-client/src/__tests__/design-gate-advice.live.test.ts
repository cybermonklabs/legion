import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { dispatchCliLive, useDispatchCliDirectories } from "../../test/dispatch-cli-live";
import {
  type DispatchStack,
  dispatchStackEnabled,
  startDispatchStack,
} from "../../test/dispatch-stack";

// The design-gate clause of a document write's advice, over the whole path it is printed through:
// the real `dispatch` CLI of this checkout, run as a child process against a real Dispatch server
// of this checkout, on a database of its own. The unit rows (dispatch-advice.test.ts) prove the
// line this CLI builds from an advice it is handed, and the server's own Go rows prove the advice
// Dispatch sends; neither proves that the field the server fills is the field the CLI reads, which
// is what every row below writes a real document to find out.
//
// No daemon runs here, so the gate record each row needs is published the way a Legion
// deployment's daemon publishes it: PUT /api/v1/issues/{key}/design-gates, over the credential the
// deployment configured. Everything else a row needs - the project, the issue, the document and
// the approval the write supersedes - is made over the routes a person reaches.

/** The person who approves every document below, as the server's header identity names them. */
const PERSON = "alice";
/** The session the CLI acts as, which is also the session a plan review is recorded by. */
const SESSION = "session-design-gate-live";
/** The project whose issues each row of its own makes; two more stand beside it for the row whose
 *  document two trees record, where the keys order the gates the server answers. */
const PROJECT = "DGATE";
const FIRST_TREE = "GATEA";
const SECOND_TREE = "GATEB";

/** The second sentence of a superseded-approval line, printed whether a gate records the document
 *  or not. */
const NEXT_STEP =
  "Meant to change the approved design? Request approval with `dispatch request-approval`. Meant a correction beside it? Tell the tree's architect, and keep the next one in a comment or in the plan.";

/** The first sentence: the version this write stored and the approval it staled. */
function storedSentence(version: number, approvedBy: string): string {
  return `Stored version ${version}, superseding version ${version - 1}, which ${approvedBy} approved. That approval is stale until someone approves version ${version} or a later write stores the text of version ${version - 1} again exactly, with no decision block in it decided since.`;
}

/** The clause between them, for the issues whose design gates record the document written. */
function gateClause(gates: readonly { readonly issue: string; readonly step: string }[]): string {
  const named = gates.map((gate) => `${gate.issue}, step ${gate.step}`).join("; ");
  return `A Legion design gate records this document, so every READY in ${gates.length === 1 ? "its" : "each"} tree is refused until that approval comes back: ${named}.`;
}

/** What one design-gate step records, as the daemon publishes it. */
interface PublishedGate {
  readonly step: string;
  readonly artifact_id: string;
}

// Reserved here, at the file's module scope: the file's own `afterAll` removes each once the
// `describe` below has stopped the server writing into it.
const directories = useDispatchCliDirectories();

describe.skipIf(!dispatchStackEnabled)(
  "the design-gate clause the real CLI prints against a real Dispatch",
  () => {
    let stack: DispatchStack | undefined;

    const { asPerson, asMachine, createIssue, upload, docEdit } = dispatchCliLive({
      stack: () => stack,
      session: SESSION,
      person: PERSON,
      spec: "# Design\n\nThe design, as approved.\n",
      directories,
    });

    /** The person's approval of a document's latest version, and the version it pinned. */
    async function approve(artifactId: string): Promise<number> {
      const review = await asPerson<{ version: number }>(
        "POST",
        `/api/v1/artifacts/${artifactId}/reviews`,
        { state: "approved" }
      );
      return review.version;
    }

    /** The issue's whole gate record, as the deployment's daemon publishes it. */
    const publishGates = (issue: string, gates: readonly PublishedGate[]): Promise<unknown> =>
      asMachine("PUT", `/api/v1/issues/${issue}/design-gates`, {
        gates,
        actor: { kind: "session", id: SESSION },
      });

    beforeAll(async () => {
      stack = await startDispatchStack({ work: directories.stack });
      for (const key of [PROJECT, FIRST_TREE, SECOND_TREE]) {
        await asPerson("POST", "/api/v1/projects", { key, name: `${key} project` });
        // The deployment's own gated steps, which its daemon publishes beside the gate records
        // and which the advice reads: a gate row on a step the deployment does not gate holds
        // no READY back and prints no clause.
        await asMachine("PUT", `/api/v1/projects/${key}/gated-steps`, {
          steps: ["decisions", "criteria", "spec"],
          actor: { kind: "session", id: SESSION },
        });
      }
    }, 600_000);
    afterAll(async () => {
      await stack?.stop();
    }, 60_000);

    test("a Verification document no gate records names no gate", async () => {
      const issue = await createIssue(PROJECT, "Approved verification");
      const document = await upload(issue.key, "verification.md", "# Verification\n\nOne check.\n");
      const approved = await approve(document.artifact.id);

      const printed = await docEdit(issue.key, document.artifact.id, "One more check.");

      expect(printed).toEndWith(`${storedSentence(approved + 1, PERSON)} ${NEXT_STEP}`);
      expect(printed).not.toContain("design gate");
    }, 120_000);

    test("a plan a plan review approved names no gate", async () => {
      const issue = await createIssue(PROJECT, "Approved plan");
      const plan = await upload(issue.key, "plan.md", "# Plan\n\nOne task.\n");
      // A plan is approved by the plan review an agent records over it, never by a person.
      await asMachine("POST", `/api/v1/artifacts/${plan.artifact.id}/plan-reviews`, {
        actor: { kind: "session", id: SESSION },
        version: plan.version.number,
      });

      const printed = await docEdit(issue.key, plan.artifact.id, "One more task.");

      expect(printed).toEndWith(`${storedSentence(plan.version.number + 1, SESSION)} ${NEXT_STEP}`);
      expect(printed).not.toContain("design gate");
    }, 120_000);

    test("a child issue's own document, which no gate records, names none", async () => {
      const parent = await createIssue(PROJECT, "Parent of the child");
      const child = await createIssue(PROJECT, "Child with its own spec", parent.key);
      const approved = await approve(child.primary_artifact_id);

      const printed = await docEdit(
        child.key,
        child.primary_artifact_id,
        "A sentence the child gained."
      );

      expect(printed).toEndWith(`${storedSentence(approved + 1, PERSON)} ${NEXT_STEP}`);
      expect(printed).not.toContain("design gate");
    }, 120_000);

    test("a live issue's gate names that issue and its step", async () => {
      const issue = await createIssue(PROJECT, "Gated spec");
      await publishGates(issue.key, [{ step: "spec", artifact_id: issue.primary_artifact_id }]);
      const approved = await approve(issue.primary_artifact_id);

      const printed = await docEdit(
        issue.key,
        issue.primary_artifact_id,
        "A sentence the gate had not seen."
      );

      expect(printed).toEndWith(
        `${storedSentence(approved + 1, PERSON)} ${gateClause([{ issue: issue.key, step: "spec" }])} ${NEXT_STEP}`
      );
    }, 120_000);

    test("two live issues' gates name both, each with its step", async () => {
      const owner = await createIssue(FIRST_TREE, "The issue the document belongs to");
      const other = await createIssue(SECOND_TREE, "The issue handed that same document");
      await publishGates(owner.key, [{ step: "spec", artifact_id: owner.primary_artifact_id }]);
      await publishGates(other.key, [{ step: "criteria", artifact_id: owner.primary_artifact_id }]);
      const approved = await approve(owner.primary_artifact_id);

      const printed = await docEdit(
        owner.key,
        owner.primary_artifact_id,
        "A sentence neither gate had seen."
      );

      expect(printed).toEndWith(
        `${storedSentence(approved + 1, PERSON)} ${gateClause([
          { issue: owner.key, step: "spec" },
          { issue: other.key, step: "criteria" },
        ])} ${NEXT_STEP}`
      );
    }, 120_000);

    test("a gate only a done issue records names none", async () => {
      const live = await createIssue(PROJECT, "The issue the document belongs to");
      const closed = await createIssue(PROJECT, "The only issue whose gate records it");
      await publishGates(closed.key, [{ step: "spec", artifact_id: live.primary_artifact_id }]);
      await asPerson("PATCH", `/api/v1/issues/${closed.key}`, { status: "done" });
      const approved = await approve(live.primary_artifact_id);

      const printed = await docEdit(
        live.key,
        live.primary_artifact_id,
        "A sentence no open gate waits on."
      );

      expect(printed).toEndWith(`${storedSentence(approved + 1, PERSON)} ${NEXT_STEP}`);
      expect(printed).not.toContain("design gate");
    }, 120_000);
  }
);
