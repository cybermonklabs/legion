import { DISPATCH_TEXT_REFERENCES } from "@legion/contracts";
import { expect, type Locator, type Page } from "@playwright/test";

import type { Issue } from "../web/src/api/types";
import { type FakeSession, setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  listIssueAsks,
  putArchitectureSource,
  type RowKey,
  syncArchitectureSource,
} from "./api";
import { blockId, openedThreadCard, setSheet } from "./editor";
import { seedFakeGithub } from "./fake-github-helpers";
import { replyRowId } from "./inbox-threads";
import { keyedSearchWord } from "./search";
import { fixedPerson, rowKey, rowKeyBehind, rowPerson, rowSessionId } from "./test";
import type { RowLogin, SignInLogin } from "./users";

/**
 * The one world every reference spec is walked across, and the surfaces it is read on.
 *
 * One fixture rather than four: the forms a reference is written in (a `dispatch://` reference, a
 * dashboard address, an issue key on its own, a `#` and a question block's id, and any of their
 * ids written short) all resolve against the same records, so the margin, the Conversation, the
 * Inbox's thread rows, the Answers page, the live editor and a past version are read against
 * one seeding.
 * `seedReferenceWorld` makes it; the helpers below are how each surface is reached.
 */

/** The agent session these specs write as, so a write is never the reader's own. */
export const referenceAgent = {
  actor: { id: "e2e-references-agent", kind: "session" as const },
  as: "agent" as const,
};

/** The paragraph of the host's spec every margin thread here is anchored to. It is also the
 *  spec block whose id a short block reference names. */
export const ANCHOR = "The margin anchor sits here.";

/** The keys of the world's three projects. The shared text table writes its own (`CORE`, `XCORE`
 *  and `ACME`: `CORE-1` the host, `CORE-2` the target, `XCORE-1` and `ACME-12`); each row makes
 *  its world under keys of its own (`rowKey`, e2e/test.ts), and `intoWorld` rewrites a text from
 *  the one to the other. This repository is public, so the fixture names no real project. */
export interface ReferenceProjects {
  readonly core: RowKey;
  /** `core` behind an `X`, as the table's `XCORE` is `CORE` behind one, so a text that writes the
   *  one key inside the other (`See XCORE-1 here.`) still does in this world. */
  readonly xcore: RowKey;
  readonly acme: RowKey;
}

/** `text`, written against the shared table's project keys, rewritten into the world's: `CORE`
 *  becomes `projects.core`, the lower-case `core` the table writes to show a key read without
 *  case is none becomes the lower case of it, and `ACME` becomes `projects.acme`. `XCORE`
 *  follows from `CORE`, since the world's `xcore` is its `core` behind an `X`. */
export function intoWorld(text: string, projects: ReferenceProjects): string {
  return text.replace(/(CORE|core)(?=[-/~])|ACME(?=-)/g, (key) => {
    if (key === "ACME") return projects.acme;
    return key === "core" ? projects.core.toLowerCase() : projects.core;
  });
}

/** The question block ids the host's spec declares: every id the shared text table's `#id` rows
 *  name, so the one world answers every row of it (`@legion/contracts`'s
 *  DISPATCH_TEXT_REFERENCES, the table the Go reader and the dashboard's reader share). */
export const QUESTION_IDS: readonly string[] = [
  ...new Set(DISPATCH_TEXT_REFERENCES.flatMap((row) => Object.keys(row.questions ?? {}))),
];

/** What a question block of `scope`'s document asks. Each document asks its own words, so a
 *  block another document declares under the same id is that document's question and not this
 *  one's. */
export function questionText(id: string, scope: string): string {
  return `Which way does question ${id} of ${scope} go?`;
}

/** The `:::ask` block that declares question `id` of `scope`'s document. */
export function questionBlock(id: string, scope: string): string {
  return `:::ask{#${id} urgency="med" multiple="false"}\n${questionText(id, scope)}\n\n- Yes\n- No\n:::`;
}

