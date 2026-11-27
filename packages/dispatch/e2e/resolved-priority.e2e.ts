/** The priority a sub-issue takes from the nearest issue above it that has one, read in a browser
 *  and by the real `dispatch` command against the real stack (dispatch://INFRA-5793, j-c-stack).
 *
 *  The server decides everything these rows read: it climbs from an issue with no priority of
 *  its own to the nearest issue above it that has one and answers `resolved_priority` and
 *  `priority_from` on every read the pages and the command make - the issue, the project's list,
 *  the Inbox, a reference's preview - and filters the list by it; and it refuses a move, a clear
 *  and a new child that would make work P0 from a credential without p0, unless the new child's
 *  writer is the session its parent's role holders name as the architect. The second row also
 *  reads back that each refused write wrote nothing.
 *
 *  The pages' own components are read by the web app's unit tests (W1 to W9 of the plan), and
 *  the command's words by `packages/envoy-client`'s; what is here is that the server's answer
 *  reaches each surface whole.
 */
import { mkdirSync } from "node:fs";

import { PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  createAsk,
  createComment,
  createIssue,
  createProject,
  getIssue,
  listIssues,
  putIssueRoleHolders,
} from "./api";
import { runDispatch } from "./dispatch-cli";
import { inboxRow } from "./inbox-pane";
import { gotoIssue, openDetails } from "./issue-page";
import { withIssuePage } from "./issue-page-option";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";

/** The titles of the tree both rows seed, each unlike the others, so no create of the seed or
 *  of a row meets the near-copy check of another. */
const TITLES = {
  root: "Rotate every signing key",
  child: "Revoke the leaked deploy token",
  ownP2: "Audit who reads the vault",
  ownP2Child: "Chart the services behind the proxy",
  p1Root: "Move the build farm to new hosts",
  ownP0: "Patch the runner escape",
  ownP0Child: "Rebuild the base image without curl",
  none: "Tidy the onboarding guide",
} as const;

/** The issues both rows read, by what each is in the tree. */
interface Tree {
  /** CORE-1, a root at its own P0. */
  readonly root: string;
  /** A child of `root` with no priority of its own: P0 from `root`. */
  readonly child: string;
  /** A middle under `root` at its own P2. */
  readonly ownP2: string;
  /** A child of `ownP2` with no priority of its own: P2 from `ownP2`, though its root is P0. */
  readonly ownP2Child: string;
  /** A root at its own P1. */
  readonly p1Root: string;
  /** A middle under `p1Root` at its own P0. */
  readonly ownP0: string;
  /** A child of `ownP0` with no priority of its own: P0 from `ownP0`, though its root is P1. */
  readonly ownP0Child: string;
  /** A root with no priority, which nothing above it gives one. */
  readonly none: string;
}

/** The tree, written by alice, whose browser sign-in holds p0, one issue after another, so the
 *  P0 root is CORE-1 as the plan's words name it. The middles are what tell the nearest issue's
 *  priority from the root's: a rule of the root alone reads `ownP2Child` as P0 and `ownP0Child`
 *  as P1. */
async function seedTree(): Promise<Tree> {
  await createProject({ key: "CORE", name: "Core" });
  const root = await createIssue({ priority: 0, project: "CORE", title: TITLES.root });
  expect(root.key, "the first issue of CORE").toBe("CORE-1");
  const child = await createIssue({ parent: root.key, project: "CORE", title: TITLES.child });
  const ownP2 = await createIssue({
    parent: root.key,
    priority: 2,
    project: "CORE",
    title: TITLES.ownP2,
  });
  const ownP2Child = await createIssue({
    parent: ownP2.key,
    project: "CORE",
    title: TITLES.ownP2Child,
  });
  const p1Root = await createIssue({ priority: 1, project: "CORE", title: TITLES.p1Root });
  const ownP0 = await createIssue({
    parent: p1Root.key,
    priority: 0,
    project: "CORE",
    title: TITLES.ownP0,
  });
  const ownP0Child = await createIssue({
    parent: ownP0.key,
    project: "CORE",
    title: TITLES.ownP0Child,
  });
  const none = await createIssue({ project: "CORE", title: TITLES.none });
  for (const issue of [child, ownP2Child, ownP0Child, none]) {
    expect(issue.priority, `${issue.key} is seeded with no priority of its own`).toBeNull();
  }
  return {
    child: child.key,
    none: none.key,
    ownP0: ownP0.key,
    ownP0Child: ownP0Child.key,
    ownP2: ownP2.key,
    ownP2Child: ownP2Child.key,
    p1Root: p1Root.key,
    root: root.key,
  };
}

