/** The ask card's browser checks (INFRA-3231's R7, INFRA-3552's R1 to R6): the question they
 *  are read against, its six surfaces, and the stand-in model's answers for Rewrite and Define.
 *  Shared by `ask-card-rewrite.e2e.ts`, `ask-card-define.e2e.ts`, `ask-card-menu.e2e.ts` and
 *  `ask-card-reviewer.e2e.ts`.
 *
 *  Everything here is made up: this repository is public, so nothing names a real person, host or
 *  project. The question is the shape of a Legion finding (INFRA-3552's c1): a short first line
 *  the controller finds the finding by, the question, a link, a `Ref:` line and a picture line,
 *  each of which a rewrite keeps.
 */
import { expect, type Locator, type Page } from "@playwright/test";

import type { Actor, Ask, AskOption } from "../web/src/api/types";
import { type FakeSession, getSentMessages } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createProject,
  getArtifactText,
  getAsk,
} from "./api";
import { blockId, indexedBlockAsk } from "./editor";
import type { FakeModelRule } from "./fake-model";
import { modelRequests } from "./fake-model-helpers";
import { inboxPane, inboxRow } from "./inbox-pane";

/** The deployment Rewrite and Define's model group names in the harness (`e2e/run-server.sh`),
 *  which every one of their calls to the stand-in carries as its model. */
export const REWRITE_DEFINE_MODEL = "stand-in-rewrite-define";

/** The first sentence of each of the component's system prompts
 *  (`packages/envoy/internal/dispatch/reviewer/prompts`), which tells its calls apart. */
export const REWRITE_PROMPT = "You write a text again for its reader.";
export const DEFINE_PROMPT = "You define words for a reader.";
export const REVIEW_PROMPT = "You review a text for its reader.";

export const PROJECT = "DEMO";

/** The session that asks: a Legion tester. Seeded into the fake Envoy only where a row reads a
 *  side question; unseeded, the listener knows no such session, so Rewrite and Define read the
 *  record alone and send it nothing. */
export const TESTER: FakeSession = {
  capabilities: ["btw", "aside", "steer"],
  roles: [],
  session_id: "legion-demo-demo-1-tester",
  title: "DEMO-1 tester",
};
export const tester: Actor = { id: TESTER.session_id, kind: "session" };
export const asTester = { actor: tester, as: "agent" as const };

export const FIRST_LINE = "Finding from legion-demo-demo-1-tester";
export const QUESTION = "Should the flip audit run before the ledger moves?";
export const REWRITTEN = "Run the flip audit first, or move the ledger first and audit after?";
export const RUN_LINK = "https://ci.example.test/runs/7";
export const REF_LINE = `Ref: dispatch://${PROJECT}-1`;
export const PICTURE_LINE = "![Flip rate](https://ci.example.test/flip-rate.png)";

/** The finding's question around `question`: every line a rewrite keeps, and the question. */
export function fixtureQuestion(question = QUESTION): string {
  return [FIRST_LINE, question, `The run that found it: ${RUN_LINK}`, REF_LINE, PICTURE_LINE].join(
    "\n"
  );
}

export const OPTIONS: readonly AskOption[] = [
  { description: "Audit before the move.", label: "Run it first" },
  { description: "Audit after the move.", label: "Move first" },
];

/** The paragraph of each issue's spec that says what a flip audit is: the block a definition's
 *  link cites. */
export const DEFINING_PARAGRAPH =
  "The flip audit compares each ledger row with its mirror before a move.";
const SPEC = `# Flip audit\n\n${DEFINING_PARAGRAPH}\n`;

/** The decision block of the decision-block surface: the same finding, and an option line whose
 *  description holds markup, which the document's own edit path keeps whole. */
export const BLOCK = "flip-order";
export const MARKUP_OPTION_LINE = "- Move first: Audit **after** the move.";
function blockSpec(question: string): string {
  return `${SPEC}\n:::ask{#${BLOCK} urgency="high" multiple="false"}\n${fixtureQuestion(question)}\n\n- Run it first: Audit before the move.\n${MARKUP_OPTION_LINE}\n:::\n`;
}

/** The surfaces of INFRA-3552's R1 an open question is answered on. */
export type Surface =
  | "inbox row"
  | "reading pane"
  | "reply thread"
  | "conversation"
  | "margin"
  | "decision block";

export const SURFACES: readonly Surface[] = [
  "inbox row",
  "reading pane",
  "reply thread",
  "conversation",
  "margin",
  "decision block",
];

