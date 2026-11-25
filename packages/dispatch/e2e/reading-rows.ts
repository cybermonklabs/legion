import {
  DISPATCH_READING_FORMS,
  DISPATCH_READING_SURFACES,
  type DispatchReadingForm,
} from "@legion/contracts";

import type { Issue } from "../web/src/api/types";
import { type FakeSession, setLiveSessions } from "./agents";
import {
  createComment,
  createIssue,
  createProject,
  dashboardOrigin,
  getDocumentQuestions,
  type RowKey,
} from "./api";
import { keyedSearchWord } from "./search";
import { expect, type Locator, rowKey, rowSessionId, test } from "./test";
import type { SignInLogin } from "./users";

/**
 * The reading list's walk: each rendered row of
 * `DISPATCH_READING_SURFACES` has a driver, a row of a `reading-*.e2e.ts` spec titled with the
 * row's own id followed by a colon, that reaches the surface the way the row says and reads
 * there a stored text holding each form a text names a node by (`DISPATCH_READING_FORMS`): an
 * issue key written bare, a `dispatch://` address, a URL of this dashboard, a question block's
 * `#id`, an address whose item id is written short, a picked party, and a link whose words its
 * author wrote (`readingText`). Each must draw through the one renderer: the first six as their
 * node's current name, the worded link as its author's words, each opening its node where the
 * host allows a link, and none left as the characters written (R6, R8).
 *
 * A row says in the list which forms its surface cannot carry, and why (`cannotCarry`): a
 * surface whose text is no record, or is a record that belongs to no document, carries no
 * question id, since a `#id` is read against the document the text's own record belongs to.
 * The world reads that from the row its driver's title names, so the text a driver stores holds
 * every form its surface carries and no other.
 *
 * `e2e/__tests__/reading-walk.test.ts` is the gate: it reads Playwright's own listing of the
 * reading specs and fails on a rendered row with no driver, two drivers for one row, a driver
 * for a row the list does not hold or does not render, a driver Playwright would skip, a
 * rendered row that is no record and does not say it carries no question id, a row that names
 * a form it cannot carry and not why, and a reading spec the references lane does not run. A
 * row the list excludes carries its reason, which the dashboard's own source test holds
 * (`features/refs/surface-lists.test.ts`).
 *
 * Each world spec seeds this world in its own rows (`seedReadingWorld`), writes `readingText`
 * into the record its surface shows, and reads it with `expectReadsReferences`.
 */

/** The live session the running row's text names as a party. */
export function readingSessionId(): string {
  return rowSessionId("tester");
}

export const READING_SESSION_TITLE = "Reading tester";

/** The reading world's live session writing as itself. */
export const asReadingSession = {
  get actor() {
    return { id: readingSessionId(), kind: "session" as const };
  },
  as: "agent" as const,
};

/** A second session writes without being the reader or the party named in the text. */
export const readingAuthor = {
  get actor() {
    return { id: rowSessionId("readingauthor"), kind: "session" as const };
  },
  as: "agent" as const,
};

/** The words the text writes for the party: the name the picker wrote when the party was picked,
 *  which the session no longer holds. A surface that drew a mention as its author's words would
 *  show these, so a surface that shows `READING_SESSION_TITLE` instead has read the party's
 *  current name (R8). */
const READING_SESSION_PICKED_NAME = "Old reading name";

/** The start of the row's own search word, which no issue title holds. */
const READING_LEAD = "Compare";

/** The words the text's worded link writes for the host: a link a person worded keeps those
 *  words, so a surface that showed the host's title instead would have read it as a bare
 *  address (R8). No title of the world holds them. */
const WORDED_LINK_WORDS = "kept words";

/** The id of the question block the text names as `#<id>`, and what that question asks, which
 *  is what a reference to its ask reads as. */
const READING_QUESTION_ID = "q1";
const READING_QUESTION = "Which reading goes first?";

/** What the comment the text names by a short id says, which is what a reference to it reads
 *  as. */
const READING_COMMENT = "The cache warms at start-up.";

/** How many characters of an id the text writes short: the fewest the server resolves
 *  (`e2e/prefix-links.e2e.ts`). */
const SHORT_ID = 8;

const readingSession: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/srv/reading",
  machine_id: "box-reading",
  roles: ["reading-reviewer"],
  get session_id() {
    return readingSessionId();
  },
  title: READING_SESSION_TITLE,
};

