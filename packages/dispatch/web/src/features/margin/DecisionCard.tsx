import { type ReactNode, useCallback, useMemo } from "react";
import { Link } from "react-router-dom";

import type { DecisionEntry, DisplayTree } from "../../api/types";
import {
  card,
  decisionUnsureBorder,
  decisionUnsureText,
  linkHoverText,
  linkText,
  selectedCardBg,
  selectedCardBorder,
  textMutedOnSelectedCard,
  textPrimaryOnSurface,
} from "../../theme/classes";
import {
  decisionName,
  holdsUnsureParagraph,
  ownTree,
  plainParagraph,
  unsureWords,
  wordsOf,
} from "../issue/decision-text";
import { MarkdownBody } from "../refs/MarkdownBody";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { usePlainHeadline } from "../refs/plain-text";
import { parseDispatchReference, referenceHref } from "../refs/routes";
import { textSourceOf } from "../refs/text-references";
import { useCardStack } from "./card-stack";
import type { DecisionMarginItem, MarkPlacement } from "./useMarginItems";

/** The name a card gives an entry: an ask's question, else the decision's name as the server and
 *  the Decisions tab read it (`decisionName`), a heading by its words and not by its `#`. */
export function entryHeading(decision: DecisionEntry): string {
  return decision.question ?? decisionName(decision.text_tree, decision.text);
}

/** The reading `entryHeading`'s name is the words of: the first block of a decision's own
 *  content that holds any (`decisionName`), alone in its tree; for an ask, named by its question,
 *  which is the entry's own text, the reading of that text (`text_tree`). None for an answer that
 *  carried no reading, whose name is then parsed. */
function entryHeadingTree(decision: DecisionEntry): DisplayTree | undefined {
  if (decision.node.kind === "ask") return decision.text_tree;
  const tree = ownTree(decision.text_tree);
  const content = tree?.content;
  if (tree === undefined || !Array.isArray(content)) return undefined;
  const named = (content as DisplayTree[]).find((node) => wordsOf(node).trim() !== "");
  return named === undefined ? undefined : { ...tree, content: [named] };
}

/** What a card previews of a decision after its words: the doubt its node holds
 *  (`unsureWords`) where those words hold no `Unsure:` paragraph, as the paragraph it was moved
 *  from ran on in the preview, and the reading of the words with that paragraph after them. The
 *  mount writes the words and the doubt into the preview's `markdown` itself, so the stored text
 *  is handed to the renderer where the reading list's walk sees it (`surface-lists.test.ts`). */
function previewOf(decision: DecisionEntry): {
  doubt: string | undefined;
  tree: DisplayTree | undefined;
} {
  const { text, text_tree: tree } = decision;
  const doubt = unsureWords(decision);
  if (doubt === undefined || holdsUnsureParagraph(tree, text)) {
    return { doubt: undefined, tree };
  }
  const content = tree?.content;
  return {
    doubt,
    tree:
      tree === undefined || !Array.isArray(content)
        ? tree
        : { ...tree, content: [...(content as DisplayTree[]), plainParagraph(doubt)] },
  };
}

/** One decision beside a spec block it shaped: whose decision it is, whether its maker was
 *  unsure, its name as a link to the node that holds it, and the head of its text. Selecting the
 *  card lights up the block, as selecting an anchored comment lights up its text. The heading
 *  names the card and words a link, neither of which holds markup, so there it is read as its
 *  plain words; with no page to link to it is drawn by the renderer, as the passage is. Both
 *  are the record of the document the decision's node stands in. */
