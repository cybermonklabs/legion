import {
  DISPATCH_WRITING_BOXES,
  type DispatchWriting,
  type DispatchWritingBox,
  type DispatchWritingBoxRow,
  dispatchWritingBox,
} from "@legion/contracts";
import { expect, type Locator, type Page } from "@playwright/test";

import { BOX_PICKER_LINES, CONTROL_LABELS } from "../web/src/features/issue-page/page-words";
import { REF_PICKER_WORDS } from "../web/src/features/refs/ref-picker";
import { type FakeSession, getSentMessages, setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  getComment,
  putArchitectureSource,
  syncArchitectureSource,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { openRefPicker, pickerRows, refPicker } from "./ref-picker";
import { sessionIds } from "./session-ids";

/**
 * The one world every writing box is driven over, and the checks each box is held to
 * (the check that every box has the picker).
 *
 * The claim is about the box, not about the surface that draws it: whichever box a person
 * writes in, the same three doors open over the same rows in the same order, the same words
 * stand where the list has none, a pick lands at the caret and leaves the words around it, and
 * the line under the box says what a pick does on its write (`hintWords`). One `test()` per
 * covered box, driven from `DISPATCH_WRITING_BOXES`, so a box the list gains with no row of its
 * own, and a row for a box the list no longer holds, are both visible.
 *
 * A box whose write delivers is held to its delivery as well (the check that delivery is
 * unchanged): the party picked in it reaches the write, and the fake Envoy listener
 * takes one frame for that party, carrying the body the box held, once the box's own send control
 * is pressed (`expectDeliveryClaim`). The driver of each box hands that control in.
 *
 * The rows the picker finds are the world's own, and the query that finds them (`QUERY`) is a
 * word no party, person or component holds: so every box lists the same three rows whatever it
 * knows about an issue, a project or the kinds it offers, and the one thing a box's own
 * knowledge decides - whether the people it lists are the issue's - is read on its own query
 * (`expectIssueRows`).
 */

/** The word the world's three findable nodes hold, and nothing else does. */
export const QUERY = "zephyr";

/** A query nothing answers, which is how each box says it has no rows. */
export const NO_MATCH = "zzqqxx";

/** The person who has signed in and is on no issue: the picker lists them in a box that writes
 *  on no issue, and nowhere else. `bob` is one of the two logins the database reset re-creates. */
export const OFF_ISSUE_PERSON = "bob";

/** The paragraph the margin's boxes are anchored to, and the line a document pick is typed on. */
export const ANCHOR = "The anchor paragraph sits here.";

/** The session the picker lists, which also writes on the host so its Agents row is in the open
 *  list rather than behind the quiet fold. It holds no role, so the picker draws no Roles row
 *  and the three rows below are the whole list. */
export const SESSION: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/srv/zephyr",
  machine_id: "box-zephyr",
  roles: [],
  session_id: sessionIds.planner,
  title: "Zephyr bot",
};

/** What a pick of `SESSION` writes into a box: the link that names the session. */
export const SESSION_MENTION = `[@${SESSION.title}](dispatch://agent/${SESSION.session_id})`;

/** The agent writes of this world, which are never the reader's own. */
const AGENT = { actor: { id: SESSION.session_id, kind: "session" as const }, as: "agent" as const };

/** The person the issue's own people hold: they write on the host, which is what puts them
 *  among its people. A stored login is an email (`model.CanonicalLogin`). */
const ISSUE_PERSON = "dana@example.test";

export interface BoxWorld {
  /** CORE-1, where every box that writes on an issue writes. */
  readonly host: string;
  /** The host's spec, whose anchor paragraph the margin's boxes hang from. */
  readonly spec: string;
  /** CORE-2, the issue the picker finds, which owns the document it finds. */
  readonly target: string;
  /** The reader's own comment thread, which the session has answered: the Inbox's Reply row,
   *  and the card the Conversation draws a reply box under. */
  readonly comment: string;
  /** The reader's own message thread on the host, answered the same way. */
  readonly message: string;
  /** The message addressed to the session, which its Agents row shows and answers. */
  readonly targeted: string;
  /** The comment anchored to the spec's anchor paragraph, which is the margin's own thread.
   *  The reader wrote it, so the margin offers them its Edit. */
  readonly anchored: string;
  /** The session's open ask on the host, whose card the Inbox draws and whose thread the
   *  Conversation's ask turn opens. */
  readonly ask: string;
}