/** The issues and records the text names, the issue that hosts what a world writes, and the
 *  forms the driven row's surface carries. */
export interface ReadingWorld {
  /** The project of the running row's own, and the word its reading text opens with. */
  readonly project: RowKey;
  readonly lead: string;
  /** The first issue, where a world writes the records it reads, and the node the worded link
   *  names, which no other form names. Its spec declares the question block. */
  readonly host: Issue;
  /** The second issue, named by its bare key. */
  readonly keyed: Issue;
  /** The third issue, named by its `dispatch://` address. */
  readonly addressed: Issue;
  /** The fourth issue, named by its URL on this dashboard. */
  readonly linked: Issue;
  /** The address of the ask the question block settled into, in the document the driven
   *  record's `#id` is read against: the host's spec, until `scopedTo` names another. */
  readonly question: string;
  /** The address of the keyed issue's comment, which the text writes with its id cut short. */
  readonly shortened: string;
  /** The forms the driven row's surface carries, in the order the text writes them. */
  readonly forms: readonly DispatchReadingForm[];
}

/** The `:::ask` block that declares the question the text names. A record's `#id` is read
 *  against one document - an issue's spec for its comments, asks and messages, the document a
 *  comment or an ask is anchored to, a document's own text (`refs.ScopeOf`) - so a driver whose
 *  record belongs to a document of its own writes this block into it and takes its world
 *  through `scopedTo`. */
export function readingQuestionBlock(): string {
  return `:::ask{#${READING_QUESTION_ID} urgency="med" multiple="false"}\n${READING_QUESTION}\n\n- Yes\n- No\n:::`;
}

/** The address of the ask the question block of document `artifactId` settled into, once the
 *  server has settled it: it indexes a block into its ask after the version holding it is
 *  written, and a page opened before that reads a `#id` that names nothing yet. */
async function settledQuestion(artifactId: string): Promise<string> {
  const read = async () =>
    ((await getDocumentQuestions(artifactId)).questions ?? []).find(
      (question) => question.block_id === READING_QUESTION_ID
    )?.reference ?? "";
  await expect
    .poll(read, { message: `the question block of ${artifactId} settles`, timeout: 60_000 })
    .not.toBe("");
  return read();
}

/** The forms the driven row's surface carries: every form but the ones its row of the list
 *  says it cannot (`cannotCarry`), or, for a driver of one of the row's arms, the ones that arm
 *  says it cannot. The row and the arm are the ones the running driver's title names before its
 *  colon (`<surface>:` or `<surface>/<arm>:`), which is the tie the gate holds every driver to. */
function carriedForms(): DispatchReadingForm[] {
  const { title } = test.info();
  const [, surface, armId] = /^([a-z0-9-]+)(?:\/([a-z0-9-]+))?:/u.exec(title) ?? [];
  const row = DISPATCH_READING_SURFACES.find((one) => one.surface === surface);
  const arm = armId === undefined ? undefined : row?.arms?.find((one) => one.arm === armId);
  if (row === undefined || (armId !== undefined && arm === undefined)) {
    throw new Error(
      `setup: "${title}" names no row of DISPATCH_READING_SURFACES, or no arm of one, before its colon`
    );
  }
  const uncarried = (arm ?? row).cannotCarry?.forms ?? [];
  return DISPATCH_READING_FORMS.filter((form) => !uncarried.includes(form));
}

/**
 * Seeds the host, the three issues the text names, and the comment it names by a short id in
 * the row's own project. It lists the row's own live session, which the text names as a party.
 * An Inbox row passes its reader's login so the host's asks wait on that person.
 */
