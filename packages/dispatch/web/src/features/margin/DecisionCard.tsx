import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useCallback, useMemo } from "react";

import { issueQuery } from "../../api/queries";
import type { DisplayTree } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  card,
  decisionUnsureBorder,
  selectedCardBg,
  selectedCardBorder,
} from "../../theme/classes";
import {
  cardPaths,
  fromDecision,
  markdownBodyBlocks,
  type StatementWords,
  withStatementTexts,
} from "../decision-card/adapters";
import { decisionsDocumentId } from "../decision-card/change-anchor";
import {
  DecisionCard as DecisionCardBody,
  type DecisionCardProps,
  type DecisionText,
} from "../decision-card/DecisionCard";
import { useStatementExcerpts } from "../decision-card/useStatementExcerpts";
import { WritableDecisionCard } from "../decision-card/WritableDecisionCard";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { useDecisionHeadingWords } from "../issue/decision-heading";
import { MarkdownBody } from "../refs/MarkdownBody";
import { parseDispatchReference, referenceHref } from "../refs/routes";
import { type TextSource, textSourceOf } from "../refs/text-references";
import { useCardStack } from "./card-stack";
import type { DecisionMarginItem, MarkPlacement } from "./useMarginItems";

/** The margin's renderer of a decision's text (R21): the text whole, by `MarkdownBody`, below
 *  the heading the card draws, so the text's own heading is left out of it: out of the reading,
 *  and out of the markdown where the answer carried no reading of it (`markdownBodyBlocks`, as
 *  the Decisions tab's renderer splits it). The passage is a record of the issue's
 *  `decisions.md` (`source`). */
function MarginDecisionText({
  markdown,
  source,
  tree,
}: DecisionText & { source: TextSource | undefined }): ReactNode {
  const body = useMemo(() => {
    if (tree === undefined) {
      return { markdown: markdownBodyBlocks(markdown).join("\n\n"), tree };
    }
    const content = tree.content;
    return {
      markdown,
      tree: !Array.isArray(content)
        ? tree
        : {
            ...tree,
            content: (content as DisplayTree[]).filter((node) => node.type !== "heading"),
          },
    };
  }, [markdown, tree]);
  return (
    <MarkdownBody
      markdown={body.markdown}
      source={source}
      surface="margin-decision-passage"
      tree={body.tree}
    />
  );
}

/** One decision beside a spec block it shaped, drawn by the decision card with its setting off
 *  (R20, R21): whose decision it is, its heading as a link to it in `decisions.md`, its text
 *  whole, the statements it shaped, its state, and Keep and Change where it waits for review.
 *  Selecting the card lights up the block, as selecting an anchored comment lights up its text.
 *  The heading names the card and words a link, neither of which holds markup, so there it is
 *  read as its plain words; with no page to link to it is drawn by the renderer, as the passage
 *  is. Both are the record the issue's `decisions.md` is. */
export function DecisionCard({
  decisionsDocument,
  hovered = false,
  issueKey,
  item,
  onSelect,
  selected,
  statementExcerpt,
}: {
  /** The decisions document the card's Keep and Change write against: undefined where the
   *  margin writes none, on a closed issue or while the issue read names no such document. */
  decisionsDocument?: string;
  hovered?: boolean;
  issueKey: string;
  item: DecisionMarginItem;
  onSelect(id: string): void;
  selected: boolean;
  /** The words of a statement the decision shaped, by its reference, from the list's one read
   *  of the statements' names (`useStatementExcerpts`). */
  statementExcerpt: (reference: string) => StatementWords;
}): ReactNode {
  const { decision } = item;
  const shown = useMemo(
    () => withStatementTexts(fromDecision(decision), statementExcerpt),
    [decision, statementExcerpt]
  );
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
  const props: DecisionCardProps = {
    authorLine: false,
    decision: shown,
    heading: {
      node: (
        <MarkdownBody
          markdown={decision.heading}
          source={source}
          surface="margin-decision-passage"
          tree={headingTree}
          variant="inline"
        />
      ),
      words: heading,
    },
    // A statement opens at its block's fragment on the issue's spec page, so the card is handed
    // no spec document (`specBlockPath`).
    paths: { ...cardPaths(issueKey, undefined), decision: href },
    renderText: (text) => (
      <MarginDecisionText markdown={text.markdown} source={source} tree={text.tree} />
    ),
  };
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
      {reads}
      {decision.waiting_review && decisionsDocument !== undefined ? (
        <WritableDecisionCard
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

/** A decision card's key in its region's stack (`useCardStack`). */
const decisionKey = (item: DecisionMarginItem): string => item.id;

/**
 * The decision cards, each beside the spec block it shaped: placed at that block's height in the
 * document, below the card before it where two would overlap, as the anchored comments are
 * (`useCardStack`).
 *
 * Keep and Change write against the decisions document, whose id comes from the reads the page
 * already holds: the issue read's documents and the decisions read's address of it
 * (`decisionsDocumentId`). Both are read here from the cache alone. A decision beside two blocks
 * is two cards, which both read `["decisions", <key>]`, so a write from one settles both. The
 * one read the margin sends of its own is the names of the statements the cards' decisions
 * shaped, which their read holds no words for: one for the whole list (`useStatementExcerpts`).
 * Where it fails, the list says so above its cards as the margin says of its other failed reads
 * (`QueryError`), and each card still draws each statement as a link.
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
  const drawn = useMemo(() => decisions.map((item) => item.decision), [decisions]);
  const { failure: namesFailure, statementOf: statementExcerpt } = useStatementExcerpts(drawn);
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
    <>
      {namesFailure === undefined ? null : (
        <QueryError
          message="Could not load the statements these decisions shaped."
          {...namesFailure}
        />
      )}
      <section
        aria-label="Decisions"
        className="relative"
        ref={region}
        style={{ minHeight: height }}
      >
        {decisions.map((item) => (
          <div
            key={item.id}
            ref={cardRef(item.id)}
            style={{ left: 0, position: "absolute", right: 0, top: topOf(item.id) }}
          >
            <DecisionCard
              decisionsDocument={decisionsDocument}
              hovered={hoveredItemId === item.id}
              issueKey={issueKey}
              item={item}
              onSelect={(id) => onSelect(id, item.blockId)}
              selected={selectedItemId === item.id}
              statementExcerpt={statementExcerpt}
            />
          </div>
        ))}
      </section>
    </>
  );
}
