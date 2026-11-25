import { createIssue, createProject, listIssues } from "./api";
import {
  callTool,
  chatCalls,
  modelsLane,
  prose,
  runCalls,
  say,
  scriptChat,
} from "./fake-models-helpers";
import { askTheAgent, newestRun, openBarOver, RUN_STEPS_MS } from "./going-locators";
import { plainWords } from "./going-panel";
import { expect, type Locator, test } from "./test";
import { withAlice } from "./users";

// Check c32 of dispatch://INFRA-1588/artifact/verification-md (spec R1, R3): an answer of the
// command bar's agent has fewer than 120 words unless the request asks for more. In a preview with
// the real models at 8a3e562b the answer to c32's first request drew 120 words or more in 6 of 11
// tries (dispatch://INFRA-1588/artifact/tester-c32-words-8a3e562b-json): the bound stood in the
// prompt alone, and the titles of an answer's chips are drawn after the model wrote. The
// architect's ruling (dispatch://INFRA-1588/comment/96cd5279-bc9b-4f8a-b8f8-435901cdca75): when
// the model ends its answer the server counts the words as the bar draws them, each chip with its
// key, its status and its title; at 120 or more it gives the model one more turn, which says how
// many words the person sees; there is at most one such turn, and an answer still long after it
// shows whole, never cut. The same ruling has a list result give its total, so a count in an
// answer's prose is the server's: the preview's answer said 41 issues beside a chart of 42.
// Every row runs in the models lane alone (`bun run e2e:models`).

/** How many plain words the title of each issue these rows seed holds, so each chip draws ten
 *  words: its key, its status and this title. */
const TITLE_WORDS = 8;

/** The words the bar draws in `answer`, as a person and a screen reader meet them: every text
 *  node, a chip's key, its status and its title each as their own words. A word holds a letter or
 *  a digit. */
async function drawnWords(answer: Locator): Promise<string[]> {
  const parts = await answer.evaluate((element) => {
    const texts: string[] = [];
    const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      texts.push(node.textContent ?? "");
    }
    return texts;
  });
  return parts
    .join(" ")
    .split(/\s+/)
    .filter((word) => /[\p{L}\p{N}]/u.test(word));
}

/** Seeds project CORE with `count` issues, each titled with TITLE_WORDS plain words of its own,
 *  since a second issue of one title is refused as a copy of the first, and answers their keys. */
async function seedIssues(count: number): Promise<string[]> {
  await createProject({ key: "CORE", name: "Core" });
  const keys: string[] = [];
  for (let index = 0; index < count; index += 1) {
    const title = plainWords(`ledger${index}x`, TITLE_WORDS);
    keys.push((await createIssue({ project: "CORE", title })).key);
  }
  return keys;
}

/** `stem`'s `count` plain words, then a link to each of `keys`, which the bar draws as its chip. */
function withChips(stem: string, count: number, keys: readonly string[]): string {
  return `${plainWords(stem, count)} ${keys.map((key) => `[${key}](/issues/${key})`).join(" ")}`;
}

/** The text of every message of `body` after its last assistant message: what the server tells
 *  the model in the call that follows a reply. */
function toldAfterLastReply(body: {
  readonly messages?: readonly { readonly role: string; readonly content?: unknown }[];
}): string {
  const messages = body.messages ?? [];
  const last = messages.map((message) => message.role).lastIndexOf("assistant");
  return messages
    .slice(last + 1)
    .map((message) =>
      typeof message.content === "string" ? message.content : JSON.stringify(message.content)
    )
    .join("\n");
}

