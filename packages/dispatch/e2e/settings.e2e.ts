import { createHash } from "node:crypto";

import { QUERY_RETRY_DELAY_MS } from "../web/src/api/query-client";
import {
  baseUrl,
  createIssue,
  createProject,
  getIssue,
  listProjectDefaultAssignees,
  putProjectDefaultAssignee,
  putProjectDefaultRepository,
  userHeaders,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { listTrigger } from "./filters";
import { openIssueDetails } from "./issue-details";
import { expect, type Locator, type Page, type Request, test } from "./test";
import { asUser } from "./users";

/** The open list named `label`. It is portalled out of the page's sections, so it is found by
 *  its own name, `<label> options`. */
function listOf(page: Page, label: string): Locator {
  return page.getByRole("listbox", { exact: true, name: `${label} options` });
}

/** Picks `option` from the list named `label`, as a reader does: presses its trigger, then the
 *  option in the list it opens, once the list holds it. */
async function pick(page: Page, label: string, option: string): Promise<void> {
  await listTrigger(page, label).click();
  await listOf(page, label).getByRole("option", { exact: true, name: option }).click();
}

/** The names of the options the open list named `label` offers: each row's words without the
 *  tick the picked one carries, which is hidden from its name. */
function offered(page: Page, label: string): Promise<string[]> {
  return listOf(page, label)
    .getByRole("option")
    .evaluateAll((options) =>
      options.map((option) =>
        [...option.children]
          .filter((part) => part.getAttribute("aria-hidden") !== "true")
          .map((part) => part.textContent ?? "")
          .join("")
      )
    );
}

/** Whether `request` writes CORE's default repository: a set or a clear. */
function writesCoreDefaultRepository(request: Request): boolean {
  return (
    request.method() === "PUT" &&
    new URL(request.url()).pathname === "/api/v1/projects/CORE/default-repository"
  );
}

// SLOW(>=2s): three page loads - Settings, the new issue's page and Settings again - and two
// writes of the default, each read back from the server.
test("a person sets a project's default repository in Settings, a new issue of the project resolves to it, and Clear puts it back to None", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const pinned = await createIssue({ project: "CORE", repo: "acme/gadgets", title: "Pinned" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const box = page.getByRole("textbox", { exact: true, name: "Default repository of CORE" });
  const save = page.getByRole("button", {
    exact: true,
    name: "Save the default repository of CORE",
  });
  const clear = page.getByRole("button", {
    exact: true,
    name: "Clear the default repository of CORE",
  });
  // Every write of CORE's default repository the page sends, by its body.
  const writes: unknown[] = [];
  page.on("request", (request) => {
    if (writesCoreDefaultRepository(request)) {
      writes.push(request.postDataJSON());
    }
  });

  try {
    await page.goto("/settings");
    await expect(
      page.locator("main").getByRole("heading", { exact: true, name: "Projects" })
    ).toBeVisible();
    await expect(
      page.getByRole("columnheader", { exact: true, name: "Default repository" })
    ).toBeVisible();
    // The project has no default: the box is empty and reads None, and there is nothing to clear.
    await expect(box).toHaveValue("");
    await expect(box).toHaveAttribute("placeholder", "None");
    await expect(clear).toHaveCount(0);
    // The Settings page has no "Repositories → Projects" section (C4).
    await expect(page.getByRole("heading", { name: "Repositories → Projects" })).toHaveCount(0);

    // A value that is not owner/name is refused at the box, with what was typed still in it,
    // and nothing is sent.
    await box.fill("acme");
    await save.click();
    await expect(
      page.getByRole("alert").filter({ hasText: "Write the repository as owner/name." })
    ).toBeVisible();
    await expect(box).toHaveValue("acme");
    await expect(box).toHaveAttribute("aria-invalid", "true");
    expect(writes).toEqual([]);

    const set = page.waitForResponse((response) => writesCoreDefaultRepository(response.request()));
    await box.fill("acme/widgets");
    await save.click();
    const setAnswer = await set;
    expect(setAnswer.request().postDataJSON()).toEqual({ repo: "acme/widgets" });
    expect(await setAnswer.json()).toEqual({ project: "CORE", repo: "acme/widgets" });
    await expect(box).toHaveValue("acme/widgets");
    await expect(clear).toBeVisible();
    const shot = testInfo.outputPath(`repository-default-${testInfo.project.name}.png`);
    await page.screenshot({ fullPage: true, path: shot });
    await testInfo.attach(`repository default (${testInfo.project.name})`, {
      contentType: "image/png",
      path: shot,
    });

    // A new issue of the project that names no repository resolves to the default, and its page
    // shows it as the issue's repository. An issue with a repository of its own keeps it.
    const inheriting = await createIssue({ force: true, project: "CORE", title: "Inheriting" });
    expect(inheriting).toMatchObject({ repo: null, resolved_repo: "acme/widgets" });
    expect(await getIssue(pinned.key)).toMatchObject({
      repo: "acme/gadgets",
      resolved_repo: "acme/gadgets",
    });
    await page.goto(`/issues/${inheriting.key}`);
    // The issue page's Repository list, in the header's Details, names the default after its
    // label, as the repository the issue inherits.
    await openIssueDetails(page);
    await expect(page.getByLabel(`Repository of ${inheriting.key}`)).toHaveAccessibleName(
      `Repository of ${inheriting.key} acme/widgets, inherited or the project's default`
    );

    // The page read again from the server holds the default. Clear sends null: the box is empty
    // and reads None again, and the issue that took the default resolves to no repository.
    await page.goto("/settings");
    await expect(box).toHaveValue("acme/widgets");
    const cleared = page.waitForResponse((response) =>
      writesCoreDefaultRepository(response.request())
    );
    await clear.click();
    const clearAnswer = await cleared;
    expect(clearAnswer.request().postDataJSON()).toEqual({ repo: null });
    expect(await clearAnswer.json()).toEqual({ project: "CORE", repo: null });
    await expect(box).toHaveValue("");
    await expect(box).toHaveAttribute("placeholder", "None");
    await expect(clear).toHaveCount(0);
    await expect
      .poll(() => getIssue(inheriting.key))
      .toMatchObject({ repo: null, resolved_repo: null });
    expect(await getIssue(pinned.key)).toMatchObject({
      repo: "acme/gadgets",
      resolved_repo: "acme/gadgets",
    });
    expect(writes).toEqual([{ repo: "acme/widgets" }, { repo: null }]);
  } finally {
    await context.close();
  }
});

// A save's answer describes the default when the server wrote it, not when the answer arrives.
// Here the server saves this page's acme/gadgets at once, another browser sets acme/widgets again
// (which this page hears: it reads the project list again), and only then does the first answer
// reach the page: the page reads the project list again rather than keep acme/gadgets.
// @pr: A two-context race: a late answer to this page's save must not undo the default another browser set since.
test("a late answer to a default repository save does not undo a default another browser set since @pr", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await putProjectDefaultRepository("CORE", "acme/widgets");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const box = page.getByRole("textbox", { exact: true, name: "Default repository of CORE" });

  try {
    const held = Promise.withResolvers<void>();
    const savedOnServer = Promise.withResolvers<void>();
    await page.route("**/api/v1/projects/CORE/default-repository", async (route) => {
      if (route.request().method() !== "PUT") return route.fallback();
      const response = await route.fetch();
      savedOnServer.resolve();
      await held.promise;
      return route.fulfill({ response });
    });
    await page.goto("/settings");
    await expect(box).toHaveValue("acme/widgets");

    await box.fill("acme/gadgets");
    await page
      .getByRole("button", { exact: true, name: "Save the default repository of CORE" })
      .click();
    await savedOnServer.promise;
    // The page hears the other browser's write: it reads the project list again, and the read
    // answers acme/widgets. The box still shows the pick whose save is out.
    const heard = page.waitForResponse(
      async (response) =>
        response.request().method() === "GET" &&
        new URL(response.url()).pathname === "/api/v1/projects" &&
        ((await response.json()) as { default_repo?: string; key: string }[]).some(
          (project) => project.key === "CORE" && project.default_repo === "acme/widgets"
        )
    );
    await putProjectDefaultRepository("CORE", "acme/widgets");
    await heard;
    await expect(box).toHaveValue("acme/gadgets");

    const answered = page.waitForResponse((response) =>
      writesCoreDefaultRepository(response.request())
    );
    const readAgain = answered.then(() =>
      page.waitForResponse(
        (response) =>
          response.request().method() === "GET" &&
          new URL(response.url()).pathname === "/api/v1/projects"
      )
    );
    held.resolve();
    await answered;
    await readAgain;
    await expect(box).toHaveValue("acme/widgets");
  } finally {
    await context.close();
  }
});

test("a human creates a project from Settings and immediately creates an issue under it", async ({
  browser,
}, testInfo) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/settings");
    await expect(
      page.locator("main").getByRole("heading", { exact: true, name: "Projects" })
    ).toBeVisible();
    await page.getByLabel("Key", { exact: true }).fill("qa");
    await page.getByLabel("Name", { exact: true }).fill("Quality");
    // The form asks for the person the new project names, and starts on the one the server would
    // take anyway: the person creating it. Its list's name begins with its label, as each
    // project's own list's in the table above does, which goes on with " of <KEY>".
    const defaultAssignee = page.getByRole("button", { name: /^Default assignee(?! of )/ });
    await expect(defaultAssignee).toHaveText("alice");
    await page.getByRole("button", { name: "New project" }).click();
    await expect(page.getByRole("cell", { exact: true, name: "QA" })).toBeVisible();
    // The architecture sources form offers the new project at once.
    await listTrigger(page, "Source project").click();
    await expect(
      listOf(page, "Source project").getByRole("option", { exact: true, name: "QA · Quality" })
    ).toHaveCount(1);
    await page.keyboard.press("Escape");
    // The project names her from the moment it exists, and the row says so.
    await expect(listTrigger(page, "Default assignee of QA")).toHaveText("alice");
    await page.screenshot({
      path: testInfo.outputPath("project-create-settings.png"),
      fullPage: true,
    });

    const issue = await createIssue({ project: "QA", title: "Track quality issues" });
    expect(issue.key).toBe("QA-1");

    await page.goto(`/issues/${issue.key}`);
    await expect(page).toHaveTitle(`${issue.key} · ${issue.title} · Dispatch`);
  } finally {
    await context.close();
  }
});

