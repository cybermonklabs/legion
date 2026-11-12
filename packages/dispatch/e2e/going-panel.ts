import type { ChatReply } from "./fake-chat-model";
import { expect, type Locator, type Page } from "./test";

// What the command bar's answer rows share: the bar, its answer panel, opening the bar over a
// page, asking it a question, and the fake writing model's replies those rows script. The panel
// and its lines are read by the test ids `features/search/AnswerPanel.tsx` gives them.

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

/** Opens `path`, waits for the shell, and opens the bar over it with `Control+K`. */
export async function openBarOver(page: Page, path: string): Promise<void> {
  await page.goto(path);
  await expect(page.getByRole("button", { name: /^search/i })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("Control+k");
  await expect(searchBox(page)).toBeFocused();
}

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
