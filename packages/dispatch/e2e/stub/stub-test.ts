/** The `test` a stubbed row takes, and the two modes it runs in.
 *
 *  A stubbed row runs in a real browser against a recording: the page and its assets come from
 *  the build under test over e2e/stub/static-server.ts, every `/api/v1/**` and `/auth/whoami`
 *  answer comes from a committed HAR, `/ws/live` answers from the contract fixtures
 *  (e2e/stub/live-frames.ts), and there is no Go server, no Postgres, no fake listener, no
 *  sign-in and no database reset in the run at all.
 *
 *  A file that holds such rows is named `*.stub.e2e.ts`, which is the `stub` project's
 *  `testMatch` and every other project's `testIgnore` (e2e/playwright.config.ts). It declares its
 *  own `test` with `stubbed(seed)`:
 *
 *      const test = stubbed(async () => {
 *        await createProject({ key: "CORE", name: "Core" });
 *        const issue = await createIssue({ project: "CORE", title: "Review at tablet width" });
 *        return { issue: issue.key };
 *      });
 *
 *      test("tablet keeps the Conversation readable", async ({ page, scene }) => {
 *        await page.goto(`/issues/${scene.issue}/conversation`);
 *        ...
 *      });
 *
 *  `seed` runs in **record mode alone**. In replay the row is handed what that seeding produced,
 *  read from the scene file recorded beside the HAR, so an id the server minted never has to be
 *  written into a spec and a re-recording changes no source. Recording:
 *
 *      bun run e2e:stub:record                      # every stub row
 *      bun run e2e:stub:record -- layout.stub       # one file
 *
 *  which runs the same rows against the real harness, writes each row's HAR normalised
 *  (e2e/stub/recording.ts) and each row's scene beside it under `e2e/__recordings__/`.
 *
 *  What a stubbed row may assert, and what it may not, is `packages/dispatch/AGENTS.md`, "The
 *  stubbed layer". The short of it: a row that writes and then reads its own write does not
 *  belong here, because the recording answers the refetch with the state recorded before the
 *  write and the row would pass while proving nothing.
 */
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";
import { test as base, type Page } from "@playwright/test";

import { answerTo, type LiveClientFrame } from "./live-frames";
import { type Har, normalise, retargeted, written } from "./recording";
import { recordingPath } from "./recordings";
import { stubRecordVariable } from "./selection";

/** Everything a recording answers and nothing else. The document and its assets are the static
 *  server's, so that a pull request's own front-end change is the one under test. */
const replayedRoutes = /\/(api\/v1\/|auth\/whoami)/;

/** Answers `GET /api/v1/dashboard/builds` from the build under test rather than from the
 *  recording.
 *
 *  `web/src/features/shell/DeploymentResilience.tsx` polls that route and compares the entry
 *  chunk every live server process serves with the one this page loaded: where they differ it
 *  offers the reader a reload. A recorded answer names the entry chunk of the build it was
 *  recorded from, so every stubbed row would be told to reload the moment the SPA is rebuilt -
 *  which is every pull request that touches `web/src`. The entry this answers with is the one
 *  the page itself loaded, so the comparison finds no change and no notice is offered. */
async function answerTheBuildPoll(page: Page): Promise<void> {
  await page.route("**/api/v1/dashboard/builds", async (route) => {
    const entry = await page.evaluate(() => {
      const script = document.querySelector<HTMLScriptElement>('script[src^="/assets/"]');
      return script === null ? "" : new URL(script.src).pathname;
    });
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({ live: [entry], serving: entry }),
    });
  });
}

/** Answers `/ws/live` from the committed contract fixtures (e2e/stub/live-frames.ts). The socket
 *  is accepted rather than refused: a page whose live socket keeps failing shows its reconnecting
 *  state, which is a state no stubbed row means to assert. */