/** One question, seeded for one surface. */
export interface Seeded {
  readonly askId: string;
  readonly issueKey: string;
  /** The issue's spec, the document a margin ask and a decision block are on. */
  readonly artifactId: string;
}

/** The project every seed is in, which each row makes before its seeds; alice is its default
 *  assignee. */
export async function seedProject(): Promise<void> {
  await createProject({ default_assignee: "alice", key: PROJECT, name: "Demo" });
}

/**
 * A question of the tester's on `surface`, on an issue of its own:
 *
 * - an Inbox row, the reading pane and an issue's Conversation: a question on an issue alice
 *   holds;
 * - a reply thread: a question on an issue bob holds, in whose thread alice asked something back
 *   and the tester answered, so alice's Inbox lists it as her Reply row and as no question of
 *   hers;
 * - the margin: a question on the issue's spec;
 * - a decision block: the tester writes it into the issue's spec.
 */
export async function seedSurface(
  surface: Surface,
  {
    multiple = false,
    options = OPTIONS,
    question = QUESTION,
    title = "Flip audit",
    urgency = "high",
  }: {
    multiple?: boolean;
    options?: readonly AskOption[];
    question?: string;
    title?: string;
    urgency?: string;
  } = {}
): Promise<Seeded> {
  // Every seed's spec is the one fixture, so a row's second issue is a near copy of its first,
  // which the create's near-copy gate would refuse without `force`.
  if (surface === "decision block") {
    const issue = await createIssue(
      { assignee: "alice", force: true, project: PROJECT, spec: blockSpec(question), title },
      asTester
    );
    const ask = await indexedBlockAsk(issue.key, BLOCK);
    return { artifactId: issue.primary_artifact_id, askId: ask.id, issueKey: issue.key };
  }
  const issue = await createIssue({
    assignee: surface === "reply thread" ? "bob" : "alice",
    force: true,
    project: PROJECT,
    spec: SPEC,
    title,
  });
  const ask = await createAsk(
    issue.key,
    {
      // The margin's question is anchored to a word of the spec, which puts it in the margin.
      ...(surface === "margin" ? { anchor: { artifact: "spec", quote: "mirror" } } : {}),
      multiple,
      options: [...options],
      question: fixtureQuestion(question),
      urgency,
    },
    asTester
  );
  if (surface === "reply thread") {
    await createComment(issue.key, { ask_id: ask.id, body: "Which ledger do you mean?" });
    await createComment(issue.key, { ask_id: ask.id, body: "The main ledger." }, asTester);
  }
  return { artifactId: issue.primary_artifact_id, askId: ask.id, issueKey: issue.key };
}

/** The card of one question where a surface draws it, and how a reader takes it in hand. */
export interface Hand {
  /** The card: its `data-ask-card` article. */
  readonly card: Locator;
  /** What draws the card and its badge: the card itself, or on a decision block the block's
   *  frame, whose header carries the badge. */
  readonly frame: Locator;
  /** The question's text as the reader reads it: the card's own, or the block's text in the
   *  document. */
  readonly question: Locator;
  /** Puts the card in hand: its row in focus on the Inbox, else a control of its own. */
  readonly grab: () => Promise<void>;
}

/** The width each surface is read at: an Inbox row draws its card inside itself below the
 *  reading column's 1280 px, and every other surface is read beside it. */
export function viewportOf(surface: Surface): { height: number; width: number } {
  return surface === "inbox row" ? { height: 900, width: 1024 } : { height: 900, width: 1440 };
}

