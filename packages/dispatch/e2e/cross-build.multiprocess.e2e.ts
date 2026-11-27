import { existsSync, readdirSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { routePath } from "../web/src/api/paths";
import type { BlockSchema, LiveClientFrame, LiveServerFrame } from "../web/src/api/types";
import { schemaCreatable } from "../web/src/features/doc/schema-creatable";
import {
  baseUrl,
  createAsk,
  createIssue,
  createProject,
  e2eAgentToken,
  getAsk,
  getIssueArtifactText,
  listComments,
} from "./api";
import { seedDeliveryFixture } from "./delivery-seed";
import { connectedDot, documentEditor, typeAtEnd } from "./editor";
import { firstRolloutCommit, firstRolloutRefusal, type LiveFleet } from "./first-rollout";
import {
  apiPinCookie,
  binaryB,
  type DispatchProcess,
  dispatchProcesses,
  entryChunkOf,
  pagePinCookie,
  pinnedTo,
  webDistOf,
} from "./harness-processes";
import { sql } from "./psql";
import type { ProxiedRequest, ProxiedSocket } from "./round-robin-proxy";
import {
  type Browser,
  type BrowserContext,
  expect,
  type Page,
  rowKey,
  test,
  type WebSocket,
} from "./test";
import { signIn } from "./users";

/**
 * A rolling release runs two builds of Dispatch at once behind one load balancer, so a page one
 * build served talks to the other build's server. Here a browser context's page requests (its
 * navigations, `index.html`, `/assets/*`) are pinned to one process and its API, event stream,
 * websockets and `/auth` to the other (the proxy's `dispatch-e2e-page` and `dispatch-e2e-api`
 * cookies, e2e/round-robin-proxy.ts). With `DISPATCH_E2E_BINARY_B` naming a previous release's
 * envoy-dispatch and `DISPATCH_E2E_WEB_DIST_B` its web dist, Dispatch B is that release, so each
 * direction runs one build's page against the other build's server; without it both processes run
 * this tree. In each direction the page signs in, opens a project's issue list and an issue, posts
 * a comment, answers an ask, opens the issue's document - where that server admits the page
 * writable it types and sees the edit saved and shown in a context of the other build's page, and
 * where it admits the page read-only the page says so and the stored document is untouched
 * (`pageWritable`) - and opens the Delivery page, whose live socket subscribes to the timeline and
 * is answered a snapshot. Every selector is one both builds' own browser suites use. Each
 * direction then reads the entry chunk its page loaded and holds it to the one that build's own
 * `index.html` names (`entryChunkOf`), so a direction whose server is the previous release but
 * whose page this tree served is a failure here rather than a row that passes unnoticed.
 *
 * With `DISPATCH_E2E_FIRST_ROLLOUT_COMMIT` set (CI's second run of this spec;
 * e2e/first-rollout.ts), Dispatch B is the first-rollout build, the newest release before the
 * first that registers its schema numbers, and the variable holds the run to that build: B's
 * `COMMIT` file, beside `DISPATCH_E2E_BINARY_B`, must hold that commit, and the live registry must
 * read a lowest schema version of 0 while B is live. Each direction checks both before it reads
 * `pageWritable`, and fails with a sentence that names what it read. A run that sets the variable
 * but not `DISPATCH_E2E_BINARY_B` fails before any row starts.
 *
 * Each seed call names the process it runs on (`process` in e2e/api.ts, which pins its sign-in
 * there too), so the build that wrote a row and the build that reads it back are both known.
 */

/** What a message calls each process's build. */
const builds: Record<DispatchProcess, string> = {
  a: "this tree's build (Dispatch A)",
  b:
    binaryB === undefined
      ? "this tree's build (Dispatch B)"
      : firstRolloutCommit === undefined
        ? "the previous build (Dispatch B)"
        : "the first-rollout build (Dispatch B)",
};

const seedAgent = { kind: "session", id: "e2e-cross-build" } as const;

/** packages/envoy, and the directory build-dispatch-build.sh wrote the previous build into: the
 *  binary's own, which also holds that build's `pmdoc/schema/blocks.json`. Its `web/` dist is
 *  `webDistOf`'s to name, which this spec and e2e/two-builds.multiprocess.e2e.ts share. */
const envoyDir = fileURLToPath(new URL("../../envoy", import.meta.url));
const buildB = binaryB === undefined ? undefined : dirname(binaryB);

if (firstRolloutCommit !== undefined && buildB === undefined) {
  throw new Error(
    `DISPATCH_E2E_FIRST_ROLLOUT_COMMIT holds Dispatch B to the first-rollout build, commit ` +
      `${firstRolloutCommit}. DISPATCH_E2E_BINARY_B is unset, so Dispatch B runs this tree. ` +
      "Set DISPATCH_E2E_BINARY_B to that build's envoy-dispatch, or unset " +
      "DISPATCH_E2E_FIRST_ROLLOUT_COMMIT."
  );
}

/** The commit build-dispatch-build.sh wrote beside B's binary (`COMMIT`), read only in the
 *  first-rollout run, which holds it to `firstRolloutCommit`. Undefined when B has no such file. */
const commitFileB = buildB === undefined ? undefined : resolve(buildB, "COMMIT");
const commitB =
  firstRolloutCommit !== undefined && commitFileB !== undefined && existsSync(commitFileB)
    ? readFileSync(commitFileB, "utf8").trim()
    : undefined;

/** The schema manifest a build's server reads and admits a page by, and its page carries. A
 *  manifest before the typed blocks names no `types`, which reads here as none. */
function manifestOf(process: DispatchProcess): BlockSchema {
  const path =
    process === "b" && buildB !== undefined
      ? resolve(buildB, "pmdoc", "schema", "blocks.json")
      : resolve(envoyDir, "internal", "dispatch", "pmdoc", "schema", "blocks.json");
  const manifest: unknown = JSON.parse(readFileSync(path, "utf8"));
  if (
    manifest === null ||
    typeof manifest !== "object" ||
    !("version" in manifest) ||
    typeof manifest.version !== "number"
  ) {
    throw new Error(`${path} declares no schema version`);
  }
  const schema = manifest as BlockSchema;
  return { ...schema, types: schema.types ?? [] };
}

/** Whether a build's page carries the manifest it was built with, which it presents as
 *  `schema_version` and `schema_creatable` (web/src/features/doc/schema.ts). A page of a build
 *  before that reads the schema from the server it talks to and sends `schema_version` alone. */
function pageCarriesManifest(process: DispatchProcess): boolean {
  if (process !== "b" || buildB === undefined) {
    return true;
  }
  const assets = resolve(buildB, "web", "assets");
  return readdirSync(assets).some(
    (file) =>
      file.endsWith(".js") &&
      readFileSync(resolve(assets, file), "utf8").includes("schema_creatable")
  );
}

/** The two numbers a page presents on its document socket: V, the schema version it reads, and
 *  R, the highest `since` among what it may create (`schemaCreatable`). A page that carries no
 *  manifest of its own takes V from the schema route of the server it talks to and presents an R
 *  of 1, since every element before the manifest has `since` 1. */
function pageNumbers(
  pageProcess: DispatchProcess,
  server: DispatchProcess
): { version: number; creatable: number } {
  if (!pageCarriesManifest(pageProcess)) {
    return { creatable: 1, version: manifestOf(server).version };
  }
  const manifest = manifestOf(pageProcess);
  return { creatable: schemaCreatable(manifest), version: manifest.version };
}

/** The numbers the live server processes register (`docs.readSchemaFleet`): the lowest V and the
 *  highest R among the rows whose heartbeat is inside the registry's 15 s live window, a copy of
 *  `processes.LiveWindow` (`packages/envoy/internal/dispatch/processes/processes.go`), which the
 *  servers read. A row of a build before the fleet columns holds no numbers, and its zeros are
 *  what it counts as. */
async function liveFleet(): Promise<LiveFleet> {
  const read = await sql(
    `select count(*), coalesce(min(schema_version), 0), coalesce(max(schema_creatable), 0)
     from server_processes where heartbeat_at > now() - make_interval(secs => 15)`
  );
  const [live, minVersion, maxCreatable] = read.split("|").map(Number);
  if (!(live > 0) || Number.isNaN(minVersion) || Number.isNaN(maxCreatable)) {
    throw new Error(`the registry's live rows read ${JSON.stringify(read)}, which names no fleet`);
  }
  return { maxCreatable, minVersion };
}

/**
 * Whether a page of `pageProcess` may write a document on the server of `server`, read from the
 * registry the two servers write, so the row holds for any previous build CI picks.
 *
 * A server of a build with the fleet columns admits a page by the whole fleet's numbers
 * (`packages/envoy/cmd/dispatch/README.md`, "Two builds at once"; `schemaFleet.admits`): the
 * page's R is at most every live process's V and at most the highest live R, and its V is at
 * least the highest live R (the document holds a heading and a paragraph, whose `since` is 1).
 * This tree's server is one, and it registers its own numbers, so a live row of V 0 is the
 * previous build's: a build before the fleet columns, which registers none. That V of 0 is under
 * every page's R, so every page of either build is read-only on this tree's server while that
 * build is live. A previous build that registers numbers equal to this tree's leaves every page of
 * either build writable on either server.
 *
 * A server of a build before the fleet columns knows no fleet: it compares the page's V with its
 * own manifest version exactly (`schemaReadOnly` in that build), so it admits a page of its own
 * version and holds a page of any other version read-only.
 *
 * With no previous build - both processes run this tree - the fleet is two processes of one
 * build, and a page of that build is writable on either.
 */
async function pageWritable(
  pageProcess: DispatchProcess,
  server: DispatchProcess
): Promise<boolean> {
  if (binaryB === undefined) {
    return true;
  }
  const page = pageNumbers(pageProcess, server);
  const fleet = await liveFleet();
  if (server === "b" && fleet.minVersion === 0) {
    return page.version === manifestOf("b").version;
  }
  return (
    page.creatable <= Math.min(fleet.minVersion, fleet.maxCreatable) &&
    page.version >= fleet.maxCreatable
  );
}

// The database's reset, before the worker's first row and before the review corpus's seed, and
// the fakes' before every row are the `test` this file imports (e2e/test.ts); the round-robin
// proxy's log is neither, so it is cleared here.
test.beforeEach(async () => {
  expect((await fetch(new URL("/_proxy/reset", baseUrl), { method: "POST" })).status).toBe(204);
});

/** A signed-in context whose page comes from `page` and whose every other request goes to `api`. */
async function splitContext(
  browser: Browser,
  page: DispatchProcess,
  api: DispatchProcess
): Promise<BrowserContext> {
  const context = await browser.newContext();
  await context.addCookies([
    { name: pagePinCookie, url: baseUrl, value: page },
    { name: apiPinCookie, url: baseUrl, value: api },
  ]);
  await signIn(context, "alice");
  return context;
}

/** The frames the page's live socket sends and receives, from the moment it opens. Both builds
 *  run every live subscription of a page over one `/ws/live` socket, with the same frames
 *  (web/src/api/live-socket.ts). */
function watchLiveSocket(page: Page): { sent: LiveClientFrame[]; received: LiveServerFrame[] } {
  const frames = { received: [] as LiveServerFrame[], sent: [] as LiveClientFrame[] };
  page.on("websocket", (socket: WebSocket) => {
    if (new URL(socket.url()).pathname !== "/ws/live") return;
    socket.on("framesent", ({ payload }) => {
      frames.sent.push(JSON.parse(String(payload)) as LiveClientFrame);
    });
    socket.on("framereceived", ({ payload }) => {
      frames.received.push(JSON.parse(String(payload)) as LiveServerFrame);
    });
  });
  return frames;
}

for (const pageProcess of dispatchProcesses) {
  const server: DispatchProcess = pageProcess === "a" ? "b" : "a";
  test(`a page of ${builds[pageProcess]} works against the server of ${builds[server]}`, async ({
    browser,
    delivery,
  }) => {
    const xb = rowKey("XB");
    test.setTimeout(120_000);
    // The first-rollout run holds B to its commit and to a registry that reads version 0 while B
    // is live. The harness starts both servers before any row, so both serve and both register
    // here, and pageWritable below reads the same registry.
    if (firstRolloutCommit !== undefined) {
      const refusal = firstRolloutRefusal(firstRolloutCommit, commitB, await liveFleet());
      if (refusal !== undefined) {
        throw new Error(refusal);
      }
    }
    const on = { process: server };
    await createProject({ key: xb, name: "Cross build" }, on);
    const issue = await createIssue(
      { project: xb, spec: "# Cross build\n\nWritten before the edit.\n", title: "Two builds" },
      on
    );
    const ask = await createAsk(
      issue.key,
      { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
      { ...on, actor: seedAgent, as: "agent" }
    );
    // The Delivery page's timeline, written in Postgres in the row's own place (`delivery`,
    // e2e/test.ts): the live socket answers its timeline subscription with a snapshot only once
    // delivery is configured.
    await seedDeliveryFixture(delivery);

    const context = await splitContext(browser, pageProcess, server);
    try {
      const page = await context.newPage();
      const live = watchLiveSocket(page);

      // The project's issue list, and the issue from it. Each build's list renders exactly one
      // link carrying `data-issue-link` per issue, and that is what this names: a list row of a
      // build before #2988 is itself that link, and the table of a build from #2988 on gives it
      // the title cell beside a second link holding the key alone, which a name read of the key
      // would match as well.
      await page.goto(`/projects/${xb}/issues`);
      await page.locator(`a[data-issue-link][href="/issues/${issue.key}"]`).click();
      await expect(documentEditor(page)).toContainText("Written before the edit.");

      // A comment, which the other build's server lists.
      await page.goto(`/issues/${issue.key}/conversation`);
      const comment = `A comment from ${builds[pageProcess]}'s page`;
      const message = page
        .getByRole("form", { name: "Comment composer" })
        .getByRole("textbox", { name: "Comment" });
      await message.fill(comment);
      await message.press("Control+Enter");
      await expect(
        page.getByRole("list", { name: "Conversation turns" }).getByText(comment)
      ).toBeVisible();
      await expect
        .poll(async () =>
          (await listComments(issue.key, undefined, { process: pageProcess })).map(
            (written) => written.body
          )
        )
        .toContain(comment);

      // The ask, answered in place, which the other build's server reads as answered.
      const card = page.getByRole("region", { name: "Conversation" }).getByTestId(`ask-${ask.id}`);
      await card.getByRole("radio", { name: "Ship" }).check();
      await card.getByRole("button", { exact: true, name: "Answer" }).click();
      await expect(
        card.getByRole("list", { name: "Options" }).locator('li[data-selected="true"]')
      ).toContainText("Ship");
      await expect
        .poll(async () => ((await getAsk(ask.id, { process: pageProcess })).answers ?? []).length)
        .toBe(1);

      // The issue's document. Where the other build's server admits this page writable
      // (pageWritable), an edit typed here is saved by that server, then read from the store by
      // this page's own server, and shown below in a context of the other build's page. Where it
      // admits the page read-only - a page whose version a server before the fleet admission does
      // not match, or any page at all on a server of this tree while a process of a build before
      // the fleet columns is live - the page says so, takes no edit, and the stored document is
      // untouched.
      await page.goto(`/issues/${issue.key}/spec`);
      await expect(documentEditor(page)).toContainText("Written before the edit.");
      await expect(connectedDot(page)).toHaveText("connected");
      const edit = `Typed on ${builds[pageProcess]}'s page.`;
      const writable = await pageWritable(pageProcess, server);
      if (writable) {
        await typeAtEnd(page, edit);
      } else {
        await expect(page.locator('.dispatch-doc[data-read-only="true"]')).toBeVisible();
      }
      for (const reader of [server, pageProcess]) {
        await expect
          .poll(
            async () =>
              (await getIssueArtifactText(issue.key, "spec", { process: reader })).markdown,
            { timeout: 30_000 }
          )
          .toContain(writable ? edit : "Written before the edit.");
        if (!writable) {
          const { markdown } = await getIssueArtifactText(issue.key, "spec", { process: reader });
          expect(markdown).not.toContain(edit);
        }
      }

      // The Delivery page on the place's day: the page's live socket subscribes to the timeline
      // view, and the other build's server answers that subscription with a snapshot.
      await page.goto(`/delivery?${delivery.timelineQuery}`);
      await expect(page.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();
      await expect
        .poll(() =>
          live.sent.flatMap((frame) => (frame.type === "subscribe" ? [frame.view.kind] : []))
        )
        .toContain("timeline");
      await expect
        .poll(() => {
          const timeline = live.sent.flatMap((frame) =>
            frame.type === "subscribe" && frame.view.kind === "timeline" ? [frame.sub] : []
          );
          return live.received.some(
            (frame) => frame.type === "snapshot" && timeline.includes(frame.sub)
          );
        })
        .toBe(true);

      // The split held: every asset came from the page's process, and the document and live
      // sockets went to the other. Read before the other build's context opens a page of its own.
      const assets = (
        (await (await fetch(new URL("/_proxy/requests", baseUrl))).json()) as ProxiedRequest[]
      ).filter((request) => request.path.startsWith("/assets/"));
      expect(assets.length).toBeGreaterThan(0);
      expect(assets.filter((request) => request.upstream !== pageProcess)).toEqual([]);
      const sockets = (await (
        await fetch(new URL("/_proxy/sockets", baseUrl))
      ).json()) as ProxiedSocket[];
      for (const path of ["/ws/doc/", "/ws/live"]) {
        const opened = sockets.filter((socket) => socket.path.startsWith(path));
        expect(opened.length, `a ${path} socket opened`).toBeGreaterThan(0);
        expect(opened.filter((socket) => socket.upstream !== server)).toEqual([]);
      }

      // And the page this whole row ran on is that process's own build's: the entry chunk the
      // document it loaded names is the one `index.html` of that build names. Nothing above can
      // tell one build's page from the other's - a page of this tree drives every row here
      // against the previous build's server just as B's own page does - so this is what holds
      // B's page to B's binary, which is read from the `web/` beside that binary and not from
      // the `DISPATCH_E2E_WEB_DIST_B` the harness was handed: a run that served B this tree's
      // page while running the previous release's server fails here, as a dropped default for
      // that variable in packages/dispatch/package.json made it do. Where there is no previous
      // build, B's dist is whatever the harness serves it (`webDistOf`, which `bun run
      // e2e:multiprocess` points at this tree's own `web/dist-b`, a second build of this source
      // under another build id, and which is A's dist when nothing names one): the assertion then
      // says the page came from the process it was pinned to and nothing about a second build.
      const loaded = await page
        .locator('head > script[type="module"][src^="/assets/"]')
        .getAttribute("src");
      expect(loaded, `the page was served by ${builds[pageProcess]}`).toBe(
        entryChunkOf(webDistOf(pageProcess))
      );

      // The document, in a context of the other build's page against this page's own server: the
      // edit where this page could make one, and the text it opened with where it could not.
      const otherBuild = await splitContext(browser, server, pageProcess);
      try {
        const reading = await otherBuild.newPage();
        await reading.goto(`/issues/${issue.key}/spec`);
        await expect(documentEditor(reading)).toContainText(
          writable ? edit : "Written before the edit."
        );
      } finally {
        await otherBuild.close();
      }
    } finally {
      await context.close();
    }
  });
}

// A newer build writes an element this one does not know into a document both serve. Each page
// opening it says so, and offers no upload to replace it: a replacement deletes that element.
test("a document holding newer content shows that state and no replace drop zone", async ({
  browser,
}) => {
  const newKey = rowKey("NEW");
  test.skip(
    binaryB !== undefined,
    "the newer-content hook is this tree's; Dispatch B runs the previous build (DISPATCH_E2E_BINARY_B)"
  );
  await createProject({ key: newKey, name: "Newer content" }, { process: "a" });
  const issue = await createIssue(
    { project: newKey, spec: "Written before the newer element.\n", title: "Newer content" },
    { process: "a" }
  );
  // A /_test/ route goes to both processes (e2e/round-robin-proxy.ts).
  const injected = await fetch(
    new URL(
      routePath("post", "/api/v1/artifacts/{id}/_test/newer-content", {
        id: issue.primary_artifact_id,
      }),
      baseUrl
    ),
    { headers: { Authorization: `Bearer ${e2eAgentToken}` }, method: "POST" }
  );
  expect(injected.status, await injected.text()).toBe(204);
  for (const target of dispatchProcesses) {
    const text = await fetch(
      new URL(
        routePath("get", "/api/v1/artifacts/{id}/text", { id: issue.primary_artifact_id }),
        baseUrl
      ),
      { headers: { Authorization: `Bearer ${e2eAgentToken}`, ...pinnedTo(target) } }
    );
    expect(text.status).toBe(409);
    expect(await text.text()).toContain('"code":"DOC_NEWER_CONTENT"');
  }

  for (const target of dispatchProcesses) {
    const context = await splitContext(browser, target, target);
    try {
      const page = await context.newPage();
      await page.goto(`/issues/${issue.key}/spec`);
      const editor = page.getByRole("region", { name: "Document editor" });
      await expect(
        editor.getByRole("status").filter({
          hasText: "This document holds content this version of Dispatch cannot show yet.",
        })
      ).toBeVisible();
      await expect(
        editor.getByText("Upload markdown to replace and repair this document.")
      ).toHaveCount(0);
      await expect(editor.getByLabel("Upload artifact")).toHaveCount(0);
      await expect(documentEditor(page)).toBeHidden();
    } finally {
      await context.close();
    }
  }
});
