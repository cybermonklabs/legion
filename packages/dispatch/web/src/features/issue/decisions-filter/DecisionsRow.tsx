import { type ReactNode, useRef, useState } from "react";
import { Link } from "react-router-dom";

import type { Artifact } from "../../../api/types";
import {
  pageFocusedRow,
  pageInkText,
  pageKindBullet,
  pageKindTag,
  pageMutedText,
  pageOwnWordsRule,
  pageQuietText,
  pageStateText,
  pageWordsText,
} from "../../../theme/classes";
import { agentRoleWord } from "../../refs/agent-role";
import { DecisionActions } from "../DecisionActions";
import { NodeGlyph } from "../NodeGlyph";
import {
  AskAnswer,
  NODE_ROW_LAYOUT,
  NodeOptions,
  RowActs,
  useClamp,
  useRowOnScreen,
} from "../NodeRow";
import { shortDay } from "../node-time";
import { type ThreadMark, ThreadReadMark } from "../outline-thread-read";
import { useOutlineWriter } from "../outline-writer";
import { authorText, pageChrome } from "../page-structure";
import {
  type AuthorKind,
  CONTROL_LABELS,
  DECISIONS_WORDS,
  GLYPH_TOKENS,
  NODE_TYPE_NAMES,
  nodeStateWord,
} from "../page-words";
import { RowText } from "../RowText";
import type { DecisionChange, DecisionsEntry } from "./decisions-parts";

/** Who the page names: by a name and never an id, with the kind that picks their shape and
 *  whether they are the reader, whose name is drawn in the ink (R9, R34). */
export interface RowPerson {
  readonly kind: AuthorKind;
  readonly name: string;
  readonly reader: boolean;
}

/**
 * One row of the Decisions filter (dispatch://INFRA-1196 R2, R9, R34, R35, the prototype's
 * `.dcard`: "outline lines, the same as the conversation's").
 *
 * It is a row of the outline, laid out by the outline's own classes (`NODE_ROW_LAYOUT`) and
 * read by the same marks (`[data-node-row]`, `[data-node-content]`, `[data-node-margin]`): its
 * bullet in its kind's colour, its words first, and its kind as one tag, who made it and when
 * on one line, at the right end of the first line from 761 px up and under the words on a
 * phone, with how it stands under them.
 *
 * An ask draws what its row in the conversation draws, by the same parts: its question, cut at
 * six lines once it is answered, its options, which are the choices of its answer while it is
 * open, and the person's answer character for character as the store holds it, with no
 * sentence of the page's beside it. A declined ask says who declined it, and draws their note
 * where they wrote one: a question the person declined, never an answer.
 *
 * A decision draws its words - whole where it waits for the reader's review, cut as an agent's
 * words are anywhere else (ruling 6) - how it stands in the states the page declares
 * (`NODE_STATE_WORDS.decision`), and the statements its `shaped` ties name, each a link to that
 * statement. A decision of the review list draws Keep and Change with no focus, as the picture
 * does; no other row draws either (R35). The two controls are `DecisionActions.tsx`'s own, so
 * the writes are the ones every other Keep and Change of the page makes. A decision a person
 * asked to change draws what they asked under its words, with a link to it in the conversation
 * (the tester's F6).
 *
 * Every entry ends on a link to the node that holds it, in the conversation. Nothing inside a
 * row takes the tab order (R9): the row's own keys reach what it holds (`useTreeKeymap.ts`).
 */
