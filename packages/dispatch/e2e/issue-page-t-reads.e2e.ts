/** the rows count the tree and view reads the page sends the server and draw each filter from
 *  the view it answers */

import { PRESET_NAMES } from "../web/src/features/issue-page/page-words";
import { treeIdOf } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { seedWholeIssue } from "./issue-layout-seed";
import { closeDetails, openDetails, phase, presetCount } from "./issue-page";
import { nodeRow } from "./issue-page-g";
import { decisionsFoot } from "./issue-page-q";
import { fixtureSeed, seedKeys, steadily } from "./issue-page-t";
import {
  checksOutOfPlace,
  editNoticesSent,
  firstLoad,
  idSet,
  idsOfType,
  pastStaleTime,
  pressFilter,
  ROW_MS,
  releaseFilter,
  unexplainedDifferences,
  VIEW_FILTERS,
  VIEW_SEEDS,
  watchLiveFrames,
} from "./issue-page-t-rows";
import {
  besideViewRead,
  blocksReads,
  decisionsReadOf,
  drawnNodes,
  headerStatus,
  heldAnswer,
  linksTo,
  owedUnless,
  partUGate,
  R2_COUNTED_KINDS,
  type TreeAnswerWatch,
  treeAnswerOf,
  treeReadGate,
  treeReads,
  viewAnswerOf,
  viewCount,
  viewNodeIds,
} from "./issue-page-t-tree";
import { decisionsViewServedGate, heldDecisionsRead, idReadsOf } from "./issue-page-u";
import { expect, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";

/**
 * Rows 2 and 3 of the rows that draw the new issue page from the tree read, each clause of theirs
 * a later gate holds a row of its own: what the first load and each filter send of the tree read,
 * and each filter's rows as its view read's. The head of `e2e/issue-page-t-rows.ts` says what the
 * rows read and what their gates are.
 */

// ---------------------------------------------------------------------------------------------
// Row 2: what the first load and each filter send.
// ---------------------------------------------------------------------------------------------

// c-tree-requests, with the clause that the page's reads are refreshed by events, never by age:
// a slot the stage mounts again past the app's 30 s `staleTime` sends no read of its own, shown
// on Plan, whose slot reads only through `useIssueTree`.
// SLOW(>=2s): the whole seeded issue, its load waited out to a 1.5 s quiet window, Details
// opened, the seed's edit notices waited for, which the server sends about 30 s after a
// person's last version of each document, three filters each pressed on, off and on again with
// each press waited out, and each of Plan's two 31 s jumps of the clock held until the live
// socket's last frame is 2 to 10 s old, up to one 15 s heartbeat.
test("the first load reads the tree once and a filter its view once", async ({ browser }) => {
  test.setTimeout(ROW_MS);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    const views = {
      plan: viewAnswerOf(page, "plan"),
      spec: viewAnswerOf(page, "spec"),
      verification: viewAnswerOf(page, "verification"),
    } satisfies Record<(typeof VIEW_FILTERS)[number], TreeAnswerWatch>;
    const traffic = watchApiRequests(page);
    const live = watchLiveFrames(page);
    await page.clock.install();
    const load = await firstLoad(page, traffic, seeded.key);
    await expect(headerStatus(page), "the page draws no header").toBeVisible();
    expect(
      treeReads(load, seeded.key, null),
      "the first load sends one tree read with no view"
    ).toBe(1);
    expect(blocksReads(load), "the first load sends no blocks read").toEqual([]);
    expect(
      load.length,
      "the first load sends no more requests under /api/ than the reads did"
    ).toBeLessThanOrEqual(fixtureSeed("seedWholeIssue").loadRequests);
    await openDetails(page);
    await expectLoadSettled(traffic);
    expect(
      blocksReads(traffic.requests),
      "the first load, with the outline, Details and the header drawn, sends no blocks read"
    ).toEqual([]);
    await closeDetails(page);
    // A person wrote versions of the seed's documents, and the server sends each document's edit
    // notice about 30 s after that person's last version, a notice the page reads the issue's
    // detail again for. The Plan re-press below holds to no read at all, so the presses are
    // counted only once every notice is sent.
    await editNoticesSent(seeded.key, traffic);

    for (const filter of VIEW_FILTERS) {
      const name = PRESET_NAMES[filter];
      const late = filter === "plan";
      if (late) await pastStaleTime(page, traffic, live);
      const first = traffic.requests.length;
      await pressFilter(page, traffic, filter);
      await heldAnswer(views[filter], `the ${name} view read`);
      await expectLoadSettled(traffic);
      const pressed = traffic.requests.slice(first);
      expect(treeReads(pressed, seeded.key, filter), `${name} sends one view read`).toBe(1);
      expect(blocksReads(pressed), `${name} sends no blocks read`).toEqual([]);
      if (late) {
        expect(
          besideViewRead(pressed, seeded.key, filter),
          "a view pressed after 31 s sends no read"
        ).toEqual([]);
      }
      await releaseFilter(page, filter);
      if (late) await pastStaleTime(page, traffic, live);
      const again = traffic.requests.length;
      await pressFilter(page, traffic, filter);
      if (late) await expectLoadSettled(traffic);
      const repressed = traffic.requests.slice(again);
      expect(
        treeReads(repressed, seeded.key, filter),
        "a filter turned on again sends no view read"
      ).toBe(0);
      expect(blocksReads(repressed), `${name} turned on again sends no blocks read`).toEqual([]);
      if (late) expect(repressed, "a view pressed after 31 s sends no read").toEqual([]);
      await releaseFilter(page, filter);
    }
  });
});

