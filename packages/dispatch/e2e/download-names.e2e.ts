import { readFile } from "node:fs/promises";

import {
  type Op,
  type PathParams,
  type PathsOf,
  type Result,
  routePath,
} from "../web/src/api/paths";
import { setLiveSessions } from "./agents";
import {
  createIssue,
  createIssueArtifact,
  createProject,
  dashboardOrigin,
  editArtifact,
  getArtifactVersion,
} from "./api";
import { connectedDot, documentEditor } from "./editor";
import { ONE_PIXEL_PNG } from "./one-pixel-png";
import { expect, type Locator, type Page, rowKey, rowSessionId, test } from "./test";

// Every "Download version N" link goes to the download address,
// `/api/v1/artifacts/<id>/versions/<n>?download=true`, where the server names the file in
// `Content-Disposition` (its own name, `-v<n>` before the extension of an older version, and
// `.md` added to a document's name that ends in neither `.md` nor `.markdown`, whatever their
// case) and answers `Cache-Control: private, no-cache`. These rows read the name the browser
// saves each download under (`suggestedFilename`), which is the engine's reading of `filename*`
// and `filename`, and a name that changes at an address the browser already followed once, which
// only an engine that asks the server again can save. A picture keeps the version address, which
// a browser may cache for a year.

/** A file to upload, in the shape Playwright's multipart writes and file inputs take. */
interface UploadFile {
  buffer: Buffer;
  mimeType: string;
  name: string;
}

/** A file the server stores as a file and not a picture: `%PDF-` is no picture's signature. */
function pdf(name: string, text: string): UploadFile {
  return { buffer: Buffer.from(`%PDF-1.4\n% ${text}\n`), mimeType: "application/pdf", name };
}

/** Uploads `file` as the signed-in page through `pattern`, an owner's artifacts route, as a
 *  multipart write, and answers what the route answers. `page.request` sends the page's session
 *  cookie and no `Origin`, and the server refuses a cookie-authenticated write whose `Origin` is
 *  not the dashboard's (enforceCookieOrigin). */
async function upload<P extends PathsOf<"post">>(
  page: Page,
  pattern: P,
  params: PathParams<Op<P, "post">>,
  file: UploadFile
): Promise<Result<P, "post">> {
  const answer = await page.request.post(routePath("post", pattern, params), {
    headers: { Origin: dashboardOrigin },
    multipart: { file, name: file.name },
  });
  expect(answer.status(), await answer.text()).toBeLessThan(300);
  return answer.json();
}

/** Picks `file` in the Artifacts tab's own upload control and confirms it, as a person does:
 *  choosing a file only stages it until the row's "Upload" is pressed. */
async function uploadOnTab(page: Page, file: UploadFile): Promise<void> {
  await page.getByLabel("Upload artifact").setInputFiles(file);
  await page.getByRole("button", { exact: true, name: "Upload" }).click();
}

/** The download `link` starts when it is clicked: the name the browser saves it under, the
 *  address it came from and, once it has finished, the file it wrote. */
async function saveFrom(page: Page, link: Locator) {
  const saved = page.waitForEvent("download");
  await link.click();
  return saved;
}

/** The path a link or a picture names for version `number`: the download address with
 *  `download`, the version address a picture is drawn from without it. */
function versionAddress(id: string, number: number, download = false): string {
  return `/api/v1/artifacts/${id}/versions/${number}${download ? "?download=true" : ""}`;
}

async function openArtifactsTab(page: Page, issueKey: string): Promise<void> {
  await page.goto(`/issues/${issueKey}`);
  await page.getByRole("tab", { name: "Artifacts" }).click();
}