/**
 * The world: the host every box writes on, the three nodes the one query finds (an issue, its
 * document and the live session), the person the host's people hold, and the comment and
 * message the reply boxes answer.
 */
export async function seedWritingBoxes(): Promise<BoxWorld> {
  await createProject({ key: "CORE", name: "Core" });
  const host = await createIssue({
    project: "CORE",
    spec: `${ANCHOR}\n`,
    title: "Where a person writes",
  });
  const target = await createIssue({ project: "CORE", title: "Zephyr rollout" });
  const document = await createIssueArtifact(
    target.key,
    { content: "# Zephyr\n\nThe zephyr plan.\n", name: "zephyr.md" },
    AGENT
  );
  expect(document.artifact.slug).toBe("zephyr-md");
  await setLiveSessions([SESSION]);
  // The session's own post on the host: it puts its Agents row in the open list, and it names
  // nothing the query finds.
  await createComment(host.key, { body: "Looking at it." }, AGENT);
  // The person of the host, who is on no other issue.
  await createComment(host.key, { body: "A person writes here." }, { login: ISSUE_PERSON });
  // The reader's own threads, which the session has answered since: that is what the Inbox
  // lists as a Reply row, and the card the Conversation draws a reply box under.
  const comment = await createComment(host.key, { body: "A thread to answer." });
  await createComment(host.key, { body: "The session answers.", reply_to: comment.id }, AGENT);
  const message = await createMessage(host.key, { body: "A message to answer." });
  await createMessage(
    host.key,
    { body: "The session answers it.", in_reply_to: message.id },
    AGENT
  );
  // The message addressed to the session, which its Agents row shows and offers a Reply under.
  const targeted = await createMessage(host.key, {
    body: "Can this ship?",
    delivery: "btw",
    target: `session:${SESSION.session_id}`,
  });
  // The margin's own thread, anchored to the spec's one paragraph and written by the reader,
  // so the card offers them both Reply and Edit.
  const anchored = await createComment(host.key, {
    anchor: { artifact: "spec", quote: ANCHOR },
    body: "A margin thread to answer.",
  });
  expect(anchored.anchor?.quote).toBe(ANCHOR);
  // The session's open ask: the Inbox draws its card, and the Conversation its thread.
  const ask = await createAsk(
    host.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Which way does it go?" },
    AGENT
  );
  return {
    anchored: anchored.id,
    ask: ask.id,
    comment: comment.id,
    host: host.key,
    message: message.id,
    spec: host.primary_artifact_id,
    target: target.key,
    targeted: targeted.id,
  };
}

/** CORE's architecture, which the Not architectural box's own page needs: the fake GitHub
 *  serves the model and the source sync reads it in, as `e2e/architecture.e2e.ts` makes one.
 *  Its one component holds no word the picker's query finds. */
export async function seedArchitecture(): Promise<void> {
  const repo = "legion/writing-boxes-arch";
  await seedFakeGithub({
    [repo]: {
      contents: "read",
      files: { "web.md": "---\ntitle: Web\n---\nThe **SPA**.\n" },
      installation_id: 101,
    },
  });
  await putArchitectureSource("CORE", { branch: "main", repo });
  const source = await syncArchitectureSource("CORE", repo);
  expect(source.last_error).toBeNull();
}

/** The rows every box lists for `QUERY`, in each door's own order: the kinds a door lifts come
 *  first, the rest in the base order (`kindOrder`). No party, person or component holds that
 *  word, so these three are every box's whole list. */
export const DOOR_ROWS: Record<"[[" | "@" | "#", readonly string[]> = {
  "[[": ["Documents: zephyr.md", "Issues: Zephyr rollout", `Agents: ${SESSION.title}`],
  "@": [`Agents: ${SESSION.title}`, "Documents: zephyr.md", "Issues: Zephyr rollout"],
  "#": ["Issues: Zephyr rollout", "Documents: zephyr.md", `Agents: ${SESSION.title}`],
};

/** The covered rows of one box, in the list's own order. */
export function rowsOfBox(box: DispatchWritingBox): DispatchWritingBoxRow[] {
  return DISPATCH_WRITING_BOXES.filter((row) => row.box === box && row.covered);
}

/** Every covered box id the list holds, which is the set this suite must have one `test()` for. */
export function coveredBoxes(): DispatchWritingBox[] {
  return [...new Set(DISPATCH_WRITING_BOXES.filter((row) => row.covered).map((row) => row.box))];
}

/** The doors line every hint opens with. */
const HINT_DOORS = "Type @ for an agent, a role or a person, # for an issue, [[ for anything else.";