// c-tree-requests' Decisions clause, owed until the server serves the tree read's Decisions view:
// the load sends no Decisions view read, the first press of Decisions in a load sends it once, and
// no press reads any document's blocks.
// SLOW(>=2s): the whole seeded issue, its load waited out to a 1.5 s quiet window, and Decisions
// pressed on and off twice with each press waited out.
test("the first load reads the tree once and a filter its view once, and Decisions reads its view once when pressed @owed", async ({
  browser,
}) => {
  test.setTimeout(ROW_MS);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    owedUnless(await decisionsViewServedGate(page, seeded.key));
    const traffic = watchApiRequests(page);
    const load = await firstLoad(page, traffic, seeded.key);
    for (const press of [1, 2]) {
      const from = traffic.requests.length;
      await pressFilter(page, traffic, "decisions");
      const pressed = traffic.requests.slice(from);
      if (press === 1) {
        expect(
          treeReads(load, seeded.key, "decisions"),
          "the load sends no Decisions view read"
        ).toBe(0);
      }
      expect(
        treeReads(pressed, seeded.key, "decisions"),
        "Decisions sends its view read once, on its first press in the load"
      ).toBe(press === 1 ? 1 : 0);
      expect(blocksReads(pressed), "Decisions reads no document's blocks").toEqual([]);
      await releaseFilter(page, "decisions");
    }
  });
});

// ---------------------------------------------------------------------------------------------
// Row 3: each filter's rows are its view read's.
// ---------------------------------------------------------------------------------------------

