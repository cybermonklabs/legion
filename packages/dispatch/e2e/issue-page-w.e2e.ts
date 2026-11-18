import { presetPath } from "../web/src/features/issue-page/issue-address";
import type { LineName } from "../web/src/features/issue-page/lenses/LensLines";
import { chipWords } from "../web/src/features/issue-page/node-row-chip";
import {
  CONTROL_LABELS,
  DECLARED_WORDS,
  LENS_ANSWERS,
  LENS_WORDS,
  NODE_TYPE_NAMES,
  type Preset,
  sentenceCase,
  wordsIn,
} from "../web/src/features/issue-page/page-words";
import { getIssueCoverage } from "./api";
import { nextFrames } from "./frames";
import { seedWholeIssue, type WholeIssueSeed } from "./issue-layout-seed";
import { gotoIssue, issuePage, issueRead, phase } from "./issue-page";
import {
  DESKTOP,
  type DrawnRow,
  drawnRows,
  focusRow,
  focusThePage,
  nodeRow,
  nodeRows,
  PHONE,
  parentOfRow,
  READER,
  rowOf,
  unfoldEveryBranch,
} from "./issue-page-g";
import { ownWords } from "./issue-page-j";
import {
  convertedGate,
  heldAnswer,
  owedUnless,
  type TreeAnswerWatch,
  treeAnswerOf,
  treeReadGate,
} from "./issue-page-t";
import {
  drawnIdOf,
  findingGate,
  focusedRowReadId,
  heldTreeAnswer,
  type LensKind,
  type LensLine,
  type LensSeed,
  lensAct,
  lensCatalogue,
  lensChainEntries,
  lensChipLinks,
  lensIdsIn,
  lensLines,
  lensMenu,
  lensMenuLine,
  lensReadIds,
  lensRows,
  lensShowChain,
  menuItems,
  seedLenses,
  treePagesOf,
} from "./issue-page-w";
import { expect, type Locator, type Page, type Route, test } from "./test";
import { type ApiRequest, expectLoadSettled, type RouteCount, watchApiRequests } from "./traffic";
import { assertPageFits } from "./unclipped";

/**
 * The browser rows of the lens rows of the new issue page: the rows of an issue's lenses,
 * findings, risks and constraints, the lines under them and under the statements a constraint
 * names, the chain a focused lens reads, and the Lens act that invokes a lens - read on the real
 * stack. Their titles and their messages are the ones the issue's browser checks name.
 *
 * Each row turns the new page on for alice with `phase` (`e2e/issue-page.ts`), seeds
 * `seedWholeIssue` (`e2e/issue-layout-seed.ts`) and, on it, `seedLenses`, opens the page with
 * `gotoIssue`, and reads it through the readers of `e2e/issue-page-g.ts` and of
 * `e2e/issue-page-w.ts`. A `@phone` row reads 1440 px on `chromium` and 390 px on `iphone`
 * (`rowSize`).
 *
 * Every node a row names, it names by the id the page's own tree read gave it
 * (`e2e/issue-page-t.ts`'s `treeAnswerOf`, read through `lensReadIds`), and every row it reads,
 * by the id the page drew it under for that read id (`drawnIdOf`): no row builds an id.
 *
 * Each row checks its gates first, each with `owedUnless` (`e2e/issue-page-t.ts`), and skips
 * where the server change a gate probes for is not on the base, with an `owed` annotation naming
 * what it waits on; it never passes then, so a row that could not run is never read as a pass.
 * Every row waits on the server serving the tree read, which `treeReadGate` reads as the read's
 * 404, and on the issue's conversion, the read's `converted: true` (`convertedGate`). These rows
 * carry no `@owed`: each runs for real once its gates are on the base. Row 2's clause on a
 * finding waits as well on the node model's commit 5c85ffdc1b, which stands a lens invoked on a
 * finding under it (`findingGate`, `e2e/issue-page-w.ts`): it is a row of its own, tagged
 * `@owed`, which checks the three gates the same way and asserts the clause once they are met.
 * Its title, "a finding's own row invokes a lens @owed", matches the `-g` of no check, so the
 * checks' runs, which invert no `@owed`, leave it out, and it stays out after its gates are met
 * too. The round that lands 5c85ffdc1b on the base gives the row back a title c-premortem's `-g`
 * matches, or adds the row to that pattern, and takes off `@owed`; until that round, no check
 * reads the clause.
 *
 * At a base that serves the tree read and holds none of the lens rows, each of rows 1 to 4 fails
 * on an assertion of its own, at the end of a 30 s poll and not at its deadline. Rows 1, 2 and 4
 * read first, of what the lens rows draw, the assertion their checks name, and fail there: row 1
 * on `the pre-mortem stands under statement 1`, row 2 on `the focused row offers the Lens act`
 * and row 4 on `the constraint stands at the top level`. Row 3 reads first the requests a load
 * sends, which hold at such a base too, since it reads no lens either, and fails on `the page
 * draws no row for the pre-mortem`.
 *
 * That base is the first half of a before-and-after run: a `git worktree` at the branch's
 * merge-base, with these test files and their helpers (`e2e/issue-page-w.ts`) copied in, and
 * with them this change's `page-words.ts`, which holds the words these files read
 * (`LENS_WORDS`, `LENS_ANSWERS` and the names of the four row types) and draws nothing. These
 * files import no module this change adds but for a type (`LineName`), which loads nothing, so
 * they load at that base.
 *
 * One departure from the checks' words, said where it stands: the write layer tries a 5xx again
 * under its key until the reader stops it or the key's day is spent (`writeUnderKey`), so row 2's
 * refused invocation answers its first attempt 500 and every later one 400, the refusal the page
 * does not try again.
 */

/** The four views, none of which draws a lens, a finding, a risk or a constraint. */
const VIEWS = ["spec", "verification", "plan", "decisions"] as const satisfies readonly Preset[];

/** The lens catalogue's read, which the menu of lens kinds sends as it opens. */
const CATALOGUE_READ = "**/api/v1/schema/blocks";

/** The invocation of a lens (`POST /api/v1/issues/{key}/lenses`). */
const INVOKE_ROUTE = "**/api/v1/issues/*/lenses";

