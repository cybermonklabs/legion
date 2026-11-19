import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useId, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type {
  Artifact,
  Decision,
  DecisionEntry,
  DecisionLink,
  DisplayTree,
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
import { markdownClassName } from "../refs/markdown-engine";
import {
  buildIssuePath,
  buildReferencePath,
  documentItemPath,
  parseDispatchReference,
} from "../refs/routes";
import { textSourceOf } from "../refs/text-references";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { ownTree, ownWords, wordsOf } from "./decision-text";

/** The word that opens a paragraph a decision's words give as its doubt, which the card draws
 *  with the unsure accent. */
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
  /** Whether the block is drawn as the plain words it holds rather than as markdown: the one
   *  block of an answer that chose options and wrote nothing (`entryBlocks`). */
  readonly plain?: true;
}

/** Whether a block is the decision's doubt: a paragraph whose markdown starts `Unsure:` and
 *  nothing else. In the reading of such a paragraph the word stands in a first child that is
 *  plain text, since a mark over it - strong, a code span, a link - would have written its own
 *  characters first. */
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

/** A decision's name: the words of the first heading its content holds, else none. */
function headingOf(tree: DisplayTree | undefined, text: string): string {
  const content = tree?.content;
  if (tree !== undefined && Array.isArray(content)) {
    const heading = (content as DisplayTree[]).find((node) => node.type === "heading");
    return heading === undefined ? "" : wordsOf(heading);
  }
  const line = text.split("\n").find((written) => written.startsWith("#"));
  return line === undefined ? "" : line.replace(/^#+\s*/, "").trim();
}

/** An entry as the cards below draw a decision: an ask by its question, with the person's answer
 *  as its text and the server's reading of that answer as its tree (`answer_tree`), or the
 *  labels they chose where they wrote no words of their own; an agent's decision by its own
 *  content. */
function entryDecision(entry: DecisionEntry): Decision {
  const ask = entry.node.kind === "ask";
  const tree = ask ? entry.answer_tree : ownTree(entry.text_tree);
  const text = ask ? (entry.answer ?? (entry.selected ?? []).join(", ")) : ownWords(entry.text);
  return {
    asks: ask ? [entry.node.id] : [],
    block_id: entry.document?.block_id ?? entry.node.id,
    changed_at: entry.changed_at,
    changed_version: entry.changed_version,
    heading: entry.question ?? headingOf(tree, text),
    kept: entry.kept,
    kind: entry.kind,
    links: entry.shaped,
    ref: entry.ref,
    replaced: entry.replaced,
    text,
    text_sha256: entry.text_sha256,
    text_tree: tree,
    unsure: entry.uncertainty === "high",
    waiting_review: entry.waiting_review,
  };
}

/** The blocks of an entry's text below its heading, from the tree the server read that text into
 *  (`readDecisionBlocks`). An ask answered by the options chosen alone is the one entry whose
 *  text no stored text holds - the labels chosen, joined - so no reading of the server's covers
 *  it, and it is drawn as the plain words it is, as the Answers page draws a chosen label,
 *  rather than parsed. */
function entryBlocks(entry: DecisionEntry, decision: Decision): readonly DecisionBlock[] {
  if (entry.node.kind !== "ask" || entry.answer !== null) return readDecisionBlocks(decision);
  if (decision.text === "") return [];
  return [
    { plain: true, references: decision.text, text: decision.text, tree: undefined, unsure: false },
  ];
}

/** The in-app path of the node a dispatch:// address names - the node that holds an entry, which
 *  is not always the spec - or undefined for one no route opens. */
function nodePath(ref: string): string | undefined {
  const route = parseDispatchReference(ref);
  return route === undefined ? undefined : buildReferencePath(route);
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
  const send = useKeyedWrite({
    inline: true,
    label: () => `Change to ${decision.heading}`,
    // The decision's document belongs to an issue, and a comment on an issue's document is the
    // issue's own write: the artifact route refuses it, naming the issue route (`send-request.ts`
    // follows the same rule for a thread's replies).
    write: (input: { body: string; quote: string }, bounds) =>
      api.createComment(
        issueKey,
        { anchor: { artifact: documentId, quote: input.quote }, body: input.body },
        bounds
      ),
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
  /** The document the decision is written in, the one its node stands in, which its `#id`s name
   *  a question block of; undefined for an ask on the issue itself. */
  documentId: string | undefined;
  issueKey: string;
  spec: Artifact | undefined;
}): ReactNode {
  const source = textSourceOf("artifact", documentId);
  // The name links the node that holds the decision, which is not always the spec.
  const path = nodePath(decision.ref);
  return (
    <li className={`space-y-2 rounded-lg border p-3 ${card}`} data-decision={decision.block_id}>
      <h3 className={`text-[15px] font-medium ${textPrimaryOnSurface}`}>
        {path === undefined ? (
          decision.heading
        ) : (
          <Link className={`${linkText} ${linkHoverText}`} to={path}>
            {decision.heading}
          </Link>
        )}
      </h3>
      {blocks.map((block, index) =>
        block.plain ? (
          // The labels an answer chose, drawn as a body's paragraph draws its words.
          // biome-ignore lint/suspicious/noArrayIndexKey: the tree's blocks have no id of their own
          <div className={`${markdownClassName} max-w-none`} key={index}>
            <p>{block.text}</p>
          </div>
        ) : block.unsure ? (
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
  const queryClient = useQueryClient();
  const [changing, setChanging] = useState(false);
  // A change anchors on the decision's first block below its heading, by the words that block
  // holds: an anchor quote names the document's own text, which is what the server matches it
  // against (`pmdoc.FindQuote`).
  const quote = blocks[0]?.text;
  const keep = useKeyedWrite({
    inline: true,
    issue: issueKey,
    label: () => `Keep ${decision.heading}`,
    // The sum of the text this card showed: a decision the agent changed since is refused
    // (`409 DECISION_CHANGED`), so a Keep never stands over words the reader did not see.
    write: (id: string, bounds) =>
      api.keepDecision(id, decision.block_id, decision.text_sha256, bounds),
    onSuccess: (kept) => {
      if (kept === null) {
        void queryClient.invalidateQueries({ exact: true, queryKey: ["decisions", issueKey] });
        return;
      }
      queryClient.setQueryData<IssueDecisions>(["decisions", issueKey], (view) =>
        view === undefined
          ? view
          : {
              ...view,
              // The Keep is of the text this card showed, which the server took, so it applies
              // to the entry of this node that still holds that text.
              entries: (view.entries ?? []).map((row) =>
                row.ref === decision.ref && row.text_sha256 === decision.text_sha256
                  ? { ...row, kept, kept_by: kept.kept_by, waiting_review: false }
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

function OpenQuestion({ entry, issueKey }: { entry: DecisionEntry; issueKey: string }): ReactNode {
  const path =
    nodePath(entry.ref) ?? buildIssuePath({ id: entry.node.id, key: issueKey, kind: "ask" });
  return (
    <li className={`rounded-lg border p-3 ${card}`}>
      <Link className={`text-sm ${linkText} ${linkHoverText}`} to={path}>
        {entry.question}
      </Link>
    </li>
  );
}

/** The issue's Decisions tab: its open questions, the agent's decisions waiting for a person's
 *  review (with Keep and Change), the person's own decisions, and the replaced ones, read from
 *  the entries of `GET /api/v1/issues/{key}/decisions`. Each decision links the node that holds
 *  it and the spec blocks it shaped. */
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
  // Each decision beside the blocks of its text, read once per answer: a card draws them and
  // the Change composer quotes the first of them, so a block is a new object only when the
  // decisions were read again and a repaint of the tab repaints no body it already drew. The
  // three lists below are this one reading filtered, never three readings of it. An ask is a
  // decision once a person answered it; an open one is a question, and a declined one decided
  // nothing.
  const rows = useMemo(
    () =>
      (view.data?.entries ?? [])
        .filter((entry) => entry.node.kind !== "ask" || entry.answered_at !== null)
        .map((entry) => {
          const decision = entryDecision(entry);
          return { blocks: entryBlocks(entry, decision), decision, entry };
        }),
    [view.data?.entries]
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
  const entries = view.data.entries ?? [];
  const questions = entries.filter((entry) => entry.answer_state === "open");
  const waiting = rows.filter((row) => row.decision.waiting_review);
  // Yours: the ones a person decided, and the agent's that the reader kept — a Keep takes a
  // decision out of "Waiting for your review", and a decision nothing holds would leave the tab.
  const mine = rows.filter(
    ({ decision }) => !decision.replaced && (decision.kind === "person" || decision.kept !== null)
  );
  const replaced = rows.filter((row) => row.decision.replaced);
  return (
    <div className="space-y-4">
      {entries.length === 0 ? (
        <p className={`text-sm ${textMutedOnSurface}`}>This issue has no decisions yet.</p>
      ) : null}
      <Section empty="No open questions." title="Open questions">
        {questions.map((entry) => (
          <OpenQuestion entry={entry} issueKey={issueKey} key={entry.ref} />
        ))}
      </Section>
      <Section empty="Nothing waits for your review." title="Waiting for your review">
        {waiting.map(({ blocks, decision, entry }) => (
          <WaitingDecision
            blocks={blocks}
            decision={decision}
            documentId={entry.document?.artifact_id}
            isClosed={isClosed}
            issueKey={issueKey}
            key={decision.ref}
            spec={spec}
          />
        ))}
      </Section>
      <Section empty="No decisions of yours." title="Your decisions">
        {mine.map(({ blocks, decision, entry }) => (
          <DecisionCard
            blocks={blocks}
            decision={decision}
            documentId={entry.document?.artifact_id}
            issueKey={issueKey}
            key={decision.ref}
            spec={spec}
          />
        ))}
      </Section>
      <Section empty="No replaced decisions." title="Replaced">
        {replaced.map(({ blocks, decision, entry }) => (
          <DecisionCard
            blocks={blocks}
            decision={decision}
            documentId={entry.document?.artifact_id}
            issueKey={issueKey}
            key={decision.ref}
            spec={spec}
          />
        ))}
      </Section>
    </div>
  );
}