export function DecisionCard({
  hovered = false,
  item,
  onSelect,
  selected,
}: {
  hovered?: boolean;
  item: DecisionMarginItem;
  onSelect(id: string): void;
  selected: boolean;
}): ReactNode {
  const { decision } = item;
  const route = parseDispatchReference(decision.ref);
  // A decision names the record it was made in; one naming no page of this dashboard - a
  // person (R5) - leaves the heading as plain words.
  const href = route === undefined ? undefined : referenceHref(route);
  const active = selected || hovered;
  const name = entryHeading(decision);
  const unsure = decision.uncertainty === "high";
  // One reading per answer: a fresh tree every render would be a fresh text to draw.
  const headingTree = useMemo(() => entryHeadingTree(decision), [decision]);
  const source = textSourceOf(
    "artifact",
    item.documentId,
    route !== undefined && "key" in route ? route.key : undefined
  );
  const { reads, text } = usePlainHeadline(headingTree, name, {
    source,
    surface: "decision-heading-words",
  });
  // Until the engine has answered, the heading as written, which the reading list's
  // `decision-heading-words` says is a load state: a card is never left with no name.
  const heading = text?.text ?? name;
  // Read once per decision, so a repaint of the card repaints no preview it already drew.
  const preview = useMemo(() => previewOf(decision), [decision]);
  return (
    <article
      aria-current={selected ? "true" : undefined}
      // The card's name is its heading's plain words: the reading list's `decision-heading-words`.
      aria-label={`Decision: ${heading}`}
      className={`rounded-xl border p-3 text-sm shadow-sm ${
        active ? `${selectedCardBorder} ${selectedCardBg}` : card
      } ${unsure ? `border-l-4 ${decisionUnsureBorder}` : ""}`}
      data-decision-unsure={unsure ? "true" : undefined}
      data-margin-item={item.id}
      onClick={(event) => {
        if (event.target instanceof Element && event.target.closest("a, button") !== null) {
          return;
        }
        event.stopPropagation();
        onSelect(item.id);
      }}
      onKeyDown={(event) => {
        if (event.target !== event.currentTarget || (event.key !== "Enter" && event.key !== " ")) {
          return;
        }
        event.preventDefault();
        onSelect(item.id);
      }}
    >
      <p className={`flex flex-wrap gap-x-2 text-xs ${textMutedOnSelectedCard}`}>
        <span>{decision.kind === "agent" ? "Agent's decision" : "A person's decision"}</span>
        {unsure ? <span className={`font-semibold ${decisionUnsureText}`}>Unsure</span> : null}
      </p>
      <h3 className={`mt-1 font-medium ${textPrimaryOnSurface}`}>
        {reads}
        {href === undefined ? (
          <MarkdownBody
            markdown={name}
            source={source}
            surface="margin-decision-passage"
            tree={headingTree}
            variant="inline"
          />
        ) : (
          <Link className={`${linkText} ${linkHoverText}`} to={href}>
            {/* A link's words hold no markup: the reading list's `decision-heading-words`. */}
            {heading}
          </Link>
        )}
      </h3>
      <MarkdownPreview
        className={`mt-1 ${textPrimaryOnSurface}`}
        lines={3}
        markdown={
          preview.doubt === undefined ? decision.text : `${decision.text}\n\n${preview.doubt}`
        }
        source={source}
        surface="margin-decision-passage"
        tree={preview.tree}
      />
    </article>
  );
}

/** A decision card's key in its region's stack (`useCardStack`). */
const decisionKey = (item: DecisionMarginItem): string => item.id;

/**
 * The decision cards, each beside the spec block it shaped: placed at that block's height in the
 * document, below the card before it where two would overlap, as the anchored comments are
 * (`useCardStack`).
 */
export function DecisionList({
  blockPlacements,
  decisions,
  hoveredItemId,
  onSelect,
  selectedItemId,
}: {
  blockPlacements: ReadonlyMap<string, MarkPlacement>;
  decisions: readonly DecisionMarginItem[];
  hoveredItemId: string | undefined;
  onSelect(id: string, blockId: string): void;
  selectedItemId: string | undefined;
}): ReactNode {
  // A card wants its block's height in the document; one whose block has no placement yet
  // stacks below the card before it.
  const wantedTop = useCallback(
    (item: DecisionMarginItem, origin: number) => {
      const placement = blockPlacements.get(item.blockId);
      if (placement === undefined) return undefined;
      const block = document.querySelector<HTMLElement>(
        `[data-block-id="${CSS.escape(item.blockId)}"]`
      );
      return block === null ? placement.top : block.getBoundingClientRect().top - origin;
    },
    [blockPlacements]
  );
  const { cardRef, height, region, topOf } = useCardStack(decisions, decisionKey, wantedTop);
  if (decisions.length === 0) {
    return null;
  }
  return (
    <section aria-label="Decisions" className="relative" ref={region} style={{ minHeight: height }}>
      {decisions.map((item) => (
        <div
          key={item.id}
          ref={cardRef(item.id)}
          style={{ left: 0, position: "absolute", right: 0, top: topOf(item.id) }}
        >
          <DecisionCard
            hovered={hoveredItemId === item.id}
            item={item}
            onSelect={(id) => onSelect(id, item.blockId)}
            selected={selectedItemId === item.id}
          />
        </div>
      ))}
    </section>
  );
}