/** A uuid in any text the page draws. */
const UUID = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/giu;

/** The issue `seedWholeIssue` makes, with what `seedLenses` wrote on it. */
interface LensIssue {
  readonly lenses: LensSeed;
  readonly whole: WholeIssueSeed;
}

async function seedLensIssue(): Promise<LensIssue> {
  const whole = await seedWholeIssue();
  return { lenses: await seedLenses(whole.key), whole };
}

/** The size a `@phone` row reads: 390 px on the `iphone` project, 1440 px on every other. */
function rowSize(): { readonly height: number; readonly width: number } {
  return test.info().project.name === "iphone" ? PHONE : DESKTOP;
}

/** The kind of the catalogue a lens was invoked as, by the id its 201 names: the server answers
 *  a lens's `kind` as the catalogue's id of the kind. */
function kindNamed(kinds: readonly LensKind[], kind: string): LensKind {
  const found = kinds.find((one) => one.id === kind);
  if (found === undefined) throw new Error(`the lens catalogue holds no kind ${kind}`);
  return found;
}

/** The id the page draws the node `readId` names under, once it draws it: each poll first opens
 *  every branch a fold left shut, which ends with the focus on no row. It fails with `message`,
 *  by default that the page draws no row for `what`. */
async function drawnId(
  page: Page,
  readId: string,
  what: string,
  message = `the page draws no row for ${what}`
): Promise<string> {
  await expect
    .poll(
      async () => {
        await unfoldEveryBranch(page);
        return drawnIdOf(page, readId);
      },
      { message, timeout: 30_000 }
    )
    .not.toBeNull();
  const id = await drawnIdOf(page, readId);
  if (id === null) throw new Error(message);
  return id;
}

/** The row the drawn row of `readId` sits under (`e2e/issue-page-g.ts`'s `parentOfRow`): null at
 *  the top level, and undefined while the page draws no row for it. */
async function drawnParentOf(page: Page, readId: string): Promise<string | null | undefined> {
  const id = await drawnIdOf(page, readId);
  return id === null ? undefined : parentOfRow(page, id);
}

/** The rows drawn directly under the row `id`, in order, by the tree's own `aria-level`. */
function childrenOf(rows: readonly DrawnRow[], id: string): DrawnRow[] {
  const at = rows.findIndex((row) => row.id === id);
  if (at === -1) return [];
  const level = Number(rows[at]?.level ?? "");
  const children: DrawnRow[] = [];
  for (const row of rows.slice(at + 1)) {
    const depth = Number(row.level ?? "");
    if (depth <= level) break;
    if (depth === level + 1) children.push(row);
  }
  return children;
}

/** One lens line as a row reads it: its label, its stored words, and each chip by its words and
 *  the read id its link names (`?node=`), or null for a chip drawn as words alone. */
interface LineReading {
  readonly label: string;
  readonly targets: readonly { readonly node: string | null; readonly words: string }[];
  readonly words: string;
}

/** The line `line` of `lines`, or undefined where the row draws none. */
function lineReading(lines: readonly LensLine[], line: LineName): LineReading | undefined {
  const found = lines.find((one) => one.line === line);
  return found === undefined
    ? undefined
    : {
        label: found.label,
        targets: found.chips.map((chip) => ({
          node:
            chip.href === null
              ? null
              : new URL(chip.href, "http://page.invalid").searchParams.get("node"),
          words: chip.words,
        })),
        words: found.words,
      };
}

/** How many times `pressFTo` presses `f` before it gives up: past every stop a row draws. */
const STOPS_AT_MOST = 40;

/** Presses `f` until `target` holds the focus, as a reader steps through the focused row's links
 *  and controls (`useTreeKeymap.ts`'s `rowStops`), and fails with `message` where none does. */
async function pressFTo(page: Page, target: Locator, message: string): Promise<void> {
  await expect(target.first(), message).toBeAttached();
  for (let pressed = 0; pressed < STOPS_AT_MOST; pressed += 1) {
    // `f` focuses the next stop inside its own keydown handler, so this read answers for it.
    if (await target.first().evaluate((element) => element === document.activeElement)) return;
    await page.keyboard.press("f");
  }
  await expect(target.first(), message).toBeFocused();
}

/** Marks the page's document, so a row can tell that a press loaded no other. */
async function markDocument(page: Page): Promise<void> {
  await page.evaluate(() => {
    Reflect.set(window, "lensRowsDocument", true);
  });
}

/** Whether the page still shows the document `markDocument` marked. */
function sameDocument(page: Page): Promise<boolean> {
  return page.evaluate(() => Reflect.get(window, "lensRowsDocument") === true);
}

/** Item `place` of the open menu of lens kinds. */
function menuItem(page: Page, place: number): Locator {
  return lensMenu(page).getByRole("menuitem").nth(place);
}

/** The one lens the newest read holds that `before` did not, once a read holds it. */
async function newLens(
  tree: TreeAnswerWatch,
  before: ReadonlySet<string>,
  what: string
): Promise<string> {
  const answer = await heldTreeAnswer(tree, `the tree read after ${what}`, (held) =>
    lensIdsIn(held).some((id) => !before.has(id))
  );
  const made = lensIdsIn(answer).filter((id) => !before.has(id));
  expect(made, `${what} made more than one lens`).toHaveLength(1);
  return made[0] ?? "";
}

/**
 * Invokes the catalogue's first kind on the node `target` names through its row's menu, as a
 * reader does - the focus on the row, `f` to the Lens act, Enter, Enter on the first kind - and
 * reads the new lens's row: focused, and under the row of `target`.
 */
async function invokeThroughMenu(
  page: Page,
  tree: TreeAnswerWatch,
  target: string,
  what: string
): Promise<void> {
  const held = await heldAnswer(tree, "the tree read");
  expect(
    held.nodes.some((node) => node.id === target),
    `the tree read holds no ${what}`
  ).toBe(true);
  const before = new Set(lensIdsIn(held));
  const targetId = await drawnId(page, target, `the ${what}`);
  const row = await focusRow(page, targetId);
  await pressFTo(page, lensAct(row), `the ${what}'s row offers no Lens act`);
  await page.keyboard.press("Enter");
  await expect(menuItem(page, 0), `the Lens menu on the ${what} lists no kind`).toBeFocused();
  await page.keyboard.press("Enter");
  const lens = await newLens(tree, before, `a lens on the ${what}`);
  await expect
    .poll(() => focusedRowReadId(page), {
      message: `the lens invoked on the ${what} does not hold the focus`,
    })
    .toBe(lens);
  expect(
    await drawnParentOf(page, lens),
    `the lens invoked on the ${what} does not stand under it`
  ).toBe(targetId);
}

