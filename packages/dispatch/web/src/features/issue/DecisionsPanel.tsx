import { type ReactNode, useId, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useCache } from "../../api/cache/cache";
import { useShownQuery } from "../../api/cache/shown-query";
import { api, decisionKeepRequest, issueCommentRequest } from "../../api/client";
import type {
  Artifact,
  Decision,
  DecisionLink,
  DisplayTree,
  IssueAskRow,
  IssueDecisions,
} from "../../api/types";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { QueryError } from "../../components/QueryError";
import {
  card,
  dangerText,
  decisionUnsureAccent,
  inputClasses,
  linkHoverText,
  linkText,
  primaryButton,
  secondaryButtonCompact,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { blockFragment } from "../refs/block-reference";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildIssuePath, DECISIONS_DOCUMENT_SLUG, documentItemPath } from "../refs/routes";
import { textSourceOf } from "../refs/text-references";
import { useKeyedWrite } from "../writes/useKeyedWrite";

/** The word that opens the paragraph the server reads as a decision's doubt
 *  (`api/decisions.go`'s `unsurePrefix`). */
const UNSURE_PREFIX = "Unsure:";

/** One block of a decision's text below its heading, as a view draws one: the block's own text,
 *  the server's reading with that block alone in it where the answer carried one, and whether
 *  the block is the decision's doubt. */
