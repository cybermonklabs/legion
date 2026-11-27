import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";

import { setLiveSessions } from "./agents";
import { devSignInPath, e2eAgentToken, sessionCookieFrom } from "./api";
import { type BaseServer, baseBuild, baseWebBuild, bootBuild, stackBinary } from "./base-build";
import { selectRow } from "./inbox-pane";
import { holdReadMarks, THREAD_SESSION } from "./inbox-threads";
import { bandHeadings } from "./inbox-view";
import { sqlAt } from "./psql";
import { type Browser, expect, type Page, test } from "./test";

/**
 * The check c-inbox-unchanged: the Inbox renders the same page before and after this
 * work, but for the one label the owner chose.
 *
 * The base build (the commit `BASELINE_SHA` names, its server and its dashboard) and this
 * checkout's own build each boot over `ISSUE_UNCHANGED_DATABASE_URL`, a database no other check
 * writes, which this row empties first. This build boots first, so the database holds its schema
 * and the corpus is written through its own writers; the base then serves the same rows over it,
 * as an older build serves a newer schema during a roll. One database gives both pages the same
 * rows, the same ids and the same moments.
 *
 * The corpus holds only rows both builds answer: open asks waiting on the reader, replies in
 * threads they wrote in that somebody else wrote in, an agent's reply to their message, and one
 * open ask waiting on somebody else, which the reader's own list draws nothing of. Each
 * row's moment is a minute after the one before, so no two rows tie and no order is the
 * renderer's choice. The row signs the reader in on each build, opens the Inbox at `/` with the
 * default in force, and captures what each draws: every row in order (its kind, its icon's name,
 * the moment its `datetime` carries and its own text, which holds its title, its issue and, for an
 * Agent row, its sender), the structure of the reading pane for the list's top row, the
 * Type filter's entries in order, the Type bands' headings and every control of the page by its
 * accessible name. The two must be the same but for the one difference this row names and
 * requires: the fourth Type value reads `Reply` on the base build and `Threads` here, in the
 * Filter control, the band heading (`Replies` on the base) and every row icon.
 *
 * The read the Inbox's assignee filter moves to is compared beside the page: for the reader and
 * for the other person, each read as themselves, this head's `GET /api/v1/asks/open?waiting_on=`
 * answers the ask rows the base's `GET /api/v1/inbox?assignee=` answers - the reader two, the
 * other person one - by id both ways, with every field the base answers equal.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** The person who reads both pages, and somebody else who writes in their threads. */
const reader = "alice";
const other = "bob";

/** Where the base's dashboard is built. The base's server goes where `baseBuild` builds it. */
const baseWebDirectory = join(tmpdir(), "base-inbox-unchanged");

/** This checkout's own dashboard, which `bun run build:web` builds before the run. */
const headWebDist = fileURLToPath(new URL("../web/dist", import.meta.url));

/** How far apart the corpus's rows are written, so each carries a moment of its own. */
const ROW_GAP_MS = 60_000;

/** Empties `url`, the row's own database, so the build that boots over it first installs its
 *  schema from nothing: every object its own role owns goes, and what the deployment's bootstrap
 *  installed as another role stays - the `vector` extension migration 0072 needs and that role
 *  cannot create. The URL carries a credential, so psql gets it as `sqlAt` passes one, and a
 *  refusal says what psql said and never the command it ran. */
async function emptyDatabase(url: string): Promise<void> {
  try {
    await sqlAt(url, ["drop owned by current_user cascade"]);
  } catch (error) {
    const stderr =
      error instanceof Error && "stderr" in error && typeof error.stderr === "string"
        ? error.stderr
        : "";
    throw new Error(`could not empty ISSUE_UNCHANGED_DATABASE_URL: ${stderr.trim()}`);
  }
}

/** A person, by the session cookie a dev sign-in on a server gave them. */
type Person = { readonly cookie: string };

/** Who writes one call of the seed: a person, or the thread's session, by the agent bearer every
 *  build of the harness takes. */
type Writer = Person | "session";

