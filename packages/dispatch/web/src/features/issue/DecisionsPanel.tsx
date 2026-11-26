import { dispatchWritingBox } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useId, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { replaceQueryData } from "../../api/query-client";
import type {
  Artifact,
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
  linkHoverText,
  linkText,
  primaryButton,
  secondaryButtonCompact,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { MentionField } from "../conversation/MentionField";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { decisionBlockId, decisionDocumentId } from "../issue-page/decision-review";
import { MarkdownBody } from "../refs/MarkdownBody";
import { PlainText } from "../refs/plain-text";
import { buildIssuePath, parseDispatchReference, referenceHref } from "../refs/routes";
import { textSourceOf } from "../refs/text-references";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { entryTextSource, isPersonDecision } from "./decision-entries";
import { decisionHeading, useDecisionHeadingWords } from "./decision-heading";
import {
  isPlainTitleBlock,
  isPlainTitleMarkdown,
  isUnsureBlock,
  nameBlock,
  ownTree,
  ownWords,
  plainParagraph,
  UNSURE_PREFIX,
  unsureWords,
  wordsOf,
} from "./decision-text";

/** A decision as the cards below draw one (`entryDecision`): its name, the block it is, the asks
 *  it answers and the nodes it shaped beside the entry's own words, sum and standing, and the
 *  entry itself, which its heading is read from (`useDecisionHeadingWords`). */
type DrawnDecision = Pick<
  DecisionEntry,
  "kept" | "kind" | "ref" | "replaced" | "text" | "text_sha256" | "text_tree" | "waiting_review"
> & {
  readonly asks: readonly string[];
  readonly block_id: string;
  readonly entry: DecisionEntry;
  readonly heading: string;
  readonly links: readonly DecisionLink[];
};

/** One block of a decision's text, or the doubt its node holds, as a view draws one: the block's
 *  own text, the server's reading with that block alone in it where the answer carried one, and
 *  whether the block is the decision's doubt. */
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
  /** Whether the block is the doubt the decision's node holds rather than a block of its text
   *  (`entryBlocks`): no document text holds it, so a Change never quotes it. */
  readonly fromNode?: true;
  /** Whether the card's plain heading shows all of this naming block (`isPlainTitleBlock`): the
   *  card omits it from the body, while a Change still quotes it, as the issue page's Change
   *  does (`anchorQuote`). A naming block with a link, mark, image, reference, or block
   *  structure is not marked `naming` and stays in the body. */
  readonly naming?: true;
}

/** The blocks of a decision whose answer carried no reading of its text: a pod of the build
 *  before this one serves none during a rolling deploy, and the markdown split the browser has
 *  always made is what draws the decision then, as every other view falls back to parsing. The
 *  decision's name there is the first line of its markdown (`decisionName`), so the first block
 *  is marked `naming` only where it equals that name and holds plain text alone
 *  (`isPlainTitleMarkdown`). A multi-line paragraph, a directive fence, or a line with a link,
 *  mark, or reference stays in the body. A heading is dropped by the split, as before. */
function markdownBlocks(text: string, name: string | undefined): DecisionBlock[] {
  return text
    .split(/\n\s*\n/)
    .map((block) => block.trim())
    .filter((block) => block !== "")
    .flatMap((markdown, index) =>
      markdown.startsWith("#")
        ? []
        : [
            {
              ...(index === 0 && markdown === name && isPlainTitleMarkdown(markdown)
                ? { naming: true as const }
                : {}),
              // The block's own markdown is its text here, the Change quote included: with no
              // reading there is nothing to flatten, and parsing the markdown in the browser to
              // build a quote is the reading a stored text's tree exists to spare every view. It
              // is what the references are read from as well, because it is what the browser
              // parsed to draw this block: a reference some other block of the decision cites is
              // no reference of this one. The server finds a quote in the document's flattened
              // words alone (`pmdoc.FindQuote`), so this quote anchors where the block holds no
              // markup, and a Change sent with one that does is refused and stores nothing (the
              // reading list's `anchor-quote`).
              text: markdown,
              references: markdown,
              tree: undefined,
              unsure: markdown.startsWith(UNSURE_PREFIX),
            },
          ]
    );
}

