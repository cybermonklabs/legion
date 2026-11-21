import { createIssue, createIssueArtifact, createProject, documentBase, search } from "./api";
import type { ChatReply, ChatRunScript } from "./fake-chat-model";
import type { DecisionReply } from "./fake-decision-model";
import { proposePlan, say } from "./fake-models-helpers";
import { expect, type Locator, type Page } from "./test";

// What the command bar's rows share: the bar, its answer panel, opening the bar over a page,
// asking it a question, the issue a question is answered from, the fake writing model's replies
// those rows script, and the conversation of the agent's runs. The panel and its lines are read by
// the test ids `features/search/AnswerPanel.tsx` gives them, the conversation, its steps and its
// plan by those of `features/going`.

/** How long a row waits for one answer to end: the run's own window (`GOING_RUN_MS`, 30 s) and
 *  the stream's grace past it. */
export const ANSWER_ENDS_MS = 40_000;

export function bar(page: Page): Locator {
  return page.getByRole("dialog", { name: "Search" });
}

export function searchBox(page: Page): Locator {
  return page.getByRole("combobox", { name: "Search" });
}

export function answerPanel(page: Page): Locator {
  return page.getByTestId("going-answer");
}

export function answerText(page: Page): Locator {
  return page.getByTestId("going-answer-text");
}

/** Opens `path` and waits for the shell, whose search button opens the bar. */
export async function openShell(page: Page, path: string): Promise<void> {
  await page.goto(path);
  await expect(page.getByRole("button", { name: /^search/i })).toBeVisible();
}

/** Opens the bar over the page with `Control+K`, from the page's body. */
export async function openBar(page: Page): Promise<void> {
  await page.locator("body").focus();
  await page.keyboard.press("Control+k");
  await expect(searchBox(page)).toBeFocused();
}

/** Closes the bar with `Escape`. */
export async function closeBar(page: Page): Promise<void> {
  await page.keyboard.press("Escape");
  await expect(bar(page)).toHaveCount(0);
}

/** Opens `path`, waits for the shell, and opens the bar over it with `Control+K`. */
export async function openBarOver(page: Page, path: string): Promise<void> {
  await openShell(page, path);
  await openBar(page);
}

/** Seeds project CORE and one issue of it titled `title`, and answers the issue's key. */
export async function seedCoreIssue(title: string): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  return (await createIssue({ project: "CORE", title })).key;
}

/** Seeds project CORE and one issue titled `title`, and answers that issue's key and the href the
 *  search for `question` gives its hit, which is the link the model is given. */
export async function seedIssue(
  question: string,
  title: string
): Promise<{ readonly key: string; readonly href: string }> {
  const key = await seedCoreIssue(title);
  const hit = ((await search(question)).results ?? []).find(
    (result) => result.owner.kind === "issue" && result.owner.key === key
  );
  if (hit === undefined) {
    throw new Error(`search for "${question}" does not find ${key}`);
  }
  return { href: hit.href, key };
}

/** A parent issue of CORE and its two children, by key. */
export interface Split {
  readonly parent: string;
  readonly first: string;
  readonly second: string;
}

/** Seeds project CORE with the issue a split splits and its two children. */
export async function seedSplit(): Promise<Split> {
  await createProject({ key: "CORE", name: "Core" });
  const parent = await createIssue({ project: "CORE", title: "Build the observatory" });
  const first = await createIssue({
    parent: parent.key,
    project: "CORE",
    title: "Pour the foundation",
  });
  const second = await createIssue({
    parent: parent.key,
    project: "CORE",
    title: "Raise the dome",
  });
  return { first: first.key, parent: parent.key, second: second.key };
}

/** Writes `content` as the spec of `issue`, over the spec the issue was made with: an upload over
 *  a document names the token of the version it replaces. */
export async function writeSpec(
  issue: { readonly key: string; readonly primary_artifact_id: string },
  content: string
): Promise<void> {
  await createIssueArtifact(issue.key, {
    content,
    name: "spec.md",
    precondition: await documentBase(issue.primary_artifact_id),
  });
}

/** A chart of project CORE's issues by status, as bars: the spec a model writes for it names a
 *  read and its fields, never a number. */
export const STATUS_CHART_SPEC = {
  mark: "bar",
  params: { project: "CORE" },
  source: "read_progress",
  table: "statuses",
  x: "status",
  y: "count",
} as const;

/** Puts `words` in the box in one input, so the bar's pause starts once, and waits for the answer
 *  the pause asks for to end. */
