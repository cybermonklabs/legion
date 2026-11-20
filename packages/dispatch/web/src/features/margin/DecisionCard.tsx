import { type ReactNode, useCallback, useLayoutEffect, useMemo, useRef, useState } from "react";
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
import { decisionName, ownTree, wordsOf } from "../issue/decision-text";
import { MarkdownBody } from "../refs/MarkdownBody";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { usePlainHeadline } from "../refs/plain-text";
import { parseDispatchReference, referenceHref } from "../refs/routes";
import { textSourceOf } from "../refs/text-references";
import { regionOrigin } from "./ThreadList";
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
        markdown={decision.text}
        source={source}
        surface="margin-decision-passage"
        tree={decision.text_tree}
      />
    </article>
  );
}

/**
 * The decision cards, each beside the spec block it shaped: placed at that block's height in the
 * document, below the card before it where two would overlap, as the anchored comments are.
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
  const region = useRef<HTMLElement>(null);
  const cardRefs = useRef(new Map<string, HTMLDivElement>());
  const [tops, setTops] = useState<ReadonlyMap<string, number>>(() => new Map());
  const measure = useCallback(() => {
    const origin = region.current === null ? undefined : regionOrigin(region.current);
    const next = new Map<string, number>();
    let previousBottom = 0;
    for (const item of decisions) {
      const placement = blockPlacements.get(item.blockId);
      const block = document.querySelector<HTMLElement>(
        `[data-block-id="${CSS.escape(item.blockId)}"]`
      );
      const wanted =
        placement === undefined
          ? previousBottom
          : block === null || origin === undefined
            ? placement.top
            : block.getBoundingClientRect().top - origin;
      const top = Math.max(wanted, previousBottom);
      next.set(item.id, top);
      previousBottom = top + (cardRefs.current.get(item.id)?.offsetHeight ?? 0) + 8;
    }
    setTops((current) =>
      current.size === next.size && [...next].every(([id, top]) => current.get(id) === top)
        ? current
        : next
    );
  }, [blockPlacements, decisions]);
  useLayoutEffect(() => {
    measure();
  }, [measure]);
  useLayoutEffect(() => {
    const observer = new ResizeObserver(measure);
    for (const element of cardRefs.current.values()) {
      observer.observe(element);
    }
    return () => observer.disconnect();
  }, [measure]);
  if (decisions.length === 0) {
    return null;
  }
  const bottom = Math.max(
    ...decisions.map(
      (item) => (tops.get(item.id) ?? 0) + (cardRefs.current.get(item.id)?.offsetHeight ?? 0) + 8
    )
  );
  return (
    <section aria-label="Decisions" className="relative" ref={region} style={{ minHeight: bottom }}>
      {decisions.map((item) => (
        <div
          key={item.id}
          ref={(element) => {
            if (element === null) {
              cardRefs.current.delete(item.id);
            } else {
              cardRefs.current.set(item.id, element);
            }
          }}
          style={{ left: 0, position: "absolute", right: 0, top: tops.get(item.id) ?? 0 }}
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