/** A person's session cookie from `server`'s own dev sign-in. */
async function signedIn(server: BaseServer, login: string): Promise<Person> {
  const response = await fetch(new URL(devSignInPath(login), server.url), { redirect: "manual" });
  const cookie = sessionCookieFrom(response);
  if (response.status !== 302 || cookie === undefined) {
    throw new Error(`the dev sign-in of ${login} answered ${response.status}`);
  }
  return { cookie };
}

/** One write of the seed against `server`, answering its body. */
async function write<T>(
  server: BaseServer,
  writer: Writer,
  method: string,
  path: string,
  body: Record<string, unknown>
): Promise<T> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  let payload = body;
  if (writer === "session") {
    headers.Authorization = `Bearer ${e2eAgentToken}`;
    payload = { ...body, actor: { id: THREAD_SESSION.session_id, kind: "session" } };
  } else {
    headers.Cookie = writer.cookie;
    headers.Origin = new URL(server.url).origin;
  }
  const response = await fetch(new URL(path, server.url), {
    body: JSON.stringify(payload),
    headers,
    method,
  });
  if (!response.ok) {
    throw new Error(`${method} ${path} answered ${response.status}: ${await response.text()}`);
  }
  return (response.status === 204 ? undefined : await response.json()) as T;
}

/** One read of `path` from `server` as `person`, answering its body. */
async function read<T>(server: BaseServer, person: Person, path: string): Promise<T> {
  const response = await fetch(new URL(path, server.url), { headers: { Cookie: person.cookie } });
  if (!response.ok) {
    throw new Error(`GET ${path} answered ${response.status}: ${await response.text()}`);
  }
  return (await response.json()) as T;
}

/** The moment the next row is written at: a minute after the one before. */
async function nextRow(): Promise<void> {
  // The corpus gives every row a moment of its own a minute after the last, which the check
  // states, so the order of the rows is the moments' and never a tie the renderer breaks.
  await sleep(ROW_GAP_MS);
}

/** The corpus, written through `server`: two open asks waiting on the reader, a comment thread
 *  and an issue message thread the reader started that somebody else answered, the session's
 *  reply to a message the reader sent it, and one open ask waiting on somebody else, which the
 *  reader's own list draws nothing of; each row's own write a minute after the last. */
async function seedCorpus(server: BaseServer): Promise<void> {
  await setLiveSessions([THREAD_SESSION]);
  const alice = await signedIn(server, reader);
  const bob = await signedIn(server, other);
  await write(server, alice, "POST", "/api/v1/projects", { key: "CORE", name: "Core" });
  const issue = (title: string) =>
    write<{ key: string }>(server, alice, "POST", "/api/v1/issues", { project: "CORE", title });
  const checklist = await issue("Release checklist");
  const rollout = await issue("Rollout plan");
  for (const { key } of [checklist, rollout]) {
    await write(server, alice, "PATCH", `/api/v1/issues/${key}`, { assignee: reader });
  }

  await write(server, "session", "POST", `/api/v1/issues/${checklist.key}/asks`, {
    question: "Approve the release window?",
  });
  await nextRow();
  const comment = await write<{ id: string }>(
    server,
    alice,
    "POST",
    `/api/v1/issues/${checklist.key}/comments`,
    { body: "Is the checklist complete?" }
  );
  await write(server, "session", "POST", `/api/v1/issues/${checklist.key}/comments`, {
    body: "Two items are still open.",
    reply_to: comment.id,
  });
  await nextRow();
  await write(server, "session", "POST", `/api/v1/issues/${rollout.key}/asks`, {
    question: "Roll out region by region?",
  });
  await nextRow();
  const message = await write<{ id: string }>(
    server,
    alice,
    "POST",
    `/api/v1/issues/${rollout.key}/messages`,
    { body: "Who owns the rollback step?" }
  );
  await write(server, bob, "POST", `/api/v1/issues/${rollout.key}/messages`, {
    body: "I own the rollback step.",
    in_reply_to: message.id,
  });
  await nextRow();
  const direct = await write<{ id: string }>(
    server,
    alice,
    "POST",
    `/api/v1/agents/${THREAD_SESSION.session_id}/messages`,
    { body: "Please check the staging gate.", delivery: "btw" }
  );
  await write(server, "session", "POST", `/api/v1/messages/${direct.id}/reply`, {
    attempt: 1,
    body: "The staging gate is open.",
  });
  await nextRow();
  const capacity = await issue("Capacity review");
  await write(server, alice, "PATCH", `/api/v1/issues/${capacity.key}`, { assignee: other });
  await write(server, "session", "POST", `/api/v1/issues/${capacity.key}/asks`, {
    question: "Add a second runner?",
  });
}