// SLOW(>=2s): a cold load of the issue and its Artifacts tab, then the artifact's page and its
// whole version list, each link and picture read where it is drawn.
// @webkit @firefox: the address the engine follows for a download and the one it draws a picture
// from, which the rows below read the saved names of.
test("each Download version link names the download address and each picture the version address @webkit @firefox", async ({
  page,
}) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Download addresses" });
  const picture = {
    buffer: Buffer.from(ONE_PIXEL_PNG, "base64"),
    mimeType: "image/png",
    name: "diagram.png",
  };
  const { artifact: diagram } = await upload(
    page,
    "/api/v1/issues/{key}/artifacts",
    { key: issue.key },
    picture
  );
  await upload(page, "/api/v1/issues/{key}/artifacts", { key: issue.key }, picture);

  await openArtifactsTab(page, issue.key);
  const row = page.getByTestId("artifact-diagram-png");
  await expect(row).toContainText("2 versions");
  await expect(row.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "href",
    versionAddress(diagram.id, 2, true)
  );
  await expect(row.getByRole("img", { name: "diagram.png version 2" })).toHaveAttribute(
    "src",
    versionAddress(diagram.id, 2)
  );

  await row.getByRole("link", { name: "diagram.png" }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  const shown = page.getByRole("region", { exact: true, name: "diagram.png version 2" });
  await expect(shown.getByRole("img", { name: "diagram.png version 2" })).toHaveAttribute(
    "src",
    versionAddress(diagram.id, 2)
  );
  await expect(shown.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "href",
    versionAddress(diagram.id, 2, true)
  );
  const versions = page.getByRole("region", { exact: true, name: "Versions for diagram.png" });
  await versions.getByRole("button", { name: "Show all versions" }).click();
  for (const number of [1, 2]) {
    await expect(
      versions.getByRole("link", { name: `Download version ${number}` })
    ).toHaveAttribute("href", versionAddress(diagram.id, number, true));
  }
});