test("an architecture source saves after the GitHub App access check, fails inline once uninstalled, and deletes", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await seedFakeGithub({ "legion/arch": { contents: "write", installation_id: 101 } });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/settings");
    await expect(page.getByRole("heading", { name: "Architecture sources" })).toBeVisible();
    await pick(page, "Source project", "CORE · Core");
    await page.getByLabel("Source repository").fill("Legion/Arch");
    const sourceSaved = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        response.url().endsWith("/api/v1/projects/CORE/architecture-sources/Legion/Arch") &&
        response.ok()
    );
    await page.getByRole("button", { name: "Add source" }).click();
    await sourceSaved;
    await expect(page.getByRole("cell", { exact: true, name: "legion/arch" })).toBeVisible();
    await expect(page.getByRole("cell", { exact: true, name: "main" })).toBeVisible();
    await expect(page.getByRole("cell", { exact: true, name: "Access verified" })).toBeVisible();

    // A branch the repository does not have is refused inline with the server's 409, and the
    // stored source keeps its branch.
    await pick(page, "Source project", "CORE · Core");
    await page.getByLabel("Source repository").fill("legion/arch");
    await page.getByLabel("Branch", { exact: true }).fill("no-such-branch");
    await page.getByRole("button", { name: "Add source" }).click();
    await expect(
      page
        .getByRole("alert")
        .filter({ hasText: "the branch does not exist: no-such-branch on legion/arch" })
    ).toBeVisible();
    await expect(page.getByRole("cell", { exact: true, name: "main" })).toBeVisible();
    await expect(page.getByRole("cell", { exact: true, name: "no-such-branch" })).toHaveCount(0);
    await page.getByLabel("Branch", { exact: true }).fill("main");

    // The App gets uninstalled; re-saving fails its access check inline and the
    // stored source survives.
    await seedFakeGithub({});
    await pick(page, "Source project", "CORE · Core");
    await page.getByLabel("Source repository").fill("legion/arch");
    await page.getByRole("button", { name: "Add source" }).click();
    await expect(
      page.getByRole("alert").filter({ hasText: "the GitHub App is not installed on legion/arch" })
    ).toBeVisible();
    await expect(page.getByRole("cell", { exact: true, name: "legion/arch" })).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath("architecture-source-settings.png"),
      fullPage: true,
    });

    await page
      .getByRole("button", { name: "Delete architecture source legion/arch for CORE" })
      .click();
    await expect(page.getByText("No architecture sources yet.")).toBeVisible();
    await expect(page.getByRole("cell", { exact: true, name: "legion/arch" })).not.toBeVisible();
  } finally {
    await context.close();
  }
});

