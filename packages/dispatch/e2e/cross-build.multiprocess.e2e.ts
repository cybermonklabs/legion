import { readdirSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import type { LiveClientFrame, LiveServerFrame } from "../web/src/api/types";
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
import {
  apiPinCookie,
  binaryB,
  type DispatchProcess,
  dispatchProcesses,
  pagePinCookie,
  pinnedTo,
} from "./harness-processes";
import type { ProxiedRequest, ProxiedSocket } from "./round-robin-proxy";
import { type Browser, type BrowserContext, expect, type Page, test, type WebSocket } from "./test";
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
 * `index.html` names (`entryChunk`), so a direction whose server is the previous release but
 * whose page this tree served is a failure here rather than a row that passes unnoticed.
 *
 * Each seed call names the process it runs on (`process` in e2e/api.ts, which pins its sign-in
 * there too), so the build that wrote a row and the build that reads it back are both known.
 */

/** What a message calls each process's build. */
const builds: Record<DispatchProcess, string> = {
  a: "this tree's build (Dispatch A)",
  b: binaryB === undefined ? "this tree's build (Dispatch B)" : "the previous build (Dispatch B)",
};

const seedAgent = { kind: "session", id: "e2e-cross-build" } as const;

/** packages/envoy, which `DISPATCH_E2E_BINARY_B` is a path from, and the directory
 *  build-dispatch-build.sh wrote that build into: the binary's own, which also holds the build's
 *  `pmdoc/schema/blocks.json` and its `web/` dist. */
const envoyDir = fileURLToPath(new URL("../../envoy", import.meta.url));
const buildB = binaryB === undefined ? undefined : dirname(resolve(envoyDir, binaryB));

/** The web dist each process's build serves: this tree's `web/dist` for A, and for B the `web/`
 *  beside the envoy-dispatch `DISPATCH_E2E_BINARY_B` names, which build-dispatch-build.sh wrote
 *  into that build's one directory, or this tree's `web/dist-b` where there is no previous build
 *  (the second build of this source `bun run e2e:multiprocess` makes). It is read off the build
 *  itself rather than off `DISPATCH_E2E_WEB_DIST_B`, which is only what the harness was told to
 *  serve: what the row below holds is that the two agree. */
const webDist: Record<DispatchProcess, string> = {
  a: resolve(envoyDir, "..", "dispatch", "web", "dist"),
  b:
    buildB === undefined
      ? resolve(envoyDir, "..", "dispatch", "web", "dist-b")
      : resolve(buildB, "web"),
};

/** The entry chunk a build's `index.html` names, `/assets/index-*.js`: the file a page of that
 *  build loads itself from, and the one thing in the page that names the build it came from
 *  (`readBuild` in e2e/two-builds.multiprocess.e2e.ts reads it the same way). */
function entryChunk(process: DispatchProcess): string {
  const page = resolve(webDist[process], "index.html");
  const entry = /<script type="module" crossorigin src="(\/assets\/[^"]+\.js)"><\/script>/.exec(
    readFileSync(page, "utf8")
  )?.[1];
  if (entry === undefined) {
    throw new Error(`${page} names no entry chunk`);
  }
  return entry;
}