/** The desktop the issue pages, the issues table and the Inbox are read at. */
const WIDE = { height: 900, width: 1440 } as const;

/** The board's width, at which its seven open columns stand side by side in each lane, as
 *  `board.e2e.ts` reads them. */
const BOARD = { height: 900, width: 1920 } as const;

/** What a surface draws for a priority `level` that comes from the issue `from`. */
function inherited(level: string, from: string): string {
  return `${level} · from ${from}`;
}

/** That today's issue page of `key` draws, in its header's priority control, the priority
 *  `level` and the issue `from` it comes from, with that issue's key a link to its page. The
 *  control is held to be in the header first, so a header that drew none fails as that. */
async function expectPageControl(
  page: Page,
  key: string,
  level: string,
  from: string
): Promise<void> {
  await page.goto(`/issues/${key}`);
  const header = page.getByTestId("issue-header");
  await expect(
    header.getByRole("combobox", { name: `Priority of ${key}` }),
    `the priority control of ${key}'s header`
  ).toHaveCount(1);
  await expect(header, `${key}'s priority control`).toContainText(inherited(level, from));
  await expect(
    header.getByRole("link", { exact: true, name: from }),
    `the link to ${from} beside ${key}'s priority`
  ).toHaveAttribute("href", `/issues/${from}`);
}

/** The keys of the project's issues table, in the order it draws them. */
function tableKeys(page: Page): Promise<string[]> {
  return page
    .locator("[data-issue-row]")
    .evaluateAll((rows) => rows.map((row) => row.getAttribute("data-issue-row") ?? ""));
}

