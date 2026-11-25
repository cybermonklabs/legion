import { type ReactNode, useRef, useState } from "react";
import { Link } from "react-router-dom";

import type { Actor, Artifact } from "../../../api/types";
import {
  pageFocusedRow,
  pageInkText,
  pageKindBullet,
  pageKindTag,
  pageMutedText,
  pageOwnWordsRule,
  pageQuietText,
  pageWordsText,
} from "../../../theme/classes";
import { useAgentText } from "../../refs/AgentName";
import { EditedMark } from "../../refs/PostMarks";
import { postDeletion } from "../../refs/post-marks";
import { DecisionActions, rowActionSelector } from "../DecisionActions";
import { useDocumentStage } from "../document-stage";
import type { ParentLine } from "../issue-tree";
import { NodeGlyph } from "../NodeGlyph";
import {
  AskAnswer,
  BodyAuthor,
  KeepOpen,
  MoreWords,
  NODE_ROW_LAYOUT,
  NodeOptions,
  RowActs,
  RowMargin,
  type Tone,
  useClamp,
  useRowOnScreen,
} from "../NodeRow";
import { DeletedLine, lineOrKindWord, NodeParentLine, useNodeLine } from "../node-line";
import { shortDay } from "../node-time";
import { type ThreadMark, ThreadReadMark } from "../outline-thread-read";
import { useOutlineWriter } from "../outline-writer";
import { authorText, pageChrome } from "../page-structure";
import {
  type AuthorKind,
  authorKind,
  CONTROL_LABELS,
  DECISIONS_WORDS,
  GLYPH_TOKENS,
  isReader,
  NODE_TYPE_NAMES,
} from "../page-words";
import { RowText } from "../RowText";
import { pressAsksCaret } from "../useNodeEditing";
import type { AskDeclined, DecisionChange, DecisionsEntry, NamedEntry } from "./decisions-parts";
import { entryStanding } from "./entry-standing";

/** Who the page names: by a name and never an id, with the kind that picks their shape and
 *  whether they are the reader, whose name is drawn in the ink (R9, R34). */
interface RowPerson {
  readonly kind: AuthorKind;
  readonly name: string;
  readonly reader: boolean;
}

/**
 * One row of the Decisions filter, drawn as the prototype's `.dcard` is: "outline lines, the
 * same as the conversation's".
 *
 * It is a row of the outline, laid out by the outline's own classes (`NODE_ROW_LAYOUT`) and
 * read by the same marks (`[data-node-row]`, `[data-node-content]`, `[data-node-margin]`): its
 * bullet in its kind's colour, its words first, and its kind as one tag, who made it and when
 * on one line, at the right end of the first line from 761 px up and under the words on a
 * phone, with how it stands under them.
 *
 * An ask draws what its row in the conversation draws, by the same parts: its question, cut at
 * six lines once it is answered, its options, which are the choices of its answer while it is
 * open, and the person's answer as the store holds it, drawn by the row's own reader
 * (`RowText`), a paragraph to each block of its words, with no sentence of the page's beside
 * it. A declined ask says who declined it, and draws their note where they wrote one: a
 * question the person declined, never an answer.
 *
 * A decision draws its words - whole where it waits for the reader's review, cut as an agent's
 * words are anywhere else (ruling 6) - how it stands in the states the page declares
 * (`NODE_STATE_WORDS.decision`), and the statements its `shaped` ties name, each by the name
 * the body gives it (`DecisionsFilterBody.tsx`) and each a link to that statement. A decision
 * of the review list draws Keep and Change with no focus, as the picture does; no other row
 * draws either (R35). The two controls are `DecisionActions.tsx`'s own, so the writes are the
 * ones every other Keep and Change of the page makes. A decision a person asked to change draws
 * what they asked under its words, with a link to it in the conversation (the tester's F6).
 *
 * The node an address names where it is neither a question nor a decision, a comment say, is
 * drawn as the outline draws a node for its context (`NodeRow.tsx`): marked as one, in its grey,
 * with no reader's ink on its author. It draws its kind where its kind is a typed one, who wrote
 * it and when - a child issue its key alone, a link to its own page, as the outline's row does -
 * its words whole, its parent and its link, and no state, no Keep and no Change, since it is no
 * entry of the list. A comment or a message its author corrected carries its mark as the
 * outline's row does (INFRA-1523, R2 and R3): a deleted one draws the deleted line in place of
 * its words (`DeletedLine`), from the delete and never from its text, which names the actor as
 * no registry knows it and the time as stored; an edited one says ` · edited` after its time in
 * the margin (`RowMargin`), outside its words. The model holds both marks on a comment or a
 * message alone (`TreeNode.deletedAt`, `editedAt`), so no other kind of node carries either
 * mark on its own row. A decision's row draws two marks of other nodes: its parent's, on the
 * parent line, and its Change's ` · edited`, after the Change's words.
 *
 * The focus opens the row's body, for a decision as for an ask: who made the node where the
 * margin does not say it whole, and the control that keeps the body open.
 *
 * Every entry ends on a link to the node that holds it, in the conversation. Nothing inside a
 * row takes the tab order (R9): the row's own keys reach what it holds (`useTreeKeymap.ts`). The
 * one exception is the focused row's, as on the outline's rows: a block of its words whose
 * content runs past its box, a wide code block or table, keeps a tab stop of its own, so that a
 * reader on the keyboard can scroll it sideways (`RowText.tsx`).
 *
 * A press on the row's words puts the caret in them, as Enter on the row does, where the list
 * says the row takes one (`onPressWords`), and the row draws its document's one editor on its
 * stage in place of its words while it is the row edited (`editing`). A press on a control inside
 * the words - a link, a button, a drawn diagram's summary - is that control's own, and a click
 * that ends a selection made in the words, or a double click's second, asks for no caret
 * (`pressAsksCaret`); the row's acts and its links stand outside the words, so a press on one is
 * theirs and never the words'.
 */