/** Puts the focus on each row a view draws in turn - each of `ids` alone, where they are given -
 *  and fails where one offers the Lens act: the act is the outline's alone. */
async function expectNoLensAct(
  page: Page,
  key: string,
  view: Preset,
  ids?: readonly string[]
): Promise<void> {
  await gotoIssue(page, presetPath(key, view));
  await expect
    .poll(() => nodeRows(page).count(), { message: `the ${view} view draws no row` })
    .toBeGreaterThan(0);
  // A view's rows come from one read, merged whole before it draws, so none come later.
  const drawn = await drawnRows(page);
  const focused =
    ids ?? new Set(drawn.flatMap((row) => (row.visible && row.id !== null ? [row.id] : [])));
  for (const id of focused) {
    const row = nodeRow(page, id).first();
    await expect(row, `the ${view} view draws no row ${id}`).toHaveCount(1);
    await row.focus();
    await expect(row, `the ${view} view's row ${id} takes no focus`).toBeFocused();
    // A row draws its acts in the frame after it takes the focus.
    await nextFrames(page);
    await expect(
      lensAct(issuePage(page)),
      `the ${view} view's row ${id} offers the Lens act`
    ).toHaveCount(0);
  }
}

/** The reads of a lens (`GET /api/v1/lenses?on=`) `requests` hold, by the address each names. */
function chainReads(requests: readonly ApiRequest[]): (string | null)[] {
  return requests.flatMap((request) =>
    request.method === "GET" && request.route === "/api/v1/lenses"
      ? [new URL(request.path, "http://page.invalid").searchParams.get("on")]
      : []
  );
}

/** The reads of the lens catalogue (`GET /api/v1/schema/blocks`) `requests` hold. */
function catalogueReads(requests: readonly ApiRequest[]): string[] {
  return requests.flatMap((request) =>
    request.route === "/api/v1/schema/blocks" ? [request.path] : []
  );
}