export function DecisionsRow({
  answerer,
  author,
  body,
  changer,
  decliner,
  document,
  entry,
  focused,
  isClosed,
  keeper,
  level,
  onFocus,
  onPin,
  ownAnswer,
  pinned,
  report,
  review,
  tabStop,
  threadRead,
}: {
  /** The name of the person whose answer this ask carries, where it carries one. */
  answerer?: string;
  /** Who made the node. */
  author: RowPerson;
  /** Whether this row draws what its focus adds: it has the focus, the reader pinned it open,
   *  or the cycle key opens every body. */
  body: boolean;
  /** The person who asked this decision to change, where one did. */
  changer?: RowPerson;
  /** The person who declined this ask, where it was declined. */
  decliner?: RowPerson;
  /** The decisions document a decision is a block of, which its Keep writes against. */
  document?: Artifact;
  entry: DecisionsEntry;
  focused: boolean;
  /** Whether the issue is closed, which Keep and Change are refused on. */
  isClosed: boolean;
  /** The name of the person who kept this decision, where it stands kept. */
  keeper?: string;
  /** How deep the row sits: 1, or 2 under the foot's own row (`aria-level`). */
  level: number;
  onFocus: () => void;
  onPin: () => void;
  /** Whether the answer this ask carries is the reader's own, which carries the ink rule. */
  ownAnswer: boolean;
  pinned: boolean;
  /** What a key pressed on this row reported when it had nowhere to go. */
  report?: string;
  /** Whether this decision is an entry of the review list, which draws Keep and Change. */
  review: boolean;
  /** Whether this row is the list's one tab stop. */
  tabStop: boolean;
  /** The thread whose newest write this row draws, which a dwell on the row marks read. */
  threadRead?: ThreadMark;
}): ReactNode {
  const node = entry.node;
  const row = useRef<HTMLDivElement | null>(null);
  const words = useRef<HTMLDivElement | null>(null);
  // Where the reader writes: an open question's options and Answer set it (R16).
  const writer = useOutlineWriter();
  const press = useRowOnScreen(row, focused);
  // An answered question and an agent's decision are cut at six lines, as an agent's words are
  // on every row; a question that waits on the reader and a decision that waits for their
  // review are drawn whole, since they are what the reader is asked to read (rulings 1 and 6).
  const cuttable = entry.kind === "ask" ? node.state === "answered" && !node.waiting : !review;
  const clamp = useClamp(words, cuttable);
  const [opened, setOpened] = useState(false);
  const clamped = clamp !== undefined && !opened && !body;
  const standing = entry.kind === "decision" ? entry.standing : undefined;
  const tone = entry.kind === "ask" ? "question" : "decision";
  return (
    <div
      aria-level={level}
      className="relative my-0.5 min-w-0 outline-none"
      data-node-id={node.id}
      data-node-row=""
      data-node-state={entry.kind === "ask" ? node.state : standing}
      data-node-type={node.type}
      data-row-key={node.id}
      onFocus={onFocus}
      onPointerDown={press}
      ref={row}
      role="treeitem"
      tabIndex={tabStop ? 0 : -1}
    >
      <div className={`${NODE_ROW_LAYOUT.card} ${focused ? pageFocusedRow : ""}`} data-node-card="">
        <span className={NODE_ROW_LAYOUT.bullet} data-node-bullet="">
          <i
            aria-hidden="true"
            className={`block size-[7px] rounded-full ${pageKindBullet[tone]}`}
          />
        </span>
        <div className={NODE_ROW_LAYOUT.content} data-node-content="">
          {/* A replaced decision's words are struck through and grey, beside the word that
              says so (the prototype's `.replaced`). */}
          <div
            className={`relative text-[15px] leading-6 ${
              standing === "replaced" ? `line-through ${pageQuietText}` : pageWordsText
            } ${clamped ? "overflow-clip" : ""}`}
            data-node-words=""
            ref={words}
            style={clamped ? { maxHeight: clamp.height } : undefined}
          >
            <RowText markdown={node.text} source={node.source} tree={node.tree} />
            {clamped && !clamp.pictures ? (
              <span
                aria-hidden="true"
                className="pointer-events-none absolute inset-x-0 bottom-0 h-8 bg-[linear-gradient(transparent,var(--color-page-paper))]"
              />
            ) : null}
          </div>
          {/* A press on it leaves the focus where it is, so the control does not move under
              the pointer before the press ends. It opens the rest in place, and closes it. The
              body draws the words whole, so it offers nothing to open there. */}
          {clamp === undefined || body ? null : (
            <button
              className={`dispatch-inline-link text-xs leading-[22px] font-medium underline decoration-page-hair underline-offset-[3px] ${pageQuietText}`}
              data-node-more=""
              onClick={() => setOpened((held) => !held)}
              onMouseDown={(event) => event.preventDefault()}
              tabIndex={-1}
              type="button"
              {...pageChrome}
            >
              {opened ? (
                CONTROL_LABELS.read_less
              ) : (
                <>
                  {CONTROL_LABELS.read_more} ·{" "}
                  <span {...authorText}>{clamp.hidden.toLocaleString()}</span>{" "}
                  {CONTROL_LABELS.read_more_words}
                </>
              )}
            </button>
          )}
          {node.options === undefined ? null : (
            <NodeOptions body={body} node={node} writer={writer} />
          )}
          {node.answer === undefined ? null : (
            <AskAnswer answerer={answerer ?? ""} node={node} own={ownAnswer} />
          )}
          {entry.kind !== "ask" || entry.declined === undefined || decliner === undefined ? null : (
            <Declined by={decliner} note={entry.declined.note} />
          )}
          {entry.kind !== "decision" ||
          entry.change === undefined ||
          changer === undefined ? null : (
            <Changed by={changer} change={entry.change} />
          )}
          {/* The node's parent in the conversation, as one line: its kind's word and its first
              line (R2, R1's first-line rule). */}
          {entry.parent === undefined ? null : (
            <p
              className={`flex min-w-0 items-baseline gap-1 text-xs leading-5 ${pageQuietText}`}
              data-node-parent-line=""
              data-parent-id={entry.parent.id}
              data-parent-type={entry.parent.type}
            >
              <span className="shrink-0" {...pageChrome}>
                {NODE_TYPE_NAMES[entry.parent.type]}
              </span>
              <span className="min-w-0 break-words" {...authorText}>
                {entry.parent.line}
              </span>
            </p>
          )}
          {entry.shapes.length === 0 ? null : (
            <p
              className={`flex min-w-0 flex-wrap items-baseline gap-x-2 text-xs leading-5 ${pageQuietText}`}
              data-decision-informs=""
            >
              <span className="shrink-0" {...pageChrome}>
                {DECISIONS_WORDS.informs}
              </span>
              {entry.shapes.map((shape) => (
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
                    <span {...authorText}>{shape.line}</span>
                  ) : (
                    <Link
                      className={`dispatch-inline-link font-semibold hover:underline ${pageKindTag.statement}`}
                      tabIndex={-1}
                      to={shape.path}
                      {...authorText}
                    >
                      {shape.line}
                    </Link>
                  )}
                </span>
              ))}
            </p>
          )}
          {/* Keep and Change on one line with the entry's link (the prototype's `.dbtns`); the
              form Change opens, and what a refused write says, take a line of their own. */}
          <div
            className="mt-1 mb-0.5 flex min-w-0 flex-wrap items-center gap-x-3 gap-y-2 [&>[role=alert]]:order-last [&>[role=alert]]:basis-full [&>form]:order-last [&>form]:basis-full"
            data-entry-actions=""
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
          {/* What the focus adds to an ask: what each option says beside its name and the rest
              of a cut question, with the control that keeps them open (R6). A decision's row
              holds nothing back, so it draws no body. */}
          {body && entry.kind === "ask" ? (
            <div
              className="mt-1 flex min-w-0 flex-col gap-1 text-sm"
              data-node-body=""
              data-pinned={pinned ? "" : undefined}
            >
              <p>
                <button
                  className={`dispatch-inline-link text-xs leading-6 font-medium ${pageQuietText}`}
                  onClick={onPin}
                  tabIndex={-1}
                  type="button"
                  {...pageChrome}
                >
                  {pinned ? CONTROL_LABELS.let_close : CONTROL_LABELS.keep_open}
                </button>
              </p>
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
        <RowMargin author={author} entry={entry} keeper={keeper} review={review} />
        {threadRead === undefined ? null : <ThreadReadMark mark={threadRead} />}
      </div>
    </div>
  );
}