interface DecisionBlock {
  /** This block's own text and no more of the decision's: what `MarkdownBody` shows where it
   *  can draw neither a tree nor a parse, and what the Change composer quotes. Where the answer
   *  carried a reading it is the block's flattened words, which is what `pmdoc.FindQuote`
   *  matches a quote against; where it carried none it is the block's own markdown, since
   *  there is no reading to flatten. */
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
      // The block's own markdown is its text here, the Change quote included: with no reading
      // there is nothing to flatten, and parsing the markdown in the browser to build a quote
      // is the reading a stored text's tree exists to spare every view. It is what the
      // references are read from as well, because it is what the browser parsed to draw this
      // block: a reference some other block of the decision cites is no reference of this one.
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
function readDecisionBlocks(decision: Decision): readonly DecisionBlock[] {
  const tree = decision.text_tree;
  const content = tree?.content;
  if (tree === undefined || !Array.isArray(content)) {
    return markdownBlocks(decision.text);
  }
  return (content as DisplayTree[])
    .filter((node) => node.type !== "heading")
    .map((node) => ({
      // What this block draws is the block alone, so its text is the words that block holds and
      // never the whole field's markdown: the field is passed beside it as the text the
      // references are read from (`MarkdownBody`'s `referenceText`), since a link's target and
      // the backticks that end a key are in the markdown alone.
      text: wordsOf(node),
      references: decision.text,
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

function ShapedLinks({
  issueKey,
  links,
  spec,
}: {
  issueKey: string;
  links: readonly DecisionLink[] | null;
  spec: Artifact | undefined;
}): ReactNode {
  const shaped = (links ?? []).filter((link) => link.to.block_id !== undefined);
  if (shaped.length === 0) {
    return null;
  }
  return (
    <div className="space-y-1">
      <p className={`text-xs ${textMutedOnSurface}`}>Shaped in the spec:</p>
      <ul className="space-y-1 text-sm">
        {shaped.map((link) => {
          const blockId = link.to.block_id ?? "";
          return (
            <li key={`${link.from_block}:${blockId}`}>
              <Link
                className={`${linkText} ${linkHoverText}`}
                to={specBlockPath(issueKey, spec, blockId)}
              >
                {link.to.text?.trim() || `Block ${blockId}`}
              </Link>
              {link.stale ? (
                <span className={`ml-2 text-xs ${textMutedOnSurface}`}>out of date</span>
              ) : null}
            </li>
          );
        })}
      </ul>
    </div>
  );
}

function ChangeComposer({
  decision,
  documentId,
  issueKey,
  onDone,
  quote,
}: {
  decision: Decision;
  documentId: string;
  issueKey: string;
  onDone: () => void;
  quote: string;
}): ReactNode {
  const [body, setBody] = useState("");
  const fieldId = useId();
  // Both decisions writes stand in the line of the document they are about (`document:<id>`,
  // the issue's decisions.md), so a Keep and a Change of it go out in press order. The decisions
  // document belongs to an issue, and a comment on an issue's document is the issue's own write:
  // the artifact route refuses it, naming the issue route (`send-request.ts` follows the same
  // rule for a thread's replies).
  const send = useKeyedWrite({
    inline: true,
    label: () => `Change to ${decision.heading}`,
    request: (input: { body: string; quote: string }) =>
      issueCommentRequest(issueKey, {
        anchor: { artifact: documentId, quote: input.quote },
        body: input.body,
      }),
    target: () => `document:${documentId}`,
    onSuccess: () => {
      setBody("");
      onDone();
    },
  });
  return (
    <form
      aria-label={`Change ${decision.heading}`}
      className="space-y-2"
      onSubmit={(event) => {
        event.preventDefault();
        if (body.trim() !== "") {
          send.mutate({ body, quote });
        }
      }}
    >
      <blockquote
        className={`border-l-2 pl-2 text-sm ${textSecondaryOnSurface}`}
        data-testid="decision-quote"
      >
        {quote}
      </blockquote>
      <label className={`block text-sm ${textPrimaryOnSurface}`} htmlFor={fieldId}>
        Your change
      </label>
      <textarea
        className={`block w-full rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
        id={fieldId}
        onChange={(event) => setBody(event.target.value)}
        rows={3}
        value={body}
      />
      {send.error === null ? null : (
        <p className={`text-sm ${dangerText}`} role="alert">
          {send.error.message}
        </p>
      )}
      <div className="flex gap-2">
        <button
          className={primaryButton}
          disabled={send.isPending || body.trim() === ""}
          type="submit"
        >
          Send
        </button>
        <button className={secondaryButtonCompact} onClick={onDone} type="button">
          Cancel
        </button>
      </div>
    </form>
  );
}

function DecisionCard({
  actions,
  blocks,
  decision,
  documentId,
  issueKey,
  spec,
}: {
  actions?: ReactNode;
  /** The blocks of this decision's text below its heading, read by the caller that also quotes
   *  the first of them (`readDecisionBlocks`). */
  blocks: readonly DecisionBlock[];
  decision: Decision;
  /** The issue's `decisions.md`, the document this text is written in, which its `#id`s name a
   *  question block of; undefined until the issue's documents are read. */
  documentId: string | undefined;
  issueKey: string;
  spec: Artifact | undefined;
}): ReactNode {
  const source = textSourceOf("artifact", documentId);
  return (
    <li className={`space-y-2 rounded-lg border p-3 ${card}`} data-decision={decision.block_id}>
      <h3 className={`text-[15px] font-medium ${textPrimaryOnSurface}`}>{decision.heading}</h3>
      {blocks.map((block, index) =>
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
            tree={block.tree}
          />
        )
      )}
      {(decision.asks ?? []).map((ask) => (
        <Link
          className={`block text-sm ${linkText} ${linkHoverText}`}
          key={ask}
          to={buildIssuePath({ id: ask, key: issueKey, kind: "ask" })}
        >
          The ask it answers
        </Link>
      ))}
      <ShapedLinks issueKey={issueKey} links={decision.links} spec={spec} />
      {actions}
    </li>
  );
}

function WaitingDecision({
  blocks,
  decision,
  documentId,
  isClosed,
  issueKey,
  spec,
}: {
  blocks: readonly DecisionBlock[];
  decision: Decision;
  documentId: string | undefined;
  isClosed: boolean;
  issueKey: string;
  spec: Artifact | undefined;
}): ReactNode {
  const cache = useCache();
  const [changing, setChanging] = useState(false);
  // A change anchors on the decision's first block below its heading, by the words that block
  // holds: an anchor quote names the document's own text, which is what the server matches it
  // against (`pmdoc.FindQuote`).
  const quote = blocks[0]?.text;
  const keep = useKeyedWrite({
    inline: true,
    label: () => `Keep ${decision.heading}`,
    // The sum of the text this card showed: a decision the agent changed since is refused
    // (`409 DECISION_CHANGED`), so a Keep never stands over words the reader did not see.
    request: (id: string) => decisionKeepRequest(id, decision.block_id, decision.text_sha256),
    target: (id) => `document:${id}`,
    onSuccess: (kept) => {
      if (kept === null) {
        void cache.invalidate({ exact: true, queryKey: ["decisions", issueKey] });
        return;
      }
      cache.setDispatched(
        ["decisions", issueKey],
        kept,
        (view: IssueDecisions | undefined, keep) =>
          view === undefined
            ? view
            : {
                ...view,
                // The Keep is of the text this card showed, which the server took, so it applies
                // to the row that still holds that text. A decision's changed version is the
                // greatest of the blocks it holds now and does not move when a block leaves it,
                // so a row whose text changed since can carry the version the Keep was taken at.
                decisions: (view.decisions ?? []).map((row) =>
                  row.block_id === keep.block_id && row.text_sha256 === decision.text_sha256
                    ? { ...row, kept: keep, waiting_review: false }
                    : row
                ),
              }
      );
    },
  });
  const writable = !isClosed && documentId !== undefined;
  return (
    <DecisionCard
      actions={
        writable ? (
          <div className="space-y-2">
            <div className="flex gap-2">
              <button
                className={secondaryButtonCompact}
                disabled={keep.isPending}
                onClick={() => keep.mutate(documentId)}
                type="button"
              >
                Keep
              </button>
              {quote === undefined ? null : (
                <button
                  aria-expanded={changing}
                  className={secondaryButtonCompact}
                  onClick={() => setChanging(!changing)}
                  type="button"
                >
                  Change
                </button>
              )}
            </div>
            {keep.error === null ? null : (
              <p className={`text-sm ${dangerText}`} role="alert">
                {keep.error.message}
              </p>
            )}
            {changing && quote !== undefined ? (
              <ChangeComposer
                decision={decision}
                documentId={documentId}
                issueKey={issueKey}
                onDone={() => setChanging(false)}
                quote={quote}
              />
            ) : null}
          </div>
        ) : undefined
      }
      blocks={blocks}
      decision={decision}
      documentId={documentId}
      issueKey={issueKey}
      spec={spec}
    />
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
        {ask.question}
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
  const view = useShownQuery({
    queryFn: () => api.getIssueDecisions(issueKey),
    queryKey: ["decisions", issueKey],
  });
  // Each decision beside the blocks of its text, read once per answer: a card draws them and
  // the Change composer quotes the first of them, so a block is a new object only when the
  // decisions were read again and a repaint of the tab repaints no body it already drew. The
  // three lists below are this one reading filtered, never three readings of it.
  const rows = useMemo(
    () =>
      (view.data?.decisions ?? []).map((decision) => ({
        blocks: readDecisionBlocks(decision),
        decision,
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
        {waiting.map(({ blocks, decision }) => (
          <WaitingDecision
            blocks={blocks}
            decision={decision}
            documentId={documentId}
            isClosed={isClosed}
            issueKey={issueKey}
            key={decision.block_id}
            spec={spec}
          />
        ))}
      </Section>
      <Section empty="No decisions of yours." title="Your decisions">
        {mine.map(({ blocks, decision }) => (
          <DecisionCard
            blocks={blocks}
            decision={decision}
            documentId={documentId}
            issueKey={issueKey}
            key={decision.block_id}
            spec={spec}
          />
        ))}
      </Section>
      <Section empty="No replaced decisions." title="Replaced">
        {replaced.map(({ blocks, decision }) => (
          <DecisionCard
            blocks={blocks}
            decision={decision}
            documentId={documentId}
            issueKey={issueKey}
            key={decision.block_id}
            spec={spec}
          />
        ))}
      </Section>
    </div>
  );
}