/** What one build's Inbox draws, in the parts the check compares. */
interface Drawn {
  /** Each row, top to bottom: `kind | icon | moment | text`. */
  readonly rows: readonly string[];
  /** The reading pane of the row the page opens with, as its elements nest: each element's tag,
   *  role and test id, indented by its depth. */
  readonly pane: readonly string[];
  /** The Type filter's entries, in order, by their accessible names. */
  readonly typeEntries: readonly string[];
  /** The Type bands' headings, top to bottom. */
  readonly bands: readonly string[];
  /** Every control of the page by its accessible name, in document order. */
  readonly controls: readonly string[];
}

const CONTROLS = [
  "a[href]",
  "button",
  "input",
  "select",
  "textarea",
  "summary",
  '[role="button"]',
  '[role="link"]',
  '[role="checkbox"]',
  '[role="switch"]',
  '[role="tab"]',
  '[role="menuitem"]',
  '[role="combobox"]',
].join(", ");

/** The reading pane's structure, as `Drawn.pane` holds it. */
function paneStructure(page: Page): Promise<string[]> {
  return page.getByTestId("inbox-pane").evaluate((pane) => {
    const lines: string[] = [];
    const walk = (element: Element, depth: number) => {
      const role = element.getAttribute("role");
      const testId = element.getAttribute("data-testid");
      lines.push(
        `${"  ".repeat(depth)}${element.tagName.toLowerCase()}` +
          `${role === null ? "" : ` role=${role}`}${testId === null ? "" : ` testid=${testId}`}`
      );
      for (const child of element.children) walk(child, depth + 1);
    };
    walk(pane, 0);
    return lines;
  });
}

/** Signs the reader in on `server` and captures its Inbox as `Drawn` holds it. */
async function capture(browser: Browser, server: BaseServer): Promise<Drawn> {
  const context = await browser.newContext();
  try {
    const signIn = await context.request.get(new URL(devSignInPath(reader), server.url).href, {
      maxRedirects: 0,
    });
    expect(signIn.status(), `the dev sign-in on ${server.url}`).toBe(302);
    const page = await context.newPage();
    // The page marks read what the reader is shown, which would change what the other build then
    // draws of the same rows: every mark is held, so both draw the corpus as it was written.
    await holdReadMarks(page);
    await page.goto(new URL("/", server.url).href);
    const rows = page.locator("[data-inbox-row]");
    await expect(rows).toHaveCount(5);
    await expect(page.locator('[data-inbox-selected="true"]')).toHaveCount(1);
    // The page selects its first row as its reads land, and the asks' read and the threads' read
    // land in either order, so the row it opens with is whichever was in first. The pane captured
    // is the one of the list's top row once every row is in, the row it opens with once it has
    // them all, which each build is then shown alike.
    await selectRow(page, (await rows.first().getAttribute("data-inbox-row")) ?? "");
    // The pane draws what it reads as each read lands; it is captured once it is loading nothing
    // and two reads of it a second apart agree.
    await expect(page.getByTestId("inbox-pane").locator('[aria-busy="true"]')).toHaveCount(0);
    let last = "";
    await expect
      .poll(
        async () => {
          const now = (await paneStructure(page)).join("\n");
          const settled = now === last;
          last = now;
          return settled;
        },
        { intervals: [1000], timeout: 30_000 }
      )
      .toBe(true);
    const pane = last.split("\n");
    const drawnRows = await rows.evaluateAll((nodes) =>
      nodes.map((row) => {
        const icon = row.querySelector("[data-inbox-kind]");
        const text = row.cloneNode(true) as Element;
        // The rendered age is the clock's, not the row's: the moment is its `datetime`.
        for (const time of text.querySelectorAll("time")) time.textContent = "";
        return [
          icon?.getAttribute("data-inbox-kind") ?? "",
          icon?.getAttribute("aria-label") ?? "",
          row.querySelector("time")?.getAttribute("datetime") ?? "",
          (text.textContent ?? "").replace(/\s+/g, " ").trim(),
        ].join(" | ");
      })
    );
    const controls = await page.locator(CONTROLS).evaluateAll((nodes) =>
      nodes.map((node) => {
        const element = node as HTMLElement;
        const labelledBy = (element.getAttribute("aria-labelledby") ?? "")
          .split(/\s+/)
          .map((id) => (id === "" ? "" : (document.getElementById(id)?.textContent ?? "")))
          .join(" ");
        const own =
          element.getAttribute("aria-label") ??
          (labelledBy.trim() !== ""
            ? labelledBy
            : element instanceof HTMLInputElement || element instanceof HTMLTextAreaElement
              ? (element.labels?.[0]?.textContent ?? element.placeholder)
              : (element.textContent ?? ""));
        const name = (own.trim() === "" ? (element.getAttribute("title") ?? "") : own)
          .replace(/\s+/g, " ")
          .trim();
        return `${element.getAttribute("role") ?? element.tagName.toLowerCase()}: ${name}`;
      })
    );
    await page.locator("[data-inbox-filter]").click();
    await page.getByRole("button", { name: "Filter by type" }).click();
    const options = page.getByRole("listbox", { name: "Type options" }).getByRole("option");
    await expect(options).not.toHaveCount(0);
    const typeEntries = await options.evaluateAll((nodes) =>
      nodes.map((node) => node.getAttribute("aria-label") ?? "")
    );
    await page.keyboard.press("Escape");
    await page.goto(new URL("/?group=type", server.url).href);
    await expect(rows).toHaveCount(5);
    const bands = await bandHeadings(page);
    return { bands, controls, pane, rows: drawnRows, typeEntries };
  } finally {
    await context.close();
  }
}

