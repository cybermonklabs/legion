import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { dispatchCliLive, useDispatchCliDirectories } from "../../test/dispatch-cli-live";
import {
  type DispatchStack,
  dispatchStackEnabled,
  startDispatchStack,
} from "../../test/dispatch-stack";

// An insert anchored on a paragraph inside a requirement, over the whole path an agent sends it
// through: the real `dispatch doc-edit` of this checkout, run as a child process against a real
// Dispatch server of this checkout, on a database of its own. The server's own Go rows
// (insert_in_typed_block_test.go) prove where the edit route writes such an insert; this row
// proves the anchor the CLI sends lands the markdown inside the requirement, which the coverage
// reading then finds holding it and served by its check, where it was written after the
// requirement and no `serves` reached it (dispatch://INFRA-3142).

/** The person who owns the project, the issue and its Verification document. */
const PERSON = "alice";
/** The session the CLI acts as. */
const SESSION = "session-typed-block-insert-live";
/** The project the row's issue is made in. */
const PROJECT = "TYPED";
/** The words the row inserts into the requirement. */
const INSERTED = "It answers within a second.";

/** A block of a document as GET /api/v1/artifacts/{id}/blocks lists it. */
interface ListedBlock {
  readonly id: string;
  readonly type: string;
  readonly from: number;
  readonly to: number;
}

/** The coverage reading of an issue, GET /api/v1/issues/{key}/coverage. */
interface Coverage {
  readonly requirements: readonly {
    readonly block_id: string;
    readonly text: string;
    readonly checks: number;
  }[];
  readonly gaps: readonly string[];
}

// Reserved here, at the file's module scope: the file's own `afterAll` removes each once the
// `describe` below has stopped the server writing into it.
const directories = useDispatchCliDirectories();

describe.skipIf(!dispatchStackEnabled)(
  "an insert the real CLI anchors inside a requirement, against a real Dispatch",
  () => {
    let stack: DispatchStack | undefined;

    const { asPerson, createIssue, upload, docEditOps } = dispatchCliLive({
      stack: () => stack,
      session: SESSION,
      person: PERSON,
      spec: ":::requirement{#r1}\nThe read succeeds.\n:::\n\nAfter.\n",
      directories,
    });

    beforeAll(async () => {
      stack = await startDispatchStack({ work: directories.stack });
      await asPerson("POST", "/api/v1/projects", { key: PROJECT, name: `${PROJECT} project` });
    }, 600_000);
    afterAll(async () => {
      await stack?.stop();
    }, 60_000);

    test("lands inside the requirement, which its check still serves", async () => {
      const issue = await createIssue(PROJECT, "Insert into a requirement");
      await upload(
        issue.key,
        "verification.md",
        `:::check{#k1 serves="dispatch://${issue.key}/spec#b-r1" surface="cli" run="go test ./..."}\nThe check that proves it.\n:::\n`
      );
      const spec = `/api/v1/artifacts/${issue.primary_artifact_id}`;
      const { markdown } = await asPerson<{ markdown: string }>("GET", `${spec}/text`);
      const blocks = await asPerson<ListedBlock[]>("GET", `${spec}/blocks`);
      const paragraph = blocks.find(
        (block) =>
          block.type === "paragraph" &&
          markdown.slice(block.from, block.to).trim() === "The read succeeds."
      );
      if (paragraph === undefined)
        throw new Error(`no paragraph of ${markdown} reads the requirement`);

      await docEditOps(issue.key, issue.primary_artifact_id, [
        { op: "insert", after: `block:${paragraph.id}`, markdown: INSERTED },
      ]);

      const after = await asPerson<{ markdown: string }>("GET", `${spec}/text`);
      expect(after.markdown).toContain(
        `:::requirement{#r1}\nThe read succeeds.\n\n${INSERTED}\n:::\n\nAfter.\n`
      );
      const coverage = await asPerson<Coverage>("GET", `/api/v1/issues/${issue.key}/coverage`);
      const requirement = coverage.requirements.find((row) => row.block_id === "r1");
      expect(requirement?.text).toContain(INSERTED);
      expect(requirement?.checks).toBe(1);
      expect(coverage.gaps).toEqual([]);
    }, 120_000);
  }
);