// c-views.
// SLOW(>=2s): two seeded issues, each load waited out, and three filters each pressed, read
// until it holds still, and released.
test("each filter draws its view read's rows", async ({ browser }) => {
  test.setTimeout(ROW_MS);
  for (const [name, seed] of VIEW_SEEDS) {
    await phase(browser, seed, async (page, seeded) => {
      owedUnless(await treeReadGate(page, seeded.key));
      const views = {
        plan: viewAnswerOf(page, "plan"),
        spec: viewAnswerOf(page, "spec"),
        verification: viewAnswerOf(page, "verification"),
      } satisfies Record<(typeof VIEW_FILTERS)[number], TreeAnswerWatch>;
      const traffic = watchApiRequests(page);
      await firstLoad(page, traffic, seeded.key);
      const reads = await idReadsOf(seeded.key);
      const slugs = (reads.issue.artifacts ?? []).map((artifact) => artifact.slug);
      const keys = seedKeys(seeded, slugs);
      const recorded = fixtureSeed(name);
      for (const filter of VIEW_FILTERS) {
        const view = PRESET_NAMES[filter];
        const where = `${name} under ${view}`;
        const drawn = await pressFilter(page, traffic, filter);
        const answer = await heldAnswer(views[filter], `the ${view} view read`);
        if (filter === "spec") {
          expect(idsOfType(drawn.nodes, "decision"), "no decision row under Spec").toEqual([]);
          for (const kind of ["constraint", "ask", "comment", "message", "check", "plan_task"]) {
            expect(idsOfType(drawn.nodes, kind), `no ${kind} row under Spec`).toEqual([]);
          }
          expect(idsOfType(drawn.nodes, "callout"), "no memo row under Spec").toEqual([]);
        }
        const expected = viewNodeIds(answer, filter, reads);
        expect(
          idSet(drawn.nodes.map((node) => node.id)),
          `${where}, the rows are the view read's nodes of root R2's row kinds`
        ).toEqual(idSet(expected.map((node) => node.id)));
        expect(
          idSet(drawn.nodes.flatMap((node) => (node.context ? [node.id] : []))),
          `${where}, the rows drawn as context are the view read's context`
        ).toEqual(idSet(expected.flatMap((node) => (node.role === "context" ? [node.id] : []))));
        if (filter === "verification") {
          expect(
            checksOutOfPlace(drawn.rows, answer, reads),
            `${where}, each check stands under each statement it serves`
          ).toEqual([]);
        }
        expect(
          drawn.count?.trim(),
          `${where}, the control counts the view read's own ${R2_COUNTED_KINDS[filter].join(", ")}`
        ).toBe(String(viewCount(answer, filter)));
        expect(
          unexplainedDifferences(filter, drawn.rows, keys, recorded),
          `${where}, each difference from the fixture's rows is a kind root R2 leaves out`
        ).toEqual([]);
        await releaseFilter(page, filter);
      }
    });
  }
});

// c-views' decision lines, owed until the server serves the tree read's Decisions view
// (`decisionsViewServedGate`) and the page draws the decisions from the tree read's nodes, once
// decisions are nodes of the tree read on this branch's base (`partUGate`, over
// `heldDecisionsRead`'s decisions read: the load sends none, so where the page received none it
// is the Decisions view the row reads itself): each decision, not replaced, whose `shaped` tie
// names a statement under Spec is named on that statement's line (`[data-spec-because]` in its
// row), never a row of its own, and the page links each decision it names there to its block
// (`spec-decided-by.ts`). So a decision is on the line where the line holds a link that
// `linksTo` reads as the decision's row id (`idOf`, as the row has it). A decision the model
// marks replaced (its node's `decision.replaced`) draws no line at all (root R5), so the row
// counts none.
// SLOW(>=2s): two seeded issues, each load waited out, and Spec pressed and read until it holds
// still.
test("each filter draws its view read's rows, and each decision that shaped a statement is a line on its row @owed", async ({
  browser,
}) => {
  test.setTimeout(ROW_MS);
  for (const [, seed] of VIEW_SEEDS) {
    await phase(browser, seed, async (page, seeded) => {
      owedUnless(await treeReadGate(page, seeded.key));
      owedUnless(await decisionsViewServedGate(page, seeded.key));
      const tree = treeAnswerOf(page);
      const decisions = decisionsReadOf(page, seeded.key);
      const spec = viewAnswerOf(page, "spec");
      const traffic = watchApiRequests(page);
      await firstLoad(page, traffic, seeded.key);
      owedUnless(
        partUGate(
          await heldAnswer(tree, "the tree read"),
          await heldDecisionsRead(seeded.key, decisions)
        )
      );
      const reads = await idReadsOf(seeded.key);
      const drawn = await pressFilter(page, traffic, "spec");
      const answer = await heldAnswer(spec, "the Spec view read");
      const idOf = treeIdOf(answer, reads);
      const nodes = new Map((answer.nodes ?? []).map((node) => [node.id, node]));
      const statements = new Set(drawn.rows.flatMap((row) => (row.id === null ? [] : [row.id])));
      const lines = (answer.tuples ?? []).flatMap((tuple) => {
        const decision = nodes.get(tuple.from.id);
        const statement = idOf(tuple.to.id);
        if (tuple.kind !== "shaped" || decision?.supertag !== "decision") return [];
        if (decision.decision?.replaced === true) return [];
        if (!statements.has(statement)) return [];
        return [{ row: idOf(tuple.from.id), statement }];
      });
      expect(
        lines.length,
        "the Spec view read holds a decision, not replaced, that shaped a statement the Spec " +
          "filter draws"
      ).toBeGreaterThan(0);
      const missing: string[] = [];
      for (const { row, statement } of lines) {
        // The Spec rows held still across two readings in `pressFilter` above, with the page
        // quiet, so one reading of a drawn row's line is the line that row settled on.
        const [links] = await nodeRow(page, statement)
          .first()
          .locator("[data-spec-because]")
          .evaluateAll((found) =>
            found.map((one) =>
              [...one.querySelectorAll("a[href]")].map((link) => link.getAttribute("href") ?? "")
            )
          );
        if (!(links ?? []).some((href) => linksTo(href, [row]))) {
          missing.push(
            `${statement}'s line holds no link to ${row}: ` +
              (links === undefined
                ? "its row draws no line"
                : `it links to ${links.length === 0 ? "nothing" : links.join(", ")}`)
          );
        }
      }
      expect(
        missing,
        "each decision, not replaced, that shaped a statement is named on that statement's line"
      ).toEqual([]);
      await releaseFilter(page, "spec");
    });
  }
});