test("Refresh imports the architecture model and a rejected model keeps the previous import", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const files = {
    "api.md": "---\ntitle: HTTP API\ndepends_on: [store]\n---\nThe API surface.\n",
    "store.md": "---\ntitle: Store\n---\nThe durable state.\n",
  };
  await seedFakeGithub({ "legion/arch": { contents: "read", files, installation_id: 101 } });
  // The fake's commit sha is sha1 over the seeded files JSON, so the test can
  // assert the exact commit the row must show.
  const commit = createHash("sha1").update(JSON.stringify(files)).digest("hex");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/settings");
    await pick(page, "Source project", "CORE · Core");
    await page.getByLabel("Source repository").fill("legion/arch");
    const sourceSaved = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        response.url().endsWith("/api/v1/projects/CORE/architecture-sources/legion/arch") &&
        response.ok()
    );
    await page.getByRole("button", { name: "Add source" }).click();
    await sourceSaved;

    const synced = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith("/api/v1/projects/CORE/architecture-sources/legion/arch/sync") &&
        response.ok()
    );
    await page
      .getByRole("button", { name: "Refresh architecture source legion/arch for CORE" })
      .click();
    await synced;
    await expect(page.getByText(`Synced ${commit.slice(0, 12)} · 2 components`)).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath("architecture-source-synced.png"),
      fullPage: true,
    });

    // A pushed model with a duplicate id (Api.md and api.md are both component
    // "api") is rejected whole: the row shows the reason and the previous
    // import — its commit — stays up.
    await seedFakeGithub({
      "legion/arch": {
        contents: "read",
        files: { "Api.md": "Duplicate casing.\n", "api.md": "Original.\n" },
        installation_id: 101,
      },
    });
    const rejected = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith("/api/v1/projects/CORE/architecture-sources/legion/arch/sync") &&
        response.ok()
    );
    await page
      .getByRole("button", { name: "Refresh architecture source legion/arch for CORE" })
      .click();
    await rejected;
    await expect(page.getByText(/duplicate component id/)).toBeVisible();
    await expect(
      page.getByText(`Previous model stays up at ${commit.slice(0, 12)} · 2 components`)
    ).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath("architecture-source-sync-rejected.png"),
      fullPage: true,
    });
  } finally {
    await context.close();
  }
});

