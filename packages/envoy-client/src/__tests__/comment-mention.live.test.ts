import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { join } from "node:path";
import type { CommentRead } from "@legion/contracts";
import { dispatchCliLive, useDispatchCliDirectories } from "../../test/dispatch-cli-live";
import {
  type DispatchStack,
  dispatchStackEnabled,
  startDispatchStack,
} from "../../test/dispatch-stack";

// A comment's --mention over the whole path it travels: the real `dispatch` CLI of this checkout,
// run as a child process against a real Dispatch server of this checkout, on a database of its
// own. The unit rows (dispatch-execute.test.ts) prove the request the command builds from the
// flag, and the server's own Go rows prove what Dispatch does with a mention it is handed; neither
// proves that the field the command fills is the field the server records an attempt for, which
// is what the row below posts a real comment to find out.

// Destructured: claude-envoy's typecheck, which compiles this package's test files, forbids
// `process.env.X`.
const { PATH: SHELL_PATH } = process.env;

/** The CLI entry the row runs, as an agent's shell reaches it: this checkout's own `dispatch`. */
const ENTRY = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");

/** The person who owns the project and the issue, as the server's header identity names them. */
const PERSON = "alice";
/** The session the CLI acts as, and so the comment's author. */
const SESSION = "session-comment-mention-live";
/** The project the row makes its issue in. */
const PROJECT = "MENTION";
/** The role the comment mentions. */
const TARGET = "role:legion-infra-infra-1-architect";

// Reserved here, at the file's module scope: the file's own `afterAll` removes each once the
// `describe` below has stopped the server writing into it.
const directories = useDispatchCliDirectories();

describe.skipIf(!dispatchStackEnabled)(
  "a comment's mention, posted by the real CLI against a real Dispatch",
  () => {
    let stack: DispatchStack | undefined;

    const { asPerson, createIssue } = dispatchCliLive({
      stack: () => stack,
      session: SESSION,
      person: PERSON,
      spec: "# Design\n\nThe design, as it stands.\n",
      directories,
    });

    /** The real CLI's `dispatch comment` on the issue, run as an agent's shell runs it, and the
     *  id of the comment it posted. A refusal, or anything on standard error, fails the row. */
    async function comment(issue: string, body: string, mention: string): Promise<string> {
      if (stack === undefined) throw new Error("the Dispatch stack did not start");
      const argv = ["comment", "--issue", issue, "--body", body, "--mention", mention];
      const child = Bun.spawn([process.execPath, ENTRY, ...argv], {
        env: {
          PATH: SHELL_PATH,
          HOME: directories.state,
          DISPATCH_STATE_DIR: directories.state,
          DISPATCH_HOST: "omp",
          DISPATCH_SESSION_ID: SESSION,
          DISPATCH_URL: stack.url,
          DISPATCH_TOKEN: stack.token,
        },
        stdout: "pipe",
        stderr: "pipe",
      });
      const [stdout, stderr, code] = await Promise.all([
        new Response(child.stdout).text(),
        new Response(child.stderr).text(),
        child.exited,
      ]);
      if (code !== 0 || stderr !== "") {
        throw new Error(`dispatch comment on ${issue} exited ${code}: ${stderr}${stdout}`);
      }
      const posted = /^Posted comment ([0-9a-f-]{36})/.exec(stdout);
      if (posted?.[1] === undefined) throw new Error(`dispatch comment printed: ${stdout}`);
      return posted[1];
    }

    beforeAll(async () => {
      stack = await startDispatchStack({ work: directories.stack });
      await asPerson("POST", "/api/v1/projects", { key: PROJECT, name: `${PROJECT} project` });
    }, 600_000);
    afterAll(async () => {
      await stack?.stop();
    }, 60_000);

    test("--mention records one delivery attempt for its target", async () => {
      const issue = await createIssue(PROJECT, "Hand the design to its architect");

      const id = await comment(issue.key, "Over to you.", TARGET);

      // The mention's first attempt commits with the comment itself (createCommentFor), so the
      // read that follows the command's answer already holds it, whatever its send did since.
      const read = await asPerson<CommentRead>("GET", `/api/v1/comments/${id}`);
      const attempts = (read.comment.deliveries ?? []).filter(
        (attempt) => attempt.target === TARGET
      );
      // The line the run prints is what Dispatch recorded of the attempt.
      console.log(`comment ${id} delivery attempts for ${TARGET}: ${JSON.stringify(attempts)}`);
      expect(read.comment.mentions?.map((mention) => mention.target)).toEqual([TARGET]);
      expect(attempts).toHaveLength(1);
      expect(attempts[0]).toMatchObject({ comment_id: id, target: TARGET, attempt: 1 });
    }, 120_000);
  }
);