/** What a box whose write tells nobody it names says of a pick, before it says what does tell:
 *  every such write but a suggestion's, whose replacement shows no link (`hintWords`). */
const HINT_QUIET =
  "A picked agent or role gets a link, and a picked person shows as a name without one. Nobody you pick is told.";

/** What does tell, by the write that tells nobody it names: where the write itself goes, or the
 *  comment that would tell a picked agent or role, or that nothing would. */
const HINT_TOLD: Partial<Record<DispatchWriting, string>> = {
  "agent-message": "The message goes to this agent.",
  "approval-reason": "An open approval request sends the reason to whoever asked.",
  "architecture-reason": "A comment on the issue tells a picked agent or role.",
  ask: "The ask goes to the assignee's Inbox.",
  "ask-decline": "The note goes to whoever asked.",
  broadcast: "The message goes to each agent you selected.",
  "comment-edit": "A new comment, not an edit, tells a picked agent or role.",
  "document-text": "A comment on the document tells a picked agent or role.",
  "issue-spec": "A comment on the new issue tells a picked agent or role.",
  "message-reply": "Your reply goes to whoever the thread is with.",
  "priority-description": "Nothing on a priority tells anyone.",
};

/**
 * The words under a box, written out here rather than read from the list: the box reads its
 * line from its row of `DISPATCH_WRITING_BOXES`, so a row that read its words from there too
 * would pass whatever they said. These are what the reader of the box is owed: a box that
 * delivers says a picked agent or role is told, and a picked person is not
 * told and gets no link; a box that does not says a pick links and tells nobody, and says what
 * does tell, in words of its write's own; and the ask card, whose one box makes both writes,
 * says both. A suggestion's replacement promises no link: until someone accepts the suggestion,
 * its card shows the replacement as the characters written (the reading list's
 * `margin-suggestion-diff`), and only the Reason box beside it tells anyone. Which kind a row is
 * comes from its `delivers`, which `expectDeliveryClaim` drives.
 */
function hintWords(box: DispatchWritingBox, write: DispatchWriting): string {
  if (box === "ask-answer") {
    return `${HINT_DOORS} An answer goes to whoever asked and to each agent or role you pick. A picked role that is not running gets the answer if a session claims the role within three hours. Ask back tells a picked agent or role while a session holds it, so a role that is not running gets a link and is not told. A picked person shows as a name without a link and is not told.`;
  }
  if (dispatchWritingBox(box, write).delivers) {
    return `${HINT_DOORS} A picked agent or role is told while a session holds it. A role that is not running gets a link and is not told. A picked person shows as a name without a link and is not told.`;
  }
  if (write === "suggestion") {
    return `${HINT_DOORS} Until someone accepts the suggestion, the replacement shows the characters you write. Nobody you pick is told. A pick in the Reason box tells an agent or a role.`;
  }
  const told = HINT_TOLD[write];
  if (told === undefined) {
    throw new Error(
      `${box} writing ${write} tells nobody it names, and no words here say what does`
    );
  }
  return `${HINT_DOORS} ${HINT_QUIET} ${told}`;
}

/** The hosts that draw the picker in an editor of their own, by the test id of the line they
 *  draw under it (`DocumentRefPicker`, which a document's body and a priority's description
 *  mount, and the agent live conversation's composer). */
const EDITOR_HINTS: Partial<Record<DispatchWritingBox, string>> = {
  "agent-live-composer": "agent-message-hint",
  "document-body": "document-body-hint",
  "priority-description": "priority-description-hint",
};

/** The words a box says in place of the list's, where its page holds it to words of its own
 *  (decision 527): the issue page's box describes its field by what its keys do
 *  (`description`), reads the list of who its comment is delivered to by the page's name for it
 *  and each party in it by its name, never its address (`targets`), and says the page's own line
 *  where the picker has no rows (`empty`). Each is read from the page's lists. Every other box
 *  says the list's hint, `session:<id>` and `REF_PICKER_WORDS.empty`. */
interface OwnWords {
  readonly description: string;
  readonly empty: string;
  readonly targets: string;
}

const OWN_WORDS: Partial<Record<DispatchWritingBox, OwnWords>> = {
  "issue-page-composer": {
    description: CONTROL_LABELS.composer_keys,
    empty: BOX_PICKER_LINES.empty,
    targets: CONTROL_LABELS.composer_mentioned,
  },
};