/** A question block's ask once the server has settled it. */
export interface SettledQuestion {
  readonly id: string;
  readonly question: string;
}

/**
 * The asks the question blocks of the document `slug` names under `issueKey` have settled into,
 * by block id, once every id of `ids` has one: the server indexes a block into its ask row after
 * the version holding it is written, one block at a time.
 */
export async function settledQuestions(
  issueKey: string,
  slug: string,
  ids: readonly string[]
): Promise<Record<string, SettledQuestion>> {
  const read = async () => {
    const settled: Record<string, SettledQuestion> = {};
    for (const ask of await listIssueAsks(issueKey)) {
      if (ask.block_id && ask.block_artifact?.slug === slug) {
        settled[ask.block_id] = { id: ask.id, question: ask.question };
      }
    }
    return settled;
  };
  await expect
    .poll(async () => Object.keys(await read()).sort(), { timeout: 60_000 })
    .toEqual([...ids].sort());
  return read();
}

/** One reference and the title every surface reads it as. */
export interface Target {
  readonly address: string;
  readonly title: string;
}

/** The kinds of target a reference names in these specs. */
export const TARGET_KINDS = ["Issue", "Document", "Question", "Comment", "Message"] as const;
export type TargetKind = (typeof TARGET_KINDS)[number];

/** An item of the host a reference may write with a short id, and the reference that names it
 *  whole. */
export interface ShortenableItem {
  readonly id: string;
  readonly reference: string;
}

/** The architecture the world's core project imports: one component, which is the only way a
 *  component exists - the fake GitHub serves the model and the source sync reads it in
 *  (`e2e/architecture.e2e.ts`). */
export const COMPONENT_REPO = "legion/reference-arch";
export const COMPONENT_ID = "web";
export const COMPONENT_TITLE = "Web";
export const COMPONENT_MODEL = { "web.md": `---\ntitle: ${COMPONENT_TITLE}\n---\nThe **SPA**.\n` };

/** The reference that names the component of `project`, a key the row made (`rowKey`,
 *  e2e/test.ts) and imported `COMPONENT_MODEL` into: the world's `projects.core`. */
export function componentReference(project: string): string {
  return `dispatch://${project}/component/${COMPONENT_ID}`;
}

/** The live session a reference names, whose id is the running row's own (`rowSessionId`,
 *  e2e/test.ts): a UUID, because the agent routes take no other (`agentStreamSessionID`), so a
 *  session named anything else would list on the Agents page with no live view to open; and the
 *  row's own, because Dispatch keeps what a session said and what was said to it by its id, for
 *  every person at once. Outside a running row it throws, as `rowSessionId` does. */
export function referenceSessionId(): string {
  return rowSessionId("tester");
}
export const SESSION_TITLE = "Reference tester";

/** The reference that names the running row's live session (`referenceSessionId`). */
export function sessionReference(): string {
  return `dispatch://agent/${referenceSessionId()}`;
}

/** The role the world's live session holds. */
export const ROLE_TOKEN = "reference-reviewer";
export const ROLE_REFERENCE = `dispatch://role/${ROLE_TOKEN}`;

/** The session the world's live list holds. */
function referenceSession(): FakeSession {
  return {
    capabilities: ["aside", "btw", "steer"],
    dir: "/srv/reference",
    machine_id: "box-reference",
    roles: [ROLE_TOKEN],
    session_id: referenceSessionId(),
    title: SESSION_TITLE,
  };
}

/** The person a reference names. A stored login is an email (`model.CanonicalLogin`, and
 *  `text/refs.go`'s `personLoginPattern`), and the two logins the reset re-creates - `alice`
 *  and `bob` - are not, so a reference naming one of them names nobody Dispatch holds; nor is a
 *  `rowPerson` login (e2e/test.ts). So the person is the running row's own `rowPerson` login at
 *  a domain, which `fixedPerson` makes a login the sign-ins take and the row's end deletes, as it
 *  deletes a `rowPerson`. This person posts on the host, which is what puts them among its
 *  people and so among the picker's rows. Outside a running row it throws, as `rowPerson`
 *  does. */