/**
 * The row's kind as one tag, who made it by a name and a shape, and when, on one line; and
 * under them how it stands, by a word and a shape (R6, R9, R34, the prototype's `.mg`), laid
 * out as the outline's row lays them out (`NODE_ROW_LAYOUT`). An ask reads the word the
 * conversation draws for its state. A decision reads the page's word for how it stands
 * (`NODE_STATE_WORDS.decision`), "Kept by" with the name of the person who kept it. The state
 * is drawn wherever the decision is, and in the decisions' violet where it asks something of
 * the reader or says what a person did with it: on a decision of the review list, on one a
 * person kept and on one they asked to change (R35: "one rule decides the list and a second
 * rule decides the mark on a row").
 */
function RowMargin({
  author,
  entry,
  keeper,
  review,
}: {
  author: RowPerson;
  entry: DecisionsEntry;
  keeper: string | undefined;
  review: boolean;
}): ReactNode {
  const node = entry.node;
  const time = node.at === undefined ? "" : shortDay(node.at);
  const standing = entry.kind === "decision" ? entry.standing : undefined;
  const asked = entry.kind === "ask" ? node.state : undefined;
  const unsure = entry.kind === "decision" && node.unsure === true;
  const askGlyphs: Readonly<Record<string, string>> = GLYPH_TOKENS.states.ask;
  const askGlyph = asked === undefined ? undefined : askGlyphs[asked];
  const authored = author.kind !== "unknown";
  return (
    <div className={`${NODE_ROW_LAYOUT.margin} ${pageQuietText}`} data-node-margin="">
      <div className={NODE_ROW_LAYOUT.marginLine} data-node-margin-line="">
        <span
          className={`inline-flex items-center gap-1 font-semibold whitespace-nowrap ${
            pageKindTag[entry.kind === "ask" ? "question" : "decision"]
          }`}
          data-node-tag=""
        >
          <NodeGlyph className="size-3 shrink-0" token={GLYPH_TOKENS.types[node.type]} />
          <span {...pageChrome}>{NODE_TYPE_NAMES[node.type]}</span>
        </span>
        {/* A node the model holds no author of draws none on its row (ruling 5). */}
        {!authored && time === "" ? null : (
          <span className="min-w-0 [overflow-wrap:anywhere]" data-node-byline="">
            {authored ? (
              <span
                className="inline-flex items-center gap-1"
                data-author-kind={author.kind}
                data-node-author=""
              >
                <NodeGlyph className="size-3 shrink-0" token={GLYPH_TOKENS.authors[author.kind]} />
                <span
                  className={author.reader ? `font-semibold ${pageInkText}` : ""}
                  {...authorText}
                >
                  {/* An agent by its role's word, as every row of the page names one
                      (ruling 6). */}
                  {author.kind === "agent" ? agentRoleWord(author.name) : author.name}
                </span>
              </span>
            ) : null}
            {time === "" ? null : (
              <>
                {authored ? <span {...pageChrome}> · </span> : null}
                <span className="whitespace-nowrap" data-node-time="" {...authorText}>
                  {time}
                </span>
              </>
            )}
          </span>
        )}
      </div>
      {asked === undefined && standing === undefined && !unsure ? null : (
        <div className={NODE_ROW_LAYOUT.marginState}>
          {asked === undefined ? null : (
            <span
              className={`inline-flex items-center gap-1 ${
                node.waiting ? pageStateText.waiting : pageStateText.quiet
              }`}
            >
              {askGlyph === undefined ? null : (
                <NodeGlyph className="size-3 shrink-0" token={askGlyph} />
              )}
              <span data-node-state={asked} {...pageChrome}>
                {nodeStateWord(asked)}
              </span>
            </span>
          )}
          {standing === undefined ? null : (
            <span
              className={`inline-flex items-center gap-1 ${
                review || standing === "kept" || standing === "change_requested"
                  ? pageStateText.decision
                  : pageStateText.quiet
              }`}
            >
              <NodeGlyph
                className="size-3 shrink-0"
                token={GLYPH_TOKENS.states.decision[standing]}
              />
              <span data-node-state={standing} {...pageChrome}>
                {nodeStateWord(standing)}
                {standing === "kept" && keeper !== undefined ? (
                  <>
                    {" "}
                    <span data-kept-by="" {...authorText}>
                      {keeper}
                    </span>
                  </>
                ) : null}
              </span>
            </span>
          )}
          {unsure ? (
            <span data-node-unsure="" {...pageChrome}>
              {CONTROL_LABELS.unsure}
            </span>
          ) : null}
        </div>
      )}
    </div>
  );
}