/** The schema manifest version a build's server reads and admits a page by. */
function manifestVersion(process: DispatchProcess): number {
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
  return manifest.version;
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

/**
 * Whether a page of `pageProcess` may write a document on the server of `server`.
 *
 * Dispatch B is the previous build when `DISPATCH_E2E_BINARY_B` names one. That build writes no
 * schema numbers into its `server_processes` row, so this tree's admission counts it as a
 * process that reads version 0, which no page's `schema_creatable` is at or under: while it is
 * live, a server of this tree admits every page read-only, its own and B's alike
 * (`docs.readSchemaFleet`, and `packages/envoy/cmd/dispatch/README.md`, "Two builds at once").
 *
 * B's own server knows no fleet: it compares the page's `schema_version` with its own manifest
 * version exactly (`schemaReadOnly` in that build), so it admits a page of its own version, and
 * holds a page of any other version read-only. A page that carries no manifest of its own takes
 * its `schema_version` from the schema route of the server it talks to, so against B's server it
 * presents B's own version.
 *
 * With no previous build - both processes run this tree - the fleet is two processes of one
 * build, and a page of that build is writable on either.
 */
function pageWritable(pageProcess: DispatchProcess, server: DispatchProcess): boolean {
  if (binaryB === undefined) {
    return true;
  }
  if (server === "b") {
    return (
      !pageCarriesManifest(pageProcess) || manifestVersion(pageProcess) === manifestVersion("b")
    );
  }
  return false;
}

/** The day `seedDeliveryFixture` writes its timeline on. */
const deliveryWindow = "from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z";

// The database reset is the `test` this file imports (e2e/test.ts); the round-robin proxy's log
// is not in the database, so it is cleared here.
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
  }) => {
    test.setTimeout(120_000);
    const on = { process: server };
    await createProject({ key: "XB", name: "Cross build" }, on);
    const issue = await createIssue(
      { project: "XB", spec: "# Cross build\n\nWritten before the edit.\n", title: "Two builds" },
      on
    );
    const ask = await createAsk(
      issue.key,
      { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
      { ...on, actor: seedAgent, as: "agent" }
    );
    // The Delivery page's timeline, written in Postgres: the live socket answers its timeline
    // subscription with a snapshot only once delivery is configured.
    await seedDeliveryFixture();

    const context = await splitContext(browser, pageProcess, server);
    try {
      const page = await context.newPage();
      const live = watchLiveSocket(page);

      // The project's issue list, and the issue from it. Each build's list renders exactly one
      // link carrying `data-issue-link` per issue, and that is what this names: a list row of a
      // build before #2988 is itself that link, and the table of a build from #2988 on gives it
      // the title cell beside a second link holding the key alone, which a name read of the key
      // would match as well.
      await page.goto("/projects/XB/issues");
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
      // admits the page read-only - a page whose version its own rule does not match, or any
      // page at all on a server of this tree while a process of the previous build is live -
      // the page says so, takes no edit, and the stored document is untouched.
      await page.goto(`/issues/${issue.key}/spec`);
      await expect(documentEditor(page)).toContainText("Written before the edit.");
      await expect(connectedDot(page)).toHaveText("connected");
      const edit = `Typed on ${builds[pageProcess]}'s page.`;
      const writable = pageWritable(pageProcess, server);
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

      // The Delivery page on the fixture's day: the page's live socket subscribes to the timeline
      // view, and the other build's server answers that subscription with a snapshot.
      await page.goto(`/delivery?${deliveryWindow}`);
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
      // build the two processes are one build of the server, and B serves this tree's own
      // `web/dist-b`, a second build of this source under another build id: the assertion then
      // says the page came from the process it was pinned to and nothing about a second build.
      const loaded = await page
        .locator('head > script[type="module"][src^="/assets/"]')
        .getAttribute("src");
      expect(loaded, `the page was served by ${builds[pageProcess]}`).toBe(entryChunk(pageProcess));

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
  test.skip(
    binaryB !== undefined,
    "the newer-content hook is this tree's; Dispatch B runs the previous build (DISPATCH_E2E_BINARY_B)"
  );
  await createProject({ key: "NEW", name: "Newer content" }, { process: "a" });
  const issue = await createIssue(
    { project: "NEW", spec: "Written before the newer element.\n", title: "Newer content" },
    { process: "a" }
  );
  // A /_test/ route goes to both processes (e2e/round-robin-proxy.ts).
  const injected = await fetch(
    new URL(`/api/v1/artifacts/${issue.primary_artifact_id}/_test/newer-content`, baseUrl),
    { headers: { Authorization: `Bearer ${e2eAgentToken}` }, method: "POST" }
  );
  expect(injected.status, await injected.text()).toBe(204);
  for (const target of dispatchProcesses) {
    const text = await fetch(
      new URL(`/api/v1/artifacts/${issue.primary_artifact_id}/text`, baseUrl),
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
