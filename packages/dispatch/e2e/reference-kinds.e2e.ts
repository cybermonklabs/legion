import { agentFilter } from "./agents";
import { baseUrl, createProjectDocument, userHeaders } from "./api";
import { recordClipboard } from "./clipboard";
import { commentField, refPicker } from "./ref-picker";
import {
  COMPONENT_ID,
  COMPONENT_REFERENCE,
  COMPONENT_TITLE,
  PERSON_LOGIN,
  PERSON_REFERENCE,
  ROLE_REFERENCE,
  ROLE_TOKEN,
  SESSION_ID,
  SESSION_REFERENCE,
  SESSION_TITLE,
  seedReferenceWorld,
} from "./reference-surfaces";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * INFRA-1702's `c-picker-reaches-every-kind`: one picker over every kind Dispatch holds.
 *
 * For each of the thirteen kinds the spec names the picker inserts that kind's address, the
 * comment the reader sends holds exactly what was picked, and every reference in the stored
 * record reads as its node's name and opens that node's own page. A block of a document, a part
 * of an ask, a block of a comment and a block of a message are reached by opening the node that
 * holds them (the row's **Blocks**), never by a query alone: their text is their holder's, so a
 * query that finds the holder is what puts them within reach.
 *
 * Two of the thirteen are not the picker's to insert, and each says so where it is read:
 * a person, whom the picker writes as their login and whose reference opens nothing, since
 * dispatch://INFRA-1149 owns that page; and a document of an agent's conversation, which no box
 * picks - a session's file is written by the upload that makes it (`e2e/pictures.e2e.ts`) - and
 * which is read here as the stored record it is.
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
    new URL(`/api/v1/agents/${encodeURIComponent(sessionID)}/artifacts`, baseUrl),
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
  /** The row whose **Blocks** must be opened first, for a kind that is a part of another node,
   *  and the heading its blocks are listed under: the row is taken from that group alone, since
   *  the holder's own row can match the same words while its blocks are still being read. */
  readonly holder?: { readonly row: RegExp; readonly blocks: string };
  /** What the pick must write, where the whole address is known before the pick; a block's id is
   *  the server's, so those rows check the shape instead. */
  readonly writes?: string;
  readonly shape?: RegExp;
  /** The address the stored text names, where the pick wrote a worded link rather than the
   *  address itself: a party's words are its name, and the reference is inside the link. */
  readonly names?: string;
  /** What the stored reference reads as: the node's own name. */
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

// SLOW(>=2s): the one reference world - three projects, fifteen issues, the host's question
// blocks settling into asks, the architecture source sync and the live session - then fourteen
// picks, each its own read of the registry, the collaborators, the components and the search
// route, and the comment they are sent in.
test("the picker reaches every kind, and each pick reads as its node's name and opens it", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const world = await seedReferenceWorld();
  await createProjectDocument("CORE", {
    content: "# Glossary\n\nThe cache glossary.\n",
    name: "glossary.md",
  });
  await uploadAgentPicture(SESSION_ID);
  const target = world.target.key;
  const agentDocument = `dispatch://agent/${SESSION_ID}/artifact/${AGENT_SLUG}`;

  // The thirteen kinds, in the order one draft names them. A block, a part and the two item
  // blocks follow the node that holds them, because that is how they are reached.
  const kinds: readonly Kind[] = [
    {
      kind: "issue",
      opens: () => `/issues/${target}`,
      query: "#warm",
      reads: `${target} Warm the cache`,
      row: /^Warm the cache/,
      writes: `dispatch://${target}`,
    },
    {
      kind: "document of an issue",
      opens: () => `/issues/${target}/artifacts/notes-md`,
      query: "[[notes",
      reads: "notes.md",
      row: /^notes\.md/,
      writes: `dispatch://${target}/artifact/notes-md`,
    },
    {
      holder: { blocks: "Document blocks", row: /^notes\.md/ },
      kind: "block of a document",
      opens: (address) => `/issues/${target}/artifacts/notes-md#b-${blockOf(address)}`,
      query: "[[notes",
      reads: "The cache notes.",
      row: /^The cache notes\./,
      shape: new RegExp(`^dispatch://${target}/artifact/notes-md#b-[\\w-]+$`),
    },
    {
      kind: "document of a project",
      opens: () => "/projects/CORE/documents/glossary-md",
      query: "[[glossary",
      reads: "glossary.md",
      row: /^glossary\.md/,
      writes: "dispatch://CORE/artifact/glossary-md",
    },
    {
      kind: "ask",
      opens: (address) => `/issues/${target}/asks/${address.split("/ask/")[1]}`,
      query: "[[warmed",
      reads: "Should the cache be warmed first?",
      row: /^Should the cache be warmed first/,
      shape: new RegExp(`^dispatch://${target}/ask/[\\w-]+$`),
    },
    {
      holder: { blocks: "Ask parts", row: /^Should the cache be warmed first/ },
      kind: "part of an ask",
      opens: (address) =>
        `/issues/${target}/asks/${address.slice(`dispatch://${target}/ask/`.length).split("#b-")[0]}#b-${blockOf(address)}`,
      query: "[[warmed",
      reads: "Should the cache be warmed first?",
      row: /^Should the cache be warmed first\?/,
      shape: new RegExp(`^dispatch://${target}/ask/[\\w-]+#b-[\\w-]+$`),
    },
    {
      kind: "comment",
      opens: (address) => `/issues/${target}/comments/${address.split("/comment/")[1]}`,
      query: "[[halves",
      reads: "The warm cache halves start-up time.",
      row: /^The warm cache halves start-up time/,
      shape: new RegExp(`^dispatch://${target}/comment/[\\w-]+$`),
    },
    {
      holder: { blocks: "Comment blocks", row: /^The warm cache halves start-up time/ },
      kind: "block of a comment",
      opens: (address) =>
        `/issues/${target}/comments/${address.slice(`dispatch://${target}/comment/`.length).split("#b-")[0]}#b-${blockOf(address)}`,
      query: "[[halves",
      reads: "The warm cache halves start-up time.",
      row: /^The warm cache halves start-up time\./,
      shape: new RegExp(`^dispatch://${target}/comment/[\\w-]+#b-[\\w-]+$`),
    },
    {
      kind: "message of an issue",
      opens: (address) => `/issues/${target}/messages/${address.split("/message/")[1]}`,
      // One word: a door closes on the first space typed after it (`readTrigger`).
      query: "[[noon",
      reads: "The rollout finished at noon.",
      row: /^The rollout finished at noon/,
      shape: new RegExp(`^dispatch://${target}/message/[\\w-]+$`),
    },
    {
      holder: { blocks: "Message blocks", row: /^The rollout finished at noon/ },
      kind: "block of a message",
      opens: (address) =>
        `/issues/${target}/messages/${address.slice(`dispatch://${target}/message/`.length).split("#b-")[0]}#b-${blockOf(address)}`,
      query: "[[noon",
      reads: "The rollout finished at noon.",
      row: /^The rollout finished at noon\./,
      shape: new RegExp(`^dispatch://${target}/message/[\\w-]+#b-[\\w-]+$`),
    },
    {
      kind: "component",
      opens: () => `/projects/CORE/architecture?component=${COMPONENT_ID}`,
      query: `@${COMPONENT_TITLE}`,
      reads: COMPONENT_TITLE,
      row: new RegExp(`^${COMPONENT_TITLE}\\b`),
      writes: COMPONENT_REFERENCE,
    },
    // A party is picked as the link that names it (R4), so its words are its name already.
    {
      kind: "agent session",
      opens: () => `/agents/${SESSION_ID}/live`,
      // The session's name is two words and a door closes on a space, so one word finds it.
      query: `@${SESSION_TITLE.split(" ")[0]}`,
      reads: SESSION_TITLE,
      row: new RegExp(`^${SESSION_TITLE}\\b`),
      names: SESSION_REFERENCE,
      writes: `[${SESSION_TITLE}](${SESSION_REFERENCE})`,
    },
    {
      kind: "role",
      opens: () => `/agents?role=${ROLE_TOKEN}`,
      query: `@${ROLE_TOKEN}`,
      reads: ROLE_TOKEN,
      row: new RegExp(`^${ROLE_TOKEN}\\b`),
      names: ROLE_REFERENCE,
      writes: `[${ROLE_TOKEN}](${ROLE_REFERENCE})`,
    },
    {
      kind: "person",
      // Nothing shows a person yet (dispatch://INFRA-1149), so their reference opens nothing.
      opens: () => "",
      query: "@dana",
      reads: PERSON_LOGIN,
      row: new RegExp(`^${PERSON_LOGIN}$`),
      names: PERSON_REFERENCE,
      writes: `[${PERSON_LOGIN}](${PERSON_REFERENCE})`,
    },
  ];

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const copied = await recordClipboard(page);
    await page.goto(`/issues/${world.host.key}/conversation`);
    const field = commentField(page);
    const list = refPicker(page);

    /** Types the kind's query at the end of the draft, opens its holder's blocks where it has
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
          .getByRole("button", { name: "Blocks" })
          .click();
      }
      // A part's row is read once the holder's blocks answer, beside the holder's own row; only
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
    // The fourteenth form, which no box picks: a file of an agent's conversation, written by the
    // upload that makes it. It is typed here as that upload writes it, and read below with the
    // rest, because a document of an agent is a document all the same.
    await field.press("End");
    await field.pressSequentially(agentDocument);

    // The picker offers no row for it, whatever is typed: an artifact of an agent's conversation
    // is no row of the search index (`a.kind = 'doc'`), and no box writes about one.
    const before = await field.inputValue();
    await field.pressSequentially(" [[shot");
    await expect(list).toBeVisible();
    await expect(list.getByRole("option", { name: /shot\.png/ })).toHaveCount(0);
    await field.press("Escape");
    await field.fill(before);

    // One comment holds every pick, exactly as the picker wrote them.
    const body = [...kinds.map((kind) => written.get(kind.kind) ?? ""), agentDocument].join("\n");
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
    const stored = (await answer.json()) as { body: string; id: string };
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
    // The agent's own document reads as its name and opens its page in that same record.
    const file = card.locator(`a[data-dispatch-ref="${agentDocument}"]`);
    await expect(file).toHaveText(AGENT_FILE);
    await expect(file).toHaveAttribute("href", `/agents/${SESSION_ID}/artifacts/${AGENT_SLUG}`);

    // The three pages the new kinds open, followed rather than read off an attribute.
    await card.locator(`a[data-dispatch-ref="${SESSION_REFERENCE}"]`).click();
    await expect(page).toHaveURL(`/agents/${SESSION_ID}/live`);
    await expect(page.getByRole("heading", { name: SESSION_TITLE }).first()).toBeVisible();

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
    await third.locator(`a[data-dispatch-ref="${COMPONENT_REFERENCE}"]`).click();
    await expect(page).toHaveURL(`/projects/CORE/architecture?component=${COMPONENT_ID}`);
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

/** The comment's Conversation card, expanded: a collapsed card draws the inert one-line
 *  preview, whose references are spans rather than links. */
async function expandedComment(page: Page, id: string): Promise<Locator> {
  const card = page.locator(`[data-turn="comment:${id}"]`);
  // The card first: a page that has just loaded holds neither control yet, and a count read
  // before it draws would take the card for one that is already open.
  await expect(card).toBeVisible();
  const collapse = card.getByRole("button", { name: "Collapse thread" });
  if ((await collapse.count()) === 0) {
    await card.getByRole("button", { name: "Expand thread" }).click();
  }
  await expect(collapse).toBeVisible();
  return card;
}