export function referencePerson(): RowLogin {
  return fixedPerson(
    `${rowPerson("dana")}@example.test`,
    "a person reference names an email, which no rowPerson login is; the row's own suffix keeps the person the row's"
  );
}

/** The reference that names the running row's person (`referencePerson`). */
export function personReference(): string {
  return `dispatch://person/${referencePerson()}`;
}

/** A person reference the world answers for nobody: a login no row signs in as, which every
 *  surface reads as the words it is written with, with no link (R8). */
export const UNKNOWN_PERSON_LOGIN = "nobody@example.test";
export const UNKNOWN_PERSON_REFERENCE = `dispatch://person/${UNKNOWN_PERSON_LOGIN}`;

/** A picker query that finds the world's tagged records without finding earlier rows' records. */
export function referenceSearchWord(project: RowKey): string {
  return keyedSearchWord("cache", project);
}

export interface ReferenceWorld {
  /** The keys the row made its three projects under, which the table's texts are rewritten
   *  into (`intoWorld`). */
  readonly projects: ReferenceProjects;
  /** Issue 1 of `projects.core`, the table's CORE-1 - the host: its spec holds the anchor,
   *  filler that puts the questions below the fold, and one question block per `QUESTION_IDS`.
   *  The texts these specs write live on it. */
  readonly host: Issue;
  /** Issue 2 of `projects.core`, the table's CORE-2, the issue references name, which owns the
   *  document, question, comment and message of `targets`. */
  readonly target: Issue;
  /** Issue 1 of `projects.xcore` and issue 12 of `projects.acme`, the table's XCORE-1 and
   *  ACME-12: the two other keys a text writes bare. */
  readonly other: Issue;
  readonly acme: Issue;
  /** The host's spec's questions, by block id. */
  readonly questions: Readonly<Record<string, SettledQuestion>>;
  /** The host's own ask, comment, message and spec block, the four records whose ids a text may
   *  write short (`dispatch://CORE-1/ask/2a7f1c90`). */
  readonly items: Readonly<Record<"ask" | "comment" | "message" | "block", ShortenableItem>>;
  /** What each kind of target reads as, by its reference. */
  readonly targets: Readonly<Record<TargetKind, Target>>;
}

/** How many filler paragraphs stand between the anchor and the first question block, so a link
 *  to a question opens a document whose question starts below the fold. */
const FILLER_PARAGRAPHS = 40;

/**
 * Seeds the world: the three projects, under the running row's own keys, the host whose spec
 * declares every question block, the target with a document, an ask, a comment and a message,
 * the cross-project issue, and the host's own ask, comment, message and spec block whose ids a
 * short reference names.
 *
 * Beside the records it seeds the parties a reference names: the core project's one
 * architecture component, the live session and the role it holds, and the person whose login is
 * a stored login's own shape, each of them the running row's own, a kind the picker offers and a
 * page a reference opens.
 *
 * The keys are deterministic — an issue's key is its project's next number — and the seeding
 * refuses any other, so a change in how keys are minted fails here rather than inside a row.
 *
 * `login` is the person who makes the projects and the issues, alice where none is named: an
 * issue is assigned to whoever made it, so the world's questions wait on that person, and a row
 * that reads them in its reader's Inbox names a person of its own (`rowPerson`, e2e/test.ts).
 *
 * `tagPickerSearch` adds a searchable word of the row's own to the nodes the picker rows take.
 * The other rows keep the shared table's text unchanged.
 */