// The Architecture sources table scrolls inside its own container, but its `sr-only` header
// cell is absolutely positioned: without a positioned ancestor inside that container it sits
// at the scrolled table's own x and widened the page to 417px at 390px.
test("Settings stays inside the viewport @phone", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const width = testInfo.project.name === "chromium" ? 1280 : 390;
    if (testInfo.project.name === "chromium") {
      await page.setViewportSize({ height: 900, width });
    }
    await page.goto("/settings");
    await expect(page.getByRole("heading", { level: 1, name: "Settings" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Architecture sources" })).toBeVisible();

    const measured = await page.evaluate(() => ({
      client: document.documentElement.clientWidth,
      scroll: document.documentElement.scrollWidth,
    }));
    expect(measured.scroll).toBeLessThanOrEqual(measured.client);
  } finally {
    await alice.close();
  }
});

test("a person sets and changes a project's default assignee, and no write clears one", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  // Creating OPS as bob makes him a person who has signed in, so he is an option in the control,
  // and OPS names him from the moment it exists: every project names somebody.
  await createProject({ key: "OPS", name: "Ops" }, { login: "bob" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/settings");
    const core = listTrigger(page, "Default assignee of CORE");
    const ops = listTrigger(page, "Default assignee of OPS");
    await expect(core).toBeVisible();
    // The sentence under the table: every project names somebody, and no write clears it.
    await expect(
      page.getByText("Every project names a default assignee, and it cannot be cleared")
    ).toBeVisible();
    // Each project names the person who created it until somebody moves it.
    await expect(core).toHaveText("alice");
    await expect(ops).toBeEnabled();
    await expect(ops).toHaveText("bob");

    // Who has signed in is read only once the reader reaches for the control, and the control
    // offers people and nothing else: there is no way to name nobody.
    await core.focus();
    await core.click();
    await expect.poll(() => offered(page, "Default assignee of CORE")).toEqual(["alice", "bob"]);

    const saved = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        new URL(response.url()).pathname === "/api/v1/projects/CORE/default-assignee"
    );
    await listOf(page, "Default assignee of CORE")
      .getByRole("option", { exact: true, name: "bob" })
      .click();
    expect((await saved).ok()).toBe(true);

    await page.reload();
    await expect(core).toHaveText("bob");
    await page.screenshot({
      path: testInfo.outputPath("project-default-assignee-settings.png"),
      fullPage: true,
    });

    const changed = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        new URL(response.url()).pathname === "/api/v1/projects/CORE/default-assignee"
    );
    await core.focus();
    await pick(page, "Default assignee of CORE", "alice");
    expect((await changed).ok()).toBe(true);
    await page.reload();
    await expect(core).toHaveText("alice");
    await expect(ops).toBeEnabled();
    await expect(ops).toHaveText("bob");

    // The clearing the page no longer offers is refused at the route as well: the body the old
    // Unassigned option sent is a 400, and the project still names its person. The call is made
    // here rather than through `putProjectDefaultAssignee`, whose body the route table now types
    // as a person's email and which cannot express this one.
    const refused = await fetch(new URL("/api/v1/projects/CORE/default-assignee", baseUrl), {
      body: JSON.stringify({ assignee: null }),
      headers: { "Content-Type": "application/json", ...(await userHeaders("alice")) },
      method: "PUT",
    });
    expect(refused.status).toBe(400);
    expect(await refused.json()).toMatchObject({ code: "INVALID_PROJECT" });
    await page.reload();
    await expect(core).toHaveText("alice");
    const defaults = await listProjectDefaultAssignees({ as: "user", login: "alice" });
    expect(defaults.map((row) => [row.project, row.assignee]).sort()).toEqual([
      ["CORE", "alice"],
      ["OPS", "bob"],
    ]);
  } finally {
    await context.close();
  }
});