// Row 1.
// SLOW(>=2s): a whole seeded issue with its lenses, every branch opened, each lens and risk
// focused in turn, two loads by `?node=`, two of the issue's address and one of each view.
test("lenses are drawn under their node with the chain that triggered them @phone", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  const size = rowSize();
  await phase(browser, seedLensIssue, async (page, { lenses, whole }) => {
    const key = whole.key;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await convertedGate(page, key));
    const plain = presetPath(key, "conversation");
    await page.setViewportSize(size);
    const tree = treeAnswerOf(page);
    const traffic = watchApiRequests(page);
    await gotoIssue(page, plain);
    const answer = await heldAnswer(tree, `the tree read of ${key}`);
    const ids = lensReadIds(answer, key, lenses);
    expect(
      answer.nodes.find((node) => node.id === ids.tradeOff)?.parent,
      "the tree read does not place the trade-off analysis under its first statement"
    ).toBe(ids.statements[0]);
    const kinds = await lensCatalogue();
    const tradeOffWords = kindNamed(kinds, lenses.tradeOff.kind).words;
    const preMortemWords = kindNamed(kinds, lenses.preMortem.kind).words;

    // (2), the first reading of what the lens rows draw: statement 1's row is drawn, and the
    // pre-mortem's row stands under it, by the tree's own levels.
    await expect
      .poll(
        async () => {
          await unfoldEveryBranch(page);
          const statement = await drawnIdOf(page, ids.statements[0] ?? "");
          return statement !== null && (await drawnParentOf(page, ids.preMortem)) === statement;
        },
        { message: "the pre-mortem stands under statement 1", timeout: 30_000 }
      )
      .toBe(true);
    const statementOne = await drawnId(page, ids.statements[0] ?? "", "statement 1");
    const tradeOff = await drawnId(page, ids.tradeOff, "the trade-off analysis");
    const preMortem = await drawnId(page, ids.preMortem, "the pre-mortem");

    // (1) After statement 1's other children, the trade-off analysis, then the pre-mortem: each
    // tagged by its kind's words, capitalised and cut as a chip is, whole in its body; each by
    // alice, with its time and its verdict, or the words that say it has none.
    const rows = await drawnRows(page);
    expect(
      childrenOf(rows, statementOne)
        .slice(-2)
        .map((row) => row.id),
      "under statement 1, after its other children, the trade-off analysis and then the pre-mortem"
    ).toEqual([tradeOff, preMortem]);
    for (const lens of [
      {
        id: tradeOff,
        kind: tradeOffWords,
        verdict: LENS_WORDS.no_verdict,
        what: "the trade-off analysis",
      },
      {
        id: preMortem,
        kind: preMortemWords,
        verdict: lenses.preMortem.verdict ?? "",
        what: "the pre-mortem",
      },
    ]) {
      const drawn = rowOf(rows, lens.id);
      expect(drawn?.words ?? drawn?.firstLine, `${lens.what} does not draw its verdict`).toBe(
        lens.verdict
      );
      expect(drawn?.author, `${lens.what} does not name alice`).toBe(READER);
      const row = nodeRow(page, lens.id).first();
      await expect(
        row.locator("[data-node-tag]"),
        `${lens.what}'s tag is not its kind's words, capitalised and cut`
      ).toHaveText(chipWords(sentenceCase(lens.kind)));
      await expect(row.locator("[data-node-time]"), `${lens.what} draws no time`).toHaveText(/\S/u);
      await focusRow(page, lens.id);
      expect(
        lineReading(await lensLines(row), "kind"),
        `${lens.what}'s body does not draw its kind's words whole`
      ).toEqual({ label: "", targets: [], words: sentenceCase(lens.kind) });
    }

    // (3) Each lens names the other by a chip, and a press on each chip goes to the other's row
    // on the same page.
    const tradeOffRow = nodeRow(page, tradeOff).first();
    const preMortemRow = nodeRow(page, preMortem).first();
    expect(
      lineReading(await lensLines(preMortemRow), "triggered_by"),
      "the pre-mortem does not read Triggered by with the trade-off analysis's chip"
    ).toEqual({
      label: LENS_WORDS.triggered_by,
      targets: [{ node: ids.tradeOff, words: chipWords(sentenceCase(tradeOffWords)) }],
      words: "",
    });
    expect(
      lineReading(await lensLines(tradeOffRow), "triggered"),
      "the trade-off analysis does not read Triggered with the pre-mortem's chip"
    ).toEqual({
      label: LENS_WORDS.triggered,
      targets: [{ node: ids.preMortem, words: chipWords(sentenceCase(preMortemWords)) }],
      words: "",
    });
    await markDocument(page);
    await focusRow(page, preMortem);
    await pressFTo(
      page,
      lensChipLinks(preMortemRow, "triggered_by"),
      "f does not reach the trade-off analysis's chip on the pre-mortem"
    );
    await page.keyboard.press("Enter");
    await expect
      .poll(() => focusedRowReadId(page), {
        message: "Enter on the trade-off analysis's chip does not focus its row",
      })
      .toBe(ids.tradeOff);
    await pressFTo(
      page,
      lensChipLinks(tradeOffRow, "triggered"),
      "f does not reach the pre-mortem's chip on the trade-off analysis"
    );
    await page.keyboard.press("Enter");
    await expect
      .poll(() => focusedRowReadId(page), {
        message: "Enter on the pre-mortem's chip does not focus its row",
      })
      .toBe(ids.preMortem);
    expect(await sameDocument(page), "a press on a lens's chip loaded the page again").toBe(true);

    // (4) The finding stands under the trade-off analysis.
    const finding = await drawnId(page, ids.finding, "the finding");
    expect(
      await parentOfRow(page, finding),
      "the finding does not stand under the trade-off analysis"
    ).toBe(tradeOff);
    await expect(
      nodeRow(page, finding).first().locator("[data-node-tag]"),
      "the finding's tag is not the word for a finding"
    ).toHaveText(NODE_TYPE_NAMES.finding);

    // (5) The early warning of risk A and the check that prevents it, whose chip goes to the
    // check; risk B, which nothing prevents, says so.
    const riskA = await drawnId(page, ids.riskA, "risk A");
    const riskB = await drawnId(page, ids.riskB, "risk B");
    const riskARow = nodeRow(page, riskA).first();
    const riskALines = await lensLines(riskARow);
    expect(
      lineReading(riskALines, "early_warning"),
      "risk A does not read Early warning: with its words"
    ).toEqual({ label: LENS_WORDS.early_warning, targets: [], words: lenses.riskA.early_warning });
    const checkText = answer.nodes.find((node) => node.id === ids.check1)?.text ?? "";
    expect(
      lineReading(riskALines, "prevented_by"),
      "risk A does not read Prevented by with check 1's chip"
    ).toEqual({
      label: LENS_WORDS.prevented_by,
      targets: [{ node: ids.check1, words: chipWords(checkText) }],
      words: "",
    });
    expect(
      lineReading(await lensLines(nodeRow(page, riskB).first()), "prevented_by_nothing"),
      "risk B does not read Prevented by nothing"
    ).toEqual({ label: LENS_WORDS.prevented_by_nothing, targets: [], words: "" });
    await focusRow(page, riskA);
    await pressFTo(
      page,
      lensChipLinks(riskARow, "prevented_by"),
      "f does not reach check 1's chip on risk A"
    );
    await page.keyboard.press("Enter");
    await expect
      .poll(() => focusedRowReadId(page), {
        message: "Enter on check 1's chip does not focus check 1's row",
      })
      .toBe(ids.check1);

    // (6) The pre-mortem's chain: one press reads it, and it draws the trade-off analysis, then
    // the pre-mortem, each with the first line the outline draws for statement 1, which both
    // stand under: the one line statement 1's own row draws while its branch is folded and no row
    // holds the focus.
    await nodeRow(page, statementOne)
      .first()
      .getByRole("button", { exact: true, name: CONTROL_LABELS.fold })
      .first()
      .click();
    await focusThePage(page);
    await expect
      .poll(async () => rowOf(await drawnRows(page), statementOne)?.firstLine ?? null, {
        message: "statement 1's folded row draws no first line",
      })
      .not.toBeNull();
    const statementLine = rowOf(await drawnRows(page), statementOne)?.firstLine;
    await unfoldEveryBranch(page);
    await focusRow(page, preMortem);
    await pressFTo(
      page,
      lensShowChain(preMortemRow),
      "f does not reach Show the whole chain on the pre-mortem"
    );
    await page.keyboard.press("Enter");
    await expect
      .poll(async () => (await lensChainEntries(preMortemRow)).map((entry) => entry.kind), {
        message: "Show the whole chain does not draw the trade-off analysis, then the pre-mortem",
      })
      .toEqual([tradeOffWords, preMortemWords]);
    await expect(
      preMortemRow.locator("[data-lens-chain] ol"),
      "Show the whole chain draws more than one chain"
    ).toHaveCount(1);
    // The chain's read is held for the load, so the poll above settled what this reads.
    for (const entry of await lensChainEntries(preMortemRow)) {
      expect(
        entry.line,
        `the chain's ${entry.kind} does not name statement 1 by its first line`
      ).toBe(statementLine);
    }

    // Every word of the page's own is declared, and a kind's words, which the catalogue
    // stores, are drawn as stored words and never as the page's.
    const chrome = await ownWords(issuePage(page));
    expect(
      [...new Set(chrome.flatMap(wordsIn).filter((word) => !DECLARED_WORDS.includes(word)))],
      "the lens rows draw words of the page's own that its list does not declare"
    ).toEqual([]);
    expect(
      chrome.filter((text) =>
        [tradeOffWords, preMortemWords].some((words) =>
          text.replace(/\s+/gu, " ").toLowerCase().includes(words.toLowerCase())
        )
      ),
      "a kind's words stand in the page's own words"
    ).toEqual([]);

    // (9) No text the page draws reads an address or a uuid.
    const text = await issuePage(page).innerText();
    expect(text.match(/dispatch:\/\/\S*/gu) ?? [], "the page draws an address").toEqual([]);
    expect(text.match(UUID) ?? [], "the page draws a uuid").toEqual([]);

    // (7) `?node=` with the id of risk B opens on risk B; with a uuid nobody holds, as the
    // issue's own address opens: the same rows in the same order, and the same focus.
    await gotoIssue(page, `${plain}?node=${encodeURIComponent(ids.riskB)}`);
    await expect
      .poll(() => focusedRowReadId(page), {
        message: "?node= with risk B's id does not open with risk B's row focused",
      })
      .toBe(ids.riskB);
    await gotoIssue(page, plain);
    await expectLoadSettled(traffic);
    const plainFocus = await focusedRowReadId(page);
    const plainRows = (await drawnRows(page)).map((row) => row.id);
    await gotoIssue(page, `${plain}?node=${crypto.randomUUID()}`);
    await expectLoadSettled(traffic);
    await expect(
      page.getByTestId("issue-column"),
      "?node= with a uuid nobody holds draws no issue"
    ).toBeVisible();
    expect(
      await focusedRowReadId(page),
      "?node= with a uuid nobody holds does not open as the issue's own address"
    ).toBe(plainFocus);
    await expect
      .poll(async () => (await drawnRows(page)).map((row) => row.id), {
        message:
          "?node= with a uuid nobody holds does not draw the rows of the issue's own address",
      })
      .toEqual(plainRows);

    // (8) No view draws a lens, a finding, a risk or a constraint.
    for (const view of VIEWS) {
      await gotoIssue(page, presetPath(key, view));
      await expect
        .poll(() => nodeRows(page).count(), { message: `the ${view} view draws no row` })
        .toBeGreaterThan(0);
      await expect(
        lensRows(page),
        `the ${view} view draws a lens, a finding, a risk or a constraint`
      ).toHaveCount(0);
    }
  });
});

