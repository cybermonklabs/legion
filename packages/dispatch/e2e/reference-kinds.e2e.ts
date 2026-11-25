import { type Result, routePath } from "../web/src/api/paths";
import { agentFilter } from "./agents";
import { baseUrl, createProjectDocument, userHeaders } from "./api";
import { recordClipboard } from "./clipboard";
import { commentField, highlightedRow, refPicker } from "./ref-picker";
import {
  COMPONENT_ID,
  COMPONENT_TITLE,
  componentReference,
  personReference,
  ROLE_REFERENCE,
  ROLE_TOKEN,
  referencePerson,
  referenceSearchWord,
  referenceSessionId,
  SESSION_TITLE,
  seedReferenceWorld,
  sessionReference,
} from "./reference-surfaces";
import { settledState } from "./settled";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * The check that the picker reaches every kind Dispatch holds, in each
 * of its hosts.
 *
 * For each of the thirteen kinds the spec names the picker inserts that kind's address, the
 * comment the reader sends holds exactly what was picked, and every reference in the stored
 * record reads as its node's name and opens that node's own page. A block of a document, a part
 * of an ask, a block of a comment and a block of a message are reached by opening the node that
 * holds them (the row's **Blocks**), never by a query alone: their text is their holder's, so a
 * query that finds the holder is what puts them within reach. A document of an agent's
 * conversation is reached the same way, by opening the agent's row (its **Documents**): the
 * search index holds no such file, and the agent's conversation lists them
 * (`GET /api/v1/agents/{session_id}/artifacts`).
 *
 * One of the thirteen is not the picker's to open, and says so where it is read: a person, whom
 * the picker writes as their login and whose reference opens nothing, since
 * Dispatch has no page for a person yet.
 *
 * The agent live conversation draws the same picker in assistant-ui's composer, where the keys
 * are assistant-ui's: there an issue's message is offered as everywhere, and each holder's parts
 * open by the row's control and by ArrowRight.
 */

/** The 1x1 PNG the suite uploads, as `e2e/artifacts.e2e.ts` and the two-build spec upload it. */
const TINY_PNG =
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL0eAAAAABJRU5ErkJggg==";

/** The file a session's conversation holds, uploaded as the Agents row's own paste uploads one
 *  (`POST /api/v1/agents/{session_id}/artifacts`, the only route that takes one). */
const AGENT_FILE = "shot.png";
const AGENT_SLUG = "shot-png";

async function uploadAgentPicture(sessionID: string): Promise<void> {
  const form = new FormData();
  form.append("name", AGENT_FILE);
  form.append("file", new Blob([Buffer.from(TINY_PNG, "base64")], { type: "image/png" }));
  const response = await fetch(
    new URL(
      routePath("post", "/api/v1/agents/{session_id}/artifacts", { session_id: sessionID }),
      baseUrl
    ),
    { body: form, headers: await userHeaders("alice"), method: "POST" }
  );
  if (!response.ok) {
    throw new Error(`agent picture upload: ${response.status} ${await response.text()}`);
  }
}

/** One kind of node, as the reader reaches it and as the record then reads. */
interface Kind {
  /** What this row proves, which is the kind's name in the failure. */
  readonly kind: string;
  /** What the reader types: the door's characters and the query after them. */
  readonly query: string;
  /** The row to take, by its own words. */
  readonly row: RegExp;
  /** The row whose parts must be opened first, for a kind that is a part of another node or a
   *  document of an agent's conversation, the words of the control that opens them (**Blocks**
   *  where none is named), and the heading they are listed under: the row is taken from that
   *  group alone, since the holder's own row can match the same words while its parts are still
   *  being read. */
  readonly holder?: { readonly row: RegExp; readonly blocks: string; readonly control?: string };
  /** What the pick must write, where the whole address is known before the pick; a block's id is
   *  the server's, so those rows check the shape instead. */
  readonly writes?: string;
  readonly shape?: RegExp;
  /** The address the stored text names, where the pick wrote a mention rather than the address
   *  itself: a party's words are `@` and its name, and the reference is inside the link. */
  readonly names?: string;
  /** What the stored reference reads as: the node's own name, `@` and the name for a party. */
  readonly reads: string;
  /** The page it opens, from the address the pick wrote. An empty path is no link at all. */
  readonly opens: (address: string) => string;
}

/** The block id an address names, which is the server's own and is never guessed. */
function blockOf(address: string): string {
  const id = address.split("#b-")[1];
  if (id === undefined) throw new Error(`${address} names no block`);
  return id;
}

/** `row`, narrowed to the picker row of the node under `owner`, an issue's key or a project's.
 *  The search the picker reads is the whole stack's, so a node of another row's world can carry
 *  the same words; the line under a row's words is part of its accessible name and holds its
 *  owner's key (`shortForm`, `features/refs/routes.ts`), which no other row's world holds. */
function within(row: RegExp, owner: string): RegExp {
  return new RegExp(`${row.source}.*${owner}`);
}

// SLOW(>=2s): the one reference world - three projects, fifteen issues, the host's question
// blocks settling into asks, the architecture source sync and the live session - then fifteen
// picks, each its own read of the registry, the collaborators, the components and the search
// route, and the comment they are sent in.
test("the picker reaches every kind, and each pick reads as its node's name and opens it", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const world = await seedReferenceWorld({ tagPickerSearch: true });
  const core = world.projects.core;
  const query = referenceSearchWord(core);
  const sessionID = referenceSessionId();
  const sessionRef = sessionReference();
  const componentRef = componentReference(core);
  const personLogin = referencePerson();
  const personRef = personReference();
  await createProjectDocument(core, {
    content: `# Glossary ${query}\n\nThe cache glossary.\n`,
    name: "glossary.md",
  });
  await uploadAgentPicture(sessionID);
  const target = world.target.key;
  const agentDocument = `dispatch://agent/${sessionID}/artifact/${AGENT_SLUG}`;

  // The thirteen kinds, in the order one draft names them, a document in each of its three
  // owners' forms. A block, a part and the two item blocks follow the node that holds them, and a
  // document of an agent's conversation is reached from the agent's row, because that is how
  // they are reached.
  const kinds: readonly Kind[] = [
    {
      kind: "issue",
      opens: () => `/issues/${target}`,
      query: `#${query}`,
      reads: world.targets.Issue.title,
      row: within(/^Warm the cache/, target),
      writes: `dispatch://${target}`,
    },
    {
      kind: "document of an issue",
      opens: () => `/issues/${target}/artifacts/notes-md`,
      query: `[[${query}`,
      reads: "notes.md",
      row: within(/^notes\.md/, target),
      writes: `dispatch://${target}/artifact/notes-md`,
    },
    {
      holder: { blocks: "Document blocks", row: within(/^notes\.md/, target) },
      kind: "block of a document",
      opens: (address) => `/issues/${target}/artifacts/notes-md#b-${blockOf(address)}`,
      query: `[[${query}`,
      reads: "The cache notes.",
      row: /^The cache notes\./,
      shape: new RegExp(`^dispatch://${target}/artifact/notes-md#b-[\\w-]+$`),
    },
    {
      kind: "document of a project",
      opens: () => `/projects/${core}/documents/glossary-md`,
      query: `[[${query}`,
      reads: "glossary.md",
      row: within(/^glossary\.md/, core),
      writes: `dispatch://${core}/artifact/glossary-md`,
    },
    {
      holder: {
        blocks: "Documents",
        control: "Documents",
        row: new RegExp(`^${SESSION_TITLE}\\b`),
      },
      kind: "document of an agent",
      opens: () => `/agents/${sessionID}/artifacts/${AGENT_SLUG}`,
      // The session's name is two words and a door closes on a space, so one word finds it.
      query: `@${SESSION_TITLE.split(" ")[0]}`,
      reads: AGENT_FILE,
      row: /^shot\.png/,
      writes: agentDocument,
    },
    {
      kind: "ask",
      opens: (address) => `/issues/${target}/asks/${address.split("/ask/")[1]}`,
      query: `[[${query}`,
      reads: world.targets.Question.title,
      row: within(/^Should the cache be warmed first/, target),
      shape: new RegExp(`^dispatch://${target}/ask/[\\w-]+$`),
    },
    {
      holder: { blocks: "Ask parts", row: within(/^Should the cache be warmed first/, target) },
      kind: "part of an ask",
      opens: (address) =>
        `/issues/${target}/asks/${address.slice(`dispatch://${target}/ask/`.length).split("#b-")[0]}#b-${blockOf(address)}`,
      query: `[[${query}`,
      reads: world.targets.Question.title,
      row: /^Should the cache be warmed first\?/,
      shape: new RegExp(`^dispatch://${target}/ask/[\\w-]+#b-[\\w-]+$`),
    },
    {
      kind: "comment",
      opens: (address) => `/issues/${target}/comments/${address.split("/comment/")[1]}`,
      query: `[[${query}`,
      reads: world.targets.Comment.title,
      row: within(/^The warm cache halves start-up time/, target),
      shape: new RegExp(`^dispatch://${target}/comment/[\\w-]+$`),
    },
    {
      holder: {
        blocks: "Comment blocks",
        row: within(/^The warm cache halves start-up time/, target),
      },
      kind: "block of a comment",
      opens: (address) =>
        `/issues/${target}/comments/${address.slice(`dispatch://${target}/comment/`.length).split("#b-")[0]}#b-${blockOf(address)}`,
      query: `[[${query}`,
      reads: world.targets.Comment.title,
      row: /^The warm cache halves start-up time\./,
      shape: new RegExp(`^dispatch://${target}/comment/[\\w-]+#b-[\\w-]+$`),
    },
    {
      kind: "message of an issue",
      opens: (address) => `/issues/${target}/messages/${address.split("/message/")[1]}`,
      // One word: a door closes on the first space typed after it (`readTrigger`).
      query: `[[${query}`,
      reads: world.targets.Message.title,
      row: within(/^The rollout finished at noon/, target),
      shape: new RegExp(`^dispatch://${target}/message/[\\w-]+$`),
    },
    {
      holder: { blocks: "Message blocks", row: within(/^The rollout finished at noon/, target) },
      kind: "block of a message",
      opens: (address) =>
        `/issues/${target}/messages/${address.slice(`dispatch://${target}/message/`.length).split("#b-")[0]}#b-${blockOf(address)}`,
      query: `[[${query}`,
      reads: world.targets.Message.title,
      row: /^The rollout finished at noon\./,
      shape: new RegExp(`^dispatch://${target}/message/[\\w-]+#b-[\\w-]+$`),
    },
    {
      kind: "component",
      opens: () => `/projects/${core}/architecture?component=${COMPONENT_ID}`,
      query: `@${COMPONENT_TITLE}`,
      reads: COMPONENT_TITLE,
      row: new RegExp(`^${COMPONENT_TITLE}\\b`),
      writes: componentRef,
    },
    // A party is picked as its mention (R4): the mention sign and its name, which the stored
    // record reads as the sign and the party's current name.
    {
      kind: "agent session",
      opens: () => `/agents/${sessionID}/live`,
      // The session's name is two words and a door closes on a space, so one word finds it.
      query: `@${SESSION_TITLE.split(" ")[0]}`,
      reads: `@${SESSION_TITLE}`,
      row: new RegExp(`^${SESSION_TITLE}\\b`),
      names: sessionRef,
      writes: `[@${SESSION_TITLE}](${sessionRef})`,
    },
    {
      kind: "role",
      opens: () => `/agents?role=${ROLE_TOKEN}`,
      query: `@${ROLE_TOKEN}`,
      reads: `@${ROLE_TOKEN}`,
      row: new RegExp(`^${ROLE_TOKEN}\\b`),
      names: ROLE_REFERENCE,
      writes: `[@${ROLE_TOKEN}](${ROLE_REFERENCE})`,
    },
    {
      kind: "person",
      // Nothing shows a person yet, so their reference opens nothing.
      opens: () => "",
      query: "@dana",
      reads: `@${personLogin}`,
      row: new RegExp(`^${personLogin}$`),
      names: personRef,
      writes: `[@${personLogin}](${personRef})`,
    },
  ];

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const copied = await recordClipboard(page);
    await page.goto(`/issues/${world.host.key}/conversation`);
    const field = commentField(page);
    const list = refPicker(page);

    /** Types the kind's query at the end of the draft, opens its holder's parts where it has
     *  one, takes the row, and answers what the pick wrote. */
    const pick = async (kind: Kind): Promise<string> => {
      const before = await field.inputValue();
      await field.press("End");
      await field.pressSequentially(kind.query);
      await expect(list, `${kind.kind}: the picker did not open`).toBeVisible();
      if (kind.holder !== undefined) {
        await list
          .getByRole("option", { name: kind.holder.row })
          .first()
          .getByRole("button", { name: kind.holder.control ?? "Blocks" })
          .click();
      }
      // A part's row is read once the holder's parts answer, beside the holder's own row; only
      // the part's group holds it, so a pick taken before the read lands cannot take the holder.
      const rows =
        kind.holder === undefined ? list : list.getByRole("group", { name: kind.holder.blocks });
      await rows.getByRole("option", { name: kind.row }).first().click();
      await expect(list).toBeHidden();
      await expect(field, `${kind.kind}: the pick wrote nothing`).not.toHaveValue(
        `${before}${kind.query}`
      );
      const written = (await field.inputValue()).slice(before.length);
      await field.press("End");
      await field.press("Enter");
      return written;
    };

    const written = new Map<string, string>();
    for (const kind of kinds) {
      const address = await pick(kind);
      written.set(kind.kind, address);
      if (kind.writes !== undefined) {
        expect(address, `${kind.kind}: what the pick wrote`).toBe(kind.writes);
      } else {
        expect(address, `${kind.kind}: what the pick wrote`).toMatch(kind.shape ?? /^$/u);
      }
    }
    // Each pick ends its own line; the last one's line break goes, so the record ends with the
    // last address. One comment holds every pick, exactly as the picker wrote them.
    await field.press("End");
    await field.press("Backspace");
    const body = kinds.map((kind) => written.get(kind.kind) ?? "").join("\n");
    await expect(field).toHaveValue(body);
    const sent = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === `/api/v1/issues/${world.host.key}/comments`
    );
    await page
      .getByRole("form", { name: "Comment composer" })
      .getByRole("button", { exact: true, name: "Send" })
      .click();
    const answer = await sent;
    expect(answer.status()).toBe(201);
    const stored = (await answer.json()) as Result<"/api/v1/issues/{key}/comments", "post">;
    expect(stored.body, "the stored record is what the picker wrote").toBe(body);

    // Every reference in the record it stored, read where a person meets it.
    const card = await expandedComment(page, stored.id);
    for (const kind of kinds) {
      const address = written.get(kind.kind) ?? "";
      const named = kind.names ?? address;
      const link = card.locator(`a[data-dispatch-ref="${named}"]`);
      const path = kind.opens(address);
      if (path === "") {
        // A person reads as their login, with no link: that page is another issue's.
        await expect(card.getByText(kind.reads), kind.kind).toBeVisible();
        await expect(link, kind.kind).toHaveCount(0);
        continue;
      }
      await expect(link, `${kind.kind}: reads as its node's name`).toHaveText(kind.reads);
      await expect(link, `${kind.kind}: opens its node`).toHaveAttribute("href", path);
    }

    // The four pages the new kinds open, followed rather than read off an attribute.
    await card.locator(`a[data-dispatch-ref="${sessionRef}"]`).click();
    await expect(page).toHaveURL(`/agents/${sessionID}/live`);
    await expect(page.getByRole("heading", { name: SESSION_TITLE }).first()).toBeVisible();

    await page.goto(`/issues/${world.host.key}/conversation`);
    const file = await expandedComment(page, stored.id);
    await file.locator(`a[data-dispatch-ref="${agentDocument}"]`).click();
    await expect(page).toHaveURL(`/agents/${sessionID}/artifacts/${AGENT_SLUG}`);
    await expect(page).toHaveTitle(`${AGENT_FILE} · Dispatch`);

    await page.goto(`/issues/${world.host.key}/conversation`);
    const again = await expandedComment(page, stored.id);
    await again.locator(`a[data-dispatch-ref="${ROLE_REFERENCE}"]`).click();
    await expect(page).toHaveURL(`/agents?role=${ROLE_TOKEN}`);
    // The filter is a multi-select (R7): its trigger says exactly one option is ticked, as the
    // page's own row reads it (`agents-groups.e2e.ts`), and the ticked option is this role.
    const roleFilter = agentFilter(page, "role");
    await expect(roleFilter).toHaveText("Role · 1");
    await roleFilter.click();
    await expect(page.getByRole("option", { exact: true, name: ROLE_TOKEN })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await page.keyboard.press("Escape");

    await page.goto(`/issues/${world.host.key}/conversation`);
    const third = await expandedComment(page, stored.id);
    await third.locator(`a[data-dispatch-ref="${componentRef}"]`).click();
    await expect(page).toHaveURL(`/projects/${core}/architecture?component=${COMPONENT_ID}`);
    await expect(page.getByRole("heading", { name: COMPONENT_TITLE }).first()).toBeVisible();

    // The address a person is handed for an issue is the issue's, from whichever tab they are
    // standing on: the seven tab forms the server refuses are written by nothing a person can
    // press (`dispatch://KEY/verification` and its six siblings).
    await page.goto(`/issues/${world.host.key}/activity`);
    await page
      .getByRole("button", { name: `Copy issue key ${world.host.key}` })
      .click({ modifiers: ["ControlOrMeta"] });
    await expect.poll(copied).toEqual([`dispatch://${world.host.key}`]);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the one reference world, then the live conversation's picker opened nine times,
// each open its own read of the registry and the search route and eight of them a read of the
// holder's parts.
test("the live conversation offers an issue's message, and opens each holder's parts by its control and by ArrowRight", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const world = await seedReferenceWorld({ tagPickerSearch: true });
  const target = world.target.key;
  const query = `[[${referenceSearchWord(world.projects.core)}`;
  // The four holders of parts and the part each is read for, as the shared field reaches them
  // above. What a pick here writes is followed by the space assistant-ui leaves after it.
  const holders = [
    {
      blocks: "Document blocks",
      kind: "a document",
      part: /^The cache notes\./,
      query,
      row: within(/^notes\.md/, target),
      shape: new RegExp(`^dispatch://${target}/artifact/notes-md#b-[\\w-]+ $`),
    },
    {
      blocks: "Ask parts",
      kind: "an ask",
      part: /^Should the cache be warmed first\?/,
      query,
      row: within(/^Should the cache be warmed first/, target),
      shape: new RegExp(`^dispatch://${target}/ask/[\\w-]+#b-[\\w-]+ $`),
    },
    {
      blocks: "Comment blocks",
      kind: "a comment",
      part: /^The warm cache halves start-up time\./,
      query,
      row: within(/^The warm cache halves start-up time/, target),
      shape: new RegExp(`^dispatch://${target}/comment/[\\w-]+#b-[\\w-]+ $`),
    },
    {
      blocks: "Message blocks",
      kind: "a message",
      part: /^The rollout finished at noon\./,
      query,
      row: within(/^The rollout finished at noon/, target),
      shape: new RegExp(`^dispatch://${target}/message/[\\w-]+#b-[\\w-]+ $`),
    },
  ] as const;

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/agents/${referenceSessionId()}/live`);
    const field = page.getByTestId("agent-composer").locator("textarea");
    await expect(field).toBeVisible();
    const list = refPicker(page);
    /** Clears the message and opens a door on `query`, typed as a reader types it. */
    const open = async (query: string) => {
      await field.fill("");
      await field.pressSequentially(query);
      await expect(list, `${query}: the picker did not open`).toBeVisible();
    };

    // A message of an issue has its own address, as every node of the issue has: it is offered
    // here as in every other box, under its own heading, and a pick writes that address.
    await open(query);
    await liveRowsUnder(list, "Messages")
      .and(list.getByRole("option", { name: within(/^The rollout finished at noon/, target) }))
      .first()
      .click();
    await expect(field).toHaveValue(new RegExp(`^dispatch://${target}/message/[\\w-]+ $`));

    for (const holder of holders) {
      for (const way of ["its control", "ArrowRight"] as const) {
        const what = `the parts of ${holder.kind}, opened by ${way}`;
        await open(holder.query);
        const holderRow = list.getByRole("option", { name: holder.row }).first();
        await expect(holderRow, `${what}: the holder is offered`).toBeVisible();
        // The holder's own row can carry the part's words, so the part is taken from under its
        // own heading alone.
        const part = liveRowsUnder(list, holder.blocks)
          .and(list.getByRole("option", { name: holder.part }))
          .first();
        if (way === "its control") {
          await holderRow.getByRole("button", { name: "Blocks" }).click();
          await part.click();
        } else {
          await arrowTo(field, holderRow, `${what}: the holder`);
          await field.press("ArrowRight");
          await expect(part, `${what}: the parts are listed`).toBeVisible();
          await arrowTo(field, part, `${what}: the part`);
          await field.press("Enter");
        }
        await expect(field, `${what}: what the pick wrote`).toHaveValue(holder.shape);
      }
    }
  } finally {
    await alice.close();
  }
});