export function DecisionsRow({
  body,
  document,
  editing,
  entry,
  focused,
  isClosed,
  level,
  onFocus,
  onPin,
  onPressWords,
  onReviewed,
  pinned,
  reader,
  report,
  review,
  rowKey,
  statementNames,
  tabStop,
  threadRead,
}: {
  /** Whether this row draws what its focus adds: it has the focus, the reader pinned it open,
   *  or the cycle key opens every body. */
  body: boolean;
  /** The decisions document a decision is a block of, which its Keep writes against. */
  document?: Artifact;
  /** The document this row's own text is being edited in, while this is the row edited (the
   *  list's `editing`): its one editor is drawn on the row's stage in place of the words. */
  editing?: string;
  entry: DecisionsEntry | NamedEntry;
  focused: boolean;
  /** Whether the issue is closed, which Keep and Change are refused on. */
  isClosed: boolean;
  /** How deep the row sits: 1, or 2 under the foot's own row (`aria-level`). */
  level: number;
  onFocus: () => void;
  onPin: () => void;
  /** A pointer's press on the row's words: the list puts the caret in the row's node where the
   *  row takes one, and does nothing else (`DecisionsFilterBody.tsx`'s `pressWords`). */
  onPressWords: () => void;
  /** Says the reader pressed Keep on this decision's row, or sent its Change form. The press
   *  alone is no review: the list reads the decision the server writes back to tell whether
   *  the reader kept it or asked to change it, and only that review takes it off the review
   *  list and moves the focus on to the next decision there. */
  onReviewed: () => void;
  pinned: boolean;
  /** Who is reading, whose own name is drawn in the ink and whose own words carry the ink rule
   *  (R34). */
  reader: string | undefined;
  /** What a key pressed on this row reported when it had nowhere to go. */
  report?: string;
  /** Whether this decision is an entry of the review list, which draws Keep and Change. */
  review: boolean;
  /** The row's key in the list's rows (`tree-keys.ts`'s `rowKeyOf`), which the editing finds the
   *  row it edits by (`data-row-key`). */
  rowKey: string;
  /** Each statement's name by its node id, as the body names it (`DecisionsFilterBody.tsx`),
   *  drawn as it is; a statement whose name is `""` is not named. */
  statementNames: ReadonlyMap<string, string>;
  /** Whether this row is the list's one tab stop. */
  tabStop: boolean;
  /** The thread whose newest write this row draws, which a dwell on the row marks read. */
  threadRead?: ThreadMark;
}): ReactNode {
  const node = entry.node;
  // Each person the row names, by the page's one rule for naming one (`AgentText.author`): a
  // name and a shape, never an id, and whether they are the reader (R9, R22, R34).
  const agentText = useAgentText();
  const person = (actor: Actor | undefined): RowPerson => ({
    kind: authorKind(actor),
    name: agentText.author(actor),
    reader: isReader(actor, reader),
  });
  const author = person(node.author);
  const ownAnswer = isReader(node.answer?.by, reader);
  const standing = entryStanding(entry, agentText.author);
  const deleted = postDeletion({ deleted_at: node.deletedAt, deleted_by: node.deletedBy });
  const row = useRef<HTMLDivElement | null>(null);
  const words = useRef<HTMLDivElement | null>(null);
  // Where the reader writes: an open question's options and Answer set it (R16).
  const writer = useOutlineWriter();
  const press = useRowOnScreen(row, focused);
  const stage = useDocumentStage(editing);
  // An answered question and an agent's decision are cut at six lines, as an agent's words are
  // on every row; a question that waits on the reader and a decision that waits for their
  // review are drawn whole, since they are what the reader is asked to read (rulings 1 and 6).
  // So is the node an address names, which the reader was sent to read: R5 (d) cuts no other
  // words.
  const cuttable =
    entry.kind === "ask"
      ? standing?.token === "answered" && !node.waiting
      : entry.kind === "decision"
        ? !review
        : false;
  const clamp = useClamp(words, cuttable);
  const [opened, setOpened] = useState(false);
  const clamped = clamp !== undefined && !opened && !body;
  const tone = toneOf(entry);
  // Each statement the entry shaped by its name, and none whose node holds no words: no
  // address, slug or id is drawn in a statement's place (R22).
  const statements = entry.shapes.flatMap((shape) => {
    const name = statementNames.get(shape.id) ?? "";
    return name === "" ? [] : [{ name, shape }];
  });
  return (
    <div
      aria-level={level}
      className="relative my-0.5 min-w-0 outline-none"
      data-node-context={entry.kind === "named" ? "" : undefined}
      data-node-id={node.id}
      data-node-row=""
      data-node-state={standing?.token}
      data-node-type={node.type}
      data-row-key={rowKey}
      onFocus={onFocus}
      onPointerDown={press}
      ref={row}
      role="treeitem"
      tabIndex={tabStop ? 0 : -1}
    >
      <div className={`${NODE_ROW_LAYOUT.box} ${focused ? pageFocusedRow : ""}`} data-node-card="">
        <span className={NODE_ROW_LAYOUT.bullet} data-node-bullet="">
          <i
            aria-hidden="true"
            className={`block size-[7px] rounded-full ${pageKindBullet[tone]}`}
          />
        </span>
        <div className={NODE_ROW_LAYOUT.content} data-node-content="">
          {/* A replaced decision's words are struck through and grey, beside the word that
              says so (the prototype's `.replaced`). */}
          {/* biome-ignore lint/a11y/noStaticElementInteractions lint/a11y/useKeyWithClickEvents: a
              pointer's way to what Enter on the row does; the words are no control of their own,
              and the keyboard reaches the same caret by Enter on the row. */}
          <div
            className={`relative text-[15px] leading-6 ${
              standing?.token === "replaced" ? `line-through ${pageQuietText}` : pageWordsText
            } ${clamped ? "overflow-clip" : ""}`}
            data-node-words=""
            onClick={(event) => {
              if (pressAsksCaret(event, editing)) onPressWords();
            }}
            ref={words}
            style={clamped ? { maxHeight: clamp.height } : undefined}
          >
            {editing !== undefined ? (
              <div className="flex min-w-0 flex-col" ref={stage} {...authorText} />
            ) : deleted === null ? (
              <RowText
                focused={focused}
                markdown={node.text}
                source={node.source}
                tree={node.tree}
              />
            ) : (
              <p className="break-words" {...authorText}>
                <DeletedLine deleted={deleted} />
              </p>
            )}
            {clamped && !clamp.pictures ? (
              <span
                aria-hidden="true"
                className="pointer-events-none absolute inset-x-0 bottom-0 h-8 bg-[linear-gradient(transparent,var(--color-page-paper))]"
              />
            ) : null}
          </div>
          {/* G's control under cut words, the one every view that cuts a node's words draws,
              so it opens and closes the rest as the outline's rows do. The body draws the words
              whole, so it offers nothing to open there. */}
          {clamp === undefined || body ? null : (
            <MoreWords
              hidden={clamp.hidden}
              onToggle={() => setOpened((held) => !held)}
              opened={opened}
              tabStops={false}
            />
          )}
          {node.options === undefined ? null : (
            <NodeOptions body={body} node={node} writer={writer} />
          )}
          {node.answer === undefined ? null : (
            <AskAnswer answerer={agentText.author(node.answer.by)} node={node} own={ownAnswer} />
          )}
          {entry.kind !== "ask" || entry.declined === undefined ? null : (
            <Declined by={person(entry.declined.by)} declined={entry.declined} focused={focused} />
          )}
          {entry.kind !== "decision" || entry.change === undefined ? null : (
            <Changed by={person(entry.change.by)} change={entry.change} focused={focused} />
          )}
          {entry.parent === undefined ? null : <EntryParentLine parent={entry.parent} />}
          {statements.length === 0 ? null : (
            <p
              className={`flex min-w-0 flex-wrap items-baseline gap-x-2 text-xs leading-5 ${pageQuietText}`}
              data-decision-informs=""
            >
              <span className="shrink-0" {...pageChrome}>
                {DECISIONS_WORDS.informs}
              </span>
              {statements.map(({ name, shape }) => (
                <span
                  className="min-w-0 break-words"
                  data-decision-statement={shape.id}
                  key={shape.id}
                >
                  {/* A sentence about the statement, so a colon sets it apart from the
                      statement's own words. */}
                  {shape.stale ? (
                    <span {...pageChrome}>{CONTROL_LABELS.target_stale}: </span>
                  ) : null}
                  {shape.path === undefined ? (
                    <span {...authorText}>{name}</span>
                  ) : (
                    <Link
                      className={`dispatch-inline-link font-semibold hover:underline ${pageKindTag.statement}`}
                      tabIndex={-1}
                      to={shape.path}
                      {...authorText}
                    >
                      {name}
                    </Link>
                  )}
                </span>
              ))}
            </p>
          )}
          {/* Keep and Change on one line with the entry's link (the prototype's `.dbtns`); the
              form Change opens, and what a refused write says, take a line of their own. A
              press of either, as a press anywhere on a row, gives the row the focus once the
              press is over (`usePressFocus`, which the row's focus reaches through `press.focus`
              in `DecisionsFilterBody.tsx`), so nothing moves under the pointer before the click
              lands. A Keep, by a press or by `Shift+K`, which presses it, and a send of the
              Change form are a press of the reader's on this row, which the row says to the
              list. Whether it wrote anything is the server's to say, on the decision itself: a
              refused write and a Change sent empty write nothing (`DecisionActions.tsx`), so the
              list reads the decision, not this row. */}
          <div
            className="mt-1 mb-0.5 flex min-w-0 flex-wrap items-center gap-x-3 gap-y-2 [&>[role=alert]]:order-last [&>[role=alert]]:basis-full [&>form]:order-last [&>form]:basis-full"
            data-entry-actions=""
            onClickCapture={(event) => {
              if (event.target instanceof Element && event.target.closest(KEEP) !== null)
                onReviewed();
            }}
            onSubmitCapture={onReviewed}
          >
            {review ? (
              <DecisionActions
                document={document}
                isClosed={isClosed}
                nodeReference={node.reference}
              />
            ) : null}
            {entry.path === undefined ? null : (
              <Link
                className={`dispatch-inline-link text-xs leading-5 font-medium underline decoration-page-hair underline-offset-[3px] ${pageMutedText}`}
                data-entry-link=""
                tabIndex={-1}
                to={entry.path}
                {...pageChrome}
              >
                {DECISIONS_WORDS.in_conversation} <span aria-hidden="true">›</span>
              </Link>
            )}
          </div>
          {/* What the focus adds to the row, as the outline's body adds it (`NodeRow.tsx`'s
              `NodeBody`), for a decision as for an ask: who made the node where the margin does
              not say it whole - "Author unknown", with its own shape, where the model holds no
              author, and an agent's whole session title where the margin names it by its role's
              word alone (ruling 5) - and the control that keeps the body open (R6). The open
              body also draws the rest of a cut text and what each option says beside its name,
              in their own places above. */}
          {body ? (
            <div
              className="mt-1 flex min-w-0 flex-col gap-1 text-sm"
              data-node-body=""
              data-pinned={pinned ? "" : undefined}
            >
              <BodyAuthor author={author} />
              <KeepOpen onPin={onPin} pinned={pinned} tabStops={false} />
            </div>
          ) : null}
          {focused && writer !== undefined ? (
            <RowActs node={node} ownAnswer={ownAnswer} writer={writer} />
          ) : null}
          {report === undefined ? null : (
            <p
              className={`text-xs leading-5 ${pageMutedText}`}
              data-row-report=""
              role="status"
              {...pageChrome}
            >
              {report}
            </p>
          )}
        </div>
        {/* The outline's own margin (`NodeRow.tsx`), given how the entry stands as its node's
            state: the token of the one reading `entryStanding` gives the row and the panel alike.
            So a decision shows its standing with the reader, not the tree's state, and "Kept
            by" with the keeper this list holds. A decision waits on the reader where it is an
            entry of the review list, which draws its state in the decisions' violet: one rule
            decides the list, and a second decides the mark on a row. An ask waits as the
            conversation says. The node an address names stands in no state here and is drawn
            for its context: in its grey, with no reader's ink on its author, and a child issue
            by its key, as the outline's row draws one. */}
        <RowMargin
          author={author}
          context={tone === "context"}
          node={{
            ...node,
            keptBy: entry.kind === "decision" ? entry.keptBy : undefined,
            state: standing?.token,
            waiting: entry.kind === "decision" ? review : node.waiting,
          }}
          tabStops={false}
          tone={tone}
        />
        {threadRead === undefined ? null : <ThreadReadMark mark={threadRead} />}
      </div>
    </div>
  );
}