/** The blocks of a decision's text, each marked unsure or not, taken from the tree Go read its
 *  text into: the top-level blocks of that tree are the paragraphs the markdown would have been
 *  split into, already divided, so the browser splits nothing and parses nothing. A heading is
 *  the decision's own, drawn by the card above its text, or one written inside it, which the
 *  markdown split dropped too. Where `named` is set, the naming block (`nameBlock`) is marked
 *  `naming` only when the plain heading shows all of it (`isPlainTitleBlock`), such as a
 *  converted section's plain-text heading paragraph. Any naming block with a link, inline code,
 *  mark, image, reference, or block structure stays in the body. */
function readDecisionBlocks(decision: DrawnDecision, named: boolean): readonly DecisionBlock[] {
  const tree = decision.text_tree;
  const content = tree?.content;
  if (tree === undefined || !Array.isArray(content)) {
    return markdownBlocks(decision.text, named ? decision.heading : undefined);
  }
  const nodes = content as DisplayTree[];
  const name = named ? nameBlock(nodes) : undefined;
  const hiddenName = name !== undefined && isPlainTitleBlock(name) ? name : undefined;
  return nodes
    .filter((node) => node.type !== "heading")
    .map((node) => ({
      ...(node === hiddenName ? { naming: true as const } : {}),
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

/** An entry as the cards below draw a decision: an ask by its question, with the person's answer
 *  as its text and the server's reading of that answer as its tree (`answer_tree`), or the
 *  labels they chose where they wrote no words of their own; an agent's decision by its own
 *  content, named by its first block that holds words, as the server and every other view name
 *  it (`decisionHeading`), with the asks its `from_ask` settles. Its block is the one the read
 *  names for it (`decisionBlockId`), and an ask on the issue itself, which stands in no block, is
 *  named by its node. */
function entryDecision(entry: DecisionEntry): DrawnDecision {
  const ask = entry.node.kind === "ask";
  const tree = ask ? entry.answer_tree : ownTree(entry.text_tree);
  const text = ask ? (entry.answer ?? (entry.selected ?? []).join(", ")) : ownWords(entry.text);
  return {
    asks: ask ? [entry.node.id] : (entry.from_ask ?? []),
    block_id: decisionBlockId(entry) ?? entry.node.id,
    entry,
    heading: decisionHeading(entry),
    kept: entry.kept,
    kind: entry.kind,
    links: entry.shaped ?? [],
    ref: entry.ref,
    replaced: entry.replaced,
    text,
    text_sha256: entry.text_sha256,
    text_tree: tree,
    waiting_review: entry.waiting_review,
  };
}

/** The blocks of an entry's text, from the tree the server read that text into
 *  (`readDecisionBlocks`), with a plain naming block marked where the card's title is the
 *  decision's own words rather than an ask's question. An ask answered by the options chosen alone
 *  is the one entry whose text no stored text holds - the labels chosen, joined - so no reading of
 *  the server's covers it, and the renderer draws it from a reading of one text run, the plain
 *  words it is, as the Answers page draws a chosen label, rather than parsing it. The doubt a
 *  decision's node holds (`unsureWords`) is drawn last, as the `Unsure:` paragraph it was moved
 *  from drew, unless the text still holds that paragraph. */
function entryBlocks(entry: DecisionEntry, decision: DrawnDecision): readonly DecisionBlock[] {
  if (entry.node.kind !== "ask" || entry.answer !== null) {
    const blocks = readDecisionBlocks(decision, entry.question === null);
    const doubt = unsureWords(entry);
    if (doubt === undefined || blocks.some((block) => block.unsure)) return blocks;
    return [
      ...blocks,
      {
        fromNode: true,
        references: doubt,
        text: doubt,
        tree: { content: [plainParagraph(doubt)], type: "doc" },
        unsure: true,
      },
    ];
  }
  if (decision.text === "") return [];
  return [
    {
      references: decision.text,
      text: decision.text,
      tree: { content: [plainParagraph(decision.text)], type: "doc" },
      unsure: false,
    },
  ];
}

/** The in-app path of the node a dispatch:// address names - the node that holds an entry, which
 *  is not always the spec - or undefined for one no page of this dashboard opens, a person's
 *  (`referenceHref`) among them. */
function nodePath(ref: string): string | undefined {
  const route = parseDispatchReference(ref);
  return route === undefined ? undefined : referenceHref(route);
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

/** The nodes a decision shaped, each linked where its own address names it - a block of the spec,
 *  or of another document of the issue, such as a plan's task (`decisionread` answers a `shaped`
 *  target in any document of the issue) - never the spec at another document's block id. A target
 *  whose address opens no page of this dashboard, or that names no block, draws nothing. */
function ShapedLinks({ links }: { links: readonly DecisionLink[] | null }): ReactNode {
  const shaped = (links ?? []).flatMap((link) => {
    const blockId = link.to.block_id;
    const path = link.to.ref === undefined ? undefined : nodePath(link.to.ref);
    return blockId === undefined || path === undefined ? [] : [{ blockId, link, path }];
  });
  if (shaped.length === 0) {
    return null;
  }
  return (
    <div className="space-y-1">
      <p className={`text-xs ${textMutedOnSurface}`}>Shaped:</p>
      <ul className="space-y-1 text-sm">
        {shaped.map(({ blockId, link, path }) => (
          <li key={`${link.from_block}:${link.to.ref}`}>
            {/* The link names the block by its id, in the panel's own words, as the characters
                they are. The decisions read sends no text of the block a link points to
                (`refs.BlockLinks` resolves each end as a graph node, which carries its text only
                on a reference read), so there is no stored text here to draw. */}
            <Link className={`${linkText} ${linkHoverText}`} to={path}>
              {`Block ${blockId}`}
            </Link>
            {link.stale ? (
              <span className={`ml-2 text-xs ${textMutedOnSurface}`}>out of date</span>
            ) : null}
          </li>
        ))}
      </ul>
    </div>
  );
}

function ChangeComposer({
  documentId,
  issueKey,
  name,
  onDone,
  quote,
}: {
  documentId: string;
  issueKey: string;
  /** The decision's heading as plain words (`useDecisionHeadingWords`), which the form and its
   *  write are named after. */
  name: string;
  onDone: () => void;
  quote: string;
}): ReactNode {
  const [body, setBody] = useState("");
  const send = useKeyedWrite({
    inline: true,
    // A write's label holds no markup: the reading list's `decision-heading-words`.
    label: () => `Change to ${name}`,
    method: "POST",
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
      // A form's name holds no markup: the reading list's `decision-heading-words`.
      aria-label={`Change ${name}`}
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
        {/* The decision's own words, as they are: the reading list's `anchor-quote`. */}
        {quote}
      </blockquote>
      <MentionField
        body={body}
        issueKey={issueKey}
        label="Your change"
        labelClassName={`block text-sm ${textPrimaryOnSurface}`}
        onChange={(change) => setBody(change.body)}
        rows={3}
        writing={dispatchWritingBox("decisions-change", "comment")}
      />
      {send.error === null ? null : (
        <p className={`text-sm ${dangerText}`} role="alert">
          {/* A change the reader stopped may have been posted, and Send is a press of its own,
              under a key of its own. The error's own words are a composer's, of its Retry. */}
          {send.error instanceof SendStoppedError
            ? stoppedWriteNote("your change", "posted", {
                anew: "Send posts it again as a new comment.",
              })
            : send.error.message}
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
}: {
  actions?: ReactNode;
  /** The blocks of this decision's text, with a plain naming block marked, read by the caller
   *  that also quotes the first of them its text holds (`entryBlocks`). */
  blocks: readonly DecisionBlock[];
  decision: DrawnDecision;
  /** The document the decision's block is written in (`decisionDocumentId`): what a Keep and a
   *  Change are written against, and the record a `decision` block's words are, whoever decided
   *  it, whose `#id`s name a question block of it. An ask's words are the ask's own record
   *  (`entryTextSource`). Undefined where the read names no document and the address names no
   *  block of a document the issue read lists: an ask, and a block of a document not listed
   *  yet. */
  documentId: string | undefined;
  issueKey: string;
}): ReactNode {
  const source = entryTextSource(decision.entry, documentId, issueKey);
  // The name links the node that holds the decision, which is not always the spec.
  const path = nodePath(decision.ref);
  // The heading's plain words word its link (the reading list's `decision-heading-words`); the
  // heading's reading draws it where there is no link.
  const {
    reads,
    tree: headingTree,
    words: heading,
  } = useDecisionHeadingWords(decision.entry, source);
  return (
    <li className={`space-y-2 rounded-lg border p-3 ${card}`} data-decision={decision.block_id}>
      <h3 className={`text-[15px] font-medium ${textPrimaryOnSurface}`}>
        {reads}
        {path === undefined ? (
          <MarkdownBody
            markdown={decision.heading}
            source={source}
            surface="issue-decision"
            tree={headingTree}
            variant="inline"
          />
        ) : (
          <Link className={`${linkText} ${linkHoverText}`} to={path}>
            {/* A link's words hold no markup: the reading list's `decision-heading-words`. */}
            {heading}
          </Link>
        )}
      </h3>
      {blocks.map((block, index) => {
        // A plain naming block is already shown in full by the heading above.
        if (block.naming) return null;
        return block.unsure ? (
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
        );
      })}
      {(decision.asks ?? []).map((ask) => (
        <Link
          className={`block text-sm ${linkText} ${linkHoverText}`}
          key={ask}
          to={buildIssuePath({ id: ask, key: issueKey, kind: "ask" })}
        >
          The ask it answers
        </Link>
      ))}
      <ShapedLinks links={decision.links} />
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
}: {
  blocks: readonly DecisionBlock[];
  decision: DrawnDecision;
  documentId: string | undefined;
  isClosed: boolean;
  issueKey: string;
}): ReactNode {
  const queryClient = useQueryClient();
  const [changing, setChanging] = useState(false);
  // A change anchors on the decision's first block that is no heading, the one that names it
  // included, by the words that block holds, as the issue page's Change anchors (`anchorQuote`):
  // an anchor quote names the document's own text, which is what the server matches it against
  // (`pmdoc.FindQuote`). The doubt the node holds is no text of the document. A block drawn from
  // its markdown, with no reading, quotes that markdown (`markdownBlocks`).
  const quote = blocks.find((block) => block.fromNode === undefined)?.text;
  // The heading as plain words, which the Keep and the Change are named after: a write's label
  // and a form's name hold no markup (the reading list's `decision-heading-words`).
  const { reads, words: name } = useDecisionHeadingWords(
    decision.entry,
    entryTextSource(decision.entry, documentId, issueKey)
  );
  const keep = useKeyedWrite({
    inline: true,
    // A write's label holds no markup: the reading list's `decision-heading-words`.
    label: () => `Keep ${name}`,
    method: "POST",
    // The sum of the text this card showed: a decision the agent changed since is refused
    // (`409 DECISION_CHANGED`), so a Keep never stands over words the reader did not see.
    write: (id: string, bounds) =>
      api.keepDecision(id, decision.block_id, decision.text_sha256, bounds),
    onSuccess: (kept) => {
      if (kept === null) {
        void queryClient.invalidateQueries({ exact: true, queryKey: ["decisions", issueKey] });
        return;
      }
      // Over any read of the decisions still out, which was asked before the Keep and would put
      // the decision back as waiting (`replaceQueryData`).
      replaceQueryData<IssueDecisions>(queryClient, ["decisions", issueKey], (view) =>
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
            {reads}
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
                {/* A Keep the reader stopped may have been made, and Keep is a press of its
                    own, under a key of its own. */}
                {keep.error instanceof SendStoppedError
                  ? // The decision in plain words, as Keep's label: `decision-heading-words`.
                    stoppedWriteNote(`the Keep of “${name}”`, "made", {
                      anew: "Pressing Keep again sends a new Keep.",
                    })
                  : keep.error.message}
              </p>
            )}
            {changing && quote !== undefined ? (
              <ChangeComposer
                documentId={documentId}
                issueKey={issueKey}
                name={name}
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
    />
  );
}

function OpenQuestion({ entry, issueKey }: { entry: DecisionEntry; issueKey: string }): ReactNode {
  const path =
    nodePath(entry.ref) ?? buildIssuePath({ id: entry.node.id, key: issueKey, kind: "ask" });
  return (
    <li className={`rounded-lg border p-3 ${card}`}>
      <Link className={`text-sm ${linkText} ${linkHoverText}`} to={path}>
        <PlainText
          markdown={entry.question ?? ""}
          source={textSourceOf("ask", entry.node.id, issueKey)}
          surface="issue-open-question"
          tree={entry.text_tree}
        >
          {/* The question as its plain words, and as written until the engine has answered:
              the reading list's `issue-open-question`. An ask's entry holds its question as its
              text, so the entry's reading is the question's. */}
          {(words) => words ?? entry.question}
        </PlainText>
      </Link>
    </li>
  );
}

/** The issue's Decisions tab: its open questions, the agent's decisions waiting for a person's
 *  review (with Keep and Change), the person's own decisions, and the replaced ones, read from
 *  the nodes of `GET /api/v1/issues/{key}/tree?view=decisions`. Each decision links the node
 *  that holds it and the nodes it shaped. */
export function DecisionsPanel({
  artifacts,
  isClosed,
  issueKey,
}: {
  artifacts: readonly Artifact[];
  isClosed: boolean;
  issueKey: string;
}): ReactNode {
  const view = useQuery(issueDecisionsQuery(issueKey, true));
  const spec = artifacts.find((artifact) => artifact.primary && artifact.kind === "doc");
  // Each decision beside the blocks of its text, read once per answer: a card draws them and
  // the Change composer quotes the first of them its text holds (`entryBlocks`), so a block is
  // a new object only when the decisions were read again and a repaint of the tab repaints no
  // body it already drew. The three lists below are this one reading filtered, never three
  // readings of it. An ask is a decision once a person answered it; an open one is a question,
  // and a declined one decided nothing. Each is written in the document the read names for it,
  // or, where it names none, the one its own address names a block of (`decisionDocumentId`);
  // an ask's words are the ask's own record (`entryTextSource`).
  const rows = useMemo(() => {
    const documents = { artifacts, key: issueKey, primary_artifact_id: spec?.id ?? "" };
    return (view.data?.entries ?? [])
      .filter((entry) => entry.node.kind !== "ask" || entry.answered_at !== null)
      .map((entry) => {
        const decision = entryDecision(entry);
        const documentId = decisionDocumentId(documents, entry);
        return { blocks: entryBlocks(entry, decision), decision, documentId };
      });
  }, [artifacts, issueKey, spec?.id, view.data?.entries]);
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
  const entries = view.data.entries ?? [];
  const questions = entries.filter((entry) => entry.answer_state === "open");
  const waiting = rows.filter((row) => row.decision.waiting_review);
  // Yours: the ones a person decided (`isPersonDecision`), and the agent's that the reader kept:
  // a Keep takes a decision out of "Waiting for your review", and a decision nothing holds would
  // leave the tab.
  const mine = rows.filter(
    ({ decision }) =>
      !decision.replaced && (isPersonDecision(decision.entry) || decision.kept !== null)
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
        {waiting.map(({ blocks, decision, documentId }) => (
          <WaitingDecision
            blocks={blocks}
            decision={decision}
            documentId={documentId}
            isClosed={isClosed}
            issueKey={issueKey}
            key={decision.ref}
          />
        ))}
      </Section>
      <Section empty="No decisions of yours." title="Your decisions">
        {mine.map(({ blocks, decision, documentId }) => (
          <DecisionCard
            blocks={blocks}
            decision={decision}
            documentId={documentId}
            issueKey={issueKey}
            key={decision.ref}
          />
        ))}
      </Section>
      <Section empty="No replaced decisions." title="Replaced">
        {replaced.map(({ blocks, decision, documentId }) => (
          <DecisionCard
            blocks={blocks}
            decision={decision}
            documentId={documentId}
            issueKey={issueKey}
            key={decision.ref}
          />
        ))}
      </Section>
    </div>
  );
}