/** The comment's Conversation card, expanded: a collapsed card draws the inert one-line
 *  preview, whose references are spans rather than links. */
async function expandedComment(page: Page, id: string): Promise<Locator> {
  const card = page.locator(`[data-turn="comment:${id}"]`);
  // Which control the card shows is read once the page has settled on one of them: a page that
  // has just loaded holds neither yet, and a count read then would take the card for an open one.
  const collapse = card.getByRole("button", { name: "Collapse thread" });
  const expand = card.getByRole("button", { name: "Expand thread" });
  const state = await settledState("the comment's card", [
    ["open", collapse],
    ["shut", expand],
  ]);
  if (state === "shut") await expand.click();
  await expect(collapse).toBeVisible();
  return card;
}

/** The live composer's rows under one kind's heading. Its popover draws each heading as the
 *  element just before that kind's first row, with no group around the kind's rows, so a row is
 *  under the heading that is the nearest element before it that is not a row. */
function liveRowsUnder(list: Locator, heading: string): Locator {
  return list.locator(
    `xpath=.//*[@role="option"][preceding-sibling::*[not(@role="option")][1][normalize-space(.)="${heading}"]]`
  );
}

/** Moves the live composer's highlight down with the arrow key until it rests on `row`, as a
 *  reader does: the highlight is assistant-ui's own, the list it highlights in can be redrawn
 *  under it as a read lands, and only the keys move it. */
async function arrowTo(field: Locator, row: Locator, what: string): Promise<void> {
  const highlighted = highlightedRow(refPicker(field.page())).and(row);
  await expect
    .poll(
      async () => {
        if ((await highlighted.count()) > 0) return true;
        await field.press("ArrowDown");
        return false;
      },
      { intervals: [150], message: `${what}: the arrows never reached it`, timeout: 15_000 }
    )
    .toBe(true);
}