export async function seedReadingWorld({
  login,
}: {
  readonly login?: SignInLogin;
} = {}): Promise<ReadingWorld> {
  const project = rowKey("CORE");
  const by = { login };
  const forms = carriedForms();
  await createProject({ key: project, name: "Core" }, by);
  await setLiveSessions([readingSession]);
  const host = await createIssue(
    {
      project,
      spec: `The reading host's spec.\n\n${readingQuestionBlock()}\n`,
      title: "Reading host",
    },
    by
  );
  const keyed = await createIssue({ project, title: "Warm the cache" }, by);
  const addressed = await createIssue({ project, title: "Drain the queue" }, by);
  const linked = await createIssue({ project, title: "Ship the index" }, by);
  const made = [host.key, keyed.key, addressed.key, linked.key];
  const keys = [1, 2, 3, 4].map((number) => `${project}-${number}`);
  if (made.join() !== keys.join()) {
    throw new Error(`setup: the reading world made ${made.join(", ")}, not ${keys.join(", ")}`);
  }
  const comment = await createComment(keyed.key, { body: READING_COMMENT }, by);
  return {
    addressed,
    forms,
    host,
    keyed,
    lead: keyedSearchWord(READING_LEAD, project),
    linked,
    project,
    question: await settledQuestion(host.primary_artifact_id),
    shortened: `dispatch://${keyed.key}/comment/${comment.id}`,
  };
}

/** `world` for a record whose `#id` is read against the document `artifactId` rather than the
 *  host's spec: the question is the one that document declares (`readingQuestionBlock`). */
export async function scopedTo(world: ReadingWorld, artifactId: string): Promise<ReadingWorld> {
  return { ...world, question: await settledQuestion(artifactId) };
}

/** One form: how the text writes it, the address of the node it names, that node's own name,
 *  what the form reads as in a line of text, the path it opens, and the characters a surface
 *  that did not read it would leave. */
export interface FormReading {
  readonly form: DispatchReadingForm;
  readonly written: string;
  readonly reference: string;
  /** An issue's title, the question an ask asks, a comment's words, a session's title. */
  readonly name: string;
  /** An issue as its key and title, a party as `@` and its name, a worded link as its words,
   *  any other as its name. */
  readonly reads: string;
  readonly opens: string | RegExp;
  readonly unread: string | undefined;
}

function formReading(world: ReadingWorld, form: DispatchReadingForm): FormReading {
  const issue = (named: Issue) => ({
    form,
    name: named.title,
    opens: `/issues/${named.key}`,
    reads: `${named.key} ${named.title}`,
    reference: `dispatch://${named.key}`,
  });
  switch (form) {
    case "bare-key":
      // A key that was not read shows as itself, which its name starts with too.
      return { ...issue(world.keyed), unread: undefined, written: world.keyed.key };
    case "address": {
      const written = `dispatch://${world.addressed.key}`;
      return { ...issue(world.addressed), unread: written, written };
    }
    case "dashboard-url":
      return {
        ...issue(world.linked),
        unread: dashboardOrigin,
        written: `${dashboardOrigin}/issues/${world.linked.key}`,
      };
    case "question-id": {
      const written = `#${READING_QUESTION_ID}`;
      // An ask opens on its issue's page or on its document's, each of which ends in its id.
      const ask = world.question.slice(world.question.lastIndexOf("/") + 1);
      return {
        form,
        name: READING_QUESTION,
        opens: new RegExp(`[/=]${ask}$`, "u"),
        reads: READING_QUESTION,
        reference: world.question,
        unread: written,
        written,
      };
    }
    case "short-id": {
      const id = world.shortened.slice(world.shortened.lastIndexOf("/") + 1);
      const prefix = id.slice(0, SHORT_ID);
      return {
        form,
        name: READING_COMMENT,
        opens: `/issues/${world.keyed.key}/comments/${id}`,
        reads: READING_COMMENT,
        reference: world.shortened,
        unread: prefix,
        written: `dispatch://${world.keyed.key}/comment/${prefix}`,
      };
    }
    case "party": {
      const sessionId = readingSessionId();
      const reference = `dispatch://agent/${sessionId}`;
      return {
        form,
        name: READING_SESSION_TITLE,
        opens: `/agents/${sessionId}/live`,
        reads: `@${READING_SESSION_TITLE}`,
        reference,
        unread: READING_SESSION_PICKED_NAME,
        written: `[@${READING_SESSION_PICKED_NAME}](${reference})`,
      };
    }
    case "worded-link":
      // The link's own words, and not the host's name, which a bare address would read as.
      return {
        ...issue(world.host),
        reads: WORDED_LINK_WORDS,
        unread: undefined,
        written: `[${WORDED_LINK_WORDS}](dispatch://${world.host.key})`,
      };
  }
}

/** Each of `forms` as the text writes it and as a surface must read it. A driver whose surface
 *  takes a text of its own shape reads these rather than `readingText`: a body that is its
 *  references and nothing else. */