/** The words of the box `field` is in, where its page holds it to words of its own
 *  (`OWN_WORDS`): a field inside the element that box's rows reach it by. */
async function ownWordsOf(field: Locator): Promise<OwnWords | undefined> {
  for (const [box, own] of Object.entries(OWN_WORDS)) {
    const reach = rowsOfBox(box as DispatchWritingBox)[0]?.reach;
    if (own === undefined || reach === undefined) continue;
    // Which box holds the field was fixed when the field was drawn, and no later frame changes it.
    if (await field.evaluate((node, selector) => node.closest(selector) !== null, reach)) {
      return own;
    }
  }
  return undefined;
}

/** The line under the box, which the box reads from its row of the list at the mount, held to
 *  the words that row must say (`hintWords`): the element the box points `aria-describedby` at.
 *  The hosts that draw the picker in an editor of their own carry their hint on a test id
 *  instead (`EDITOR_HINTS`), since their box is no `<label>` of the shared field's. */
export async function expectHint(
  page: Page,
  field: Locator,
  box: DispatchWritingBox,
  write: DispatchWriting
): Promise<void> {
  // A box held to words of its own describes its field in them, in place of its row's hint.
  const own = OWN_WORDS[box];
  if (own !== undefined) {
    const ownIds = ((await field.getAttribute("aria-describedby")) ?? "")
      .split(/\s+/)
      .filter((id) => id !== "");
    expect(ownIds, `${box}: the box names no line under it`).not.toEqual([]);
    const ownLines = page.locator(ownIds.map((id) => `[id="${id}"]`).join(","));
    await expect(
      ownLines.filter({ hasText: own.description }),
      `${box} writing ${write}: its description`
    ).toHaveText([own.description]);
    return;
  }
  const hint = hintWords(box, write);
  const testid = EDITOR_HINTS[box];
  if (testid !== undefined) {
    await expect(page.getByTestId(testid), `${box} writing ${write}: its hint`).toHaveText(hint);
    return;
  }
  // `aria-describedby` is a list: the field's own hint, and whatever else the host names beside
  // it (the decline note's count of the characters left). The hint is the one of them holding
  // the doors line, so the row watches every id at once, narrows to the ones holding it
  // (`hasText`, which matches a substring) and then holds that one line to the whole of the
  // words. Both assertions retry, since a hint may render a tick after the box.
  const described = await field.getAttribute("aria-describedby");
  expect(described, `${box}: the box names no line under it`).not.toBeNull();
  const ids = (described ?? "").split(/\s+/).filter((id) => id !== "");
  const lines = page.locator(ids.map((id) => `[id="${id}"]`).join(","));
  await expect(
    lines.filter({ hasText: HINT_DOORS }),
    `${box} writing ${write}: its hint`
  ).toHaveText([hint]);
}

/** The three doors over the same rows, each in its own order, and the words the box shows when
 *  the query answers nothing. */
async function expectDoors(field: Locator): Promise<void> {
  await keyboardOnly(field);
  for (const door of ["[[", "@", "#"] as const) {
    await field.fill("");
    const list = await openRefPicker(field, `${door}${QUERY}`);
    await expect
      .poll(() => pickerRows(list), { message: `the ${door} door's rows` })
      .toEqual(DOOR_ROWS[door]);
  }
  await field.fill("");
  const empty = await openRefPicker(field, `[[${NO_MATCH}`);
  const own = await ownWordsOf(field);
  if (own !== undefined) {
    await expect(empty.getByTestId("ref-picker-empty")).toHaveText(own.empty);
    await field.fill("");
    return;
  }
  await expect(empty.getByTestId("ref-picker-empty")).toHaveText(REF_PICKER_WORDS.empty);
  await field.fill("");
}

/** Whether this box lists the people of the issue it writes on, or everyone who has signed in:
 *  a person on no issue is listed by the second kind of box alone. */
export async function expectIssueRows(field: Locator, issueRows: boolean): Promise<void> {
  await field.fill("");
  const list = await openRefPicker(field, `@${OFF_ISSUE_PERSON}`);
  const person = list.getByRole("option", { name: new RegExp(`^${OFF_ISSUE_PERSON}$`) });
  if (issueRows) {
    const own = await ownWordsOf(field);
    if (own !== undefined) {
      await expect(list.getByTestId("ref-picker-empty")).toHaveText(own.empty);
      await expect(person, "a box that writes on an issue lists that issue's people").toHaveCount(
        0
      );
      await field.fill("");
      return;
    }
    await expect(list.getByTestId("ref-picker-empty")).toHaveText(REF_PICKER_WORDS.empty);
    await expect(person, "a box that writes on an issue lists that issue's people").toHaveCount(0);
  } else {
    await expect(person, "a box that writes on no issue lists everyone signed in").toBeVisible();
  }
  await field.fill("");
}