/** What this head must draw, given what the base drew: the base's page with the fourth Type
 *  value's label renamed in its three places, and nothing else. */
function renamed(base: Drawn): Drawn {
  return {
    ...base,
    bands: base.bands.map((band) => band.replace(/^Replies\b/, "Threads")),
    rows: base.rows.map((row) => row.replace(/^reply \| Reply \|/, "reply | Threads |")),
    typeEntries: base.typeEntries.map((entry) => entry.replace(/^Reply, /, "Threads, ")),
  };
}

/** Every way `head` differs from `expected`, each printed with the side it came from: an entry one
 *  side draws more often than the other, then the first place the two orders part, with both
 *  orders. A structure (`ordered`) is told apart by the first place it parts alone, since one
 *  element of it repeats as often as the tree holds one. */
function differences(
  part: string,
  expected: readonly string[],
  head: readonly string[],
  { ordered = false }: { readonly ordered?: boolean } = {}
): string[] {
  const found: string[] = [];
  if (!ordered) {
    const counted = (values: readonly string[], value: string) =>
      values.filter((candidate) => candidate === value).length;
    for (const entry of new Set([...expected, ...head])) {
      const onHead = counted(head, entry);
      const onBase = counted(expected, entry);
      if (onHead > onBase) {
        found.push(
          `${part}: ${JSON.stringify(entry)} is present on this head and absent on the base`
        );
      } else if (onBase > onHead) {
        found.push(
          `${part}: ${JSON.stringify(entry)} is present on the base and absent on this head`
        );
      }
    }
  }
  const at = expected.findIndex((value, index) => value !== head[index]);
  if (found.length === 0 && (at !== -1 || expected.length !== head.length)) {
    const index = at === -1 ? expected.length : at;
    found.push(
      `${part}: the orders part at ${index + 1}, the base build (with the rename) drawing ` +
        `${JSON.stringify(expected[index])} and this head ${JSON.stringify(head[index])}` +
        `\n  base build (with the rename):\n    ${expected.join("\n    ")}` +
        `\n  this head:\n    ${head.join("\n    ")}`
    );
  }
  return found;
}