/** Keep, by the mark `DecisionActions.tsx` writes and reads through its own
 *  `rowActionSelector`, so this file does not spell it. */
const KEEP = rowActionSelector("keep");

/** A question's colour and a decision's; and for the node an address names, which is no entry
 *  of the list, the grey G's outline gives a node it draws for its context (`NodeRow.tsx`). */
function toneOf(entry: DecisionsEntry | NamedEntry): Tone {
  switch (entry.kind) {
    case "ask":
      return "question";
    case "decision":
      return "decision";
    case "named":
      return "context";
  }
}

/**
 * That a person declined this question, under it (R2: "a declined ask reads as a question the
 * person declined and never as an answer"): who declined, by a name and a shape as every
 * author is drawn (R9), and the note they wrote where they wrote one. The note is drawn as a
 * person's answer under an ask is, by the row's own reader (`RowText`): from the server's
 * reading of it, and where the read sent none from the page's own parse of it, so its marks
 * are drawn as what they are and a reference reads as its target (R6). The reader's own note
 * carries the ink rule their own words carry (R34).
 */
function Declined({
  by,
  declined,
  focused,
}: {
  by: RowPerson;
  declined: AskDeclined;
  /** Whether the row holds the focus (`RowText`'s `focused`). */
  focused: boolean;
}): ReactNode {
  const { note } = declined;
  const own = by.reader && note !== undefined;
  return (
    <div
      className={`mt-1 flex min-w-0 flex-col ${own ? pageOwnWordsRule : ""}`}
      data-ask-declined=""
    >
      <p
        className={`flex min-w-0 flex-wrap items-center gap-x-1.5 text-xs leading-5 ${pageQuietText}`}
      >
        <span className="inline-flex items-center gap-1" data-declined-by="">
          <NodeGlyph className="size-3 shrink-0" token={GLYPH_TOKENS.authors[by.kind]} />
          <span className={by.reader ? `font-semibold ${pageInkText}` : ""} {...authorText}>
            {by.name}
          </span>
        </span>
        {/* A space between the two, so the line reads as one sentence to a reader who hears it
            or copies it: the gap the line draws is no character. */}{" "}
        <span {...pageChrome}>{DECISIONS_WORDS.declined}</span>
      </p>
      {note === undefined ? null : (
        <div
          className={`text-[15px] leading-6 ${own ? pageInkText : pageWordsText}`}
          data-declined-note=""
          {...authorText}
        >
          <RowText
            focused={focused}
            markdown={note}
            source={declined.source}
            tree={declined.tree}
          />
        </div>
      )}
    </div>
  );
}

