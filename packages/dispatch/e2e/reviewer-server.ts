/** The reviewer's Dispatch server (INFRA-3231's R5), which `e2e/ask-card-reviewer.e2e.ts` reads
 *  a held text through, and the `test` that row takes it from.
 *
 *  The reviewer holds every ask, comment, message and new issue an agent writes to a person while
 *  its settings say `hold`, which is a setting of the server's boot. On the stack's own server it
 *  would hold every other spec's agent writes as well, so the reviewer's rows start a second
 *  server of their own instead: `run-server.sh` against the stack's database and fakes, under the
 *  reviewer's settings and the stand-in model, started once per worker that runs one of them and
 *  stopped with that worker. No other spec pays for it. In CI its rows run in the `ask-card` job of
 *  `.github/workflows/dispatch-browser.yaml` on the pull-request lane, beside `ask-card-rewrite`,
 *  `ask-card-define`, `ask-card-menu` and `ask-card-quote`, and in the full lane's shards.
 *
 *  Both servers serve one database, which `e2e/test.ts` empties before every row. The writes a
 *  row means to be held go to this server, and so does the person's browser, which signs in here
 *  with this server's own cookie: the live socket that shows a held text once it posts is this
 *  server's. Seeds that no reviewer would read (a project, an issue a person opens) may go through
 *  either. Only this server runs the reviewer's sweep, since only it sets the reviewer's model
 *  group, so a held text posts here or nowhere. After every row this server's live documents are
 *  closed, as `resetDatabase` closes the stack server's, so a room a row left open settles before
 *  the next reset empties its tables.
 */
import { join } from "node:path";

import type { Browser, BrowserContext } from "@playwright/test";

import { devSignInError, devSignInPath, quiesceDocuments } from "./api";
import { ownDispatchServer } from "./harness-listeners";
import {
  harnessPorts,
  harnessVariables,
  stacksVariable,
  stackValue,
  withoutHarnessPort,
} from "./harness-ports";
import { type Harness, startHarness } from "./harness-start";
import { test as base } from "./test";

/** The deployment the reviewer's model group names, which its calls to the stand-in carry. */
export const REVIEWER_MODEL = "stand-in-reviewer";

/** How long a writer session has to act on a held text's findings before the setting acts: short,
 *  so a row reads the reviewer's own rewrite post within the reviewer's 10-second sweep after it. */
export const REVIEWER_WRITER_WINDOW = "2s";

/** The reviewer's settings, every one of R5's choices that the rows read: each kind held, the
 *  reviewer's rewrite posted when the writer does not act, and the model group at the stand-in. */
const reviewerSettings: Readonly<Record<string, string>> = {
  DISPATCH_REVIEWER_ASKS: "hold",
  DISPATCH_REVIEWER_COMMENTS: "hold",
  DISPATCH_REVIEWER_ISSUES: "hold",
  DISPATCH_REVIEWER_MESSAGES: "hold",
  DISPATCH_REVIEWER_MODEL_DEPLOYMENT: REVIEWER_MODEL,
  DISPATCH_REVIEWER_MODEL_KIND: "anthropic",
  DISPATCH_REVIEWER_MODEL_TOKENS_PER_MINUTE: "1000000",
  DISPATCH_REVIEWER_UNEDITED: "rewrite",
  DISPATCH_REVIEWER_WRITER_WINDOW: REVIEWER_WRITER_WINDOW,
};

/** The variables `run-server.sh` reads from its stack: every harness port it names a fake by,
 *  the database and the file store's bucket, the stack count it sizes its runtime by, and the
 *  stack's cookie key, which every server of one database must sign with. In a pooled run they
 *  are in the stack's manifest record alone, so they are handed on as plain variables, as
 *  `e2e/harness-pool.ts` hands its own server the same ones. */
const sharedKey = "DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY";
const stackInputs = [
  ...Object.values(harnessVariables).filter((name) => name !== harnessVariables.dispatch),
  "DATABASE_URL",
  "DISPATCH_E2E_FILE_STORE_BUCKET",
  "DISPATCH_E2E_STACK_COUNT",
  sharedKey,
];

/** The reviewer's server, as a row addresses it. */
export interface ReviewerServer {
  /** Its origin, `http://127.0.0.1:<port>`, which it serves its dashboard and API under. */
  readonly url: string;
}

async function startReviewerServer(logDir: string): Promise<{ harness: Harness; url: string }> {
  // The stack server's port is cleared under both names it is carried by, so this server's row
  // binds a port the OS chooses rather than the one that server holds.
  const cleared = withoutHarnessPort("dispatch");
  const own: Record<string, string> = {};
  for (const name of stackInputs) {
    const value = stackValue(cleared, name);
    if (value !== undefined && value !== "") own[name] = value;
  }
  if (own[sharedKey] === undefined) {
    throw new Error(
      `the reviewer's server needs its stack's ${sharedKey}, which e2e/harness-pool.ts gives ` +
        "each stack of a one-process dev sign-in run: without it the stack's server refuses a " +
        "second server on its database, which would sign cookies with another key"
    );
  }
  const manifest = cleared[stacksVariable];
  const settings = {
    ...reviewerSettings,
    DISPATCH_TEST_MODEL_URL: `http://127.0.0.1:${harnessPorts().fakeModel.port}`,
  };
  const harness = await startHarness({
    env: {
      ...own,
      ...(manifest === undefined ? {} : { [stacksVariable]: manifest }),
      [harnessVariables.dispatch]: "",
    },
    listeners: [
      ownDispatchServer("reviewer-dispatch", "the reviewer's Dispatch server", {
        ...settings,
        DISPATCH_E2E_PASS_THROUGH: Object.keys(settings).join(" "),
      }),
    ],
    logDir,
  });
  return { harness, url: `http://127.0.0.1:${harness.env[harnessVariables.dispatch]}` };
}

/** The suite's `test` with the reviewer's server: started for the worker the first time a row
 *  asks for it, and every row that has it ends with its live documents closed. */
export const test = base.extend<{ reviewerDocuments: undefined }, { reviewer: ReviewerServer }>({
  reviewer: [
    // biome-ignore lint/correctness/noEmptyPattern: the server takes no other fixture.
    async ({}, use, workerInfo) => {
      const { harness, url } = await startReviewerServer(
        join(workerInfo.project.outputDir, `reviewer-server-${workerInfo.workerIndex}`)
      );
      try {
        await use({ url });
      } finally {
        await harness.stop();
      }
    },
    // A cold `go build` of the server is the long pole of a first start, as it is the stack
    // server's (`serverBootMs`, e2e/harness-listeners.ts); CI hands both a prebuilt binary.
    { scope: "worker", timeout: 600_000 },
  ],
  reviewerDocuments: [
    async ({ reviewer }, use) => {
      await use(undefined);
      await quiesceDocuments({ baseUrl: reviewer.url });
    },
    { auto: true },
  ],
});

export { expect } from "./test";

/** A browser context signed in as `login` at the reviewer's server, under its own cookie, with
 *  that server as the context's base URL. */
export async function atReviewer(
  browser: Browser,
  reviewer: ReviewerServer,
  login = "alice"
): Promise<BrowserContext> {
  const context = await browser.newContext({
    baseURL: reviewer.url,
    viewport: { height: 900, width: 1440 },
  });
  const response = await context.request.get(devSignInPath(login), { maxRedirects: 0 });
  if (response.status() !== 302) {
    await context.close();
    throw devSignInError(login, response.status(), await response.text());
  }
  return context;
}
