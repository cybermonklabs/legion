/** the rows read Conversation against the tree read with kind the server answers for an issue
 *  a person and an agent wrote through the server, and count the tree reads its load sends */

import type { Page } from "@playwright/test";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import { GAP_LINES, PRESET_NAMES } from "../web/src/features/issue-page/page-words";
import { treeIdOf } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { createComment, createMessage, editArtifact } from "./api";
import { seedWholeIssue } from "./issue-layout-seed";
import { asPlanner } from "./issue-layout-seed-support";
import { requirementBlockId, SUMMARY_BLOCK_ID } from "./issue-layout-seed-text";
import { gotoIssue, phase } from "./issue-page";
import { blockNode, DESKTOP, expectOpeningLanded, nodeRow, PHONE, rowAct } from "./issue-page-g";
import { childEntry, childRow, expectActivePreset, focusedNodeId } from "./issue-page-i";
import type { RowReading } from "./issue-page-t";
import {
  anchoredComments,
  answerRowIds,
  artifactSlugs,
  awaitIndexedCoverage,
  firstRowOutOfPlace,
  foldedRowIds,
  idSet,
  nodesById,
  nodesNoRowDraws,
  type OpenedOutline,
  openOutline,
  ROW_MS,
  rowsNotWhole,
  servingOutOfPlace,
} from "./issue-page-t-rows";
import { kindsSent, outlineReads, owedUnless, treeReadGate } from "./issue-page-t-tree";
import { expect, test } from "./test";

/**
 * Row 5 of the rows that draw the new issue page from the tree read: Conversation draws
 * Conversation's read, the tree read with `kind=` repeated for spec R9's kinds, whole and with no
 * node folded. The head of `e2e/issue-page-t-rows.ts` says what the rows read and what their
 * gates are.
 */

/** The kinds Conversation's read names, as spec R9 lists them and as the tree route's `kind`
 *  filter names each: the asks, the comments, the readings, the lenses with their findings and
 *  risks, the criteria, the requirements, the checks, the decisions, the child issues, the blocks
 *  written in the conversation, and the memo's callout, which the three sentences draw. */
const R9_KINDS = [
  "ask",
  "callout",
  "check",
  "comment",
  "criterion",
  "decision",
  "finding",
  "issue",
  "lens",
  "prose",
  "reading",
  "requirement",
  "risk",
] as const;

/** The kinds Conversation draws no row of (spec R9): a result and a plan task are rows of their
 *  filters, and a constraint is a line on each requirement it constrains. */
const NO_CONVERSATION_ROW: readonly string[] = ["constraint", "plan_task", "result"];

/** The block id of the statement the planner writes under the message thread. */
const THREAD_STATEMENT = "r-from-thread";

/** How many lines the long post holds, each a paragraph of its own. */
const LONG_POST_LINES = 60;

/** What row 5 reads of the whole issue beyond `seedWholeIssue`'s. */
interface ConversationReadSeed {
  readonly key: string;
  /** The block id of the plan task row 5 opens by its address. */
  readonly planTask: string;
  readonly planSlug: string;
  /** The person's post of sixty lines, by its id. */
  readonly longPost: string;
  /** The person's message under which the planner wrote a statement, by its id. */
  readonly message: string;
  readonly specSlug: string;
  /** The first plan task's block id, the requirement it serves, and a check and a decision of
   *  the spec, each by its block id, whose tags the row presses. */
  readonly tagged: {
    readonly check: string;
    readonly decision: string;
    readonly statement: string;
  };
  readonly child: string;
}

/**
 * The whole issue (`seedWholeIssue`), which already holds a comment anchored to a statement's text,
 * plan tasks under statements and results under checks, with what it lacks: a person's post of
 * sixty lines, and a person's message under which the planner writes a statement into the spec,
 * through the route an agent writes through, by the statement's `parent` pointer.
 */