export async function askAndWait(page: Page, words: string): Promise<void> {
  await searchBox(page).fill(words);
  await expect(answerPanel(page)).toHaveAttribute("data-state", "ended", {
    timeout: ANSWER_ENDS_MS,
  });
}

/** Counts the bar's `POST /api/v1/going/requests` from here on, each with the words it asked. */
export function countAskedRuns(page: Page): { readonly words: string[] } {
  const asked = { words: [] as string[] };
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (request.method() === "POST" && url.pathname === "/api/v1/going/requests") {
      const body: unknown = request.postDataJSON();
      const words =
        body !== null && typeof body === "object" && "words" in body ? String(body.words) : "";
      asked.words.push(words);
    }
  });
  return asked;
}

/** A structured reply whose answer is `answer` and which draws no block. */
export function prose(answer: string, pace: Partial<ChatReply> = {}): ChatReply {
  return { final: { answer }, ...pace };
}

/** A structured reply with a block of `kind`, its spec naming the read that fills it. */
export function withBlock(kind: string, spec: unknown, answer = "Here it is."): ChatReply {
  return { final: { answer, block: { kind, spec, title: `The ${kind}` } } };
}

/** `count` distinct plain words, none of which a renderer links or a lexicon reads as a block. */
export function plainWords(stem: string, count: number): string {
  return Array.from({ length: count }, (_, index) => `${stem}${index}`).join(" ");
}

/** The words the panel's text shows, split on whitespace. */
export async function shownWords(page: Page): Promise<string[]> {
  const text = (await answerText(page).textContent()) ?? "";
  return text.split(/\s+/).filter((word) => word !== "");
}

/** A decision of `action` at `confidence`, about the items the words name: the probabilities
 *  are written out, so the action holds them all and not the fake's even share of the rest. */
export function decided(action: string, confidence: number): DecisionReply {
  return {
    action: { choice: action, confidence, probabilities: { [action]: 1 } },
    context: { choice: "named", confidence: 0.99 },
  };
}

/** The run Enter on File as a new issue starts for `words`, as the fake writing model answers it:
 *  one plan of one create of an issue of CORE titled `title`, which the server files at once, as
 *  the agent's new-issue prompt asks (`going/agent/prompt.go`). */
export function newIssueRun(words: string, title: string): ChatRunScript {
  return {
    afterwards: say("Filed."),
    turns: [
      proposePlan(`File ${title} in CORE.`, [
        {
          arguments: { project: "CORE", spec: `${words}.\n`, title },
          tool: "dispatch_issue",
        },
      ]),
    ],
    words,
  };
}

/** How long a row waits for a run of the fake model to reach a step, a plan or its end. */
export const RUN_STEPS_MS = 40_000;

/** The line on top of the bar's list: the next step the words lead to (`search-option-line`). */
export function lineOnTop(page: Page): Locator {
  return bar(page).locator("#search-option-line");
}

/** Types `words` in the bar's box and presses Enter on the line on top, which starts a run. */
export async function askTheAgent(page: Page, words: string): Promise<void> {
  await searchBox(page).fill(words);
  await expect(lineOnTop(page)).toBeVisible();
  await searchBox(page).press("Enter");
  await expect(newestRun(page)).toBeVisible({ timeout: RUN_STEPS_MS });
}

/** The newest run of the conversation on the screen. */
export function newestRun(page: Page): Locator {
  return page.getByTestId("going-run").last();
}

/** Approves the newest run's waiting plan from the bar's empty box, and waits for the run to
 *  end done. */
export async function approveAndFinish(page: Page): Promise<void> {
  await expect(newestRun(page)).toHaveAttribute("data-state", "waiting", { timeout: RUN_STEPS_MS });
  await searchBox(page).focus();
  await searchBox(page).press("Enter");
  await expect(newestRun(page)).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });
}

/** The panel at the side of the page. */
export function sidePanel(page: Page): Locator {
  return page.getByTestId("going-side-panel");
}

/** The plan card of the conversation's newest run, in the bar or the panel. */
export function planCard(page: Page): Locator {
  return page.getByTestId("going-plan");
}

/** The rows of the open plan the page draws on itself. */
export function planOnPage(page: Page): Locator {
  return page.getByTestId("going-plan-on-page");
}

/** The plan rows of `where`, a card or the page's drawing. */
export function planRows(where: Locator): Locator {
  return where.getByTestId("going-plan-row");
}
