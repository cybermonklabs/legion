import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useCallback, useLayoutEffect, useMemo, useRef, useState } from "react";

import { issueQuery } from "../../api/queries";
import type { Decision, DisplayTree } from "../../api/types";
import {
  card,
  decisionUnsureBorder,
  selectedCardBg,
  selectedCardBorder,
} from "../../theme/classes";
import { fromDecision } from "../decision-card/adapters";
import { decisionsDocumentId } from "../decision-card/change-anchor";
import {
  DecisionCard as DecisionCardBody,
  type DecisionCardProps,
  type DecisionText,
} from "../decision-card/DecisionCard";
import { useDecisionWrites } from "../decision-card/useDecisionWrites";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { blockFragment } from "../refs/block-reference";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildIssuePath, buildReferencePath, parseDispatchReference } from "../refs/routes";
import { regionOrigin } from "./ThreadList";
import type { DecisionMarginItem, MarkPlacement } from "./useMarginItems";

/** The margin's renderer of a decision's text (R21): the text whole, by `MarkdownBody`, below
 *  the heading the card draws, so the reading's own heading is left out of it. */
function MarginDecisionText({ markdown, tree }: DecisionText): ReactNode {
  const body = useMemo(() => {
    const content = tree?.content;
    return tree === undefined || !Array.isArray(content)
      ? tree
      : { ...tree, content: (content as DisplayTree[]).filter((node) => node.type !== "heading") };
  }, [tree]);
  return <MarkdownBody markdown={markdown} tree={body} />;
}

/** A decision waiting for review, with the writes the margin hands the card: Keep and Change,
 *  the Decisions tab's own (`useDecisionWrites`). */
function WritableDecision({
  documentId,
  issueKey,
  model,
  ...props
}: DecisionCardProps & { documentId: string; issueKey: string; model: Decision }): ReactNode {
  const { change, keep } = useDecisionWrites({ decision: model, documentId, issueKey });
  return <DecisionCardBody {...props} change={change} keep={keep} />;
}

/** One decision beside a spec block it shaped, drawn by the decision card with its setting off
 *  (R20, R21): whose decision it is, its heading as a link to it in `decisions.md`, its text
 *  whole, the statements it shaped, its state, and Keep and Change where it waits for review.
 *  Selecting the card lights up the block, as selecting an anchored comment lights up its text. */
export function DecisionCard({
  decisionsDocument,
  hovered = false,
  issueKey,
  item,
  onSelect,
  selected,
}: {
  /** The decisions document the card's Keep and Change write against: undefined where the
   *  margin writes none, on a closed issue or while the issue read names no such document. */
  decisionsDocument?: string;
  hovered?: boolean;
  issueKey: string;
  item: DecisionMarginItem;
  onSelect(id: string): void;
  selected: boolean;
}): ReactNode {
  const { decision } = item;
  const shown = useMemo(() => fromDecision(decision), [decision]);
  const route = parseDispatchReference(decision.ref);
  const active = selected || hovered;
  const props: DecisionCardProps = {
    authorLine: false,
    decision: shown,
    paths: {
      ask: (id) => buildIssuePath({ id, key: issueKey, kind: "ask" }),
      decision: route === undefined ? undefined : buildReferencePath(route),
      statement: (blockId) =>
        `${buildIssuePath({ key: issueKey, kind: "spec" })}${blockFragment(blockId)}`,
    },
    renderText: (text) => <MarginDecisionText markdown={text.markdown} tree={text.tree} />,
  };
  return (
    <article
      aria-current={selected ? "true" : undefined}
      aria-label={`Decision: ${decision.heading}`}
      className={`rounded-xl border p-3 text-sm shadow-sm ${
        active ? `${selectedCardBorder} ${selectedCardBg}` : card
      } ${decision.unsure ? `border-l-4 ${decisionUnsureBorder}` : ""}`}
      data-decision-unsure={decision.unsure ? "true" : undefined}
      data-margin-item={item.id}
      onClick={(event) => {
        if (event.target instanceof Element && event.target.closest("a, button, form") !== null) {
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
      {decision.waiting_review && decisionsDocument !== undefined ? (
        <WritableDecision
          {...props}
          documentId={decisionsDocument}
          issueKey={issueKey}
          model={decision}
        />
      ) : (
        <DecisionCardBody {...props} />
      )}
    </article>
  );
}

/**
 * The decision cards, each beside the spec block it shaped: placed at that block's height in the
 * document, below the card before it where two would overlap, as the anchored comments are.
 *
 * Keep and Change write against the decisions document, whose id comes from the reads the page
 * already holds: the issue read's documents and the decisions read's address of it
 * (`decisionsDocumentId`). Both are read here from the cache alone, so the margin sends no read
 * of its own. A decision beside two blocks is two cards, which both read `["decisions", <key>]`,
 * so a write from one settles both.
 */
export function DecisionList({
  blockPlacements,
  decisions,
  hoveredItemId,
  isClosed,
  issueKey,
  onSelect,
  selectedItemId,
}: {
  blockPlacements: ReadonlyMap<string, MarkPlacement>;
  decisions: readonly DecisionMarginItem[];
  hoveredItemId: string | undefined;
  isClosed: boolean;
  issueKey: string;
  onSelect(id: string, blockId: string): void;
  selectedItemId: string | undefined;
}): ReactNode {
  const issue = useQuery({ ...issueQuery(issueKey), enabled: false });
  const decisionsRead = useQuery(issueDecisionsQuery(issueKey, false));
  const decisionsDocument = isClosed
    ? undefined
    : decisionsDocumentId(issue.data?.artifacts, decisionsRead.data);
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
            decisionsDocument={decisionsDocument}
            hovered={hoveredItemId === item.id}
            issueKey={issueKey}
            item={item}
            onSelect={(id) => onSelect(id, item.blockId)}
            selected={selectedItemId === item.id}
          />
        </div>
      ))}
    </section>
  );
}