// SLOW(>=2s): eight issues, three asks and a comment seeded, then three issue pages, the board,
// the issues table twice, the Inbox, a preview card and the new issue page's Details read.
test("a sub-issue shows the priority it inherits, and the issue it comes from, on every page", async ({
  browser,
}) => {
  const tree = await seedTree();
  const asker = {
    actor: { id: "e2e-priority-asker", kind: "session" as const },
    as: "agent" as const,
  };
  // Each ask waits on alice, who filed its issue and so holds it.
  const rootAsk = await createAsk(tree.root, { question: "Which keys rotate first?" }, asker);
  const childAsk = await createAsk(tree.child, { question: "Who holds the leaked token?" }, asker);
  const noneAsk = await createAsk(tree.none, { question: "Which guide page goes first?" }, asker);
  await createComment(tree.none, { body: `The guide waits on dispatch://${tree.child}.` }, asker);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize(WIDE);

    // The issue page's control: the child's P0 from the root, and each middle's child's priority
    // from its middle, whatever its root holds.
    await expectPageControl(page, tree.child, "P0", tree.root);
    await expectPageControl(page, tree.ownP0Child, "P0", tree.ownP0);
    await expectPageControl(page, tree.ownP2Child, "P2", tree.ownP2);

    // The issues table sorts the two inherited P0s with the two own ones, before the P1 root,
    // and the P2 middle's child with its middle, before the issue that has none. It is read
    // before the board, since the project page keeps the reader's choice of Board for its
    // Issues tab from then on (`project.issue-view`, `ProjectPage.tsx`).
    await page.goto("/projects/CORE/issues?sort=priority,key");
    await expect(page.getByRole("table", { name: "Project issues table" })).toBeVisible();
    await expect
      .poll(() => tableKeys(page), "the issues table sorted by priority, then key")
      .toEqual([
        tree.root,
        tree.child,
        tree.ownP0,
        tree.ownP0Child,
        tree.p1Root,
        tree.ownP2,
        tree.ownP2Child,
        tree.none,
      ]);
    const priorityCell = page
      .locator(`[data-issue-row="${tree.child}"]`)
      .getByRole("cell")
      .filter({ has: page.getByRole("combobox", { name: `Priority of ${tree.child}` }) });
    await expect(priorityCell, "the child's Priority cell").toContainText(
      inherited("P0", tree.root)
    );
    await expect(
      priorityCell.getByRole("link", { exact: true, name: tree.root }),
      "the link to the root in the child's Priority cell"
    ).toHaveAttribute("href", `/issues/${tree.root}`);
    // Its P0 filter keeps them both.
    await page.goto("/projects/CORE/issues?f.priority=0");
    await expect
      .poll(async () => (await tableKeys(page)).sort(), "the issues table's P0 filter")
      .toEqual([tree.root, tree.child, tree.ownP0, tree.ownP0Child].sort());

    // The board's priority lanes put the child's card with the root's own, naming the root.
    await page.setViewportSize(BOARD);
    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    await page.getByRole("button", { name: "Show lanes" }).click();
    await expect(page.getByRole("button", { name: "Hide lanes" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    const p0Lane = page.getByRole("region", { name: "P0 lane" });
    await expect(
      p0Lane.getByRole("article", { name: `${tree.root} ${TITLES.root}` }),
      "the root's own P0 card in the P0 lane"
    ).toBeVisible();
    await expect(
      p0Lane.getByRole("article", { name: `${tree.child} ${TITLES.child}` }),
      "the child's card in the P0 lane"
    ).toContainText(inherited("P0", tree.root));

    // The Inbox bands the ask on the child with the root's ask, and tags it P0, saying in its
    // name where the P0 comes from; the ask on the issue with none stands apart.
    await page.setViewportSize(WIDE);
    await page.goto("/?group=priority");
    await expect(inboxRow(page, rootAsk.id), "the ask on the root").toHaveAttribute(
      "data-inbox-group",
      "P0"
    );
    await expect(inboxRow(page, noneAsk.id), "the ask on the issue with none").toHaveAttribute(
      "data-inbox-group",
      "none"
    );
    await expect(inboxRow(page, childAsk.id), "the ask on the child").toHaveAttribute(
      "data-inbox-group",
      "P0"
    );
    await expect(page.getByRole("button", { name: "P0 (2)" })).toBeVisible();
    const tag = inboxRow(page, childAsk.id).getByTitle(`P0, from ${tree.root}`);
    await expect(tag, "the child's priority tag").toHaveText("P0");
    await expect(tag, "the child's priority tag").toHaveAccessibleName(`P0, from ${tree.root}`);

    // A reference to the child previews it with its priority and where it comes from.
    await page.goto(`/issues/${tree.none}/conversation`);
    const reference = page
      .getByRole("list", { name: "Conversation turns" })
      .getByRole("link", { name: TITLES.child });
    await expect(reference).toBeVisible();
    await reference.hover();
    const card = page.getByRole("tooltip");
    await expect(card, "the preview card of the child").toContainText(TITLES.child);
    await expect(card, "the preview card of the child").toContainText(inherited("P0", tree.root));

    // The new issue page's Details say the same in its Priority row. The page is on from here
    // for every load of this context, so it is read last.
    await withIssuePage(alice, "alice");
    await gotoIssue(page, `/issues/${tree.child}`);
    await openDetails(page);
    // The row of Details by the words of its label, the label and its value (`IssueMargin.tsx`'s
    // `Row`), read as `e2e/issue-page-h.ts` reads one for its own rows.
    const row = page
      .getByTestId("issue-margin")
      .locator("[data-margin-section] > div > div")
      .filter({ has: page.getByText(PROPERTY_LABELS.priority, { exact: true }) });
    await expect(row, "the Priority row of the child's Details").toContainText(
      inherited("P0", tree.root)
    );
    await expect(
      row.getByRole("link", { exact: true, name: tree.root }),
      "the link to the root in the child's Priority row"
    ).toHaveAttribute("href", `/issues/${tree.root}`);
  } finally {
    await alice.close();
  }
});

/** The `Priority:` lines one `dispatch read` printed. */
function priorityLines(stdout: string): string[] {
  return stdout.split("\n").filter((line) => line.startsWith("Priority:"));
}

// The words a sentence of the help is read for, the ones the route index's own test reads in the
// routes' text (`TestTheRouteIndexSaysWhenAMoveAClearOrANewChildNeedsP0`), so the row reads the
// words and not a copy of one sentence.
const MOVES = /\bmov(?:e|es|ing)\b/i;
const CLEARS = /\bclear(?:s|ing|ed)?\b/i;
const NEEDS_P0 = /\bneeds? (?:the )?p0\b/i;
/** A new child of a P0 issue: its parent, that the parent reads P0, that it needs p0, and the
 *  parent's architect, whose session files it without p0. */
const NEW_CHILD = [/\bparent\b/i, /\bP0\b|\b[Pp]riority (?:of )?0\b/, NEEDS_P0, /\barchitect\b/i];

/** That one sentence of `help` under `flags` holds every one of `words`. The sentences are each
 *  flag's description - the lines under its own, up to the next flag - read as one text and cut
 *  at each sentence's end, so a description that names a move and p0 in two sentences says
 *  nothing of a move that needs p0. */
function expectHelpSays(
  help: string,
  flags: readonly string[],
  words: readonly RegExp[],
  says: string
): void {
  const lines = help.split("\n");
  const sentences = flags
    .flatMap((flag) => {
      const start = lines.findIndex(
        (line) => line === `  ${flag}` || line.startsWith(`  ${flag} `)
      );
      const next = lines.findIndex((line, index) => index > start && /^ {2}--/.test(line));
      return start === -1 ? [] : lines.slice(start + 1, next === -1 ? undefined : next);
    })
    .map((line) => line.trim())
    .join(" ")
    .split(/[.!?](?:\s+|$)/);
  expect
    .soft(
      sentences.filter((sentence) => words.every((word) => word.test(sentence))),
      `the help under ${flags.join(" and ")} says that ${says}, among ${JSON.stringify(sentences)}`
    )
    .not.toEqual([]);
}

/** The session the P0 middle's role holders name as its architect, which files a child under it
 *  with the shared token, which holds no p0. */
const ARCHITECT = "e2e-runner-architect";

/** The title of the child a session that is no architect of the P0 middle asks to file under
 *  it, and of the one its architect files. */
const REFUSED_CHILD = "Pin the toolchain checksum";
const ARCHITECT_CHILD = "Write the escape incident timeline";

// The command's claims are checked softly where nothing later reads them, so one run says every
// one that fails; what a later step reads is checked hard.
// This row carries no `@pr` tag: the pull-request lane's real-stack share is at its cap
// (`realStackBudget`, e2e/__tests__/pull-request-lane.test.ts), so the full lane runs it.
// SLOW(>=2s): eleven cold `bun` starts of the real `dispatch` command around a role holder
// published, and one issue page read.
test("the command prints an inherited priority, and refuses a move, a clear and a new child of a P0 issue without p0", async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the command runs outside the browser");
  test.setTimeout(240_000);
  const tree = await seedTree();
  const home = testInfo.outputPath("dispatch-home");
  mkdirSync(home, { recursive: true });

  // `dispatch read` prints the priority each child takes and the issue it comes from: the root's
  // for the child, and the P0 middle's for its child, whose root is P1.
  for (const [key, line] of [
    [tree.child, `Priority: P0 (from ${tree.root})`],
    [tree.ownP0Child, `Priority: P0 (from ${tree.ownP0})`],
  ] as const) {
    const read = await runDispatch(["read", "--issue", key], home);
    expect(read.code, `read --issue ${key}: ${read.stdout}${read.stderr}`).toBe(0);
    expect
      .soft(priorityLines(read.stdout), `the Priority line of read --issue ${key}`)
      .toEqual([line]);
  }

  // `dispatch issues --priority 0` lists the child, which has no priority of its own.
  const listed = await runDispatch(["issues", "--project", "CORE", "--priority", "0"], home);
  expect(listed.code, `issues --priority 0: ${listed.stdout}${listed.stderr}`).toBe(0);
  expect
    .soft(
      listed.stdout.split("\n").find((line) => line.startsWith(`${tree.child} `)),
      `the row of ${tree.child} that issues --project CORE --priority 0 prints: ${listed.stdout}`
    )
    .toContain(`P0 (from ${tree.root})`);

  // The help says when a move, a clear and a new child of a P0 issue need p0.
  const updateHelp = await runDispatch(["issue-update", "--help"], home);
  expect(updateHelp.code, `issue-update --help: ${updateHelp.stderr}`).toBe(0);
  expectHelpSays(updateHelp.stdout, ["--parent"], [MOVES, NEEDS_P0], "a move under it needs p0");
  expectHelpSays(
    updateHelp.stdout,
    ["--priority", "--clear-priority"],
    [CLEARS, NEEDS_P0],
    "a clear needs p0"
  );
  const createHelp = await runDispatch(["issue", "--help"], home);
  expect(createHelp.code, `issue --help: ${createHelp.stderr}`).toBe(0);
  expectHelpSays(
    createHelp.stdout,
    ["--parent", "--priority"],
    NEW_CHILD,
    "a new child of a P0 issue needs p0, but from its architect"
  );

  // Under the shared token, which holds no p0, a move under the P0 root of an issue with no
  // priority, a clear of the P2 middle's own priority under that root, and a new child with no
  // priority of the P0 middle from a session that is not its architect are each refused, and
  // write nothing. The middle's architect is published first, so the refusal is the writer's
  // session and not a middle that has none.
  await putIssueRoleHolders(tree.ownP0, [{ role: "architect", session: ARCHITECT }]);
  for (const [what, argv] of [
    ["a move under the P0 root", ["issue-update", "--issue", tree.none, "--parent", tree.root]],
    ["a clear under the P0 root", ["issue-update", "--issue", tree.ownP2, "--clear-priority"]],
    [
      "a new child of the P0 middle",
      ["issue", "--project", "CORE", "--title", REFUSED_CHILD, "--parent", tree.ownP0],
    ],
  ] as const) {
    const refused = await runDispatch(argv, home);
    const printed = `${refused.stdout}${refused.stderr}`;
    expect.soft(refused.code, `${what}: ${printed}`).toBe(1);
    // Dispatch's refusal for want of p0 prints as one line, its code and its message, which
    // names the scope: `OAUTH_SCOPE_REQUIRED: <message>`.
    expect
      .soft(
        printed
          .split("\n")
          .some((line) => line.includes("OAUTH_SCOPE_REQUIRED") && /\bp0\b/.test(line)),
        `${what} prints OAUTH_SCOPE_REQUIRED and p0: ${printed}`
      )
      .toBe(true);
  }
  expect
    .soft((await getIssue(tree.none)).parent, `${tree.none}'s parent after the refused move`)
    .toBeNull();
  expect
    .soft((await getIssue(tree.ownP2)).priority, `${tree.ownP2}'s priority after the refused clear`)
    .toBe(2);
  expect
    .soft(
      (await listIssues("CORE")).map((issue) => issue.key).sort(),
      "CORE's issues after the refused new child"
    )
    .toEqual(Object.values(tree).sort());

  // The architect's own session files the same kind of child with the same token, and the child
  // takes P0 from the middle.
  const filed = await runDispatch(
    ["issue", "--project", "CORE", "--title", ARCHITECT_CHILD, "--parent", tree.ownP0],
    home,
    { session: ARCHITECT }
  );
  expect(filed.code, `the architect's new child: ${filed.stdout}${filed.stderr}`).toBe(0);
  const created = /^Created (\S+):/m.exec(filed.stdout)?.[1];
  expect(created, `the key dispatch issue printed: ${filed.stdout}`).toBeDefined();
  const key = created ?? "";
  const read = await runDispatch(["read", "--issue", key], home);
  expect(read.code, `read --issue ${key}: ${read.stdout}${read.stderr}`).toBe(0);
  expect
    .soft(priorityLines(read.stdout), `the Priority line of read --issue ${key}`)
    .toEqual([`Priority: P0 (from ${tree.ownP0})`]);

  // And a person reads the same on its page.
  await page.setViewportSize(WIDE);
  await expectPageControl(page, key, "P0", tree.ownP0);
});