export function formReadings(
  world: ReadingWorld,
  forms: readonly DispatchReadingForm[] = world.forms
): FormReading[] {
  return forms.map((form) => formReading(world, form));
}

/** The text every driver stores: each form the driven row's surface carries, the party picked
 *  as the picker writes one, `[@<name>](<address>)`, under the name it held when it was picked
 *  (`READING_SESSION_PICKED_NAME`). One line, so a surface that shows a line of a record shows it
 *  whole, and few enough words that a search hit's snippet is the whole text
 *  (`pmdoc.SearchSnippetWords`). `forms` names fewer for a surface that shows a head too short
 *  to hold them all (`firstLineForms`). */
export function readingText(
  world: ReadingWorld,
  forms: readonly DispatchReadingForm[] = world.forms
): string {
  return `${world.lead} ${formReadings(world, forms)
    .map((reading) => reading.written)
    .join(", ")}.`;
}

/** How many characters the server keeps of a record's first line, where a surface shows that
 *  line rather than the record (`refs.FirstLine`): a thread's subject. */
const FIRST_LINE_CHARACTERS = 160;

/** The forms the driven row's surface carries, in as few groups as each make a text a first
 *  line holds whole: what a driver writes one record apiece for where its surface shows the
 *  head the server cut of a record's first line. The cut leaves out whole a reference it would
 *  cross, and no one line of that length holds every form, so such a surface is held to
 *  every form over as many records as it takes. */
export function firstLineForms(world: ReadingWorld): DispatchReadingForm[][] {
  const groups: DispatchReadingForm[][] = [];
  for (const form of world.forms) {
    const last = groups.at(-1);
    const fits = (forms: readonly DispatchReadingForm[]) =>
      [...readingText(world, forms)].length <= FIRST_LINE_CHARACTERS;
    if (last !== undefined && fits([...last, form])) {
      last.push(form);
      continue;
    }
    if (!fits([form])) {
      throw new Error(`setup: the ${form} form alone is longer than a first line`);
    }
    groups.push([form]);
  }
  return groups;
}

/**
 * How a surface's host shows a reference (R6): `link`, a record a reader reads, where each
 * reference is a link; `inert`, the inside of a link or a button, where each reference draws as
 * its name and links nothing; `written`, a diff, where the text is the characters as written.
 */
export type ReadingShape = "link" | "inert" | "written";

/**
 * Holds `scope` to what R6 and R8 say a surface of `shape` shows for `readingText(world,
 * forms)`. `link` and `inert`: each form reads as its node's current name - an issue's key and
 * title, the question a `#id` names, the comment a short id names, and `@` and the session's
 * current title rather than the name the text wrote for it - but a worded link, which reads as
 * the words its author wrote; and none is left as the characters written: no `dispatch://`, no
 * origin of this dashboard, no `#id`, no id prefix. `link` makes each a link that opens its
 * node, `inert` none. `written`: the text as stored.
 */
export async function expectReadsReferences(
  scope: Locator,
  world: ReadingWorld,
  shape: ReadingShape,
  forms: readonly DispatchReadingForm[] = world.forms
): Promise<void> {
  if (shape === "written") {
    await expect(scope).toContainText(readingText(world, forms));
    return;
  }
  await expectReadsForms(scope, formReadings(world, forms), shape);
}

/** Holds `scope` to each of `readings`, for a host of `shape`: the check `expectReadsReferences`
 *  makes of `readingText`, for a driver that stores the forms in a text of its own shape. */
export async function expectReadsForms(
  scope: Locator,
  readings: readonly FormReading[],
  shape: "link" | "inert"
): Promise<void> {
  for (const { form, opens, reads, reference, unread } of readings) {
    const drawn = scope.locator(`[data-dispatch-ref="${reference}"]`).first();
    await expect(drawn, `the ${form} reads as its node's name`).toHaveText(reads);
    if (shape === "link") {
      await expect(drawn, `the ${form} opens its node`).toHaveAttribute("href", opens);
    }
    if (unread !== undefined) {
      await expect(scope, `the ${form} is not left as written`).not.toContainText(unread);
    }
  }
  await expect(scope).not.toContainText("dispatch://");
  if (shape === "inert") {
    await expect(scope.locator("a[href][data-dispatch-ref]")).toHaveCount(0);
  }
}