// Row 2.
// SLOW(>=2s): a whole seeded issue with its lenses, seven invocations through the menu, a walk
// of every row of three views, a retried write and three loads that each read the catalogue.
test("a person invokes a lens from a node's menu @phone", async ({ browser }) => {
  test.setTimeout(360_000);
  const size = rowSize();
  await phase(browser, seedLensIssue, async (page, { lenses, whole }) => {
    const key = whole.key;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await convertedGate(page, key));
    const plain = presetPath(key, "conversation");
    await page.setViewportSize(size);
    const tree = treeAnswerOf(page);
    await gotoIssue(page, plain);
    const answer = await heldAnswer(tree, `the tree read of ${key}`);
    const ids = lensReadIds(answer, key, lenses);
    const kinds = await lensCatalogue();
    expect(kinds, "the lens catalogue does not hold its 22 kinds").toHaveLength(22);
    // Statement 2's address, as the coverage read gives it (as `seedLenses` reads statement 1's):
    // the `on` its row sends.
    const statementTwoAddress = (await getIssueCoverage(key)).requirements?.[1]?.ref;
    expect(statementTwoAddress, "the coverage read holds no statement 2").toBeDefined();

    // The first reading of what the lens rows draw: statement 2's row, found by the read id it
    // draws, offers the Lens act once it holds the focus.
    const statementTwo = await drawnId(
      page,
      ids.statements[1] ?? "",
      "statement 2",
      "the focused row offers the Lens act"
    );
    const row = await focusRow(page, statementTwo);
    await pressFTo(page, lensAct(row), "the focused row offers the Lens act");

    // Enter opens the menu, which says it is reading the catalogue while that read is held, and
    // then lists every kind in the catalogue's order with its first source's standard.
    const catalogue = Promise.withResolvers<void>();
    const holdCatalogue = async (route: Route) => {
      await catalogue.promise;
      await route.continue();
    };
    await page.route(CATALOGUE_READ, holdCatalogue);
    try {
      await page.keyboard.press("Enter");
      await expect(
        lensMenu(page),
        "Enter on the Lens act does not open the menu of lens kinds"
      ).toHaveAccessibleName(LENS_WORDS.menu_title);
      await expect(
        lensMenuLine(page),
        "the menu does not say it is reading the lens kinds"
      ).toHaveText(LENS_ANSWERS.loading_kinds);
    } finally {
      catalogue.resolve();
    }
    await expect
      .poll(() => menuItems(lensMenu(page)), {
        message: "the menu does not list each kind in the catalogue's order with its standard",
      })
      .toEqual(
        kinds.map((kind) => ({ standard: kind.sources?.[0]?.standard ?? "", words: kind.words }))
      );
    await page.unroute(CATALOGUE_READ, holdCatalogue);
    await assertPageFits(page);
    await page.keyboard.press("Escape");
    await expect(row, "Escape does not give the focus back to statement 2's row").toBeFocused();
    await expect(lensMenu(page), "Escape leaves the menu open").toHaveCount(0);

    // Again: two down arrows and Enter pick the third kind, which says it is examining while the
    // write is held, and lands on the new lens's row, focused, under statement 2.
    const before = new Set(lensIdsIn(await heldAnswer(tree, "the tree read")));
    await pressFTo(page, lensAct(row), "the focused row offers the Lens act");
    await page.keyboard.press("Enter");
    await expect(
      menuItem(page, 0),
      "the menu does not put the focus on its first kind"
    ).toBeFocused();
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("ArrowDown");
    await expect(menuItem(page, 2), "two down arrows do not reach the third kind").toBeFocused();
    const write = Promise.withResolvers<void>();
    const sent: unknown[] = [];
    const holdWrite = async (route: Route) => {
      if (route.request().method() === "POST") {
        sent.push(route.request().postDataJSON());
        await write.promise;
      }
      await route.continue();
    };
    await page.route(INVOKE_ROUTE, holdWrite);
    try {
      await page.keyboard.press("Enter");
      await expect(
        lensMenuLine(page),
        "the menu does not say it is examining the statement"
      ).toHaveText(LENS_ANSWERS.examining);
    } finally {
      write.resolve();
    }
    expect(sent, "the pick does not send the statement and the third kind alone").toEqual([
      { kind: kinds[2]?.id, on: statementTwoAddress },
    ]);
    const made = await newLens(tree, before, "the pick on statement 2");
    await page.unroute(INVOKE_ROUTE, holdWrite);
    await expect
      .poll(() => focusedRowReadId(page), {
        message: "the lens invoked on statement 2 does not hold the focus",
      })
      .toBe(made);
    const madeId = await drawnIdOf(page, made);
    expect(madeId, "the page draws no row for the lens invoked on statement 2").not.toBeNull();
    expect(
      await parentOfRow(page, madeId ?? ""),
      "the lens invoked on statement 2 does not stand under it"
    ).toBe(statementTwo);
    expect(
      rowOf(await drawnRows(page), madeId ?? "")?.author,
      "the lens invoked on statement 2 does not name alice"
    ).toBe(READER);
    await page.reload();
    await issueRead(page);
    // The tree the reload draws arrives after its column: each poll opens every branch a fold
    // left shut and reads statement 2's lenses as they are drawn then.
    await expect
      .poll(
        async () => {
          await unfoldEveryBranch(page);
          return childrenOf(await drawnRows(page), statementTwo)
            .filter((child) => child.type === "lens")
            .map((child) => child.id)
            .at(-1);
        },
        {
          message: "after a reload the lens invoked on statement 2 is not its last lens",
          timeout: 30_000,
        }
      )
      .toBe(madeId);

    // The act is the outline's alone: no row of the four views offers it - statement 2's row of
    // the Spec view, and every row of the other three.
    for (const view of VIEWS) {
      await expectNoLensAct(page, key, view, view === "spec" ? [statementTwo] : undefined);
    }

    // Each kind of node the route takes by its address takes the act: an open question, a
    // comment, a message, a lens, a risk and a constraint.
    await gotoIssue(page, plain);
    for (const [target, what] of [
      [whole.openQuestion.askId, "open question"],
      [whole.comments.threads[0] ?? "", "comment"],
      [whole.messages.root, "message"],
      [ids.tradeOff, "trade-off analysis"],
      [ids.riskA, "risk A"],
      [ids.standard, "standard constraint"],
    ] as const) {
      await invokeThroughMenu(page, tree, target, what);
    }

    // A catalogue read the server refuses at every attempt: the menu says the kinds could not be
    // read. A new load, since the catalogue is read once in a load.
    const refuseCatalogue = async (route: Route) => {
      await route.fulfill({ json: { code: "INTERNAL", error: "boom" }, status: 500 });
    };
    await page.route(CATALOGUE_READ, refuseCatalogue);
    await gotoIssue(page, plain);
    const refused = await focusRow(
      page,
      await drawnId(page, ids.statements[1] ?? "", "statement 2")
    );
    await pressFTo(page, lensAct(refused), "the focused row offers the Lens act");
    await page.keyboard.press("Enter");
    await expect(
      lensMenuLine(page),
      "a refused catalogue read does not say the lens kinds could not be read"
    ).toHaveText(LENS_ANSWERS.kinds_failed);
    await page.keyboard.press("Escape");
    await expect(refused, "Escape does not give the focus back to the row").toBeFocused();
    await page.unroute(CATALOGUE_READ, refuseCatalogue);

    // An invocation the server fails: the page tries a 5xx again under its key, so the first
    // attempt answers 500 and every later one 400, the refusal the write layer does not try
    // again. The row says it could not examine the node, as a status, and gains no lens.
    const lensesBefore = lensIdsIn(await heldAnswer(tree, "the tree read"));
    const rowsBefore = await lensRows(page).count();
    let attempts = 0;
    const failWrite = async (route: Route) => {
      if (route.request().method() !== "POST") {
        await route.continue();
        return;
      }
      attempts += 1;
      await route.fulfill({
        json: { code: attempts === 1 ? "INTERNAL" : "LENS_INPUT", error: "Dispatch refused it" },
        status: attempts === 1 ? 500 : 400,
      });
    };
    await page.route(INVOKE_ROUTE, failWrite);
    await pressFTo(page, lensAct(refused), "the focused row offers the Lens act");
    await page.keyboard.press("Enter");
    await expect(menuItem(page, 0), "the menu opened again lists no kind").toBeFocused();
    await page.keyboard.press("Enter");
    await expect(
      refused.getByRole("status").filter({ hasText: LENS_ANSWERS.failed }),
      "a failed invocation does not say, as a status, that it could not examine the node"
    ).toBeVisible();
    expect(attempts, "the page did not try the 500 again under its key").toBeGreaterThanOrEqual(2);
    await expect(refused, "the refusal does not give the focus back to the row").toBeFocused();
    expect(
      lensIdsIn(await heldAnswer(tree, "the tree read")),
      "a failed invocation made a lens"
    ).toEqual(lensesBefore);
    await expect(lensRows(page), "a failed invocation draws a row").toHaveCount(rowsBefore);
    await page.unroute(INVOKE_ROUTE, failWrite);

    // An invocation on a node the server no longer holds: the row says it is no longer here.
    const goneWrite = async (route: Route) => {
      if (route.request().method() !== "POST") {
        await route.continue();
        return;
      }
      await route.fulfill({ json: { code: "NOT_FOUND", error: "no such node" }, status: 404 });
    };
    await page.route(INVOKE_ROUTE, goneWrite);
    await pressFTo(page, lensAct(refused), "the focused row offers the Lens act");
    await page.keyboard.press("Enter");
    await expect(menuItem(page, 0), "the menu opened again lists no kind").toBeFocused();
    await page.keyboard.press("Enter");
    await expect(
      refused.getByRole("status").filter({ hasText: LENS_ANSWERS.gone }),
      "an invocation answered 404 does not say the node is no longer here"
    ).toBeVisible();
    await page.unroute(INVOKE_ROUTE, goneWrite);
  });
});