/** Opens `surface` on `seeded`'s question and waits for its card. */
export async function openSurface(page: Page, surface: Surface, seeded: Seeded): Promise<Hand> {
  const { askId, issueKey } = seeded;
  await page.setViewportSize(viewportOf(surface));
  const cardSelector = `[data-ask-card="${askId}"]`;
  const focusOption = (card: Locator) => async () => {
    const option = card.locator("[data-ask-option]").first();
    await option.focus();
    await expect(option).toBeFocused();
  };
  switch (surface) {
    case "inbox row": {
      await page.goto("/");
      const row = inboxRow(page, askId);
      const card = row.locator(cardSelector);
      await expect(card).toBeVisible();
      const grab = async () => {
        await row.focus();
        await expect(row).toBeFocused();
      };
      return { card, frame: card, grab, question: card.locator("[data-ask-question]") };
    }
    case "reading pane": {
      await page.goto(`/?item=${askId}`);
      const row = inboxRow(page, askId);
      const card = inboxPane(page).locator(cardSelector);
      await expect(card).toBeVisible();
      await expect(row).toHaveAttribute("data-inbox-selected", "true");
      const grab = async () => {
        await row.focus();
        await expect(row).toBeFocused();
      };
      return { card, frame: card, grab, question: card.locator("[data-ask-question]") };
    }
    case "reply thread": {
      await page.goto(`/?item=reply:${askId}`);
      const card = inboxPane(page).locator(cardSelector);
      await expect(card).toBeVisible();
      return {
        card,
        frame: card,
        grab: focusOption(card),
        question: card.locator("[data-ask-question]"),
      };
    }
    case "conversation": {
      await page.goto(`/issues/${issueKey}/conversation`);
      const card = page.locator(`[data-turn] ${cardSelector}`);
      await expect(card).toBeVisible();
      return {
        card,
        frame: card,
        grab: focusOption(card),
        question: card.locator("[data-ask-question]"),
      };
    }
    case "margin": {
      await page.goto(`/issues/${issueKey}/spec`);
      const card = page.locator(`[data-margin-item="${askId}"] ${cardSelector}`);
      await expect(card).toBeVisible();
      return {
        card,
        frame: card,
        grab: focusOption(card),
        question: card.locator("[data-ask-question]"),
      };
    }
    case "decision block": {
      await page.goto(`/issues/${issueKey}/spec`);
      const frame = page
        .locator('[data-proof-block-type="ask"]')
        .filter({ has: page.locator(cardSelector) });
      const card = frame.locator(cardSelector);
      await expect(card).toBeVisible();
      return {
        card,
        frame,
        grab: focusOption(card),
        question: frame.locator("[data-proof-block-content]"),
      };
    }
  }
}

/** The badge a rewrite leaves on what `frame` draws, in whichever state it is. */
export function badgeOf(frame: Locator): Locator {
  return frame.locator("[data-ask-rewrite]");
}

/** Selects `words` inside `scope` as a reader's drag would, the first text that holds them, and
 *  tells the page the selection changed. Nothing here moves the focus. */
export async function selectWords(scope: Locator, words: string): Promise<void> {
  await scope.evaluate((root, wanted) => {
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const at = node.textContent?.indexOf(wanted) ?? -1;
      if (at < 0) continue;
      const range = document.createRange();
      range.setStart(node, at);
      range.setEnd(node, at + wanted.length);
      const selection = window.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
      document.dispatchEvent(new Event("selectionchange"));
      return;
    }
    throw new Error(`no text under the scope holds ${JSON.stringify(wanted)}`);
  }, words);
}

/** A rewrite's answer as the component's schema reads it: the wording, the options and the
 *  choice. An option is its label and description alone: the server keeps each option's own id,
 *  and compares a rewrite's options with the record's by their number, order and labels. */
export function rewriteText(body: string, options: readonly AskOption[], multiple = false): string {
  return JSON.stringify({
    body,
    multiple,
    options: options.map((option) => ({
      description: option.description ?? "",
      label: option.label,
    })),
  });
}

/** The ask as Dispatch holds it now. */
export async function askNow(askId: string) {
  return (await getAsk(askId)).ask;
}

/** A rewrite of the ask that keeps everything a rewrite keeps: its options as the record holds
 *  them, and every line of its question but the question's own, which `change` rewrites. */
export async function keptRewrite(
  askId: string,
  change: (question: string) => string = (question) => question.replace(QUESTION, REWRITTEN)
): Promise<string> {
  const ask = await askNow(askId);
  return rewriteText(change(await recordedQuestion(ask)), ask.options ?? [], ask.multiple);
}

/** The question as the record of a Rewrite holds it: the ask's own, or a decision block's as the
 *  document holds it, in markdown with its picture and links, which the ask's row leaves out
 *  (the server's `docs.AskBlockQuestionMarkdown`): the paragraphs between the block's opening
 *  line and its options. */
async function recordedQuestion(
  ask: Pick<Ask, "block_artifact" | "block_id" | "question">
): Promise<string> {
  if (ask.block_id === null || ask.block_artifact === undefined) return ask.question;
  const lines = (await getArtifactText(ask.block_artifact.id)).markdown.split("\n");
  const opener = lines.findIndex((line) => line.startsWith(`:::ask{#${ask.block_id}`));
  if (opener < 0) throw new Error(`the document holds no decision block ${ask.block_id}`);
  const end = lines.findIndex(
    (line, index) => index > opener && (line.startsWith("- ") || line.startsWith(":::"))
  );
  return lines
    .slice(opener + 1, end)
    .join("\n")
    .trim();
}

/** The stand-in's answer to a Rewrite whose request holds `contains` (any rewrite where it is
 *  the prompt's own sentence). */
