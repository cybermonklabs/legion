import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useId, useMemo } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, Decision, DisplayTree, IssueAskRow } from "../../api/types";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { QueryError } from "../../components/QueryError";
import {
  card,
  decisionUnsureAccent,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { fromDecision } from "../decision-card/adapters";
import {
  type CardDecision,
  DecisionCard,
  type DecisionCardProps,
} from "../decision-card/DecisionCard";
import { useDecisionWrites } from "../decision-card/useDecisionWrites";
import { blockFragment } from "../refs/block-reference";
import { MarkdownBody } from "../refs/MarkdownBody";
import { PlainText } from "../refs/plain-text";
import { buildIssuePath, DECISIONS_DOCUMENT_SLUG, documentItemPath } from "../refs/routes";
import { type TextSource, textSourceOf } from "../refs/text-references";
import { useDecisionHeadingWords } from "./decision-heading";

/** The word that opens the paragraph the server reads as a decision's doubt
 *  (`api/decisions.go`'s `unsurePrefix`). */
const UNSURE_PREFIX = "Unsure:";

/** One block of a decision's text below its heading, as a view draws one: the block's own text,
 *  the server's reading with that block alone in it where the answer carried one, and whether
 *  the block is the decision's doubt. */
interface DecisionBlock {
  /** This block's own text and no more of the decision's: what `MarkdownBody` shows where it
   *  can draw neither a tree nor a parse. Where the answer carried a reading it is the block's
   *  flattened words; where it carried none it is the block's own markdown, since there is no
   *  reading to flatten. */
  readonly text: string;
  /** The markdown this block's references are read from (`MarkdownBody`'s `referenceText`): the
   *  whole decision where the answer carried a reading, since a block of that reading holds a
   *  link's target and a key's backticks nowhere but in the field's own markdown; this block's
   *  markdown alone where it carried none, since the browser parsed that markdown to draw this
   *  block and a reference another block cites is none of this one's - the whole decision would
   *  link an escaped `\#q2` here because some other block cites `#q2`. */
  readonly references: string;
  readonly tree: DisplayTree | undefined;
  readonly unsure: boolean;
}

/** The words a node holds, its children's included: what `pmdoc.FindQuote` matches a quote
 *  against, since a quote names the document's text and never its markdown. The server writes
 *  one space wherever two text runs are not adjacent in the document's own positions
 *  (`pmdoc.buildFlattenedText`), which is wherever anything was entered, left or passed between
 *  them: the break inside a wrapped line, or the end of one list item and the start of the
 *  next. Two runs are adjacent only as consecutive children of one parent, so everything else
 *  this walk crosses opens the space the server wrote. */
function wordsOf(node: DisplayTree): string {
  let words = "";
  let apart = false;
  const read = (current: DisplayTree): void => {
    if (typeof current.text === "string") {
      // A tree's words joined to match a quote against, drawn nowhere.
      words += apart && words !== "" ? ` ${current.text}` : current.text;
      apart = false;
      return;
    }
    // The node's own open position, and its close once its children are read; a leaf that is no
    // text, a hard break among them, takes the one position it is.
    apart = true;
    const content = current.content;
    if (Array.isArray(content)) {
      for (const child of content as DisplayTree[]) {
        read(child);
      }
      apart = true;
    }
  };
  read(node);
  return words;
}

/** Whether a block is the decision's doubt, as the server marks one: a paragraph whose markdown
 *  starts `Unsure:` and nothing else (`api/decisions.go`'s `decisionSections`). In the reading
 *  of such a paragraph the word stands in a first child that is plain text, since a mark over
 *  it - strong, a code span, a link - would have written its own characters first. */
function isUnsureBlock(node: DisplayTree): boolean {
  if (node.type !== "paragraph" || !Array.isArray(node.content)) {
    return false;
  }
  const first = (node.content as DisplayTree[])[0];
  const marks = first?.marks;
  return (
    typeof first?.text === "string" &&
    (!Array.isArray(marks) || marks.length === 0) &&
    first.text.startsWith(UNSURE_PREFIX)
  );
}

/** The blocks of a decision whose answer carried no reading of its text: a pod of the build
 *  before this one serves none during a rolling deploy, and the markdown split the browser has
 *  always made is what draws the decision then, as every other view falls back to parsing. */
function markdownBlocks(text: string): DecisionBlock[] {
  return text
    .split(/\n\s*\n/)
    .map((block) => block.trim())
    .filter((block) => block !== "" && !block.startsWith("#"))
    .map((markdown) => ({
      // The block's own markdown is its text here: with no reading there is nothing to flatten.
      // It is what the references are read from as well, because it is what the browser parsed
      // to draw this block: a reference some other block of the decision cites is no reference
      // of this one.
      text: markdown,
      references: markdown,
      tree: undefined,
      unsure: markdown.startsWith(UNSURE_PREFIX),
    }));
}

/** The blocks of a decision below its heading, each marked unsure or not, taken from the tree Go
 *  read its text into: the top-level blocks of that tree are the paragraphs the markdown would
 *  have been split into, already divided, so the browser splits nothing and parses nothing. A
 *  heading is the decision's own, drawn by the card above its text, or one written inside it,
 *  which the markdown split dropped too. */
function readDecisionBlocks(
  markdown: string,
  tree: DisplayTree | undefined
): readonly DecisionBlock[] {
  const content = tree?.content;
  if (tree === undefined || !Array.isArray(content)) {
    return markdownBlocks(markdown);
  }
  return (content as DisplayTree[])
    .filter((node) => node.type !== "heading")
    .map((node) => ({
      // What this block draws is the block alone, so its text is the words that block holds and
      // never the whole field's markdown: the field is passed beside it as the text the
      // references are read from (`MarkdownBody`'s `referenceText`), since a link's target and
      // the backticks that end a key are in the markdown alone.
      text: wordsOf(node),
      references: markdown,
      tree: { ...tree, content: [node] },
      unsure: isUnsureBlock(node),
    }));
}

/** The path that opens the issue's spec at `blockId`: through the spec's own document route when
 *  the issue's artifacts are loaded, as every in-app block link is built. */
function specBlockPath(issueKey: string, spec: Artifact | undefined, blockId: string): string {
  return spec === undefined
    ? `${buildIssuePath({ key: issueKey, kind: "spec" })}${blockFragment(blockId)}`
    : documentItemPath(spec, { blockID: blockId, id: blockId, kind: "comment" });
}

function Section({
  children,
  empty,
  title,
}: {
  children: ReactNode[];
  empty: string;
  title: string;
}): ReactNode {
  const id = useId();
  return (
    <section aria-labelledby={id} className="space-y-3">
      <h2 className={`text-xs font-semibold uppercase ${textSecondaryOnSurface}`} id={id}>
        {title}
      </h2>
      {children.length === 0 ? (
        <p className={`text-sm ${textMutedOnSurface}`}>{empty}</p>
      ) : (
        <ul className="space-y-3">{children}</ul>
      )}
    </section>
  );
}

/** The tab's renderer of a decision's text (R21): each block below its heading drawn by
 *  `MarkdownBody` with the decisions document as its source, and the block that is the
 *  decision's doubt in the unsure accent. The blocks are read once per answer of the decisions
 *  read, so a repaint of the tab repaints no body it already drew. */
function DecisionBlocks({
  markdown,
  source,
  tree,
}: {
  markdown: string;
  source: TextSource | undefined;
  tree: DisplayTree | undefined;
}): ReactNode {
  const blocks = useMemo(() => readDecisionBlocks(markdown, tree), [markdown, tree]);
  return blocks.map((block, index) =>
    block.unsure ? (
      <div
        className={`border-l-2 pl-2 ${decisionUnsureAccent}`}
        data-testid="decision-unsure"
        // biome-ignore lint/suspicious/noArrayIndexKey: the tree's blocks have no id of their own
        key={index}
      >
        <MarkdownBody
          markdown={block.text}
          referenceText={block.references}
          source={source}
          surface="issue-decision"
          tree={block.tree}
        />
      </div>
    ) : (
      <MarkdownBody
        // biome-ignore lint/suspicious/noArrayIndexKey: the tree's blocks have no id of their own
        key={index}
        markdown={block.text}
        referenceText={block.references}
        source={source}
        surface="issue-decision"
        tree={block.tree}
      />
    )
  );
}

/** A decision waiting for the reader's review, with the writes the tab hands the card: Keep and
 *  Change (`useDecisionWrites`), named after the heading's plain words. */
function WritableDecision({
  documentId,
  issueKey,
  model,
  name,
  ...props
}: DecisionCardProps & {
  documentId: string;
  issueKey: string;
  model: Decision;
  name: string;
}): ReactNode {
  const { change, keep } = useDecisionWrites({ decision: model, documentId, issueKey, name });
  return <DecisionCard {...props} change={change} issueKey={issueKey} keep={keep} />;
}

/** One decision in its section, drawn by the decision card with its setting off (R20, R21) and
 *  the tab's own renderer; in "Waiting for your review" with Keep and Change, where the issue is
 *  open and its decisions document is known. The heading is a record of the decisions document
 *  too: the card draws it by the renderer, and names a link, a write and a form by its plain
 *  words (`useDecisionHeadingWords`). */
function TabDecision({
  decision,
  documentId,
  isClosed,
  issueKey,
  shown,
  spec,
  writes,
}: {
  decision: Decision;
  /** The issue's `decisions.md`, the document this text is written in, which its `#id`s name a
   *  question block of; undefined until the issue's documents are read. */
  documentId: string | undefined;
  isClosed: boolean;
  issueKey: string;
  /** The decision as the card draws it (`fromDecision`). */
  shown: CardDecision;
  spec: Artifact | undefined;
  /** Whether this section takes the decision's Keep and Change. */
  writes: boolean;
}): ReactNode {
  const source = textSourceOf("artifact", documentId, issueKey);
  const { reads, tree: headingTree, words: name } = useDecisionHeadingWords(decision, source);
  const props: DecisionCardProps = {
    authorLine: false,
    decision: shown,
    heading: {
      node: (
        <MarkdownBody
          markdown={decision.heading}
          source={source}
          surface="issue-decision"
          tree={headingTree}
          variant="inline"
        />
      ),
      words: name,
    },
    paths: {
      ask: (id) => buildIssuePath({ id, key: issueKey, kind: "ask" }),
      statement: (blockId) => specBlockPath(issueKey, spec, blockId),
    },
    renderText: (text) => (
      <DecisionBlocks markdown={text.markdown} source={source} tree={text.tree} />
    ),
  };
  return (
    <li className={`rounded-lg border p-3 ${card}`} data-decision={decision.block_id}>
      {reads}
      {writes && !isClosed && documentId !== undefined ? (
        <WritableDecision
          {...props}
          documentId={documentId}
          issueKey={issueKey}
          model={decision}
          name={name}
        />
      ) : (
        <DecisionCard {...props} />
      )}
    </li>
  );
}

function OpenQuestion({
  ask,
  issueKey,
  spec,
}: {
  ask: IssueAskRow;
  issueKey: string;
  spec: Artifact | undefined;
}): ReactNode {
  const path =
    ask.block_id === undefined || ask.block_id === null
      ? buildIssuePath({ id: ask.id, key: issueKey, kind: "ask" })
      : specBlockPath(issueKey, spec, ask.block_id);
  return (
    <li className={`rounded-lg border p-3 ${card}`}>
      <Link className={`text-sm ${linkText} ${linkHoverText}`} to={path}>
        <PlainText
          markdown={ask.question}
          source={textSourceOf("ask", ask.id, issueKey)}
          surface="issue-open-question"
          tree={ask.question_tree}
        >
          {/* The question as its plain words, and as written until the engine has answered:
              the reading list's `issue-open-question`. */}
          {(words) => words ?? ask.question}
        </PlainText>
      </Link>
    </li>
  );
}

/** The issue's Decisions tab: its open questions, the agent's decisions waiting for a person's
 *  review (with Keep and Change), the person's own decisions, and the replaced ones, read from
 *  `GET /api/v1/issues/{key}/decisions`. Each decision links the spec blocks it shaped. */
export function DecisionsPanel({
  artifacts,
  isClosed,
  issueKey,
}: {
  artifacts: readonly Artifact[];
  isClosed: boolean;
  issueKey: string;
}): ReactNode {
  const view = useQuery({
    queryFn: () => api.getIssueDecisions(issueKey),
    queryKey: ["decisions", issueKey],
  });
  // Each decision beside the shape the card draws it by, made once per answer, so a card's text
  // is a new object only when the decisions were read again and a repaint of the tab repaints no
  // body it already drew. The three lists below are this one reading filtered, never three
  // readings of it.
  const rows = useMemo(
    () =>
      (view.data?.decisions ?? []).map((decision) => ({
        decision,
        shown: fromDecision(decision),
      })),
    [view.data?.decisions]
  );
  if (view.isPending) {
    return <LoadingSkeleton label="Decisions loading" />;
  }
  if (view.isError) {
    return (
      <QueryError
        message="Could not load the decisions."
        onRetry={() => void view.refetch()}
        retrying={view.isFetching}
      />
    );
  }
  const spec = artifacts.find((artifact) => artifact.primary && artifact.kind === "doc");
  const documentId = artifacts.find(
    (artifact) => artifact.slug === DECISIONS_DOCUMENT_SLUG && artifact.kind === "doc"
  )?.id;
  const questions = view.data.open_questions ?? [];
  const waiting = rows.filter((row) => row.decision.waiting_review);
  // Yours: the ones a person decided, and the agent's that the reader kept — a Keep takes a
  // decision out of "Waiting for your review", and a decision nothing holds would leave the tab.
  const mine = rows.filter(
    ({ decision }) => !decision.replaced && (decision.kind === "person" || decision.kept !== null)
  );
  const replaced = rows.filter((row) => row.decision.replaced);
  return (
    <div className="space-y-4">
      {view.data.document === null ? (
        <p className={`text-sm ${textMutedOnSurface}`}>This issue has no decisions.md yet.</p>
      ) : null}
      <Section empty="No open questions." title="Open questions">
        {questions.map((ask) => (
          <OpenQuestion ask={ask} issueKey={issueKey} key={ask.id} spec={spec} />
        ))}
      </Section>
      <Section empty="Nothing waits for your review." title="Waiting for your review">
        {waiting.map(({ decision, shown }) => (
          <TabDecision
            decision={decision}
            documentId={documentId}
            isClosed={isClosed}
            issueKey={issueKey}
            key={decision.block_id}
            shown={shown}
            spec={spec}
            writes
          />
        ))}
      </Section>
      <Section empty="No decisions of yours." title="Your decisions">
        {mine.map(({ decision, shown }) => (
          <TabDecision
            decision={decision}
            documentId={documentId}
            isClosed={isClosed}
            issueKey={issueKey}
            key={decision.block_id}
            shown={shown}
            spec={spec}
            writes={false}
          />
        ))}
      </Section>
      <Section empty="No replaced decisions." title="Replaced">
        {replaced.map(({ decision, shown }) => (
          <TabDecision
            decision={decision}
            documentId={documentId}
            isClosed={isClosed}
            issueKey={issueKey}
            key={decision.block_id}
            shown={shown}
            spec={spec}
            writes={false}
          />
        ))}
      </Section>
    </div>
  );
}