// Row 2's Lens act on a finding, owed until the node model's commit 5c85ffdc1b is on the base:
// titled so that no check's `-g` matches it, above all the one that picks rows 1 and 2
// (`lenses are drawn under their node|a person invokes a lens`), since no project inverts
// `@owed` and that run does not either.
// SLOW(>=2s): a whole seeded issue with its lenses, the gate's invocation and its tree read,
// every branch opened and one invocation through the menu.
test("a finding's own row invokes a lens @owed", async ({ browser }) => {
  test.setTimeout(120_000);
  await phase(browser, seedLensIssue, async (page, { lenses, whole }) => {
    const key = whole.key;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await convertedGate(page, key));
    owedUnless(await findingGate(page, key, lenses));
    const tree = treeAnswerOf(page);
    await gotoIssue(page, presetPath(key, "conversation"));
    const answer = await heldAnswer(tree, `the tree read of ${key}`);
    await invokeThroughMenu(page, tree, lensReadIds(answer, key, lenses).finding, "finding");
  });
});

// Row 3.
// SLOW(>=2s): two whole seeded issues each loaded to a 1.5 s quiet window, then `j` over every
// row, two chain presses, two menu openings and an invocation, each waited out to that window.
test("the first load adds no request for lenses and reads a chain only at a press", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  let without: readonly RouteCount[] = [];
  await phase(browser, seedWholeIssue, async (page, whole) => {
    owedUnless(await treeReadGate(page, whole.key));
    owedUnless(await convertedGate(page, whole.key));
    const tree = treeAnswerOf(page);
    const traffic = watchApiRequests(page);
    await gotoIssue(page, presetPath(whole.key, "conversation"));
    await expectLoadSettled(traffic);
    await heldAnswer(tree, `the tree read of ${whole.key}`);
    without = traffic.byRoute();
  });
  await phase(browser, seedLensIssue, async (page, { lenses, whole }) => {
    const key = whole.key;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await convertedGate(page, key));
    const tree = treeAnswerOf(page);
    const traffic = watchApiRequests(page);
    await gotoIssue(page, presetPath(key, "conversation"));
    await expectLoadSettled(traffic);
    const answer = await heldAnswer(tree, `the tree read of ${key}`);
    expect(
      traffic.byRoute(),
      "the load with lenses sends the requests of the load without them"
    ).toEqual(without);
    expect(chainReads(traffic.requests), "the load sends a read of a lens").toEqual([]);
    expect(catalogueReads(traffic.requests), "the load reads the lens catalogue").toEqual([]);
    const addresses = [
      lenses.tradeOff.address,
      lenses.preMortem.address,
      lenses.finding.address,
      lenses.riskA.address,
      lenses.riskB.address,
      lenses.standard.address,
      lenses.measurement.address,
      lenses.catalogueConstraint,
    ];
    expect(
      traffic.requests.flatMap((request) => {
        const url = new URL(request.path, "http://page.invalid");
        return url.pathname === "/api/v1/reference-targets"
          ? url.searchParams
              .getAll("ref")
              .filter(
                (ref) =>
                  addresses.includes(ref) || /^dispatch:\/\/(lens|risk|constraint)\//u.test(ref)
              )
          : [];
      }),
      "the batched reference read names a lens, a finding, a risk or a constraint"
    ).toEqual([]);
    const ids = lensReadIds(answer, key, lenses);

    // `j` over every row focuses each lens in turn, which reads nothing.
    await unfoldEveryBranch(page);
    // Every branch is open and the load has settled, so the rows drawn now are all of them.
    const count = await nodeRows(page).count();
    traffic.reset();
    await nodeRows(page).first().focus();
    for (let row = 1; row < count; row += 1) await page.keyboard.press("j");
    await expectLoadSettled(traffic);
    expect(chainReads(traffic.requests), "j over every row sends a read of a lens").toEqual([]);

    // One press on the pre-mortem's Show the whole chain reads its chain once; away, back and a
    // second press read nothing more.
    const preMortem = await drawnId(page, ids.preMortem, "the pre-mortem");
    const statementOne = await drawnId(page, ids.statements[0] ?? "", "statement 1");
    traffic.reset();
    for (const press of ["first", "second"]) {
      const preMortemRow = await focusRow(page, preMortem);
      await pressFTo(
        page,
        lensShowChain(preMortemRow),
        "f does not reach Show the whole chain on the pre-mortem"
      );
      await page.keyboard.press("Enter");
      await expect(
        preMortemRow.locator("[data-lens-chain-entry]"),
        `the ${press} press of Show the whole chain draws no chain of two lenses`
      ).toHaveCount(2);
      await expectLoadSettled(traffic);
      expect(
        chainReads(traffic.requests),
        `the ${press} press of Show the whole chain does not leave one read of the pre-mortem's chain`
      ).toEqual([lenses.preMortem.address]);
      await focusRow(page, statementOne);
    }

    // Two openings of the menu read the catalogue once.
    const preMortemRow = await focusRow(page, preMortem);
    traffic.reset();
    for (const opening of ["first", "second"]) {
      await pressFTo(page, lensAct(preMortemRow), "the pre-mortem's row offers no Lens act");
      await page.keyboard.press("Enter");
      await expect(
        menuItem(page, 0),
        `the ${opening} opening of the menu lists no kind`
      ).toBeFocused();
      if (opening === "first") await page.keyboard.press("Escape");
    }
    await expectLoadSettled(traffic);
    expect(
      catalogueReads(traffic.requests),
      "two openings of the menu read the lens catalogue more than once"
    ).toHaveLength(1);

    // An invocation sends its write and one read of the tree, one request for each page of it.
    const before = new Set(lensIdsIn(answer));
    const treePages = treePagesOf(page);
    traffic.reset();
    await page.keyboard.press("Enter");
    const made = await newLens(tree, before, "the invocation on the pre-mortem");
    await expect
      .poll(() => focusedRowReadId(page), {
        message: "the lens invoked on the pre-mortem does not hold the focus",
      })
      .toBe(made);
    await expectLoadSettled(traffic);
    const read = await treePages();
    expect(
      traffic.requests.map((request) => {
        const url = new URL(request.path, "http://page.invalid");
        return `${request.method} ${url.pathname}${url.searchParams.has("view") ? " with a view" : ""}`;
      }),
      "an invocation sends more than its write and one read of the tree"
    ).toEqual([
      `POST /api/v1/issues/${key}/lenses`,
      ...read.map(() => `GET /api/v1/issues/${key}/tree`),
    ]);
    expect(
      read.map((one) => one.cursor),
      "the tree read an invocation sends asks for a page its read did not name"
    ).toEqual([null, ...read.slice(0, -1).map((one) => one.next)]);
    expect(read.at(-1)?.next, "the tree read an invocation sends stops before its last page").toBe(
      null
    );
  });
});