export async function seedReferenceWorld({
  login,
  tagPickerSearch = false,
}: {
  readonly login?: SignInLogin;
  readonly tagPickerSearch?: boolean;
} = {}): Promise<ReferenceWorld> {
  const core = rowKey("CORE");
  const searchable = (text: string) =>
    tagPickerSearch ? `${text} ${referenceSearchWord(core)}` : text;
  const projects: ReferenceProjects = {
    acme: rowKey("ACME"),
    core,
    xcore: rowKeyBehind("X", core),
  };
  const by = { login };
  await createProject({ key: projects.core, name: "Core" }, by);
  await createProject({ key: projects.xcore, name: "Cross core" }, by);
  await createProject({ key: projects.acme, name: "Acme" }, by);
  // The core project's architecture, the one component a reference names: the fake GitHub
  // serves the model and the source sync reads it in, which is how every component of a project
  // is made.
  await seedFakeGithub({
    [COMPONENT_REPO]: { contents: "read", files: COMPONENT_MODEL, installation_id: 101 },
  });
  await putArchitectureSource(projects.core, { branch: "main", repo: COMPONENT_REPO });
  const source = await syncArchitectureSource(projects.core, COMPONENT_REPO);
  expect(source.last_error).toBeNull();
  // The live session and the role it holds: a row of the picker, and a page a reference opens.
  await setLiveSessions([referenceSession()]);
  const filler = Array.from(
    { length: FILLER_PARAGRAPHS },
    (_, index) => `Filler paragraph ${index + 1} keeps the questions below the fold.`
  );
  const spec = [ANCHOR, ...filler, ...QUESTION_IDS.map((id) => questionBlock(id, "spec")), ""].join(
    "\n\n"
  );
  const host = await createIssue({ project: projects.core, spec, title: "Reference host" }, by);
  const target = await createIssue({ project: core, title: searchable("Warm the cache") }, by);
  const other = await createIssue(
    { project: projects.xcore, title: searchable("Cross-project target") },
    by
  );
  // The table's ACME-12 is the twelfth issue of its project, so eleven stand before it.
  let acme = await createIssue({ project: projects.acme, title: "Acme first" }, by);
  for (let made = 2; made <= 12; made += 1) {
    acme = await createIssue({ project: projects.acme, title: `Acme number ${made}` }, by);
  }
  const made = { acme: acme.key, host: host.key, other: other.key, target: target.key };
  const want = {
    acme: `${projects.acme}-12`,
    host: `${projects.core}-1`,
    other: `${projects.xcore}-1`,
    target: `${projects.core}-2`,
  };
  for (const [name, key] of Object.entries(want)) {
    if (made[name as keyof typeof made] !== key) {
      throw new Error(`setup: the world made ${JSON.stringify(made)}, not ${JSON.stringify(want)}`);
    }
  }

  const document = await createIssueArtifact(
    target.key,
    {
      content: `${searchable("# Notes")}\n\nThe cache notes.\n`,
      name: "notes.md",
    },
    referenceAgent
  );
  expect(document.artifact.slug).toBe("notes-md");
  const targetAsk = await createAsk(
    target.key,
    { question: searchable("Should the cache be warmed first?") },
    referenceAgent
  );
  const targetComment = await createComment(
    target.key,
    { body: searchable("The warm cache halves start-up time.") },
    referenceAgent
  );
  const targetMessage = await createMessage(
    target.key,
    { body: searchable("The rollout finished at noon.") },
    referenceAgent
  );

  // The host's own four, which a text writes short: an ask, a comment, a message and the spec
  // block the anchor is.
  const hostAsk = await createAsk(
    host.key,
    { question: "Which cache backs the rollout?" },
    referenceAgent
  );
  const hostComment = await createComment(
    host.key,
    { body: "The rollout waits on the cache." },
    referenceAgent
  );
  const hostMessage = await createMessage(
    host.key,
    { body: "The cache is warm now." },
    referenceAgent
  );
  const hostBlock = await blockId(host.primary_artifact_id, ANCHOR);
  // The person: their own post on the host, which is what puts them among its people. Writing
  // it signs them in, which is the only way Dispatch holds a person at all. Nothing reads the
  // comment itself; writing it is the whole point.
  await createComment(
    host.key,
    { body: "The cache is the slow part." },
    { login: referencePerson() }
  );

  const questions = await settledQuestions(host.key, "spec", QUESTION_IDS);
  for (const id of ["q2", "q3"]) {
    if (questions[id] === undefined) {
      throw new Error(`setup: the shared text table no longer declares the question ${id}`);
    }
  }

  return {
    acme,
    host,
    items: {
      ask: { id: hostAsk.id, reference: `dispatch://${host.key}/ask/${hostAsk.id}` },
      block: { id: hostBlock, reference: `dispatch://${host.key}/spec#b-${hostBlock}` },
      comment: {
        id: hostComment.id,
        reference: `dispatch://${host.key}/comment/${hostComment.id}`,
      },
      message: {
        id: hostMessage.id,
        reference: `dispatch://${host.key}/message/${hostMessage.id}`,
      },
    },
    other,
    projects,
    questions,
    target,
    targets: {
      Comment: {
        address: `dispatch://${target.key}/comment/${targetComment.id}`,
        title: searchable("The warm cache halves start-up time."),
      },
      Document: {
        address: `dispatch://${target.key}/artifact/notes-md`,
        title: "notes.md",
      },
      Issue: { address: `dispatch://${target.key}`, title: `${target.key} ${target.title}` },
      Message: {
        address: `dispatch://${target.key}/message/${targetMessage.id}`,
        title: searchable("The rollout finished at noon."),
      },
      Question: {
        address: `dispatch://${target.key}/ask/${targetAsk.id}`,
        title: searchable("Should the cache be warmed first?"),
      },
    },
  };
}