/**
 * The node's parent in the conversation, as one line (R2, R1's first-line rule): its kind's word
 * and its one line, as every one line of a node is drawn (`node-line.tsx`'s `useNodeLine`, the
 * reading list's `node-row-parent-line`). A parent with words is drawn by the outline's own parent
 * line (`NodeParentLine`): its line is the link a reader follows to the parent where the model
 * gives it an address, out of the tab order as everything inside a row is, which the row's own
 * keys reach; a parent its author deleted is drawn as the deleted line, and one its author edited
 * says ` · edited` after its line (deleted-post R2 and R3). A parent with no words of its own is
 * named by its kind's word once, as plain text with no link, which is the line G gives it
 * (`lineOrKindWord`), and never by its id (R7); an edited one says ` · edited` after that word.
 * Nothing follows the word until the projection answers, one pre-paint commit on a page whose
 * engine is loaded.
 */
function EntryParentLine({ parent }: { parent: ParentLine }): ReactNode {
  const { line, reads } = useNodeLine(parent, { surface: "node-row-parent-line" });
  const words = lineOrKindWord(line, parent.type);
  if (words !== "" && words !== NODE_TYPE_NAMES[parent.type]) {
    return <NodeParentLine line={parent} tabStops={false} />;
  }
  return (
    <p
      className={`flex min-w-0 items-baseline gap-1 text-xs leading-5 ${pageQuietText}`}
      data-node-parent-line=""
      data-parent-id={parent.id}
      data-parent-type={parent.type}
    >
      <span className="shrink-0" {...pageChrome}>
        {NODE_TYPE_NAMES[parent.type]}
        <EditedMark at={parent.editedAt} />
      </span>
      {reads}
    </p>
  );
}