test("a default assignee picked while the last pick is saving waits its turn, and the last pick is the one kept", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  // Creating OPS as bob makes him a person who has signed in, so he is an option in the control.
  await createProject({ key: "OPS", name: "Ops" }, { login: "bob" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    // The first PUT waits until the test lets it through, so the second pick lands while it is
    // still on the wire. `wire` records each PUT as it reaches the network and the first's answer.
    const held = Promise.withResolvers<void>();
    const wire: string[] = [];
    await page.route("**/api/v1/projects/CORE/default-assignee", async (route) => {
      if (route.request().method() !== "PUT") return route.fallback();
      const { assignee } = route.request().postDataJSON() as { assignee: string | null };
      const first = wire.length === 0;
      wire.push(`sent ${assignee}`);
      if (!first) return route.fallback();
      await held.promise;
      const response = await route.fetch();
      wire.push(`answered ${assignee}`);
      return route.fulfill({ response });
    });
    await page.goto("/settings");
    const core = listTrigger(page, "Default assignee of CORE");
    await expect(core).toBeEnabled();
    await expect(core).toHaveText("alice");
    await core.focus();
    await core.click();
    await expect.poll(() => offered(page, "Default assignee of CORE")).toEqual(["alice", "bob"]);

    // The pick shows at once, while its save is still held.
    await listOf(page, "Default assignee of CORE")
      .getByRole("option", { exact: true, name: "bob" })
      .click();
    await expect(core).toHaveText("bob");
    await expect.poll(() => wire).toEqual(["sent bob"]);

    await pick(page, "Default assignee of CORE", "alice");
    await expect(core).toHaveText("alice");
    // One at a time: the second pick has not left the browser while the first is unanswered.
    expect(wire).toEqual(["sent bob"]);

    const second = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        new URL(response.url()).pathname === "/api/v1/projects/CORE/default-assignee" &&
        response.request().postDataJSON()?.assignee === "alice"
    );
    held.resolve();
    expect((await second).ok()).toBe(true);
    expect(wire).toEqual(["sent bob", "answered bob", "sent alice"]);
    await expect(core).toHaveText("alice");

    await page.reload();
    await expect(core).toHaveText("alice");
    // Every project is listed now, so CORE's own row is the one this reads.
    const defaults = await listProjectDefaultAssignees({ as: "user", login: "alice" });
    expect(defaults.filter((row) => row.project === "CORE")).toEqual([
      { assignee: "alice", project: "CORE" },
    ]);
  } finally {
    await context.close();
  }
});