/** What a rendered body links: its reference anchors and the placeholder a Dispatch picture
 *  links its artifact's page from. */
export const RENDERED_REFERENCE = "a[data-dispatch-ref], a[data-dispatch-picture]";
/** Every reference a body draws, whether its host makes it a link or a link-styled span with
 *  no link inside it (a preview inside a control): the same attributes either way. */
export const DRAWN_REFERENCE = "[data-dispatch-ref], [data-dispatch-picture]";
/** Every reference a body's reader found in its text: each one it draws (`DRAWN_REFERENCE`),
 *  and each whose target Dispatch answered does not exist, which it draws as its writer's words
 *  with no link (`data-dispatch-missing`, holding the reference). Which of the two a reference
 *  is depends on what the world holds; that it was found depends on the text alone, which is
 *  what the shared table names. */
export const FOUND_REFERENCE = `${DRAWN_REFERENCE}, [data-dispatch-missing]`;
/** The rendered body of a turn or a card, which is never one of the one-line previews beside
 *  it (a reference card's two lines of its target, a reply's quote of its parent). */
export const RENDERED_BODY = ".dispatch-markdown:not([data-markdown-preview])";

/** The reference each element in scope matching `selector` names, in the order they are drawn:
 *  a Dispatch picture's `data-dispatch-picture`, a missing target's `data-dispatch-missing`,
 *  any other's `data-dispatch-ref`. */
export function referenceTargetsIn(scope: Locator, selector: string): Promise<string[]> {
  return scope
    .locator(selector)
    .evaluateAll((elements) =>
      elements.map(
        (element) =>
          element.getAttribute("data-dispatch-picture") ??
          element.getAttribute("data-dispatch-missing") ??
          element.getAttribute("data-dispatch-ref") ??
          ""
      )
    );
}

/** The `data-dispatch-href` of every title a document editor draws in scope, in the order it
 *  draws them (`@legion/proof-editor`'s reference decorations). */
export function editorTitlesIn(scope: Locator): Promise<string[]> {
  return scope
    .locator("a.dispatch-reference")
    .evaluateAll((elements) =>
      elements.map((element) => element.getAttribute("data-dispatch-href") ?? "")
    );
}