async function seedConversationRead(): Promise<ConversationReadSeed> {
  const seeded = await seedWholeIssue();
  const lines = Array.from(
    { length: LONG_POST_LINES },
    (_, index) => `Line ${index + 1} of the long post says one thing of its own.`
  );
  const longPost = await createComment(seeded.key, { body: lines.join("\n\n") });
  const message = await createMessage(seeded.key, {
    body: "The board needs one more statement, and this thread is where it came from.",
  });
  await editArtifact(
    seeded.specArtifactId,
    {
      ops: [
        {
          after: `block:${requirementBlockId(12)}`,
          markdown:
            `:::requirement{#${THREAD_STATEMENT} parent="dispatch://${seeded.key}/message/${message.id}"}\n` +
            "The statement written under the thread stands under its message.\n:::",
          op: "insert",
        },
      ],
    },
    asPlanner
  );
  const slugOf = await artifactSlugs(seeded.key);
  return {
    child: seeded.children.keys[0] ?? "",
    key: seeded.key,
    longPost: longPost.id,
    message: message.id,
    planSlug: slugOf(seeded.planArtifactId),
    planTask: seeded.planTasks[0] ?? "",
    specSlug: slugOf(seeded.specArtifactId),
    tagged: {
      check: seeded.checks.passed,
      decision: seeded.decisions[0] ?? "",
      statement: seeded.requirements[0] ?? "",
    },
  };
}

/** The read ids of the nodes of Conversation's answer that Conversation draws no row of: those no
 *  outline draws (`nodesNoRowDraws`), each node of a kind Conversation draws no row of
 *  (`NO_CONVERSATION_ROW`), and each plain block of a document, which Graph draws and
 *  Conversation keeps out (spec R10). */
function conversationLeftOut(opened: OpenedOutline, anchored: ReadonlySet<string>): Set<string> {
  const left = nodesNoRowDraws(opened, anchored, SUMMARY_BLOCK_ID);
  for (const node of opened.answer.nodes ?? []) {
    const documentProse =
      node.kind === "block" && node.supertag === "block" && node.document !== undefined;
    if (documentProse || NO_CONVERSATION_ROW.includes(node.supertag)) left.add(node.id);
  }
  return left;
}

/** `text` with no whitespace at all: a row's drawn words join its paragraphs with none, and the
 *  stored text with a blank line, so the two are compared without it. */
function unspaced(text: string | null | undefined): string {
  return (text ?? "").replace(/\s+/g, "");
}

/** The stored text the answer holds for the node whose row carries `id`. */
function storedText(opened: OpenedOutline, id: string): string | undefined {
  const idOf = treeIdOf(opened.answer, opened.reads);
  return (opened.answer.nodes ?? []).find((node) => idOf(node.id) === id)?.text;
}

/** Each statement row of `rows` that no check of the answer serves and that draws no gap for it:
 *  a requirement no check serves carries its gap (spec R9). */
function statementsWithoutGap(rows: readonly RowReading[], opened: OpenedOutline): string[] {
  const idOf = treeIdOf(opened.answer, opened.reads);
  const nodes = nodesById(opened.answer);
  const served = new Set(
    (opened.answer.tuples ?? []).flatMap((tuple) =>
      tuple.kind === "serves" && nodes.get(tuple.from.id)?.supertag === "check"
        ? [idOf(tuple.to.id)]
        : []
    )
  );
  return rows.flatMap((row) =>
    row.kind.type !== "requirement" ||
    row.id === null ||
    served.has(row.id) ||
    row.gaps.some((gap) => gap.kind === "no_check" && gap.line === GAP_LINES.no_check)
      ? []
      : [row.id]
  );
}

/** Each row of a kind that takes a reply - a comment, an ask and a message that is no context
 *  row - that draws no Reply, by its node id. */
function rowsWithoutReply(page: Page): Promise<string[]> {
  return page.evaluate(() =>
    Array.from(
      document.querySelectorAll<HTMLElement>(
        "[data-issue-page] [data-node-row]:is([data-node-type='comment'], [data-node-type='ask'], [data-node-type='message']):not([data-node-context])"
      )
    )
      .filter((row) => row.querySelector(":scope [data-row-act='reply']") === null)
      .map((row) => row.dataset.nodeId ?? "")
  );
}