// The saves still out when the reader leaves Settings belong to the app, not to the page: on
// the way back the row's next pick waits behind the held save and supersedes the pick queued
// before leaving, so it is the only one sent after the held save and the one the server keeps.
test("default assignee picks made when Settings is left and reopened supersede the pick queued before leaving, and the newest is the one sent after the held save", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  // Creating OPS as bob and QA as carol makes each a person who has signed in, so both are
  // options in the control.
  await createProject({ key: "OPS", name: "Ops" }, { login: "bob" });
  await createProject({ key: "QA", name: "Quality" }, { login: "carol" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    // The first PUT waits until the test lets it through. `wire` records each PUT as it reaches
    // the network and as its answer comes back.
    const held = Promise.withResolvers<void>();
    const wire: string[] = [];
    await page.route("**/api/v1/projects/CORE/default-assignee", async (route) => {
      if (route.request().method() !== "PUT") return route.fallback();
      const { assignee } = route.request().postDataJSON() as { assignee: string | null };
      const first = wire.length === 0;
      wire.push(`sent ${assignee}`);
      if (first) await held.promise;
      const response = await route.fetch();
      wire.push(`answered ${assignee}`);
      return route.fulfill({ response });
    });
    await page.goto("/settings");
    const core = listTrigger(page, "Default assignee of CORE");
    await expect(core).toBeEnabled();
    await expect(core).toHaveText("alice");
    await core.focus();
    await core.click();
    await expect
      .poll(() => offered(page, "Default assignee of CORE"))
      .toEqual(["alice", "bob", "carol"]);

    await listOf(page, "Default assignee of CORE")
      .getByRole("option", { exact: true, name: "bob" })
      .click();
    await expect(core).toHaveText("bob");
    await expect.poll(() => wire).toEqual(["sent bob"]);
    await pick(page, "Default assignee of CORE", "carol");
    await expect(core).toHaveText("carol");

    // Away and back inside the app: Settings unmounts and mounts again, the app stays loaded.
    const navigation = page.getByRole("navigation", { name: "Navigation" });
    await navigation.getByRole("link", { name: "Inbox" }).click();
    await expect(core).toHaveCount(0);
    await navigation.getByRole("link", { name: "Settings" }).click();
    await expect(core).toBeEnabled();
    await core.focus();
    await core.click();
    await expect
      .poll(() => offered(page, "Default assignee of CORE"))
      .toEqual(["alice", "bob", "carol"]);
    await listOf(page, "Default assignee of CORE")
      .getByRole("option", { exact: true, name: "alice" })
      .click();
    await expect(core).toHaveText("alice");

    held.resolve();
    await expect.poll(() => wire.includes("answered alice")).toBe(true);
    await expect(core).toHaveText("alice");

    await page.reload();
    await expect(core).toHaveText("alice");
    const defaults = await listProjectDefaultAssignees({ as: "user", login: "alice" });
    expect(defaults.filter((row) => row.project === "CORE")).toEqual([
      { assignee: "alice", project: "CORE" },
    ]);
    expect(wire).toEqual(["sent bob", "answered bob", "sent alice", "answered alice"]);
  } finally {
    await context.close();
  }
});

