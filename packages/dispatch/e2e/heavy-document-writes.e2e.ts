// What a heavy body costs the server when it is written as a document: the API answers every one
// of these shapes in under a second, stored or refused. That the cost grows with the body rather
// than faster than it is held in Go over these same shapes, in CPU time
// (`packages/envoy/internal/dispatch/pmdoc/heavy_bodies_test.go`). The same shapes drawn in a
// browser are `heavy-bodies.e2e.ts`, and both specs read them from one module, so the two measure
// the very same bytes. This one drives no browser, so it runs in one Playwright project and
// nowhere else: every project of a run shares the one server process, so a second project would
// time these writes against a server that already holds what each shape touches, and what the row
// says below about the first document write of its shape would be false there
// (sjawhar/legion#2519).
import { DISPATCH_BODY_MAX } from "@legion/contracts/dispatch-tools";
import { expect, test } from "@playwright/test";
import { createIssue, createIssueArtifact, createProject } from "./api";
import {
  bracketRun,
  calloutBracketRun,
  lazyListItem,
  lazyQuote,
  markerChain,
  nested49ThenBlankLines,
  nestedThenBlankLines,
  plainList,
} from "./heavy-bodies";
import { sql } from "./psql";
import { resetDatabase } from "./seed";

const agent = {
  actor: { id: "e2e-heavy-documents-agent", kind: "session" as const },
  as: "agent" as const,
};

async function seedIssue(title: string): Promise<{ key: string }> {
  await createProject({ key: "CORE", name: "Core" });
  return await createIssue({ project: "CORE", title });
}

test.beforeEach(async () => {
  await resetDatabase();
});

interface HeavyDocument {
  /** The body an agent writes as a document of its own. */
  readonly body: string;
  /** What the row is called, in its assertions and in its annotations. */
  readonly name: string;
  /** Whether the API stores a write of this body, or refuses it as invalid markdown. */
  readonly stored: boolean;
  /** The one shape whose first write in a server process pays for the memory it touches rather
   *  than for the parse, which the row writes once before it measures anything (the table's own
   *  comment below). */
  readonly warm?: true;
}

/** Each heavy shape written as an issue document through the API, with the verdict Go gives it:
 *  stored, or refused as invalid markdown because it nests past `pmdoc/nesting.go`'s bound.
 *
 *  `warm` names the one shape whose cost is the parse itself rather than anything the write does
 *  with it, and whose first parse in a server process costs far more than its later ones. Its 49
 *  markers hold 50 blocks open down every one of the ~31,900 blank lines under them, and
 *  goldmark records a `lineStat` for each block on each line
 *  (`goldmark@v1.8.6/parser/parser.go:1106`), so the slice it appends into grows with the depth
 *  times the lines - about 3.1 million entries and ~75 MB for a body at the cap. Touching that
 *  much fresh memory for the first time in a process is page faults, not parsing: the same write
 *  is 316-343 ms once the process has done it and up to about 1.3 s the first time. The row
 *  writes such a body once to warm the process, records that first write's time, and holds the
 *  1 s bound against the second, which is the steady state a reader's writes meet. */
const documentRows: readonly HeavyDocument[] = [
  { body: plainList, name: "8,000 list items", stored: true },
  { body: lazyQuote, name: "a quote followed by lines without its marker", stored: true },
  { body: lazyListItem, name: "a list item followed by lines without its marker", stored: true },
  { body: bracketRun, name: "15,000 opening brackets then 15,000 closing ones", stored: true },
  {
    body: nested49ThenBlankLines,
    name: "49 list markers on one line then blank lines",
    stored: true,
    warm: true,
  },
  {
    body: nestedThenBlankLines,
    name: "100 list items nested by indentation then blank lines",
    stored: false,
  },
  { body: markerChain, name: "15,999 list markers on one line", stored: false },
  { body: calloutBracketRun, name: "a callout opener holding 15,000 brackets", stored: false },
];