// SLOW(>=2s): two uploads through the tab, each written and listed, and four downloads the
// browser saves from the tab, the artifact's page and its version list.
// @webkit @firefox: the name the engine saves from `filename*` and `filename`, and whether it asks
// the server again for a download address it already followed: version 1's address saves a new
// name once version 2 exists.
test("a file saves under its own name, and its older version under its number once a newer one exists @webkit @firefox", async ({
  page,
}) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Report downloads" });
  await openArtifactsTab(page, issue.key);
  const row = page.getByTestId("artifact-report-pdf");

  await uploadOnTab(page, pdf("report.pdf", "first draft"));
  await expect(row).toContainText("1 version");
  const first = await saveFrom(page, row.getByRole("link", { name: "Download version 1" }));
  expect(first.suggestedFilename()).toBe("report.pdf");

  await uploadOnTab(page, pdf("report.pdf", "second draft"));
  await expect(row).toContainText("2 versions");
  await row.getByRole("link", { name: "report.pdf" }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/report-pdf`);
  const shown = page.getByRole("region", { exact: true, name: "report.pdf version 2" });
  const current = await saveFrom(page, shown.getByRole("link", { name: "Download version 2" }));
  expect(current.suggestedFilename()).toBe("report.pdf");

  const versions = page.getByRole("region", { exact: true, name: "Versions for report.pdf" });
  await versions.getByRole("button", { name: "Show all versions" }).click();
  const older = await saveFrom(page, versions.getByRole("link", { name: "Download version 1" }));
  expect(older.url()).toBe(first.url());
  expect(older.suggestedFilename()).toBe("report-v1.pdf");
  const newest = await saveFrom(page, versions.getByRole("link", { name: "Download version 2" }));
  expect(newest.suggestedFilename()).toBe("report.pdf");
});

// SLOW(>=2s): a cold load of the issue and its Artifacts tab, an upload through it, and the
// download the browser saves.
// @webkit @firefox: the name the engine saves from `filename*` where the name holds spaces and a
// character outside ASCII, which the ASCII `filename` beside it cannot carry, and WebKit's own
// rule of writing each space of the name it read as `_`.
test("a file whose name holds spaces and a character outside ASCII saves under that name @webkit @firefox", async ({
  page,
  browserName,
}) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Accented downloads" });
  await openArtifactsTab(page, issue.key);

  const name = "Q3 final résumé.pdf";
  await uploadOnTab(page, pdf(name, "the quarter"));
  const row = page
    .getByRole("listitem")
    .filter({ has: page.getByRole("link", { exact: true, name }) });
  const saved = await saveFrom(page, row.getByRole("link", { name: "Download version 1" }));
  // WebKit reads `filename*`, since the `é` stays where the ASCII `filename` has `_`, and then
  // writes each space as `_` (CI run 43750246861). Chromium and Firefox save the name exactly.
  const expected = browserName === "webkit" ? "Q3_final_résumé.pdf" : name;
  expect(saved.suggestedFilename()).toBe(expected);
});

// SLOW(>=2s): a cold load of the issue and its Artifacts tab, and a download the browser saves
// and the row reads back from disk.
// @webkit @firefox: the name the engine saves a document's Markdown under, and the bytes it writes.
test("the spec saves as spec.md holding its Markdown @webkit @firefox", async ({ page }) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({
    project: core,
    spec: "# Launch plan\n\nEvery download keeps its own name.\n",
    title: "Spec download",
  });
  await openArtifactsTab(page, issue.key);

  const row = page.getByTestId("artifact-spec");
  const saved = await saveFrom(page, row.getByRole("link", { name: "Download version 1" }));
  expect(saved.suggestedFilename()).toBe("spec.md");
  const spec = await getArtifactVersion(issue.primary_artifact_id, 1);
  expect(spec.markdown).toContain("Every download keeps its own name.");
  expect(await readFile(await saved.path(), "utf8")).toBe(spec.markdown);
});

// SLOW(>=2s): a cold load of a document's page, whose live editor connects before the version
// list opens, and a download the browser saves and the row reads back from disk.
// @webkit @firefox: the name the engine saves an older document version under, `-v1` before its
// `.md`, and the bytes of that version rather than the latest.
test("an older version of a document saves with its number and its own Markdown @webkit @firefox", async ({
  page,
}) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Notes download" });
  // A document that is not the issue's spec: the issue page draws no Versions list on its spec's
  // route, only on another document's (`ArtifactRoutePanel`).
  const notes = await createIssueArtifact(issue.key, {
    content: "# Review notes\n\nThe ledger keeps three leases.\n",
    name: "notes.md",
  });
  await editArtifact(
    notes.artifact.id,
    {
      ops: [
        {
          find: "The ledger keeps three leases.",
          op: "replace",
          with: "The ledger keeps seven leases.",
        },
      ],
      summary: "Seven leases",
    },
    { as: "agent" }
  );

  await page.goto(`/issues/${issue.key}/artifacts/notes-md`);
  // The live editor draws the document's text above the list as it connects, which moves the
  // list down. A press that landed while it did left the list closed in Firefox, so the row
  // presses once the text is drawn and the editor connected.
  await expect(documentEditor(page)).toContainText("The ledger keeps seven leases.");
  await expect(connectedDot(page)).toHaveText("connected");
  const versions = page.getByRole("region", { exact: true, name: "Versions for notes.md" });
  // The edit's version 2 is named and the upload's version 1 is not, so the list shows version 1
  // only once it shows every version.
  await versions.getByRole("button", { name: "Show all versions" }).click();
  await expect(versions.getByRole("button", { name: "Show named versions" })).toBeVisible();
  const saved = await saveFrom(page, versions.getByRole("link", { name: "Download version 1" }));
  expect(saved.suggestedFilename()).toBe("notes-v1.md");
  const first = await getArtifactVersion(notes.artifact.id, 1);
  expect(first.markdown).toContain("three leases");
  expect(await readFile(await saved.path(), "utf8")).toBe(first.markdown);
});

// SLOW(>=2s): two cold page loads, a project's file and a conversation's, and the three
// downloads the browser saves from them.
// @webkit @firefox: the name the engine saves a project's file and a conversation's file under,
// the conversation's older version with its number.
test("a project's file and a conversation's file save under their own names, an older version with its number @webkit @firefox", async ({
  page,
}) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const { artifact: brief } = await upload(
    page,
    "/api/v1/projects/{key}/artifacts",
    { key: core },
    pdf("brief.pdf", "brief")
  );
  await page.goto(`/projects/${core}/documents/${brief.slug}`);
  const projectFile = await saveFrom(page, page.getByRole("link", { name: "Download version 1" }));
  expect(projectFile.suggestedFilename()).toBe("brief.pdf");

  // A session of the row's own (`rowSessionId`, e2e/test.ts): Dispatch keeps a conversation's
  // documents by the session's id, so under a fixed id a repeat of this row would upload a third
  // and a fourth version of `trace.pdf`.
  const planner = { session_id: rowSessionId("planner"), title: "Planner" };
  await setLiveSessions([planner]);
  const conversation = "/api/v1/agents/{session_id}/artifacts";
  const { artifact: trace } = await upload(
    page,
    conversation,
    { session_id: planner.session_id },
    pdf("trace.pdf", "first run")
  );
  await upload(
    page,
    conversation,
    { session_id: planner.session_id },
    pdf("trace.pdf", "second run")
  );
  await page.goto(`/agents/${planner.session_id}/artifacts/${trace.slug}`);
  const shown = page.getByRole("region", { exact: true, name: "trace.pdf version 2" });
  const current = await saveFrom(page, shown.getByRole("link", { name: "Download version 2" }));
  expect(current.suggestedFilename()).toBe("trace.pdf");
  const versions = page.getByRole("region", { exact: true, name: "Versions for trace.pdf" });
  await versions.getByRole("button", { name: "Show all versions" }).click();
  const older = await saveFrom(page, versions.getByRole("link", { name: "Download version 1" }));
  expect(older.suggestedFilename()).toBe("trace-v1.pdf");
});
