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
  entryChunkOf,
  pagePinCookie,
  pinnedTo,
  webDistOf,
} from "./harness-processes";
import { sql } from "./psql";
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
 * `index.html` names (`entryChunkOf`), so a direction whose server is the previous release but
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

/** packages/envoy, and the directory build-dispatch-build.sh wrote the previous build into: the
 *  binary's own, which also holds that build's `pmdoc/schema/blocks.json`. Its `web/` dist is
 *  `webDistOf`'s to name, which this spec and e2e/two-builds.multiprocess.e2e.ts share. */
const envoyDir = fileURLToPath(new URL("../../envoy", import.meta.url));
const buildB = binaryB === undefined ? undefined : dirname(binaryB);

/** An entry of a schema manifest that carries versions: an element a build reads from `since` on
 *  and creates from `creatable_since` on, each 1 where the entry names none. */
type ElementVersions = { since?: number; creatable_since?: number };

/** What this spec reads of a build's schema manifest (`pmdoc/schema/blocks.json`): V, its version;
 *  R, the highest since among the elements a build of that version may create; and the since of
 *  each node it declares. A build before the fleet admission declares no nodes. */
type Manifest = { version: number; creatable: number; nodeSince: ReadonlyMap<string, number> };

function manifestOf(process: DispatchProcess): Manifest {
  const path =
    process === "b" && buildB !== undefined
      ? resolve(buildB, "pmdoc", "schema", "blocks.json")
      : resolve(envoyDir, "internal", "dispatch", "pmdoc", "schema", "blocks.json");
  const manifest = JSON.parse(readFileSync(path, "utf8")) as {
    version?: unknown;
    nodes?: (ElementVersions & { name: string })[];
    marks?: ElementVersions[];
    types?: (ElementVersions & {
      attributes?: Record<
        string,
        ElementVersions & { choices?: string[]; choice_versions?: Record<string, ElementVersions> }
      >;
    })[];
  };
  const { version } = manifest;
  if (typeof version !== "number") {
    throw new Error(`${path} declares no schema version`);
  }
  const nodes = manifest.nodes ?? [];
  // Every element: each node, mark and typed block, each attribute of a typed block, and each
  // choice of an enum attribute.
  const elements: ElementVersions[] = [
    ...nodes,
    ...(manifest.marks ?? []),
    ...(manifest.types ?? []).flatMap((type) => [
      type,
      ...Object.values(type.attributes ?? {}).flatMap((attribute) => [
        attribute,
        ...(attribute.choices ?? []).map((choice) => attribute.choice_versions?.[choice] ?? {}),
      ]),
    ]),
  ];
  const creatable = elements
    .filter((element) => (element.creatable_since ?? 1) <= version)
    .map((element) => element.since ?? 1);
  return {
    creatable: Math.max(0, ...creatable),
    nodeSince: new Map(nodes.map((node) => [node.name, node.since ?? 1])),
    version,
  };
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

/** The schema numbers a page presents on its document socket: C, its `schema_version`, and R, its
 *  `schema_creatable`. */
type PageSchema = { version: number; creatable: number };

/** C and R of a page of `pageProcess` that talks to the server of `server`. A page that carries
 *  its build's manifest presents that manifest's V and R. A page of a build before that presents
 *  the version of the server it talks to, which it reads from that server's schema route, and no
 *  `schema_creatable`, which the server reads as R 1. */
function pageSchema(pageProcess: DispatchProcess, server: DispatchProcess): PageSchema {
  if (!pageCarriesManifest(pageProcess)) {
    return { creatable: 1, version: manifestOf(server).version };
  }
  const { creatable, version } = manifestOf(pageProcess);
  return { creatable, version };
}

/** The nodes the seeded document holds: the document, its heading and its paragraph, and their
 *  text. */
const seededNodes = ["doc", "heading", "paragraph", "text"];

/** D on the server of `server`: the schema version the seeded document's elements need, which is
 *  the highest since that server's manifest gives the nodes the document holds. A node the
 *  manifest does not declare counts for nothing. */
function documentSince(server: DispatchProcess): number {
  const { nodeSince } = manifestOf(server);
  return Math.max(0, ...seededNodes.map((name) => nodeSince.get(name) ?? 0));
}

/** One live server process's row in the registry (`server_processes`): the commit its build is
 *  stamped with, and the schema numbers it registers. A process of a build before the fleet
 *  columns registers none, and its row reads 0 for both. */
type ProcessRow = { commit: string; version: number; creatable: number };

/** The rows of the live server processes, as the admission reads them (`processes.Live`): each row
 *  whose heartbeat is within the registry's live window, 15 seconds (`processes.LiveWindow`), of
 *  the database's now. Both of this run's processes register before they serve, so a registry with
 *  no live row is a failure here, not an empty fleet. */
async function liveProcesses(): Promise<ProcessRow[]> {
  const rows = (
    await sql(
      `select commit, schema_version, schema_creatable from server_processes
       where heartbeat_at > now() - interval '15 seconds' order by started_at, instance`
    )
  )
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => {
      const [commit = "", version, creatable] = line.split("|");
      return { commit, creatable: Number(creatable), version: Number(version) };
    });
  if (rows.length === 0) {
    throw new Error("the registry holds no live server process, so no fleet admits a page");
  }
  return rows;
}

