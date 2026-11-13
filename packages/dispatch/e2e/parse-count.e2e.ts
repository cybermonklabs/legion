/**
 * Every page that draws stored text draws the reading Go sent with it, and parses no markdown in
 * either of the browser's engines.
 *
 * Each row here opens one page of the dashboard, waits for a stored text to be on it - a body
 * formatted, so the row is never a count of a page that drew nothing - and reads the counters
 * `e2e/parse-counting.ts` installs, which say how that count reaches a built page. The last row
 * is the control: the same pages, served with every tree stripped from their answers, draw the
 * same texts by parsing the markdown themselves, and the counter sees those parses.
 */
import { type BrowserContext, expect, type Locator, type Page, test } from "@playwright/test";

import { plannerSession, reviewerSession, setLiveSessions } from "./agents";
import {
  answerAsk,
  createAgentMessage,
  createAsk,
  createBroadcast,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  createProjectDocument,
  editArtifact,
  putArchitectureSource,
  syncArchitectureSource,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { nodeRows } from "./issue-page";
import {
  countParses,
  type ParseCounts,
  readParseCounts,
  rewrittenParseCallSites,
} from "./parse-counting";
import { resetDatabase } from "./seed";
import { serveWithoutDisplayTrees } from "./strip-trees";
import { asUser } from "./users";

const session = {
  actor: { id: plannerSession.session_id, kind: "session" as const },
  as: "agent" as const,
};

/** The architecture model the fake GitHub serves: one component, whose prose is markdown. */
const architecture = {
  "web.md": "---\ntitle: Web\npaths: [packages/web]\n---\nThe **SPA** every reader opens.\n",
};

/** A word no seeded text but the document's holds, so the palette's hits are its own. */
const searchTerm = "astrolabe";

/** The paragraph a block reference cites. Its markdown is the stored text `GET /api/v1/references`
 *  answers for that block, and the citing link's own title is read from the same node, so the
 *  card and the link both draw it. */
const quotedBlock = "A **quoted** paragraph of its own.";

/** `quotedBlock` as a reader sees it, which is the citing link's accessible name. */
const quotedTitle = "A quoted paragraph of its own.";

/** The question of an answered ask the related-item check finds. Its rare words are the ones
 *  `relatedDraft` is typed with, and the issue the ask sits on carries none of them, so the ask
 *  itself is what the check offers and the panel draws its answer
 *  (`e2e/create-issue-decision-claim.e2e.ts` seeds the same shape). */
const hangarQuestion = "Which zeppelin hangar stores the blimps over winter?";

/** The title typed into the create dialog, which reaches `hangarQuestion` by those rare words. */
const relatedDraft = "Zeppelin hangar for the blimps over winter";

/** What the pages this spec opens were seeded with. */
interface Seeded {
  readonly documentSlug: string;
  readonly issueKey: string;
  readonly openAskID: string;
  readonly targetKey: string;
}

async function seedStoredTexts(): Promise<Seeded> {
  await createProject({ key: "CORE", name: "Core" });
  await seedFakeGithub({
    "legion/arch": { contents: "read", files: architecture, installation_id: 101 },
  });
  await putArchitectureSource("CORE", { branch: "main", repo: "legion/arch" });
  const source = await syncArchitectureSource("CORE", "legion/arch");
  expect(source.last_error).toBeNull();

  const target = await createIssue({
    project: "CORE",
    spec: "The **target** issue, which a reference previews.\n",
    title: "Reference target",
  });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Plan\n\nThe **spec** of the issue page.\n",
    title: "Stored texts",
  });
  await createComment(
    issue.key,
    { body: `A **comment** body, beside dispatch://${target.key}.` },
    session
  );
  await createMessage(issue.key, { body: "A **message** body." }, session);
  // An open ask, which the Inbox draws, and an answered one, whose answer the Answers page
  // draws.
  const openAsk = await createAsk(issue.key, { question: "An **open** question?" }, session);
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "An **ask** question?" },
    session
  );
  await answerAsk(ask.id, {
    expected_edited_at: null,
    selected: ["Ship"],
    text: "An **answer** of mine.",
  });
  // A thread alice wrote in that a session replied to: the Replies page draws both bodies.
  const thread = await createComment(issue.key, { body: "A **thread** alice opened." });
  await createComment(
    issue.key,
    { body: "A **reply** of the planner's.", reply_to: thread.id },
    session
  );
  // A comment whose second paragraph another comment cites by its block id: a block reference
  // reads `GET /api/v1/references`, and both the citing link's title and its hover card are
  // drawn from the text that read's node carries.
  const quoted = await createComment(
    issue.key,
    { body: `First, a line of its own.\n\n${quotedBlock}` },
    session
  );
  await createComment(
    issue.key,
    { body: `The detail is in dispatch://${issue.key}/comment/${quoted.id}#b-b2.` },
    session
  );
  // A message to an agent, which the Agents page draws, and a broadcast for the Broadcasts page.
  await setLiveSessions([plannerSession, reviewerSession]);
  await createAgentMessage(plannerSession.session_id, {
    body: "A **direct** message.",
    delivery: "aside",
  });
  await createBroadcast({
    body: "A **broadcast** body.",
    delivery: "aside",
    session_ids: [plannerSession.session_id],
  });

  // Two versions of a document, so version 1 is a past one, and the word the palette searches.
  const document = await createProjectDocument("CORE", {
    content: `# Instruments\n\nThe ${searchTerm} measures altitude.\n`,
    name: "instruments.md",
  });
  await editArtifact(
    document.artifact.id,
    { ops: [{ after: "end", markdown: "A **second** version.\n", op: "insert" }] },
    { as: "agent" }
  );

  // The issue's decisions.md, which the Decisions preset draws as a node of the issue's tree:
  // its one decision holds a paragraph of its own so the preset has a stored text to draw, and
  // it links no spec block, so the issue page above draws no margin card for it.
  await createIssueArtifact(issue.key, {
    content: "## Store the notices\n\nA **decided** thing.\n",
    name: "decisions.md",
  });

  // An answered ask on an issue of its own, which the related-item check behind the create
  // dialog offers for a title typed with the question's rare words: the panel draws that
  // answer, and the issue around the ask shares no word with the draft, so the item the check
  // offers is the ask itself.
  const mast = await createIssue({
    project: "CORE",
    spec: "The mast is repainted each spring, before the sailing season.\n",
    title: "Mooring mast paint rota",
  });
  const hangarAsk = await createAsk(
    mast.key,
    { options: [{ label: "North" }, { label: "South" }], question: hangarQuestion },
    session
  );
  await answerAsk(hangarAsk.id, {
    expected_edited_at: null,
    selected: ["North"],
    text: "The **northern** one, which holds them all winter.",
  });

  return {
    documentSlug: document.artifact.slug,
    issueKey: issue.key,
    openAskID: openAsk.id,
    targetKey: target.key,
  };
}