test("the inbox renders the same page before and after the one read, but for the Threads label", async ({
  browser,
}) => {
  // The base build and its dashboard are built once per commit, which the first run pays for,
  // and the corpus is written a minute a row.
  test.setTimeout(3_600_000);
  const build = await baseBuild();
  if ("skip" in build) {
    test.skip(true, build.skip);
    return;
  }
  const web = await baseWebBuild(baseWebDirectory);
  if ("skip" in web) {
    test.skip(true, web.skip);
    return;
  }
  const databaseUrl = (process.env.ISSUE_UNCHANGED_DATABASE_URL ?? "").trim();
  if (databaseUrl === "") {
    throw new Error(
      "ISSUE_UNCHANGED_DATABASE_URL names no database: this row boots both builds over a " +
        "database of its own, which no other check writes"
    );
  }
  await emptyDatabase(databaseUrl);

  let head: BaseServer | undefined;
  let base: BaseServer | undefined;
  try {
    head = await bootBuild(stackBinary(), { databaseUrl, webDist: headWebDist });
    await seedCorpus(head);
    base = await bootBuild(build.binary, { databaseUrl, webDist: web.webDist });
    const before = await capture(browser, base);
    const after = await capture(browser, head);
    for (const [name, drawn] of [
      ["the base build", before],
      ["this head", after],
    ] as const) {
      await test.info().attach(`what ${name} draws`, {
        body: JSON.stringify(drawn, null, 2),
        contentType: "application/json",
      });
    }

    // The rename is proved, not permitted: the base draws `Reply` in all three places, and this
    // head draws `Threads` in each.
    expect(before.typeEntries[3], "the base's fourth Type entry").toMatch(/^Reply, /);
    expect(after.typeEntries[3], "this head's fourth Type entry").toMatch(/^Threads, /);
    expect(before.bands, "the base's Type bands").toContainEqual(expect.stringMatching(/^Replies/));
    expect(after.bands, "this head's Type bands").toContainEqual(expect.stringMatching(/^Threads/));
    expect(before.rows, "the base's row icons").toContainEqual(
      expect.stringMatching(/^reply \| Reply \|/)
    );
    expect(after.rows, "this head's row icons").toContainEqual(
      expect.stringMatching(/^reply \| Threads \|/)
    );

    // Every other difference fails the run.
    const expected = renamed(before);
    const found = [
      ...differences("row", expected.rows, after.rows),
      ...differences("reading pane", expected.pane, after.pane, { ordered: true }),
      ...differences("Type entry", expected.typeEntries, after.typeEntries),
      ...differences("Type band", expected.bands, after.bands),
      ...differences("control", expected.controls, after.controls),
    ];
    expect(found, found.join("\n")).toEqual([]);

    // The read the Inbox's assignee filter moves to: `GET /api/v1/asks/open?waiting_on=` on this
    // head answers each seeded person, read as that person, the ask rows the base's
    // `GET /api/v1/inbox?assignee=` answers them - the same rows by id both ways, and every field
    // the base answers equal on this head's row.
    for (const [login, count] of [
      [reader, 2],
      [other, 1],
    ] as const) {
      const onBase = await read<Record<string, unknown>[]>(
        base,
        await signedIn(base, login),
        `/api/v1/inbox?assignee=${login}`
      );
      const onHead =
        (
          await read<{ asks: Record<string, unknown>[] | null }>(
            head,
            await signedIn(head, login),
            `/api/v1/asks/open?waiting_on=${login}`
          )
        ).asks ?? [];
      const ids = (rows: readonly Record<string, unknown>[]) =>
        rows.map((row) => String(row.id)).sort();
      expect(ids(onBase), `${login}'s asks on the base build`).toHaveLength(count);
      expect(ids(onHead), `${login}'s asks on this head, by id`).toEqual(ids(onBase));
      for (const row of onBase) {
        const answered = onHead.find((candidate) => candidate.id === row.id) ?? {};
        const shared = Object.fromEntries(Object.keys(row).map((key) => [key, answered[key]]));
        expect(shared, `${login}'s ask ${String(row.id)}, every field the base answers`).toEqual(
          row
        );
      }
    }
  } finally {
    await base?.stop();
    await head?.stop();
  }
});