/**
 * Whether a page of `pageProcess` may write the seeded document on the server of `server` while
 * the processes of `fleet` are live, and why: the numbers the answer rests on, which the row
 * records beside its expectation.
 *
 * A server that admits by the fleet takes minV, the lowest `schema_version`, and maxR, the highest
 * `schema_creatable`, over the live processes' rows. It lets a page write a document whose
 * elements need D only where R <= min(minV, maxR) and C >= max(maxR, D)
 * (`packages/envoy/cmd/dispatch/README.md`, "Two builds at once"). This tree's server admits so,
 * and so does Dispatch B's where that build registers schema numbers, as every release from
 * legion-envoy-v30.21.0 does. The rule is written out here from those numbers and is not read off
 * a server, so a server that decides otherwise fails the row.
 *
 * A build before that registers no schema numbers, and its row reads 0 for both. This tree's
 * processes always register theirs, so a live row that reads 0 is Dispatch B's. Such a server
 * knows no fleet: it compares the page's `schema_version` with its own manifest version exactly
 * (`schemaReadOnly` in that build), so it admits a page of its own version and holds a page of any
 * other version read-only. On a server of this tree, that row makes minV 0, and no page's R is at
 * or under 0: while B is live, every page is read-only there, B's own and this tree's alike.
 *
 * With no previous build, both processes run this tree. The fleet is two processes of one build,
 * and a page of that build passes the rule on either.
 */
function pageWritable(
  pageProcess: DispatchProcess,
  server: DispatchProcess,
  fleet: readonly ProcessRow[]
): { writable: boolean; why: string } {
  const page = pageSchema(pageProcess, server);
  const rows = fleet
    .map((row) => `${row.commit.slice(0, 12) || "unstamped"} ${row.version}/${row.creatable}`)
    .join(", ");
  const numbers =
    `the page presents C ${page.version} and R ${page.creatable}; ` +
    `the live processes register schema_version/schema_creatable ${rows}`;
  if (server === "b" && fleet.some((row) => row.version === 0)) {
    const own = manifestOf("b").version;
    const writable = page.version === own;
    return {
      why: `${numbers}; ${builds.b} registers none and admits by its own version alone: C === ${own} is ${writable}`,
      writable,
    };
  }
  const minVersion = Math.min(...fleet.map((row) => row.version));
  const maxCreatable = Math.max(...fleet.map((row) => row.creatable));
  const since = documentSince(server);
  const writable =
    page.creatable <= Math.min(minVersion, maxCreatable) &&
    page.version >= Math.max(maxCreatable, since);
  return {
    why:
      `${numbers}; minV ${minVersion}, maxR ${maxCreatable}, D ${since}: ` +
      `R <= min(minV, maxR) && C >= max(maxR, D) is ${writable}`,
    writable,
  };
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
      // admits the page read-only, the page says so, takes no edit, and the stored document is
      // untouched. The expectation comes from the numbers each build's manifest gives the page
      // and the numbers the live processes register, and the row records them, so a failed run
      // says what it expected and why.
      await page.goto(`/issues/${issue.key}/spec`);
      await expect(documentEditor(page)).toContainText("Written before the edit.");
      await expect(connectedDot(page)).toHaveText("connected");
      const edit = `Typed on ${builds[pageProcess]}'s page.`;
      const { writable, why } = pageWritable(pageProcess, server, await liveProcesses());
      const expected = `${writable ? "writable" : "read-only"}: ${why}`;
      test.info().annotations.push({ description: expected, type: "document admission" });
      console.log(
        `${builds[pageProcess]}'s page on ${builds[server]}'s server is expected ${expected}`
      );
      if (writable) {
        await typeAtEnd(page, edit);
      } else {
        await expect(page.locator('.dispatch-doc[data-read-only="true"]'), expected).toBeVisible();
      }
      for (const reader of [server, pageProcess]) {
        await expect
          .poll(
            async () =>
              (await getIssueArtifactText(issue.key, "spec", { process: reader })).markdown,
            { message: expected, timeout: 30_000 }
          )
          .toContain(writable ? edit : "Written before the edit.");
        if (!writable) {
          const { markdown } = await getIssueArtifactText(issue.key, "spec", { process: reader });
          expect(markdown, expected).not.toContain(edit);
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