// A person who changes their mind while a save is out is choosing, not saving each choice: the
// picks made in between are superseded before they leave the browser, as the issue's own
// assignee picker does (useIssueFieldWrite). Each pick the server stores is a default an issue
// created in that moment is assigned to for good, so a person arrowing through the list must not
// walk the project's default through everyone they passed.
test("default assignee picks made while a save is out are superseded by the newest, so only it follows the save", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Ops" }, { login: "bob" });
  await createProject({ key: "QA", name: "Quality" }, { login: "carol" });
  await createProject({ key: "ENG", name: "Engineering" }, { login: "dave" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    const held = Promise.withResolvers<void>();
    const wire: string[] = [];
    await page.route("**/api/v1/projects/CORE/default-assignee", async (route) => {
      if (route.request().method() !== "PUT") return route.fallback();
      const { assignee } = route.request().postDataJSON() as { assignee: string | null };
      const first = wire.length === 0;
      wire.push(`sent ${assignee}`);
      if (first) await held.promise;
      const response = await route.fetch();
      wire.push(`answered ${assignee}`);
      return route.fulfill({ response });
    });
    await page.goto("/settings");
    const core = listTrigger(page, "Default assignee of CORE");
    await expect(core).toBeEnabled();
    await core.focus();
    await core.click();
    await expect
      .poll(() => offered(page, "Default assignee of CORE"))
      .toEqual(["alice", "bob", "carol", "dave"]);

    await listOf(page, "Default assignee of CORE")
      .getByRole("option", { exact: true, name: "bob" })
      .click();
    await expect.poll(() => wire).toEqual(["sent bob"]);
    for (const login of ["carol", "dave", "alice"]) {
      await pick(page, "Default assignee of CORE", login);
      await expect(core).toHaveText(login);
    }

    held.resolve();
    // Wait for the newest pick's answer, then read every PUT that left the browser before it.
    await expect.poll(() => wire.includes("answered alice")).toBe(true);
    await expect(core).toHaveText("alice");
    expect(wire).toEqual(["sent bob", "answered bob", "sent alice", "answered alice"]);
    const defaults = await listProjectDefaultAssignees({ as: "user", login: "alice" });
    expect(defaults.filter((row) => row.project === "CORE")).toEqual([
      { assignee: "alice", project: "CORE" },
    ]);
  } finally {
    await context.close();
  }
});