// Row 4.
// SLOW(>=2s): a whole seeded issue with its lenses, every branch opened, a chip pressed, and a
// load of the Spec view and of `?node=`.
test("a constraint stands under the issue and a statement names it @phone", async ({ browser }) => {
  test.setTimeout(240_000);
  const size = rowSize();
  await phase(browser, seedLensIssue, async (page, { lenses, whole }) => {
    const key = whole.key;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await convertedGate(page, key));
    const plain = presetPath(key, "conversation");
    await page.setViewportSize(size);
    const tree = treeAnswerOf(page);
    await gotoIssue(page, plain);
    const answer = await heldAnswer(tree, `the tree read of ${key}`);
    const ids = lensReadIds(answer, key, lenses);

    // (1) The two constraints stand at the top level, in the read's order, each with its words,
    // its line, its tag, alice and its time.
    await expect
      .poll(
        async () => {
          await unfoldEveryBranch(page);
          return Promise.all([ids.standard, ids.measurement].map((id) => drawnParentOf(page, id)));
        },
        { message: "the constraint stands at the top level", timeout: 30_000 }
      )
      .toEqual([null, null]);
    const standard = await drawnId(page, ids.standard, "the standard constraint");
    const measurement = await drawnId(page, ids.measurement, "the measurement constraint");
    const rows = await drawnRows(page);
    expect(
      rows.flatMap((row) => (row.id === standard || row.id === measurement ? [row.id] : [])),
      "the two constraints are not drawn in the read's order"
    ).toEqual(
      answer.nodes.flatMap((node) =>
        node.id === ids.standard ? [standard] : node.id === ids.measurement ? [measurement] : []
      )
    );
    for (const constraint of [
      {
        id: standard,
        lines: [
          { chips: [], label: "", line: "reference", words: lenses.standard.reference ?? "" },
        ],
        what: "the standard constraint",
        words: lenses.standard.standard ?? "",
      },
      {
        id: measurement,
        lines: [
          {
            chips: [],
            label: LENS_WORDS.taken_at,
            line: "taken_at",
            words: lenses.measurement.taken_where ?? "",
          },
        ],
        what: "the measurement constraint",
        words: `${lenses.measurement.measurement ?? ""}: ${lenses.measurement.value ?? ""}`,
      },
    ]) {
      const drawn = rowOf(rows, constraint.id);
      expect(drawn?.words, `${constraint.what} does not read its words`).toBe(constraint.words);
      expect(drawn?.author, `${constraint.what} does not name alice`).toBe(READER);
      const row = nodeRow(page, constraint.id).first();
      expect(await lensLines(row), `${constraint.what} does not draw its line`).toEqual(
        constraint.lines
      );
      await expect(
        row.locator("[data-node-tag]"),
        `${constraint.what}'s tag is not the word for a constraint`
      ).toHaveText(NODE_TYPE_NAMES.constraint);
      await expect(row.locator("[data-node-time]"), `${constraint.what} draws no time`).toHaveText(
        /\S/u
      );
    }

    // (2) Statement 5 names the standard by a chip, which goes to the standard's row.
    const statementFive = await drawnId(page, ids.statements[4] ?? "", "statement 5");
    const fiveRow = nodeRow(page, statementFive).first();
    const standardChip = {
      label: LENS_WORDS.constrained_by,
      targets: [{ node: ids.standard, words: chipWords(lenses.standard.standard ?? "") }],
      words: "",
    };
    expect(
      lineReading(await lensLines(fiveRow), "constrained_by"),
      "statement 5 does not read Constrained by with the standard's chip"
    ).toEqual(standardChip);
    await focusRow(page, statementFive);
    await pressFTo(
      page,
      lensChipLinks(fiveRow, "constrained_by"),
      "f does not reach the standard's chip on statement 5"
    );
    await page.keyboard.press("Enter");
    await expect
      .poll(() => focusedRowReadId(page), {
        message: "Enter on the standard's chip does not focus the standard's row",
      })
      .toBe(ids.standard);

    // (3) Statement 6 names the catalogue's constraint by its standard, as words alone: the
    // catalogue's constraint is no row of the issue.
    const catalogueStandard = (await lensCatalogue())[0]?.sources?.[0]?.standard ?? "";
    const statementSix = await drawnId(page, ids.statements[5] ?? "", "statement 6");
    expect(
      lineReading(await lensLines(nodeRow(page, statementSix).first()), "constrained_by"),
      "statement 6's chip does not read the catalogue constraint's standard as words alone"
    ).toEqual({
      label: LENS_WORDS.constrained_by,
      targets: [{ node: null, words: chipWords(catalogueStandard) }],
      words: "",
    });

    // (4) Under Spec, statement 5 reads the same line, the standard's chip by its words.
    await gotoIssue(page, presetPath(key, "spec"));
    const specFive = nodeRow(page, statementFive).first();
    await expect(specFive, "the Spec view draws no row for statement 5").toHaveCount(1);
    await expect
      .poll(
        async () => {
          const line = lineReading(await lensLines(specFive), "constrained_by");
          return line === undefined
            ? undefined
            : { label: line.label, words: line.targets.map((target) => target.words) };
        },
        { message: "under Spec, statement 5 does not read Constrained by with the standard's chip" }
      )
      .toEqual({
        label: LENS_WORDS.constrained_by,
        words: [chipWords(lenses.standard.standard ?? "")],
      });

    // (5) `?node=` with the measurement's id focuses its row.
    await gotoIssue(page, `${plain}?node=${encodeURIComponent(ids.measurement)}`);
    await expect
      .poll(() => focusedRowReadId(page), {
        message: "?node= with the measurement's id does not open with its row focused",
      })
      .toBe(ids.measurement);
  });
});