/**
 * Resolves once this server owes nothing an earlier write left it: no settlement of a document
 * queued or running, and no document owed a block index.
 *
 * A durable document write records the settlement it owes in `doc_settlements_pending` within its
 * own transaction, and the settlement that has read every update deletes that row in the
 * transaction committing its writes (`packages/envoy/internal/dispatch/docs/persistence.go`,
 * `clearSettlementPending`). A version of more than 2,000 blocks leaves its blocks to the
 * block-index reconciliation instead of indexing them in the request
 * (`deferBlockIndexBlocks`, `docs/reference_excerpts.go`), and migration 0122's trigger owes that
 * work by leaving `document_block_versions.version` null, which the reconciliation reads and sets
 * again when it has indexed the document (`docs/block_index.go`,
 * `RunBlockIndexReconciliation`). An empty `doc_settlements_pending` and no null version in
 * `document_block_versions` is therefore the server with neither piece of work outstanding.
 *
 * The harness runs one worker against one database (`e2e/playwright.config.ts`) and
 * `resetDatabase` empties it before each row, so every owed row of either table is this row's own.
 */
async function everythingOwedFinished(what: string): Promise<void> {
  await expect
    .poll(
      () =>
        sql(
          "select (select count(*) from doc_settlements_pending) || ' settlements, ' || " +
            "(select count(*) from document_block_versions where version is null) || ' block indexes'"
        ),
      {
        intervals: [200],
        message: `${what}: work queued before this write never finished`,
        timeout: 60_000,
      }
    )
    .toBe("0 settlements, 0 block indexes");
}

/**
 * One write at a time, with the server otherwise idle. Each write leaves the server two pieces of
 * work, and both are waited out before the next write is timed. The first is the document's own
 * settlement, which the server runs two seconds later (the document service's own default
 * `settle`, `packages/envoy/internal/dispatch/docs/service.go`; `e2e/run-server.sh` sets no
 * `DISPATCH_TEST_SETTLE_DELAY`) and which costs about what the write did. The second is the block
 * index of a body of more than 2,000 blocks, which the write defers (`deferBlockIndexBlocks`,
 * `docs/reference_excerpts.go`) and the block-index reconciliation indexes on a tick of its own,
 * up to 30 s later (`docs/block_index.go`, `BlockIndexReconcileInterval`). Of the bodies here
 * only the 8,000-item list passes that bound - a list item is a block, so it holds 8,001 of
 * them, against one for the bracket run, two for each lazy body and fifty for the 49 markers
 * (`document_blocks` holds them all) - and the server says so as it writes it: "a document
 * version left its blocks to the block-index reconciliation ... blocks=8001 bound=2000".
 * Indexing 8,001 blocks is work that would otherwise land on whichever write the tick caught.
 * `write` therefore waits for both to be finished before it starts its clock - no settlement
 * queued and no document owed a block index (`everythingOwedFinished`) - the warming write
 * included, and what the bound below holds is one write alone. Those two ticks are what the row
 * spends most of its time waiting for: about 50 s of the 1.3 min the twelve writes it made,
 * while it also timed three of the bodies at half their count, took in Chromium on this pod.
 *
 * So what a write costs while another document of the same server settles beside it is not
 * measured here: measured with these writes back to back it reached 1,322 ms, which
 * sjawhar/legion#2519 names under Not proven / risk.
 *
 * One bound holds each body: the write answers in under 1,000 ms. It is asserted on every row,
 * stored or refused, under no condition, and it is never skipped, widened or stood down for a
 * slow node, because a second is what the writer waits and CI's runners are where that is
 * judged. What the write cost is annotated before the bound is asserted, so a run that reds
 * reports the number it read. The headroom is about twofold: 494 ms is the most any row asserted
 * over the eight `dispatch` CI jobs INFRA-985 measured.
 *
 * That the cost grows with the body rather than faster than it is held in Go instead, over these
 * same shapes at an eightfold step and in the CPU time the parse itself spends
 * (`TestHeavyBodiesAtTheBodyCapParseAndEncodeInTimeProportionalToTheirLength`,
 * `packages/envoy/internal/dispatch/pmdoc/heavy_bodies_test.go`). This row used to write three of
 * the shapes at half their count as well and hold the body's write to 3.5x that half, a bound
 * that could not separate the two things it was asked to tell apart: over those eight jobs the
 * server's own `elapsed_ms` for these writes read 1.90x to 2.36x on the honest runs and 3.57x on
 * the one that red, while a parse gone quadratic reads about 3.5x to 4.0x at a 2x step
 * (`growthStep`, `packages/envoy/internal/dispatch/pmdoc/linear_test.go`, has that arithmetic).
 * Two wall times a shared runner took, a doubling apart, cannot separate a regression from the
 * runner's own noise; eight times the input, measured in CPU time, can (INFRA-985).
 *
 * On every row but the 49-marker one the write the bound reads is the first document write of
 * its shape in the server process, which is what the bound has always measured over them, with
 * nothing but the drain above before it. On two of them it is not the first parse of those
 * bytes: in the one project this spec runs in, `heavy-bodies.e2e.ts` runs first on the same
 * server (one worker, files in name order) and creates comments holding the 8,000 items and the
 * bracket run, whose create parses each of them once. The 49-marker row is the exception: `warm`
 * has the row write that shape once before the write the bound reads, so what the bound reads
 * there is the second write of its shape - the steady state every later writer of such a body
 * meets, with the page faults of a first parse out of it - and the first write's own time is
 * annotated beside it and held to no bound (the table above).
 */
