import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { dispatchCliLive, useDispatchCliDirectories } from "../../test/dispatch-cli-live";
import {
  type DispatchStack,
  dispatchStackEnabled,
  startDispatchStack,
} from "../../test/dispatch-stack";

// The role a recorded result stores, over the whole path an agent records and reads it through:
// the real `dispatch` CLI, run as a child process against a real Dispatch server of this checkout,
// on a database of its own. Dispatch stores the role the recording session holds on the issue, as
// the Legion daemon last published it (PUT /api/v1/issues/{key}/role-holders), and never the role
// the request names (dispatch://INFRA-3994). The server's own Go rows (result_role_test.go) prove
// the stored role and the answer's notice, and the CLI's unit rows prove the sentences it builds
// from an answer it is handed; neither proves that the answer the server sends is the answer the
// CLI reads and prints, which is what the rows below record a real result to find out.
//
// The control: against a server built with results.go of the base commit, which stores the role
// the request names, the first row's result stores tester, and the row fails at what the CLI
// prints and at the stored role.

/** The person who owns the project and every document below, as the server's header identity
 *  names them. */
const PERSON = "ada@example.com";
/** The project whose issues each row makes one of its own in. */
const PROJECT = "ACME";
/** A session that holds no role of any issue below. */
const NO_ROLE = "ses-none";
/** The session the third row publishes as its issue's tester. */
const TESTER = "ses-tester";
/** The commit every result below is recorded at. The project names no repository, so Dispatch
 *  stores a result at any commit. */
const COMMIT = "a".repeat(40);
/** The Verification document of every issue below: one check, k1, before the merge, which no CI
 *  run proves, so a session's result is what records it. */
const VERIFICATION =
  "# Verification\n\n" +
  ':::check{#k1 surface="cli" run="go test ./..."}\n' +
  "The server records a result against a check.\n" +
  ":::\n";
/** What a role notice ends with, whichever role the call named. */
const LATER = "A later release refuses a result that names a role.";

/** A result as the server stores it, the fields a row reads. */
interface StoredResult {
  readonly role: string;
  readonly session: string;
}

/** A result's record as the route answers it, the field a row reads. */
interface RecordedResult {
  readonly result: StoredResult;
}

/** The checks of an issue with the results recorded against each, the fields a row reads. */
interface IssueResults {
  readonly checks: readonly {
    readonly block_id: string;
    readonly results: readonly StoredResult[];
  }[];
}

// Reserved here, at the file's module scope: the file's own `afterAll` removes the state directory
// once the `describe` below has ended, and hands the stack's to a removal it does not wait for.
const directories = useDispatchCliDirectories();

describe.skipIf(!dispatchStackEnabled)(
  "the role of a recorded result over the real CLI against a real Dispatch",
  () => {
    let stack: DispatchStack | undefined;

    const { asPerson, asMachine, createIssue, upload, runAs, holdRoles } = dispatchCliLive({
      stack: () => stack,
      session: NO_ROLE,
      person: PERSON,
      spec: "# Design\n\nThe design, as it stands.\n",
      directories,
    });

    /** A new issue of the project with the Verification document above, and its key. */
    async function issueWithCheck(title: string): Promise<string> {
      const issue = await createIssue(PROJECT, title);
      await upload(issue.key, "verification.md", VERIFICATION);
      return issue.key;
    }

    /** Every result the server stores against check k1 of the issue, newest first. */
    async function storedResults(issue: string): Promise<readonly StoredResult[]> {
      const { checks } = await asPerson<IssueResults>("GET", `/api/v1/issues/${issue}/results`);
      const check = checks.find((each) => each.block_id === "k1");
      if (check === undefined) throw new Error(`the results of ${issue} name no check k1`);
      return check.results;
    }

    beforeAll(async () => {
      stack = await startDispatchStack({ work: directories.stack });
      await asPerson("POST", "/api/v1/projects", { key: PROJECT, name: `${PROJECT} project` });
    }, 600_000);
    afterAll(async () => {
      await stack?.stop();
    }, 60_000);

    test("a session that holds no role names tester, and Dispatch records no role and says so", async () => {
      const issue = await issueWithCheck("Record a result as a session that holds no role");

      const run = await runAs(NO_ROLE, "result", [
        "--issue",
        issue,
        "--check",
        "k1",
        "--role",
        "tester",
        "--verdict",
        "pass",
        "--commit",
        COMMIT,
      ]);

      expect(run).toMatchObject({ code: 0 });
      expect(run.stdout).toContain(
        `Recorded pass for check k1 of ${issue} at ${COMMIT}, as session ${NO_ROLE}, which holds no role of ${issue}.`
      );
      const notice =
        "Dispatch did not take the role tester that this call named. It recorded the role that " +
        `the session holds on ${issue}: no role. ${LATER}`;
      expect(run.stdout).toContain(notice);
      expect(run.stdout.indexOf(notice)).toBeLessThan(
        run.stdout.indexOf("Pull request body line:")
      );
      expect(await storedResults(issue)).toEqual([
        expect.objectContaining({ role: "", session: NO_ROLE }),
      ]);
    }, 120_000);

    test("a result that names no role, recorded by a session that holds none, reads as that session's", async () => {
      const issue = await issueWithCheck("Read a result of a session that holds no role");
      const recorded = await asMachine<RecordedResult>("POST", `/api/v1/issues/${issue}/results`, {
        actor: { kind: "session", id: NO_ROLE },
        check: "k1",
        verdict: "pass",
        commit: COMMIT,
      });
      expect(recorded.result).toMatchObject({ role: "", session: NO_ROLE });
      // A call that names no role is told nothing about one: the answer carries no notice.
      expect(recorded).not.toHaveProperty("role_notice");
      expect(recorded).not.toHaveProperty("role_not_taken");

      const run = await runAs(NO_ROLE, "results", ["--issue", issue]);

      expect(run).toMatchObject({ code: 0 });
      expect(run.stdout).toContain(
        `  - pass by session ${NO_ROLE}, which holds no role of ${issue}, at ${COMMIT}.`
      );
    }, 120_000);

    test("the session the daemon published as tester names tester, and Dispatch records tester", async () => {
      const issue = await issueWithCheck("Record a result as the issue's tester");
      await holdRoles(issue, { tester: TESTER });

      const run = await runAs(TESTER, "result", [
        "--issue",
        issue,
        "--check",
        "k1",
        "--role",
        "tester",
        "--verdict",
        "pass",
        "--commit",
        COMMIT,
      ]);

      expect(run).toMatchObject({ code: 0 });
      expect(run.stdout).toContain(
        `Recorded pass for check k1 of ${issue} at ${COMMIT}, as tester.`
      );
      expect(run.stdout).toContain(
        "This call named the role tester. Dispatch records the role that the session holds, and " +
          `that is tester. ${LATER}`
      );
      expect(await storedResults(issue)).toEqual([
        expect.objectContaining({ role: "tester", session: TESTER }),
      ]);
    }, 120_000);
  }
);