/**
 * The change a person asked of this decision, under its words (the tester's F6: "the change
 * must be drawn under, or tied to, the decision it changes"): who asked and when, as every
 * author is drawn (R9), what they wrote, and a link to it in the conversation, where it is
 * replied to. What they wrote is drawn as a person's answer under an ask is (`NodeRow.tsx`'s
 * `AskAnswer`), by the row's own reader (`RowText`): from the server's reading of it, and where
 * the read sent none from the page's own parse of it. The reader's own words carry the ink rule
 * (R34). A change its author edited says ` · edited` after the words (`EditedMark`, deleted-post
 * R2), in the page's own words; a deleted one is no change of the decision (`decisionChanges`)
 * and is not drawn here.
 */
function Changed({
  by,
  change,
  focused,
}: {
  by: RowPerson;
  change: DecisionChange;
  /** Whether the row holds the focus (`RowText`'s `focused`). */
  focused: boolean;
}): ReactNode {
  return (
    <div
      className={`mt-1 flex min-w-0 flex-col ${by.reader ? pageOwnWordsRule : ""}`}
      data-change-id={change.id}
      data-decision-change=""
    >
      <p
        className={`flex min-w-0 flex-wrap items-center gap-x-1.5 text-xs leading-5 ${pageQuietText}`}
      >
        <Link
          className="dispatch-inline-link underline decoration-page-hair underline-offset-[3px]"
          data-change-link=""
          tabIndex={-1}
          to={change.path}
          {...pageChrome}
        >
          {DECISIONS_WORDS.change}
        </Link>
        <span className="inline-flex items-center gap-1" data-change-by="">
          <NodeGlyph className="size-3 shrink-0" token={GLYPH_TOKENS.authors[by.kind]} />
          <span className={by.reader ? `font-semibold ${pageInkText}` : ""} {...authorText}>
            {by.name}
          </span>
        </span>
        <span {...pageChrome}>·</span>
        <span className="whitespace-nowrap" {...authorText}>
          {shortDay(change.at)}
        </span>
      </p>
      <div
        className={`text-[15px] leading-6 ${by.reader ? pageInkText : pageWordsText}`}
        data-change-words=""
        {...authorText}
      >
        <RowText
          focused={focused}
          markdown={change.body}
          source={change.source}
          tree={change.tree}
        />
        <EditedMark at={change.editedAt} marks={pageChrome} />
      </div>
    </div>
  );
}