export function rewriteRule(
  text: string,
  { contains = REWRITE_PROMPT, delayMs }: { contains?: string; delayMs?: number } = {}
): FakeModelRule {
  return { contains, model: REWRITE_DEFINE_MODEL, reply: { delayMs, text } };
}

/** The stand-in's definition of `words` for any Define, citing `links`. */
export function defineRule(
  words: string,
  meaning: string,
  links: readonly string[]
): FakeModelRule {
  return {
    contains: DEFINE_PROMPT,
    model: REWRITE_DEFINE_MODEL,
    reply: { text: JSON.stringify({ definitions: [{ links, meaning, words }] }) },
  };
}

/** The address of the spec block that says what a flip audit is, which a definition cites. */
export async function definingBlockLink(seeded: Seeded): Promise<{ block: string; link: string }> {
  const block = await blockId(seeded.artifactId, DEFINING_PARAGRAPH);
  return { block, link: `dispatch://${seeded.issueKey}/spec#b-${block}` };
}

/** How many calls of one purpose the stand-in got since the last reset: Rewrite's or Define's. */
export async function callsOf(prompt: string): Promise<number> {
  return (await modelRequests()).filter(
    (request) => request.model === REWRITE_DEFINE_MODEL && request.text.includes(prompt)
  ).length;
}

/** One frame the fake Envoy was asked to deliver to a session: the message or definition id it
 *  carries, its words, who sent it and how. */
export interface SentFrame {
  readonly id: string;
  readonly body: string;
  readonly actor: Actor;
  readonly mode: string;
}

/** Every frame the fake Envoy was sent for `session`, oldest first. */
export async function framesTo(session: string): Promise<SentFrame[]> {
  const frames: SentFrame[] = [];
  for (const send of await getSentMessages()) {
    if (send.target_session !== session || typeof send.payload !== "string") continue;
    try {
      const parsed = JSON.parse(send.payload) as {
        delivery?: { mode?: string };
        event?: { actor?: Actor; payload?: { body?: string; id?: string } };
      };
      frames.push({
        actor: parsed.event?.actor ?? { id: "", kind: "user" },
        body: parsed.event?.payload?.body ?? "",
        id: parsed.event?.payload?.id ?? "",
        mode: parsed.delivery?.mode ?? "",
      });
    } catch {
      // A send whose payload is no frame is no side question.
    }
  }
  return frames;
}

/** The ask.edited event a rewrite appended, as `GET /api/v1/issues/{key}/events` returns it. */
export interface RewriteEdit {
  readonly actor: Actor;
  readonly notify: boolean;
  readonly payload: {
    readonly id: string;
    readonly quiet?: boolean;
    readonly previous?: { readonly question?: string };
    readonly rewrite?: {
      readonly by: string;
      readonly asked_by: Actor;
      readonly agent_answer: boolean;
    };
  };
  readonly type: string;
}

function isRewriteEdit(value: unknown): value is RewriteEdit {
  if (typeof value !== "object" || value === null) return false;
  if (!("type" in value) || value.type !== "ask.edited") return false;
  if (!("payload" in value) || typeof value.payload !== "object" || value.payload === null) {
    return false;
  }
  return "id" in value.payload && typeof value.payload.id === "string";
}

/** The newest ask.edited of the ask `askId` among `events` (`getIssueEvents`'s answer). */
export function rewriteEditOf(events: readonly unknown[], askId: string): RewriteEdit | undefined {
  return events
    .filter(isRewriteEdit)
    .filter((event) => event.payload.id === askId)
    .at(-1);
}

/** Starts Ask back on the question `card` draws, as a reader does with the pointer: the item of
 *  the menu its split button's chevron opens. The box keeps any text it holds, the card's
 *  compact form opens its box, and the split button's action then reads Ask back. */
export async function startAskBack(card: Locator): Promise<void> {
  await card.locator("[data-ask-menu-button]").click();
  await card.page().locator('[data-ask-menu] [data-ask-menu-item="ask-back"]').click();
  await expect(card.locator("[data-ask-answer]")).toHaveAttribute("data-asking-back", "true");
  await expect(card.locator("[data-ask-action]")).toHaveText("Ask back");
}

/** Asks the question `card` draws back with `text`: Ask back from the menu, the text in the box,
 *  and the split button's action, which sends it as a comment in the ask's thread. */
export async function askBack(card: Locator, text: string): Promise<void> {
  await startAskBack(card);
  await card.locator("[data-ask-answer]").fill(text);
  await card.locator("[data-ask-action]").click();
}