// SLOW(>=2s): four seeded issues and a run of two model calls.
test("an answer that draws 120 words or more gets one more turn, which says how many words the person sees, and the bar shows the shorter answer", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  const keys = await seedIssues(4);
  const words = "where does the northern team keep what it saw of the quasars";
  // 96 words of prose and four links: 100 words as the model wrote them, under the bound, and 136
  // as the bar draws them, each chip its key, its status and its title of eight words.
  const long = withChips("long", 96, keys);
  const short = plainWords("brief", 30);
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [{ turns: [say(long), say(short)], words }],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, words);

    const shown = newestRun(page).getByTestId("going-run-answer");
    await expect(shown).toContainText("brief29", { timeout: RUN_STEPS_MS });
    await expect(newestRun(page)).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });
    await expect(shown).not.toContainText("long95");
    expect((await drawnWords(shown)).length).toBeLessThan(120);

    const calls = await runCalls(words);
    expect(calls.map((call) => call.turn)).toEqual([0, 1]);
    // The turn after the long answer names the count the person sees: 96 words and four chips of
    // a key, a status and eight words of title, which is 136 with a key and a status of one word
    // each and no more than 150 however a counter splits a key or a status.
    const second = (await chatCalls()).requests.find((body) =>
      (body.messages ?? []).some(
        (message) =>
          message.role === "assistant" &&
          typeof message.content === "string" &&
          message.content.includes("long95")
      )
    );
    expect(second, "the call that follows the long answer").toBeDefined();
    const counts = (toldAfterLastReply(second ?? {}).match(/\b\d{3}\b/g) ?? []).map(Number);
    expect(counts.some((count) => count >= 130 && count <= 150)).toBe(true);
  });
});

// SLOW(>=2s): a run of two model calls.
test("an answer still long after its one more turn shows whole, and the run ends there", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const words = "say everything the southern team wrote about the comet";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      {
        afterwards: say("thirdturn"),
        turns: [say(plainWords("first", 150)), say(plainWords("still", 140))],
        words,
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, words);

    const shown = newestRun(page).getByTestId("going-run-answer");
    await expect(shown).toContainText("still139", { timeout: RUN_STEPS_MS });
    await expect(newestRun(page)).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });
    // Whole, never cut: its first word and its last, all 140 of them, and none of a third turn's.
    await expect(shown).toContainText("still0 ");
    expect((await drawnWords(shown)).length).toBeGreaterThanOrEqual(140);
    await expect(shown).not.toContainText("thirdturn");
    expect((await runCalls(words)).map((call) => call.turn)).toEqual([0, 1]);
  });
});

// SLOW(>=2s): two seeded issues and a run of one model call.
test("an answer that draws fewer than 120 words ends the run with no other turn", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  const keys = await seedIssues(2);
  const words = "which ledger holds the dimmest quasar of the winter";
  // 60 words of prose and two chips of ten words each: 80 words as drawn.
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [{ afterwards: say("otherturn"), turns: [say(withChips("under", 60, keys))], words }],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, words);

    const shown = newestRun(page).getByTestId("going-run-answer");
    await expect(shown).toContainText("under59", { timeout: RUN_STEPS_MS });
    await expect(newestRun(page)).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });
    await expect(shown.getByTestId("item-chip")).toHaveCount(2);
    await expect(shown).not.toContainText("otherturn");
    expect((await runCalls(words)).map((call) => call.turn)).toEqual([0]);
  });
});

// SLOW(>=2s): three seeded issues and a run of two model calls and a read.
test("the list of a project's issues gives the model its total", async ({ browser }) => {
  modelsLane();
  test.setTimeout(120_000);
  await seedIssues(3);
  const total = (await listIssues("CORE")).length;
  const words = "how many ledgers does the core project hold in all";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      {
        turns: [callTool("dispatch_issues", { all: true, project: "CORE" }), say("Listed them.")],
        words,
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, words);

    await expect(newestRun(page).getByTestId("going-run-answer")).toContainText("Listed them.", {
      timeout: RUN_STEPS_MS,
    });
    expect((await listIssues("CORE")).length).toBe(total);
    const calls = await runCalls(words);
    expect(calls.map((call) => call.turn)).toEqual([0, 1]);
    expect(calls[1].toolResults).toHaveLength(1);
    // The result names its total, the count the project's list shows, beside its rows: a row's
    // own "total" is the count of its children, inside its progress, and is not that.
    expect(calls[1].toolResults[0]).toMatch(
      new RegExp(`(?<!"progress":\\{[^}]{0,80})total\\D{0,12}${total}\\b`, "i")
    );
  });
});