/** One page this spec counts the parses of: its name, and what it does to put a stored text on
 *  the screen. */
interface Surface {
  readonly name: string;
  readonly show: (page: Page) => Promise<void>;
}

/** A word a stored text holds in bold: drawn, it is a `strong` of its own, whether the page read
 *  it from a tree or parsed it; undrawn, there is no such element at all. */
function bold(page: Page, word: string): Locator {
  return page
    .locator("strong")
    .filter({ hasText: new RegExp(`^${word}$`) })
    .first();
}

function surfaces(seeded: Seeded): Surface[] {
  return [
    {
      name: "the issue page",
      show: async (page) => {
        await page.goto(`/issues/${seeded.issueKey}`);
        await expect(bold(page, "spec")).toBeVisible();
      },
    },
    {
      name: "the Decisions preset",
      show: async (page) => {
        await page.goto(`/issues/${seeded.issueKey}/decisions`);
        // The preset draws a decision as a node of the tree, its text as the decision holds it
        // (`NodeRow.tsx`), so what says the stored text is on the screen is the node's own words.
        // Until dispatch://INFRA-1196 this read the Decisions tab, which laid a decision out
        // block by block from its tree (`DecisionsPanel.tsx`) and drew `decided` in bold.
        await expect(nodeRows(page).filter({ hasText: "Store the notices" }).first()).toBeVisible();
      },
    },
    {
      name: "the Inbox",
      show: async (page) => {
        await page.goto("/");
        // The row is selected by this spec rather than left to the pane's own first choice: the
        // list is fed by two reads (the asks and the reply threads), and whichever lands first
        // decides what the pane opens, so waiting on the ask's question alone is a race this
        // spec would lose about half the time. Selecting it draws the same body deterministically.
        const row = page.locator(`[data-inbox-row="${seeded.openAskID}"]`);
        await expect(row).toBeVisible();
        await row.click();
        await expect(bold(page, "open")).toBeVisible();
      },
    },
    {
      name: "the activity feed",
      show: async (page) => {
        await page.goto(`/issues/${seeded.issueKey}/activity`);
        // The feed draws each event as a line of its own rather than as a body, so its own
        // rows are what says the stored history is on the page.
        await expect(
          page.getByRole("list", { name: "Issue activity" }).getByRole("listitem").first()
        ).toBeVisible();
      },
    },
    {
      name: "a past version",
      show: async (page) => {
        await page.goto(`/projects/CORE/documents/${seeded.documentSlug}?version=1`);
        // The version's own section, not the document around it: the view that draws a past
        // version is what this row counts.
        await expect(
          page.getByRole("region", { name: "Document version 1" }).getByText("Instruments")
        ).toBeVisible();
      },
    },
    {
      name: "the search palette",
      show: async (page) => {
        await page.goto("/");
        await page.locator("body").focus();
        await page.keyboard.press("Control+k");
        const search = page.getByRole("combobox", { name: "Search" });
        await search.fill(searchTerm);
        await expect(
          page.getByRole("dialog", { name: "Search" }).getByRole("option").first()
        ).toBeVisible();
      },
    },
    {
      name: "a reference preview",
      show: async (page) => {
        await page.goto(`/issues/${seeded.issueKey}/conversation`);
        await page.getByRole("link", { name: "Reference target" }).first().hover();
        await expect(page.getByRole("tooltip")).toContainText(seeded.targetKey);
      },
    },
    {
      name: "a block reference's card",
      show: async (page) => {
        await page.goto(`/issues/${seeded.issueKey}/conversation`);
        // A reference to a block of a comment is the surface `GET /api/v1/references` draws:
        // the link's title and the card under it are both the text that read's node carries.
        await page.getByRole("link", { name: quotedTitle }).first().hover();
        const card = page.getByRole("tooltip");
        await expect(card.locator("strong").filter({ hasText: /^quoted$/ })).toBeVisible();
      },
    },
    {
      name: "the Answers page",
      show: async (page) => {
        await page.goto("/answers");
        await expect(bold(page, "answer")).toBeVisible();
      },
    },
    {
      name: "the Replies page",
      show: async (page) => {
        await page.goto("/replies");
        await expect(bold(page, "reply")).toBeVisible();
      },
    },
    {
      name: "the Agents page",
      show: async (page) => {
        await page.goto("/agents");
        // A session's row opens its conversation with Dispatch; the direct message sits behind
        // the broadcast that followed it.
        await page.getByRole("button", { exact: true, name: plannerSession.title }).click();
        await page.getByRole("button", { name: /^Show \d+ older$/ }).click();
        await expect(bold(page, "direct")).toBeVisible();
      },
    },
    {
      name: "the Broadcasts page",
      show: async (page) => {
        await page.goto("/agents/broadcasts");
        await expect(bold(page, "broadcast")).toBeVisible();
      },
    },
    {
      name: "a component's page",
      show: async (page) => {
        await page.goto("/projects/CORE/architecture?component=web");
        await expect(bold(page, "SPA")).toBeVisible();
      },
    },
    {
      name: "the architecture page, into a component",
      show: async (page) => {
        await page.goto("/projects/CORE/architecture");
        // The tree read carries every component's `prose_tree`, and `ComponentDetails` is its
        // one reader, so the row walks the tree into a component the way a person does: the
        // rows themselves draw titles and bars, and the page's stored text is the prose below
        // them. The row above reaches the same component by its URL instead.
        await page
          .getByRole("list", { name: "Components" })
          .getByRole("link", { exact: true, name: "Web" })
          .click();
        await expect(bold(page, "SPA")).toBeVisible();
      },
    },
    {
      name: "the related panel",
      show: async (page) => {
        // The create dialog's related-item check, whose answered ask carries the display tree
        // of its answer: `AskHitStatus` draws that answer under the hit it found.
        //
        // The row covers the check's one-project leg, which is the only leg this harness can
        // drive: the cross-project one is reached from the meaning stage alone
        // (`relatedMeaningStage`), which needs an embedder, and a run of this suite starts the
        // server with none (`e2e/harness-embedder.ts`). The dashboard's own read - the GET
        // form of `/api/v1/projects/{key}/related`, which this dialog makes - runs the
        // one-project leg whatever the server embeds, so this is the leg the panel draws from
        // in a browser.
        await page.goto("/");
        await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();
        await page.locator("body").focus();
        await page.keyboard.press("c");
        const dialog = page.getByRole("dialog", { name: "Create issue" });
        await expect(dialog).toBeVisible();
        await dialog.getByRole("textbox", { name: "Title" }).fill(relatedDraft);
        await expect(bold(page, "northern")).toBeVisible();
      },
    },
  ];
}