test("a document write of each heavy body answers in under a second, stored or refused", async () => {
  // Nine writes - one of every body and the warming write - each waiting out the settlement the
  // write before it queued, two seconds and the settlement's own run, and then the block index
  // the 8,000-item body defers, a tick up to 30 s away: the twelve writes this row made, while it
  // also timed three of the bodies at half their count, took 1.3 min here, past the default
  // deadline. The deadline below is twice that, so a tick at the far end of its interval still
  // leaves the row its measurements.
  test.setTimeout(180_000);
  const issue = await seedIssue("Heavy documents");

  /** Writes one body as a document of its own, with no settlement and no block index of an
   *  earlier write running beside it, and answers what the write cost and refused. */
  async function write(body: string, name: string): Promise<{ refusal: unknown; took: number }> {
    await everythingOwedFinished(name);
    const started = performance.now();
    const refusal = await createIssueArtifact(issue.key, { content: body, name }, agent).then(
      () => undefined,
      (error: unknown) => error
    );
    return { refusal, took: performance.now() - started };
  }

  for (const [index, row] of documentRows.entries()) {
    // A document takes far more than a comment does, so each row is held to the comment cap: the
    // browser rows of `heavy-bodies.e2e.ts` and this one then read the very same shapes.
    expect(
      row.body.length,
      `${row.name}: the row is longer than a comment's body cap`
    ).toBeLessThanOrEqual(DISPATCH_BODY_MAX);
    if (row.warm === true) {
      // The first parse of this shape in a server process pays for the memory it touches, not for
      // the parsing (the table above): write it once and record that, so the write the 1 s bound
      // reads is the one after it - every later writer's steady state, with a first parse's page
      // faults out of the bound rather than in it.
      const first = await write(row.body, `heavy-${index}-first.md`);
      expect(first.refusal, `${row.name}: the warming write was refused`).toBeUndefined();
      test.info().annotations.push({
        description: `${Math.round(first.took)} ms`,
        type: `${row.name}: the first write of this shape in a fresh server`,
      });
    }
    const { refusal, took } = await write(row.body, `heavy-${index}.md`);
    // What the write cost, so a reader of a CI run sees the number and not only that the row
    // passed.
    test.info().annotations.push({
      description: `${Math.round(took)} ms`,
      type: `${row.name}: what the write cost`,
    });

    if (row.stored) {
      expect(refusal, `${row.name}: the write was refused`).toBeUndefined();
    } else {
      expect(refusal, `${row.name}: the write was accepted`).toBeInstanceOf(Error);
      const message = refusal instanceof Error ? refusal.message : "";
      expect(message, row.name).toContain(" failed: 400 ");
      expect(message, row.name).toContain('"code":"INVALID_MARKDOWN"');
    }
    expect(took, `${row.name}: the write took ${Math.round(took)} ms`).toBeLessThan(1_000);
  }
});