/** Each kind tag a row draws as a link, with the row's node id and kind. */
function tagLinks(
  page: Page
): Promise<{ readonly href: string; readonly id: string; readonly type: string }[]> {
  return page.evaluate(() =>
    Array.from(
      document.querySelectorAll<HTMLElement>("[data-issue-page] [data-node-row]"),
      (row) => ({
        href:
          row
            .querySelector<HTMLAnchorElement>("[data-node-margin] a[data-node-tag-link]")
            ?.getAttribute("href") ?? "",
        id: row.dataset.nodeId ?? "",
        type: row.dataset.nodeType ?? "",
      })
    )
  );
}

/** The filter a row's kind tag opens its node in (spec R9), by the row's kind. */
const TAG_FILTERS = {
  check: "verification",
  decision: "decisions",
  requirement: "spec",
} as const;

// c-conversation-read.
// SLOW(>=2s): the whole seeded issue and what it lacks, Conversation loaded at two widths with
// each load waited out, three kind tags and a child's tag pressed, and a plan task's address
// loaded.
test("Conversation draws its read whole and unfolded @phone", async ({ browser }) => {
  test.setTimeout(2 * ROW_MS);
  await phase(browser, seedConversationRead, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    await awaitIndexedCoverage(seeded.key);
    const anchored = await anchoredComments(seeded.key);
    for (const size of [DESKTOP, PHONE]) {
      const where = `at ${size.width} px`;
      await page.setViewportSize(size);
      const opened = await openOutline(page, seeded.key, "conversation", {
        loaded: (load) => {
          expect(
            {
              withKind: outlineReads(load, seeded.key, "conversation"),
              withoutKind: outlineReads(load, seeded.key, "graph"),
            },
            `${where}, Conversation sends the tree read with kind and none without`
          ).toEqual({ withKind: 1, withoutKind: 0 });
          expect(
            kindsSent(load, seeded.key).map((kinds) => [...kinds].sort()),
            `${where}, Conversation's read names R9's kinds, none of them block`
          ).toEqual([[...R9_KINDS]]);
        },
      });
      expect(await foldedRowIds(page), `${where}, no node opens folded`).toEqual([]);

      const rows = opened.rows;
      expect(
        idSet(rows.flatMap((row) => (row.id === null ? [] : [row.id]))),
        `${where}, the drawn rows are the answer's nodes but the memo's callout and the comment on text`
      ).toEqual(answerRowIds(opened, conversationLeftOut(opened, anchored)));
      expect(
        firstRowOutOfPlace(`${where}, in Conversation`, rows, opened),
        `${where}, each row stands under the parent the answer names, in its order`
      ).toBeUndefined();
      expect(
        rows.flatMap((row) =>
          NO_CONVERSATION_ROW.includes(row.kind.type ?? "") ? [row.id ?? ""] : []
        ),
        `${where}, no result, plan-task or constraint row is drawn`
      ).toEqual([]);
      expect(
        servingOutOfPlace(rows, opened),
        `${where}, each check stands under each requirement it serves`
      ).toEqual([]);

      // The message under which the planner wrote a statement: one context row, whole, with its
      // kind, and no Reply; the statement stands under it (`firstRowOutOfPlace` above).
      const message = rows.find((row) => row.id === seeded.message);
      expect(message?.kind.type, `${where}, the message thread is no row of its kind`).toBe(
        "message"
      );
      await expect(
        nodeRow(page, seeded.message).first(),
        `${where}, the message thread is no context row`
      ).toHaveAttribute("data-node-context", "");
      expect(unspaced(message?.words), `${where}, the message thread is not drawn whole`).toBe(
        unspaced(storedText(opened, seeded.message))
      );
      await expect(
        rowAct(nodeRow(page, seeded.message).first(), "reply"),
        `${where}, the message thread's context row carries a Reply`
      ).toHaveCount(0);
      expect(
        rows.find((row) => row.id === blockNode(seeded.specSlug, THREAD_STATEMENT))?.kind.type,
        `${where}, the statement written under the thread is no row`
      ).toBe("requirement");

      expect(
        statementsWithoutGap(rows, opened),
        `${where}, each requirement that no check serves carries its gap`
      ).toEqual([]);
      expect(await rowsNotWhole(page), `${where}, each row's text is whole`).toEqual([]);
      expect(
        unspaced(rows.find((row) => row.id === seeded.longPost)?.words),
        `${where}, the 60-line post is drawn whole, as the answer's text`
      ).toBe(unspaced(storedText(opened, seeded.longPost)));
      expect(
        await rowsWithoutReply(page),
        `${where}, each row that takes a reply draws Reply`
      ).toEqual([]);
      await expect(
        nodeRow(page, seeded.longPost).first().locator('[data-row-act="edit"]'),
        `${where}, the person's own comment draws no Edit`
      ).toHaveCount(1);
    }

    // Each kind tag opens its node in its own view: a check's Verification, a statement's Spec, a
    // decision's Decisions, each at the node's block.
    await page.setViewportSize(DESKTOP);
    await gotoIssue(page, presetPath(seeded.key, "conversation"));
    await expectOpeningLanded(page);
    const tags = await tagLinks(page);
    expect(
      tags.flatMap(({ href, id, type }) => {
        const filter = TAG_FILTERS[type as keyof typeof TAG_FILTERS];
        if (filter === undefined) return [];
        const block = id.slice(id.indexOf("#b-") + "#b-".length);
        const want = `${presetPath(seeded.key, filter)}#b-${block}`;
        return href === want ? [] : [`the ${type} ${id} tags ${href || "nothing"}, not ${want}`];
      }),
      "each check's, statement's and decision's tag opens its node in its view"
    ).toEqual([]);
    for (const [type, block] of [
      ["check", seeded.tagged.check],
      ["requirement", seeded.tagged.statement],
      ["decision", seeded.tagged.decision],
    ] as const) {
      const filter = TAG_FILTERS[type];
      const slug = type === "check" ? undefined : seeded.specSlug;
      const row = page
        .locator(`[data-issue-page] [data-node-row][data-node-type="${type}"]`)
        .filter({ has: page.locator(`:scope[data-node-id$="#b-${block}"]`) })
        .first();
      await gotoIssue(page, presetPath(seeded.key, "conversation"));
      await expectOpeningLanded(page);
      await row.locator("[data-node-margin] a[data-node-tag-link]").click();
      await expect(
        page,
        `the ${type} ${block}'s tag did not open ${PRESET_NAMES[filter]}`
      ).toHaveURL(new RegExp(`${presetPath(seeded.key, filter)}#b-${block}$`));
      await expectActivePreset(page, PRESET_NAMES[filter]);
      if (slug !== undefined) {
        await expect
          .poll(() => focusedNodeId(page), {
            message: `the ${type} ${block}'s tag did not open ${PRESET_NAMES[filter]} at the node`,
          })
          .toBe(blockNode(slug, block));
      }
    }
    // A child's tag opens the child in the children's section.
    await gotoIssue(page, presetPath(seeded.key, "conversation"));
    await expectOpeningLanded(page);
    await childRow(page, seeded.child).first().locator("[data-child-key]").click();
    await expect(
      childEntry(page, seeded.child).locator("[data-child-key]"),
      "a child's tag does not open the children's section at the child"
    ).toBeFocused();

    // An address of a plan task, which Conversation draws no row of, opens Plan at it.
    await gotoIssue(page, `/issues/${seeded.key}#b-${seeded.planTask}`);
    await expectActivePreset(page, PRESET_NAMES.plan);
    await expect
      .poll(() => focusedNodeId(page), {
        message: "the plan task's address does not open Plan focused on it",
      })
      .toBe(blockNode(seeded.planSlug, seeded.planTask));
  });
});