/** One surface's counts, under its own name: a walk compares whole objects, so the page that
 *  parsed is named in the comparison rather than found by its place in the list. */
interface SurfaceParses extends ParseCounts {
  readonly surface: string;
}

/**
 * Opens a page of `context`, shows `surface` on it and answers what it parsed.
 *
 * The surface names itself in whatever showing it fails with: each walk below is one test over
 * fifteen pages, so the surface that drew nothing has to be in the failure itself and not only
 * in the line number under it. And the stored text has to stand in place of the document rather
 * than beside an apology for it: a view that gave up and drew its error can still leave the word
 * the walk waits for somewhere else on the page. Both readings hold wherever a surface is
 * opened, with the trees served or stripped, so both are read here.
 */
async function parsesOf(context: BrowserContext, surface: Surface): Promise<SurfaceParses> {
  const page = await context.newPage();
  try {
    try {
      await surface.show(page);
    } catch (cause) {
      const said = cause instanceof Error ? cause.message : String(cause);
      throw new Error(`${surface.name} drew no stored text: ${said}`, { cause });
    }
    await expect(
      page.getByText(/Could not load/),
      `${surface.name} drew an error in place of its document`
    ).toHaveCount(0);
    return { ...(await readParseCounts(page)), surface: surface.name };
  } finally {
    await page.close();
  }
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("no page that draws stored text parses markdown in either browser engine", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one engine proves what the pages parse");
  // Fifteen pages, each loaded in a page of its own, on a two-core box: the default deadline
  // is one page's.
  test.setTimeout(310_000);
  const seeded = await seedStoredTexts();
  const alice = await asUser(browser, "alice");
  await countParses(alice);
  try {
    const pages = surfaces(seeded);
    // Every surface is walked, not merely the ones still in the list: a row that stops reaching
    // its page, or a surface dropped from `surfaces`, leaves the comparison below green over
    // what remains, so the count the claim rests on is pinned here and moves only deliberately.
    expect(pages).toHaveLength(15);
    const counted = [];
    for (const surface of pages) counted.push(await parsesOf(alice, surface));
    // One comparison over all fifteen, so a page that parses is named beside the fourteen that
    // do not rather than stopping the walk at the first.
    expect(counted).toEqual(
      pages.map((surface) => ({ display: 0, editor: 0, surface: surface.name, workerRequests: 0 }))
    );
    // The counter is in the build these pages ran: a rename that left it counting nothing would
    // pass every row above.
    expect(rewrittenParseCallSites(alice)).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

test("the counter sees the parse a text served with no reading costs", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the control runs where the rows run");
  // The same fifteen pages as the row above, each in a page of its own: the deadline is that
  // walk's, not one page's.
  test.setTimeout(310_000);
  const seeded = await seedStoredTexts();
  const alice = await asUser(browser, "alice");
  await countParses(alice);
  // Every tree out of the answer, as a server that sends none would: the page then has the
  // markdown alone, which it parses itself.
  await serveWithoutDisplayTrees(alice);
  try {
    const pages = surfaces(seeded);
    // The same list the row above walks, pinned the same way: the control is what says each of
    // those pages falls back to the markdown, so a surface missing from one list is missing
    // from both.
    expect(pages).toHaveLength(15);
    const counted = [];
    // The same walk the row above makes, so a view that refuses an answer carrying no tree, or
    // draws nothing from one, fails under its own surface's name here too: the fallback is a
    // rule of the whole list rather than of the views someone has fixed so far.
    for (const surface of pages) counted.push(await parsesOf(alice, surface));
    // And the counter sees the parse a page with no reading has to make: the issue page holds
    // the spec's markdown alone and reads it itself, which is what says the zeros the row above
    // counts are pages that could have parsed and did not.
    expect(counted.find((page) => page.surface === "the issue page")?.display).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});
