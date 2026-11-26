import type { Artifact, Decision } from "../../api/types";
import { blockFragment } from "../refs/block-reference";
import { buildIssuePath, documentItemPath } from "../refs/routes";
import type { CardDecision, CardLink, CardPaths } from "./DecisionCard";

/** A statement's words, as the caller's batched read of the statements' names gives them: the
 *  words and the server's reading of them, or no words until the read answers for it. */
export type StatementWords = Pick<CardLink, "text" | "text_tree">;

/**
 * A decision of the bare `decisions` member of `GET /api/v1/issues/{key}/decisions`
 * (`model.Decision`) as the card draws it (R20): its `unsure` is the level, and the read holds no
 * author, no level and no reason apart from the text. Only a tie to a block is a statement it
 * shaped, as the tab and the margin read the ties on `main`. A Keep names its person by login.
 */
export function fromDecision(decision: Decision): CardDecision {
  return {
    asks: decision.asks ?? [],
    changed_at: decision.changed_at,
    heading: decision.heading,
    kept_by: decision.kept === null ? undefined : { id: decision.kept.kept_by, kind: "user" },
    kind: decision.kind === "person" ? "person" : "agent",
    links: (decision.links ?? []).flatMap((link) =>
      link.to.block_id === undefined
        ? []
        : [
            {
              block_id: link.to.block_id,
              changed_at: link.to.changed_at,
              created_at: link.created_at,
              ref: link.to.ref,
              stale: link.stale,
              text: link.to.text,
            },
          ]
    ),
    replaced: decision.replaced,
    text: decision.text,
    text_tree: decision.text_tree,
    unsure: decision.unsure,
    waiting_review: decision.waiting_review,
  };
}

/** `card` with the words of each statement it shaped that the decision's read holds none for -
 *  the bare read sends none today - from the caller's one batched read of the statements' names
 *  (`statementOf`: `useStatementExcerpts`, or Dashboard's names read), which gives none until it
 *  answers for one, so the card reads no statement of its own and draws no line for it until
 *  then. */
export function withStatementTexts(
  card: CardDecision,
  statementOf: (reference: string) => StatementWords
): CardDecision {
  return {
    ...card,
    links: card.links.map((link) =>
      link.text !== undefined || link.ref === undefined
        ? link
        : { ...link, ...statementOf(link.ref) }
    ),
  };
}

/** A directive's own opening line, `:::plan_task{#t1 serves="…"}`, and its closing fence. */
const DIRECTIVE_OPENER = /^:{3,}[a-z_]/;
const DIRECTIVE_FENCE = /^:{3,}\s*$/;

/**
 * The words of the name the batched reference read gives a target (`ReferenceTarget.name`).
 * The read names a document's block by its stored excerpt, which is the block's source: a typed
 * block is a directive, so its excerpt opens on the directive's line - its type and its
 * attributes - and, where the excerpt holds the whole block, ends on its closing fence. Both are
 * markup and no word of the block (R21), so they are left out. Any other name is left as the
 * read gives it. The page names a target by it (`issue-page/target-naming.ts`), and a card's
 * statement line reads it (`useStatementExcerpts`).
 */
export function blockWords(name: string): string {
  const lines = name.trimEnd().split("\n");
  if (!DIRECTIVE_OPENER.test(lines[0] ?? "")) return name;
  lines.shift();
  if (DIRECTIVE_FENCE.test(lines.at(-1) ?? "")) lines.pop();
  return lines.join("\n").trim();
}

/** The blocks of a decision's text below its heading, as the browser splits a text whose answer
 *  carried no reading of it - a pod of the build before this one serves none during a rolling
 *  deploy: by its blank lines, each block trimmed, an empty one and a heading left out. The
 *  heading is the decision's own, which the card draws above the text, or one written inside
 *  it. */
export function markdownBodyBlocks(text: string): string[] {
  return text
    .split(/\n\s*\n/)
    .map((block) => block.trim())
    .filter((block) => block !== "" && !block.startsWith("#"));
}

/** The path that opens the issue's spec at `blockId`: through the spec's own document route when
 *  the issue's documents are loaded (`spec`), as every in-app block link is built, and else by
 *  the block's fragment on the issue's spec. */
export function specBlockPath(
  issueKey: string,
  spec: Artifact | undefined,
  blockId: string
): string {
  return spec === undefined
    ? `${buildIssuePath({ key: issueKey, kind: "spec" })}${blockFragment(blockId)}`
    : documentItemPath(spec, { blockID: blockId, id: blockId, kind: "comment" });
}

/** Where a card's links go on issue `issueKey`'s surfaces: an ask to its address on the issue,
 *  and a statement it shaped to its block of the spec (`specBlockPath`). */
export function cardPaths(issueKey: string, spec: Artifact | undefined): CardPaths {
  return {
    ask: (id) => buildIssuePath({ id, key: issueKey, kind: "ask" }),
    statement: (blockId) => specBlockPath(issueKey, spec, blockId),
  };
}
