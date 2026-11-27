/** the rows count the tree and view reads the page sends the server and draw each filter from
 *  the view it answers */

import { conversationItemId, eventItemId, pinnedItemMarker } from "../web/src/features/issue/pins";
import { coverageNotes, PRESET_NAMES } from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/tree-drafts";
import { treeIdOf } from "../web/src/features/issue-page/tree-read/tree-nodes";
import {
  createComment,
  createIssueArtifact,
  deleteComment,
  getIssue,
  getIssueCoverage,
  putIssueState,
} from "./api";
import { seedNoChildren, seedWholeIssue } from "./issue-layout-seed";
import { closeDetails, gotoIssue, issuePage, openDetails, phase, presetCount } from "./issue-page";
import { focusRow, nodeRow } from "./issue-page-g";
import { decisionsFoot } from "./issue-page-q";
import { fixtureSeed, seedKeys, steadily } from "./issue-page-t";
import {
  architectureSettled,
  awaitIndexedCoverage,
  checksOutOfPlace,
  editNoticesSent,
  firstLoad,
  idSet,
  idsOfType,
  LOAD_BOUND,
  loadViews,
  pastStaleTime,
  pressFilter,
  ROW_MS,
  releaseFilter,
  restatingSections,
  unexplainedDifferences,
  VIEW_FILTERS,
  VIEW_SEEDS,
  watchLiveFrames,
} from "./issue-page-t-rows";
import {
  besideViewRead,
  blocksReads,
  coverageIndexEventGate,
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
import { decisionsViewServedGate, idReadsOf } from "./issue-page-u";
import { expect, test } from "./test";
import { type ApiRequest, expectLoadSettled, watchApiRequests } from "./traffic";

/**
 * Rows 2 and 3 of the rows that draw the new issue page from the tree read, each clause of theirs
 * a later gate holds a row of its own: what the first load and each filter send of the tree read,
 * and each filter's rows as its view read's. The head of `e2e/issue-page-t-rows.ts` says what the
 * rows read and what their gates are.
 */

// ---------------------------------------------------------------------------------------------
// Row 2: what the first load and each filter send.
// ---------------------------------------------------------------------------------------------
/** Every read of an issue's whole comments list among `requests`. */
function commentsListReads(requests: readonly ApiRequest[]): readonly ApiRequest[] {
  return requests.filter(
    (request) => request.method === "GET" && request.route === "/api/v1/issues/:id/comments"
  );
}

/** Includes a pinned comment and a deleted post in the first load. */
async function seedCommentsLoad(): ReturnType<typeof seedWholeIssue> {
  const seeded = await seedWholeIssue();
  const removed = await createComment(seeded.key, { body: "A post since deleted." });
  await deleteComment(removed.id);
  await putIssueState(seeded.key, {
    dismissed: [
      pinnedItemMarker(eventItemId({ id: seeded.pinnedEventId })),
      pinnedItemMarker(conversationItemId("comment", seeded.comments.anchored)),
    ],
    pinned: true,
    seq: 2,
  });
  return seeded;
}

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
  await phase(browser, seedCommentsLoad, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    // The seed's architecture source holds now what the server's own import pass would find, so
    // that pass sends no event the page answers with reads while this row counts them.
    await architectureSettled(seeded.key);
    const views = {
      plan: viewAnswerOf(page, "plan"),
      spec: viewAnswerOf(page, "spec"),
      verification: viewAnswerOf(page, "verification"),
    } satisfies Record<(typeof VIEW_FILTERS)[number], TreeAnswerWatch>;
    const conversation = treeAnswerOf(page);
    const traffic = watchApiRequests(page);
    const live = watchLiveFrames(page);
    await page.clock.install();
    const load = await firstLoad(page, traffic, seeded.key);
    // The load's requests, printed so what the page sent is on the record when the row passes too.
    console.log(
      `FIRST-LOAD [${test.info().project.name}] ${seeded.key}: ${load.length} requests: ` +
        load.map((request) => `${request.method} ${request.path}`).join(" | ")
    );
    await expect(headerStatus(page), "the page draws no header").toBeVisible();
    expect(
      treeReads(load, seeded.key, null),
      "the first load sends one tree read with no view"
    ).toBe(1);
    expect(blocksReads(load), "the first load sends no blocks read").toEqual([]);
    // The load's one asks read is the outline's, `asks?thread=1`, and no plain asks read stands
    // beside it (R5, decision 441).
    const asksReads = load.flatMap((request) => {
      const url = new URL(request.path, "http://requests.invalid");
      return request.method === "GET" && url.pathname === `/api/v1/issues/${seeded.key}/asks`
        ? [url]
        : [];
    });
    expect(
      asksReads.filter((url) => url.searchParams.get("thread") === "1").length,
      "the first load sends one asks read with thread=1"
    ).toBe(1);
    expect(
      asksReads.flatMap((url) =>
        url.searchParams.has("thread") ? [] : [`${url.pathname}${url.search}`]
      ),
      "the first load sends no plain asks read"
    ).toEqual([]);
    // Each read of the load is sent once: the fixture's count leaves room for requests the page
    // no longer sends, so it does not catch a read sent twice, the coverage's among them.
    const sent = load.map((request) => `${request.method} ${request.path}`);
    expect(
      sent.filter((one, at) => sent.indexOf(one) !== at),
      "the first load sends no request twice"
    ).toEqual([]);
    expect(
      load.length,
      `the first load sends at most ${LOAD_BOUND} requests under /api/`
    ).toBeLessThanOrEqual(LOAD_BOUND);
    // The view reads R4 sends with a load of Conversation for this seed's issue, which is a child
    // (the seed files it under a parent) and a parent whose Conversation's answer holds no check
    // of a child: Plan's always, Spec's for a child, Verification's for such a parent. The
    // fixture's count holds the decisions read the page sent when it was recorded
    // (`GET /api/v1/issues/{key}/decisions`), which this load sends no read in place of: every
    // decision is a node of the tree read, and the Decisions view's read waits for the filter's
    // first press.
    const issue = await getIssue(seeded.key);
    const answer = await heldAnswer(conversation, "Conversation's read");
    const extras = loadViews(issue, answer);
    const recorded = fixtureSeed("seedWholeIssue").loadRequests;
    // Every request of the load but the first page of each of those view reads, in the order sent.
    const counted = load.filter(
      (request) => !extras.some((view) => treeReads([request], seeded.key, view) > 0)
    );
    expect(
      counted.length,
      `each request beyond the fixture's count is one of R4's view reads for the seed: ${extras
        .map((view) => `view=${view}`)
        .join(", ")}; past the fixture's ${recorded} the load sent ${counted
        .slice(recorded)
        .map((request) => `${request.method} ${request.path}`)
        .join(", ")}`
    ).toBeLessThanOrEqual(recorded);
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
    // counted only once every notice is sent, or held by the server (`editNoticesSent`).
    await editNoticesSent(seeded.key, traffic);
    await pressFilter(page, traffic, "decisions");
    await releaseFilter(page, "decisions");
    expect(
      commentsListReads(traffic.requests),
      "the load, opening Details and pressing Decisions send no whole-issue comments read"
    ).toEqual([]);

    for (const filter of VIEW_FILTERS) {
      const name = PRESET_NAMES[filter];
      const late = filter === "plan";
      // A view R4 sent with the load is held, and its filter turned on sends no second read.
      const withLoad = extras.includes(filter);
      if (late) await pastStaleTime(page, traffic, live);
      const first = traffic.requests.length;
      await pressFilter(page, traffic, filter);
      await heldAnswer(views[filter], `the ${name} view read`);
      await expectLoadSettled(traffic);
      const pressed = traffic.requests.slice(first);
      expect(
        treeReads(pressed, seeded.key, filter),
        withLoad ? `${name}, read with the load, sends no view read` : `${name} sends one view read`
      ).toBe(withLoad ? 0 : 1);
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
    expect(
      commentsListReads(traffic.requests),
      "the load, Details and every filter send no whole-issue comments read"
    ).toEqual([]);
    // Once a document's editor opens on the stage, the margin in Details draws its comment cards
    // beside it and sends one whole-issue comments read.
    await pressFilter(page, traffic, "spec");
    await openDetails(page);
    await expectLoadSettled(traffic);
    expect(commentsListReads(traffic.requests), "Details sends no comments read by itself").toEqual(
      []
    );
    const specSlug =
      issue.artifacts?.find((artifact) => artifact.id === issue.primary_artifact_id)?.slug ??
      "spec";
    const statement = await focusRow(page, blockNodeId(specSlug, seeded.requirements[0]));
    await page.keyboard.press("Enter");
    await expect(
      statement.locator("[data-editor-box] .dispatch-doc"),
      "Enter on a statement row did not open the spec's editor"
    ).toBeVisible();
    await expect
      .poll(() => commentsListReads(traffic.requests).length, {
        message: "opening a document's editor did not send one whole-issue comments read",
      })
      .toBe(1);
    await expectLoadSettled(traffic);
    expect(
      commentsListReads(traffic.requests),
      "opening a document's editor sent more than one whole-issue comments read"
    ).toHaveLength(1);
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

// SLOW(>=2s): the page opens over deferred coverage and waits for the next block-index pass.
test("the line that says a version is still being indexed goes without a reload @owed", async ({
  browser,
}) => {
  owedUnless(coverageIndexEventGate);
  test.setTimeout(ROW_MS);
  await phase(browser, seedNoChildren, async (page, seeded) => {
    // An upload beyond the server's block bound defers indexing to the reconciliation.
    const name = "index-sample.md";
    const content = Array.from({ length: 2_001 }, (_, index) => `- Item ${index + 1}`).join("\n");
    await createIssueArtifact(seeded.key, { content, name });
    const [pending] = coverageNotes(await getIssueCoverage(seeded.key)).filter((line) =>
      line.startsWith(`${name} version `)
    );
    expect(pending, "the uploaded document's coverage did not defer").toBeDefined();
    await gotoIssue(page, `/issues/${seeded.key}`);
    const notices = issuePage(page)
      .locator("[data-coverage-note]")
      .filter({ hasText: pending ?? "" });
    await expect(notices.first(), "the page does not show the pending coverage line").toBeVisible();
    let navigated = false;
    page.on("framenavigated", (frame) => {
      if (frame === page.mainFrame()) navigated = true;
    });
    await awaitIndexedCoverage(seeded.key);
    await expect(notices, "the indexed document still draws its coverage line").toHaveCount(0);
    expect(navigated, "the page reloaded to clear the coverage line").toBe(false);
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

// c-views' decision lines, owed until the page draws the decisions from the tree read's nodes
// (`partUGate`): each decision, not replaced, whose `shaped` tie names a statement under Spec is
// named on that statement's line (`[data-spec-because]` in its row), never a row of its own, and
// the page links each decision it names there to its block (`spec-decided-by.ts`). So a decision
// is on the line where the line holds a link that `linksTo` reads as the decision's row id
// (`idOf`, as the row has it). A decision the model marks replaced (its node's
// `decision.replaced`) draws no line at all (root R5), so the row counts none.
// SLOW(>=2s): two seeded issues, each load waited out, and Spec pressed and read until it holds
// still.
test("each filter draws its view read's rows, and each decision that shaped a statement is a line on its row @owed", async ({
  browser,
}) => {
  owedUnless(partUGate);
  test.setTimeout(ROW_MS);
  for (const [, seed] of VIEW_SEEDS) {
    await phase(browser, seed, async (page, seeded) => {
      owedUnless(await treeReadGate(page, seeded.key));
      const spec = viewAnswerOf(page, "spec");
      const traffic = watchApiRequests(page);
      await firstLoad(page, traffic, seeded.key);
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
// (`decisionsViewServedGate`, read before Decisions is pressed) and on the page drawing the
// decisions from the tree read's nodes, whose parts the Decisions filter draws (`partUGate`): the
// Decisions filter draws its view read's rows, as Spec, Verification and Plan draw theirs, which
// is how the root issue settled row 3's gap for Decisions, and a press that sends no view read
// fails the row.
// SLOW(>=2s): two seeded issues, each load waited out, and Decisions pressed, read until it holds
// still with its foot drawn unfolded as the view opens, and released.
test("each filter draws its view read's rows, and the Decisions filter draws its view read's rows @owed", async ({
  browser,
}) => {
  owedUnless(partUGate);
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
      // A section that restates a person's answer is no row and is counted nowhere (decisions 513
      // and 521), so the rows and the count are the view read's but those sections.
      const restating = restatingSections(answer, reads);
      const expected = viewNodeIds(answer, "decisions", reads).filter(
        (node) => !restating.drawn.has(node.id)
      );
      // Each once, by its read id, as `viewCount` counts the view's nodes.
      const counted = new Set(
        (answer.nodes ?? []).flatMap((node) =>
          node.role === "query" && restating.read.has(node.id) ? [node.id] : []
        )
      ).size;
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
      ).toBe(String(viewCount(answer, "decisions") - counted));
      await releaseFilter(page, "decisions");
    });
  }
});