/** Takes the pointer off the list before a keyboard pick. The list is drawn under the caret,
 *  so a pointer left where the reader last clicked can come to rest over a row, and a row the
 *  pointer is over is the highlighted one (`RefPickerList`'s `onMouseEnter`) - which is a
 *  reader's own doing and no part of what these rows are about. */
export async function keyboardOnly(field: Locator): Promise<void> {
  await field.page().mouse.move(0, 0);
}

/** A pick lands where the reader is typing: the trigger it replaces and nothing else, with the
 *  words after the caret left where they were. Enter takes the row, in every box. */
async function expectCaretPick(field: Locator, world: BoxWorld): Promise<void> {
  await keyboardOnly(field);
  await field.fill(" then deploy");
  await field.press("Home");
  await field.pressSequentially(`Ship after #${QUERY}`);
  const list = refPicker(field.page());
  await expect.poll(() => pickerRows(list)).toEqual(DOOR_ROWS["#"]);
  await field.press("Enter");
  await expect(field).toHaveValue(`Ship after dispatch://${world.target} then deploy`);
  await field.fill("");
}

/** The words a delivering box's draft holds after the party picked in it, so the frame its send
 *  makes is read against the whole body the box held, not the mention alone. */
const DELIVERED_WORDS = " can you take a look?";

/** Presses a box's own send control: the button the box draws, which its driver names. */
export type SendBox = () => Promise<void>;

/** The mode a plain write delivers in to the world's session: Send, since the session
 *  advertises it (`delivery.ts`'s `plainDelivery`, and the server's own default for a write that
 *  names none, `comment_create.go`). */
const PLAIN_MODE = "steer";

/** What the fake listener took, read as the three facts a delivery claim is about: who it went
 *  to, the body it carried, and the mode its frame names (`delivery.mode` of the payload the
 *  comment's delivery wrote, `comment_delivery.go`). */
async function framesSince(
  before: number
): Promise<{ message: unknown; mode: unknown; target_session: unknown }[]> {
  return (await getSentMessages()).slice(before).map((sent) => {
    const payload = typeof sent.payload === "string" ? JSON.parse(sent.payload) : undefined;
    return {
      message: sent.message,
      mode: payload?.delivery?.mode,
      target_session: sent.target_session,
    };
  });
}

/** The rows of the list the running test's driver has held: a row whose write delivers once its
 *  send made its one frame (`expectSend`), every other row once its box was seen to tell nobody
 *  (`expectDeliveryClaim`) or, for a box that draws no party list, once its driver read what its
 *  write does (`heldRow`). Each box's test compares them with that box's covered rows of the list
 *  once its driver is done, so a row the list gains that no driver holds fails, and so does a
 *  delivering row whose hint alone a driver checks. The set is the worker's, so each test starts
 *  it afresh (`startHolding`). */
const held = new Set<string>();

/** Empties the record of held rows, for a test about to drive a box. */
export function startHolding(): void {
  held.clear();
}

/** The rows held since `startHolding`, as `<box>/<write>`, sorted. */
export function heldRows(): string[] {
  return [...held].sort();
}

/** Holds a row of a box that draws no party list, whose driver reads what its write does itself:
 *  the document body, whose pick is read in the document's stored text. A row whose write
 *  delivers is held only by the frame its send makes, so this refuses one. */
export function heldRow(box: DispatchWritingBox, write: DispatchWriting): void {
  if (dispatchWritingBox(box, write).delivers) {
    throw new Error(`${box} writing ${write} delivers, so only the frame its send makes holds it`);
  }
  held.add(`${box}/${write}`);
}

/** The comment a frame of the fake listener's delivers, from the `delivery.comment_id` of its
 *  payload (`comment_delivery.go`). */
function commentOfFrame(sent: Record<string, unknown> | undefined): string {
  const payload = typeof sent?.payload === "string" ? JSON.parse(sent.payload) : undefined;
  const id = payload?.delivery?.comment_id;
  if (typeof id !== "string") throw new Error(`a frame names no comment: ${JSON.stringify(sent)}`);
  return id;
}