/**
 * That a person declined this question, under it (R2: "a declined ask reads as a question the
 * person declined and never as an answer"): who declined, by a name and a shape as every
 * author is drawn (R9), and the note they wrote where they wrote one, word for word. The
 * reader's own note carries the ink rule their own words carry (R34).
 */
function Declined({ by, note }: { by: RowPerson; note: string | undefined }): ReactNode {
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
        <p
          className={`text-[15px] leading-6 whitespace-pre-wrap break-words ${own ? pageInkText : pageWordsText}`}
          data-declined-note=""
          {...authorText}
        >
          {note}
        </p>
      )}
    </div>
  );
}

/**
 * The change a person asked of this decision, under its words (the tester's F6: "the change
 * must be drawn under, or tied to, the decision it changes"): who asked and when, as every
 * author is drawn (R9), what they wrote word for word, and a link to it in the conversation,
 * where it is replied to. The reader's own words carry the ink rule (R34).
 */
function Changed({ by, change }: { by: RowPerson; change: DecisionChange }): ReactNode {
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
      <p
        className={`text-[15px] leading-6 whitespace-pre-wrap break-words ${by.reader ? pageInkText : pageWordsText}`}
        data-change-words=""
        {...authorText}
      >
        {change.body}
      </p>
    </div>
  );
}
