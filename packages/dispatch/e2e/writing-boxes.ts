import {
  DISPATCH_WRITING_BOXES,
  type DispatchWriting,
  type DispatchWritingBox,
  type DispatchWritingBoxRow,
  dispatchWritingBox,
} from "@legion/contracts";
import { expect, type Locator, type Page } from "@playwright/test";

import { REF_PICKER_WORDS } from "../web/src/features/refs/ref-picker";
import { type FakeSession, setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  putArchitectureSource,
  syncArchitectureSource,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { openRefPicker, pickerRows, refPicker } from "./ref-picker";
import { sessionIds } from "./session-ids";

/**
 * The one world every writing box is driven over, and the checks each box is held to
 * (INFRA-1702's `c-field-everywhere`).
 *
 * The claim is about the box, not about the surface that draws it: whichever box a person
 * writes in, the same three doors open over the same rows in the same order, the same words
 * stand where the list has none, a pick lands at the caret and leaves the words around it, and
 * the line under the box is the hint its row of `DISPATCH_WRITING_BOXES` holds. One `test()`
 * per covered box, driven from that list, so a box the list gains with no row of its own, and a
 * row for a box the list no longer holds, are both visible.
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

/** The line under the box, which the box reads from its row of the list at the mount: the
 *  element the box points `aria-describedby` at. The two hosts that draw the picker in an
 *  editor of their own carry their hint on a test id instead, since their box is no `<label>`
 *  of the shared field's. */
export async function expectHint(
  page: Page,
  field: Locator,
  box: DispatchWritingBox,
  write: DispatchWriting
): Promise<void> {
  const { hint } = dispatchWritingBox(box, write);
  if (box === "document-body" || box === "agent-live-composer") {
    const testid = box === "document-body" ? "document-body-hint" : "agent-message-hint";
    await expect(page.getByTestId(testid), `${box} writing ${write}: its hint`).toHaveText(hint);
    return;
  }
  const described = await field.getAttribute("aria-describedby");
  expect(described, `${box}: the box names no line under it`).not.toBeNull();
  await expect(page.locator(`[id="${described}"]`), `${box} writing ${write}: its hint`).toHaveText(
    hint
  );
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

/** Everything the box itself is held to, which is every claim but the hint: the three doors,
 *  their rows and order, the words of the state with no rows, and the pick at the caret. */
export async function walkBox(field: Locator, world: BoxWorld): Promise<void> {
  await expectDoors(field);
  await expectCaretPick(field, world);
}