/** Sends a delivering box's write with `send`, its own control, and holds the send to exactly one
 *  frame: for the session picked, carrying `body`, in `PLAIN_MODE`. The first poll waits for that
 *  frame. The server sends after it has answered the write, so the comment the frame names is
 *  then read until its one delivery has settled as `sent` (`CommentDelivery.State`), and only
 *  then are the frames read once more: a duplicate that lands after the first frame fails too,
 *  which a poll that passes the moment one frame is there would let through. */
export async function expectSend(
  box: DispatchWritingBox,
  write: DispatchWriting,
  body: string,
  send: SendBox
): Promise<void> {
  const one = [{ message: body, mode: PLAIN_MODE, target_session: SESSION.session_id }];
  const message = `${box} writing ${write}: the frames its send made`;
  const before = (await getSentMessages()).length;
  await send();
  await expect.poll(() => framesSince(before), { message }).toEqual(one);
  const comment = commentOfFrame((await getSentMessages())[before]);
  const settled = async () =>
    ((await getComment(comment)).comment.deliveries ?? []).map((sent) => sent.state);
  await expect
    .poll(settled, {
      message: `${box} writing ${write}: how comment ${comment}'s delivery settled`,
    })
    .toEqual(["sent"]);
  expect(await framesSince(before), `${message}, read again once its delivery settled`).toEqual(
    one
  );
  held.add(`${box}/${write}`);
}

/** What a box does with a picked party, which is its row's own `delivers`. A box that does not
 *  deliver lists nobody under the field. A box that delivers lists the party there, and its own
 *  send then reaches that party: the fake Envoy listener takes one frame, for the session picked,
 *  carrying the body the box held (a comment's frame carries its body as `message`), in the mode
 *  the composer's own rule gives for the session's capabilities (`PLAIN_MODE`). The claim is
 *  driven rather than read, because a row that promises a delivery no reader of the write makes
 *  is exactly what misleads the writer: the suggestion Replacement box said a picked party was
 *  told while neither reader of a comment reads `suggestion.replace_with`. So a delivering row
 *  needs `send`, and a delivering row the list gains with a driver that hands none fails here
 *  rather than passing unsent. A delivering box's send may close it, so the box is left as the
 *  send leaves it, and its driver goes on from there. A row is held once its claim is checked
 *  (`heldRows`). */
export async function expectDeliveryClaim(
  field: Locator,
  box: DispatchWritingBox,
  write: DispatchWriting,
  send?: SendBox
): Promise<void> {
  const sends = dispatchWritingBox(box, write).delivers ? send : null;
  if (sends === undefined) {
    throw new Error(`${box} writing ${write} delivers, and its driver passes no send to prove it`);
  }
  const page = field.page();
  await keyboardOnly(field);
  await field.fill("");
  await field.pressSequentially(`@${QUERY}`);
  const list = refPicker(page);
  await expect.poll(() => pickerRows(list)).toEqual(DOOR_ROWS["@"]);
  await field.press("Enter");
  // A box held to words of its own lists who its write is delivered to under its own name for
  // the list, and each party by its name, never its address.
  const own = OWN_WORDS[box];
  if (own !== undefined) {
    const named = page.getByRole("list", { name: own.targets });
    if (sends === null) {
      await expect(named).toHaveCount(0);
      await field.fill("");
      held.add(`${box}/${write}`);
      return;
    }
    await expect(named).toHaveText(SESSION.title);
    await field.press("End");
    await field.pressSequentially(DELIVERED_WORDS);
    const ownBody = `${SESSION_MENTION}${DELIVERED_WORDS}`;
    await expect(field).toHaveValue(ownBody);
    await expectSend(box, write, ownBody, sends);
    return;
  }
  const targets = page.getByRole("list", { name: "Mention targets" });
  if (sends === null) {
    await expect(targets).toHaveCount(0);
    await field.fill("");
    held.add(`${box}/${write}`);
    return;
  }
  await expect(targets).toHaveText(`session:${SESSION.session_id}`);
  await field.press("End");
  await field.pressSequentially(DELIVERED_WORDS);
  const body = `${SESSION_MENTION}${DELIVERED_WORDS}`;
  await expect(field).toHaveValue(body);
  await expectSend(box, write, body, sends);
}

/** Everything the box itself is held to, which is every claim but the hint: the three doors,
 *  their rows and order, the words of the state with no rows, and the pick at the caret. */
export async function walkBox(field: Locator, world: BoxWorld): Promise<void> {
  await expectDoors(field);
  await expectCaretPick(field, world);
}