/** A margin thread's collapsed card: the toggle holding its one-line preview. On the phone
 *  layout the margin is a bottom sheet, which this opens first. */
export async function collapsedMarginThread(
  page: Page,
  project: string,
  rootId: string
): Promise<Locator> {
  await setSheet(page, project, true);
  const toggle = page
    .getByTestId("margin-sheet")
    .locator(`[data-margin-item="${rootId}"]`)
    .locator('button[aria-expanded="false"]');
  await expect(toggle).toBeVisible();
  return toggle;
}

/** Opens a margin thread from its collapsed card, as a reader's click does, and answers the
 *  opened card: beside the document, or in the Thread dialog on the phone layout. The click
 *  lands on the card's anchor quote, since its preview keeps its links live and the thread's
 *  root must be anchored. The opened card holds no collapsed preview, so what it renders is the
 *  comment itself. */
export async function openMarginThread(
  page: Page,
  project: string,
  rootId: string
): Promise<Locator> {
  await (await collapsedMarginThread(page, project, rootId)).locator("blockquote").click();
  const card = openedThreadCard(page, project, rootId);
  await expect(card).toBeVisible();
  await expect(card.locator('button[aria-expanded="false"]')).toHaveCount(0);
  return card;
}

/** The phone layout opens a margin thread in the Thread dialog over the sheet; Back closes it. */
export async function closeMarginThread(page: Page, project: string): Promise<void> {
  if (project !== "iphone") {
    return;
  }
  const thread = page.getByRole("dialog", { name: "Thread" });
  await thread.getByRole("button", { name: "Back" }).click();
  await expect(thread).toHaveCount(0);
}

/**
 * The one-line preview a reader still meets: an answered ask's question on the Answers page
 * (`web/src/features/answers/AnswersPage.tsx`, a `MarkdownPreview`). Its host is a record a
 * reader reads (`DISPATCH_READING_SURFACES`'s `answers-subject`), so its references are live
 * links (`ReferenceHostIs`).
 *
 * The Replies page's own row carried this before INFRA-450, and that page is gone: `/replies`
 * lands on the Inbox, and the Inbox draws a thread row's thread whole with `MarkdownBody` and
 * live links (`features/inbox/ThreadBody.tsx`), so no one-line preview of a reply is left to
 * read.
 */
export function answeredQuestionPreview(page: Page, askId: string): Locator {
  return page.locator(`[data-answer-row="${askId}"]`).locator("[data-markdown-preview]").first();
}

/** Opens the Inbox drawer, whose every row is the card that draws its own thread rather than a
 *  title alone: the Inbox page draws short rows at every width and reads one row at a time
 *  (`e2e/inbox-pane.ts`). The drawer opens over another page, so this navigates to one. */
export async function openInboxReplies(page: Page): Promise<void> {
  await page.goto("/agents");
  await page.locator("body").focus();
  await page.keyboard.press("i");
  await expect(page.getByRole("dialog", { name: "Inbox" })).toBeVisible();
}

/** One turn of the thread an Inbox Reply row draws: the row is `[data-inbox-row="reply:<thread
 *  id>"]` (`features/inbox/list-row.ts`, `replyRowId`) and each turn in it carries the comment's
 *  or message's own id, as `refresh-freshness.e2e.ts` reads the same row. The turn's body is
 *  rendered rather than previewed, so a reference there is a live link. */
export function inboxReplyTurn(page: Page, threadId: string, turnId: string): Locator {
  return page
    .locator(`[data-inbox-row="${replyRowId(threadId)}"]`)
    .locator(`[data-inbox-thread-turn="${turnId}"]`);
}

/** A Conversation turn: the comment, message or ask the id names. */
export function conversationTurn(
  page: Page,
  kind: "comment" | "message" | "ask",
  id: string
): Locator {
  return page.locator(`[data-turn="${kind}:${id}"]`);
}
