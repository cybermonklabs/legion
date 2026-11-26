import { type ReactNode, useCallback } from "react";
import { Link } from "react-router-dom";

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
import { useDecisionHeadingWords } from "../issue/decision-heading";
import { MarkdownBody } from "../refs/MarkdownBody";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { parseDispatchReference, referenceHref } from "../refs/routes";
import { textSourceOf } from "../refs/text-references";
import { useCardStack } from "./card-stack";
import type { DecisionMarginItem, MarkPlacement } from "./useMarginItems";

/** One decision beside a spec block it shaped: whose decision it is, whether its maker was
 *  unsure, its heading as a link to it in `decisions.md`, and the head of its text. Selecting the
 *  card lights up the block, as selecting an anchored comment lights up its text. The heading
 *  names the card and words a link, neither of which holds markup, so there it is read as its
 *  plain words; with no page to link to it is drawn by the renderer, as the passage is. Both
 *  are the record the issue's `decisions.md` is. */
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
  const source = textSourceOf(
    "artifact",
    item.documentId,
    route !== undefined && "key" in route ? route.key : undefined
  );
  // The heading's plain words name the card and word its link (the reading list's
  // `decision-heading-words`); the heading's reading draws it where there is no link.
  const { reads, tree: headingTree, words: heading } = useDecisionHeadingWords(decision, source);
  return (
    <article
      aria-current={selected ? "true" : undefined}
      // The card's name is its heading's plain words: the reading list's `decision-heading-words`.
      aria-label={`Decision: ${heading}`}
      className={`rounded-xl border p-3 text-sm shadow-sm ${
        active ? `${selectedCardBorder} ${selectedCardBg}` : card
      } ${decision.unsure ? `border-l-4 ${decisionUnsureBorder}` : ""}`}
      data-decision-unsure={decision.unsure ? "true" : undefined}
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
        <span>{decision.kind === "person" ? "A person's decision" : "Agent's decision"}</span>
        {decision.unsure ? (
          <span className={`font-semibold ${decisionUnsureText}`}>Unsure</span>
        ) : null}
      </p>
      <h3 className={`mt-1 font-medium ${textPrimaryOnSurface}`}>
        {reads}
        {href === undefined ? (
          <MarkdownBody
            markdown={decision.heading}
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
        markdown={decision.text}
        source={source}
        surface="margin-decision-passage"
        tree={decision.text_tree}
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