// A save refused while the reader is away from Settings is still theirs to hear about: on the way
// back the row says its last save was refused, beside the stored value it fell back to, rather
// than showing the old default as if nothing had been asked.
test("a default assignee save refused while Settings is left says so when Settings is reopened", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Ops" }, { login: "bob" });
  await putProjectDefaultAssignee("CORE", { assignee: "alice" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    const held = Promise.withResolvers<void>();
    let answered = false;
    await page.route("**/api/v1/projects/CORE/default-assignee", async (route) => {
      if (route.request().method() !== "PUT") return route.fallback();
      await held.promise;
      answered = true;
      // Dispatch's verdict, a 4xx: a 5xx is tried again under the save's key, not refused.
      return route.fulfill({
        json: { code: "ASSIGNEE_NOT_ALLOWED", error: "the default assignee save was refused" },
        status: 400,
      });
    });
    await page.goto("/settings");
    const core = listTrigger(page, "Default assignee of CORE");
    await expect(core).toBeEnabled();
    await core.focus();
    await core.click();
    await expect.poll(() => offered(page, "Default assignee of CORE")).toEqual(["alice", "bob"]);
    await listOf(page, "Default assignee of CORE")
      .getByRole("option", { exact: true, name: "bob" })
      .click();

    const navigation = page.getByRole("navigation", { name: "Navigation" });
    await navigation.getByRole("link", { name: "Inbox" }).click();
    await expect(core).toHaveCount(0);
    held.resolve();
    await expect.poll(() => answered).toBe(true);

    await navigation.getByRole("link", { name: "Settings" }).click();
    await expect(core).toBeEnabled();
    await expect(core).toHaveText("alice");
    await expect(
      page.getByRole("alert").filter({ hasText: "the default assignee save was refused" })
    ).toBeVisible();
  } finally {
    await context.close();
  }
});

test("a default assignee the page has not read yet is never shown as Unassigned", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await putProjectDefaultAssignee("CORE", { assignee: "alice" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    // Each read of the defaults is answered as `read` says when it arrives: held until the test
    // lets it go, refused, or passed through to the server.
    let read: "hold" | "fail" | "pass" = "hold";
    const held = Promise.withResolvers<void>();
    await page.route("**/api/v1/settings/project-default-assignees", async (route) => {
      if (read === "hold") {
        await held.promise;
        // The page this read was for may be gone by now.
        return route.abort().catch(() => {});
      }
      if (read === "fail") {
        return route.fulfill({ json: { code: "INTERNAL", error: "boom" }, status: 500 });
      }
      return route.fallback();
    });
    // The refused read below is retried twice, `QUERY_RETRY_DELAY_MS` apart, before the page
    // calls it failed: page timers, which the page's own clock fires.
    await page.clock.install();
    await page.goto("/settings");
    const core = listTrigger(page, "Default assignee of CORE");
    await expect(core).toBeVisible();
    // Not read yet: the control says so and takes no pick.
    await expect(core).toBeDisabled();
    await expect(core).not.toHaveText(/Unassigned/);
    await expect(core).toHaveText("Loading…");

    read = "fail";
    held.resolve();
    await page.reload();
    const loadError = page
      .getByRole("alert")
      .filter({ hasText: "Couldn't load the default assignees." });
    // Each refusal arms the next retry, so the jump repeats until the page gives the read up.
    await expect
      .poll(
        async () => {
          await page.clock.runFor(QUERY_RETRY_DELAY_MS + 50);
          return loadError.count();
        },
        { intervals: [50, 50, 100, 200, 500] }
      )
      .toBe(1);
    await expect(core).toBeDisabled();
    await expect(core).toHaveText("Unavailable");

    read = "pass";
    await loadError.getByRole("button", { name: "Retry" }).click();
    await expect(core).toBeEnabled();
    await expect(core).toHaveText("alice");
    await expect(loadError).toHaveCount(0);
  } finally {
    await context.close();
  }
});