async function answerTheLiveSocket(page: Page): Promise<void> {
  await page.routeWebSocket("**/ws/live", (socket) => {
    socket.onMessage((sent) => {
      if (typeof sent !== "string") return;
      let frame: LiveClientFrame;
      try {
        frame = JSON.parse(sent) as LiveClientFrame;
      } catch {
        return;
      }
      const answer = answerTo(frame);
      if (answer !== undefined) socket.send(JSON.stringify(answer));
    });
  });
}

/** The `test` of one stubbed spec file, with the `scene` its rows read.
 *
 *  `seed` is the API seeding the rows need, run against the real harness in record mode and never
 *  in replay. What it returns is written beside the recording as JSON and handed back to the row,
 *  so the row names no server-minted id of its own. */
export function stubbed<Scene>(seed: () => Promise<Scene>) {
  return base.extend<{ scene: Scene }>({
    scene: async ({ page, context, baseURL }, use, test) => {
      if ((process.env[stubRecordVariable] ?? "") !== "1") {
        const har = recordingPath(test, ".har");
        if (!existsSync(har)) {
          // Not a refusal here, because a fixture is set up before the body runs and a row
          // whose own `test.skip` takes it out of this project - a phone row under `stub` -
          // would be refused before it could skip, and would have to be given an empty
          // recording to keep. So the row is let through with nothing routed, and a row that
          // did run is refused at teardown below, where its status is known.
          await use({} as Scene);
          if (test.status !== "skipped") {
            throw new Error(
              `This row has no recording at ${har}. Record it with \`bun run e2e:stub:record\` ` +
                "in packages/dispatch, which runs the stub rows against the real harness and " +
                "writes each row's recording and scene."
            );
          }
          return;
        }
        const retargetedHar = test.outputPath("recording.har");
        writeFileSync(
          retargetedHar,
          retargeted(readFileSync(har, "utf8"), new URL(baseURL ?? "").origin)
        );
        await answerTheLiveSocket(page);
        await page.routeFromHAR(retargetedHar, {
          notFound: "abort",
          update: false,
          url: replayedRoutes,
        });
        // After the HAR router, deliberately: Playwright runs the last registered handler
        // first (`page.route` unshifts, playwright-core 1.64.1 coreBundle.js:63255), and the
        // HAR router with `notFound: "abort"` never falls through. Registered before it, this
        // handler would never run and the build poll would be aborted rather than answered.
        await answerTheBuildPoll(page);
        await use(JSON.parse(readFileSync(recordingPath(test, ".scene.json"), "utf8")) as Scene);
        return;
      }

      // Record mode alone reaches the harness, so its modules are loaded only here: a replay run
      // has no server, no database and no fakes, and `e2e/seed.ts` would spawn `psql` against a
      // database that does not exist.
      const { resetDatabase } = await import("../seed");
      const { signIn } = await import("../users");
      await resetDatabase();
      await signIn(context, "alice");
      const value = await seed();
      const scene = recordingPath(test, ".scene.json");
      mkdirSync(dirname(scene), { recursive: true });
      writeFileSync(scene, `${JSON.stringify(value, null, 2)}\n`);
      const fresh = test.outputPath("recorded.har");
      await answerTheLiveSocket(page);
      await page.routeFromHAR(fresh, {
        update: true,
        updateContent: "embed",
        updateMode: "full",
        url: replayedRoutes,
      });
      await use(value);
      // Playwright writes the HAR when the context closes, and a fixture's teardown runs before
      // the context it depends on is destroyed, so the close is made here rather than waited for.
      await context.close();
      // A row this project's own `test.skip` took out wrote nothing worth keeping: its scene
      // was seeded before the body ran, and its recording would hold the page's first requests
      // or nothing at all. Both go, so the committed set is exactly the rows that run.
      if (test.status === "skipped") {
        rmSync(scene, { force: true });
        return;
      }
      const recorded = JSON.parse(readFileSync(fresh, "utf8")) as Har;
      writeFileSync(
        recordingPath(test, ".har"),
        written(normalise(recorded, new URL(baseURL ?? "").origin))
      );
    },
  });
}