// c-views for Decisions, owed until the server serves the tree read's Decisions view
// (`decisionsViewServedGate`, read before Decisions is pressed): the Decisions filter draws its
// view read's rows, as Spec, Verification and Plan draw theirs, which is how the root issue
// settled row 3's gap for Decisions, and a press that sends no view read fails the row.
// SLOW(>=2s): two seeded issues, each load waited out, and Decisions pressed, read until it holds
// still with its foot drawn unfolded as the view opens, and released.
test("each filter draws its view read's rows, and the Decisions filter draws its view read's rows @owed", async ({
  browser,
}) => {
  test.setTimeout(ROW_MS);
  for (const [name, seed] of VIEW_SEEDS) {
    await phase(browser, seed, async (page, seeded) => {
      owedUnless(await treeReadGate(page, seeded.key));
      owedUnless(await decisionsViewServedGate(page, seeded.key));
      const decisions = viewAnswerOf(page, "decisions");
      const traffic = watchApiRequests(page);
      await firstLoad(page, traffic, seeded.key);
      const reads = await idReadsOf(seeded.key);
      const view = PRESET_NAMES.decisions;
      const where = `${name} under ${view}`;
      await pressFilter(page, traffic, "decisions");
      const foot = decisionsFoot(page);
      // `pressFilter` above read the view once its rows and count held still with the network
      // quiet, so its foot is drawn by now or not at all, and one read says which. The foot is
      // drawn unfolded as the view opens (part U's R2), so its rows are read with nothing pressed.
      if ((await foot.count()) > 0) {
        await expect(
          foot,
          `${where}, the view's foot is drawn unfolded as the view opens`
        ).toHaveAttribute("aria-expanded", "true");
      }
      const drawn = await steadily(page, traffic, `${where} with its foot open`, async () => ({
        count: await presetCount(page, "decisions").textContent(),
        nodes: await drawnNodes(page),
      }));
      const answer = await heldAnswer(decisions, `the ${view} view read`);
      const expected = viewNodeIds(answer, "decisions", reads);
      expect(
        idSet(drawn.nodes.map((node) => node.id)),
        `${where}, the rows are the view read's nodes of root R2's row kinds`
      ).toEqual(idSet(expected.map((node) => node.id)));
      expect(
        idSet(drawn.nodes.flatMap((node) => (node.context ? [node.id] : []))),
        `${where}, the rows drawn as context are the view read's context`
      ).toEqual(idSet(expected.flatMap((node) => (node.role === "context" ? [node.id] : []))));
      expect(
        drawn.count?.trim(),
        `${where}, the control counts the view read's own ${R2_COUNTED_KINDS.decisions.join(", ")}`
      ).toBe(String(viewCount(answer, "decisions")));
      await releaseFilter(page, "decisions");
    });
  }
});
