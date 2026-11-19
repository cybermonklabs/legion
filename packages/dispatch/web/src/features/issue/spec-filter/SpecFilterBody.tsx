import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { issueResultsQuery } from "../../../api/queries";
import type { Actor, IssueDetails } from "../../../api/types";
import { QueryError } from "../../../components/QueryError";
import {
  pageDecisionText,
  pageFocusedRow,
  pageHairBorder,
  pageInkText,
  pageKindBullet,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
  pageStateText,
  pageWordsText,
} from "../../../theme/classes";
import { issueDecisionsQuery } from "../../doc/spec-marks";
import { DETAILS_ATTRIBUTE } from "../../margin/margin-slot";
import { useAgentText } from "../../refs/AgentName";
import { buildReferencePath, parseNodeReference } from "../../refs/routes";
import { CutWords } from "../CutWords";
import { conversationPath } from "../decisions-filter/decisions-parts";
import { useSetFocusedNode } from "../focused-node";
import { IssueThreeThings, specSummary } from "../IssueThreeThings";
import type { OutlineRequest } from "../IssueTree";
import { firstLineOf, nodeWords, type TreeNode } from "../issue-tree";
import { NodeGlyph } from "../NodeGlyph";
import { NodeEdges, useRowOnScreen } from "../NodeRow";
import { shortDay } from "../node-time";
import { useOutlineWriter } from "../outline-writer";
import { authorText, KEY_REPORTS, pageChrome } from "../page-structure";
import {
  ANSWER_LINES,
  authorKind,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  GLYPH_TOKENS,
  NODE_TYPE_NAMES,
  nodeStateWord,
  SPEC_WORDS,
  sentenceCase,
  VERIFICATION_WORDS,
} from "../page-words";
import { RowText } from "../RowText";
import { treeContext } from "../tree-drafts";
import type { DrawnRow } from "../tree-fold";
import { useTreeKeymap } from "../useTreeKeymap";
import {
  type SpecDecision,
  type SpecGroup,
  type SpecMark,
  type SpecParent,
  type SpecStatement,
  specDecidedBy,
} from "./spec-decided-by";

/** What a key that folds, zooms, cycles or opens a branch does here, where the columns hold
 *  none: every statement is drawn whole (R2: "a filter draws every branch it kept open"). */
const NOTHING = (): void => {};

/**
 * The Spec filter's body (dispatch://INFRA-1196 R5, R17, R22, R35, the prototype's `.spec`).
 *
 * It is the approved picture `page-v3-spec-1440.png`: a legend of the four marks a statement
 * carries, then two columns, Decided by and Statement. In the left column stands, once per run
 * of statements one decision shaped, that decision in the words it came from, with a bracket
 * tying it to its statements; in the right column each statement's words in full, with one line
 * under them naming the decision that shaped them, and its mark at the right of the row. Below
 * 900 px it is one column and each decision stands above its statements, as
 * `page-v3-spec-390.png` draws it.
 *
 * Which decision shaped which statement is `spec-decided-by.ts`, which is pure; this module
 * draws it and holds two rules of its own:
 *
 * - **The question is drawn once.** Where the decision's own question is the node the
 *   statements hang under, the group's heading is that question and the cell does not say it
 *   again: "where the answered ask is the node's own parent, its parent line and its decision
 *   line are one line" (R5).
 * - **A statement's own words carry its number.** The picture draws a tag, "R1", at the right
 *   of each statement; the model holds no short name for a statement, and a block id is an
 *   identifier, which R22 refuses where a name belongs. The spec's own words open "**R5.**"
 *   where its author wrote a number, and the row draws them in full, so the number a reader
 *   cites is the author's own.
 *
 * The body heads its columns with the issue's three sentences under their labels, as the
 * picture's first group (ruling 5 of build-rulings-md: "Under Spec: the first group").
 *
 * It is the outline under the Spec filter, so it is one `role="tree"` named as the outline is,
 * with the outline's keys (`useTreeKeymap.ts`): the statements are its rows, the focused one is
 * its one tab stop, the arrows and `j`/`k` move between them, `w` moves to the decision beside
 * a statement, `r` replies to it and Enter goes to it in the conversation, where it is edited.
 * The focus on a statement puts its facts in Details, as a row of the outline does, and an
 * address naming a statement (`/spec#b-<block>`) puts the focus on it (R10, R19).
 */
export function SpecFilterBody({
  context,
  emptyLine,
  holdsEscape,
  issue,
  reader,
  reading,
  request,
  reveal,
  root,
  view,
}: {
  /** The ids of the rows the filter draws only as context, drawn faint (R2, R32). */
  context: ReadonlySet<string>;
  /** What the body says where the filter's query left no statement. */
  emptyLine: string;
  /** Whether the page takes Escape from a row before the body is left: a filter by words to
   *  clear or the filter to turn off comes first in the key table's order (`IssueBody.tsx`). */
  holdsEscape: boolean;
  issue: IssueDetails;
  /** Who is reading, so a decision of theirs reads as their own words (R34). */
  reader: string | undefined;
  /** Whether the spec's blocks are still being read, which says whether it holds a summary. */
  reading: boolean;
  /** What the page last asked of the outline: go to a node. Each ask is acted on once. */
  request?: OutlineRequest;
  /** The node the address names, which takes the focus once per address (R19). */
  reveal?: string;
  /** The whole tree, which holds the asks, the decisions and each statement's checks. */
  root: TreeNode;
  /** The tree as the Spec filter leaves it (`presetView`). */
  view: TreeNode;
}): ReactNode {
  const navigate = useNavigate();
  // The decisions the page already read for its tree, under the key `useIssueTree.ts` reads
  // them with, so the body costs no request of its own (R20). Until that read answers the
  // column says nothing of a statement no decision shaped, since it does not know yet; a read
  // that failed says so over the columns, with a way to read again.
  const decisionsRead = useQuery(issueDecisionsQuery(issue.key, true));
  const decisions = decisionsRead.data;
  // The issue's results, under the key the tree reads them with (no request of its own): each
  // statement's mark is how it stands at the commit the page names, as the Verification filter
  // reads it (`statement-state.ts`).
  const results = useQuery(issueResultsQuery(issue.key)).data;
  // How this issue addresses a node, which is the one rule the tree was built with, so a
  // `shaped` tie resolves to the node it names (`tree-drafts.ts`).
  const nodeIdOf = useMemo(() => treeContext({ issue }).nodeIdOf, [issue]);
  const { groups, others } = useMemo(
    () => specDecidedBy({ checks: results?.checks ?? [], decisions, nodeIdOf, reader, root, view }),
    [decisions, nodeIdOf, reader, results, root, view]
  );
  const summary = specSummary(root, primarySpec(issue)?.slug ?? "");
  const setFocusedNode = useSetFocusedNode();
  const writer = useOutlineWriter();
  const tree = useRef<HTMLDivElement | null>(null);
  const [focused, setFocused] = useState<string | undefined>(undefined);
  // Where the tree's one tab stop rests, which outlives the focus leaving it (R9).
  const [stop, setStop] = useState<string | undefined>(undefined);
  // What a key reported when it had nowhere to go, and the statement it was pressed on, which
  // draws it under its words until the reader moves on (the tester's F16, `NodeRow`'s `report`).
  const [report, setReport] = useState<{ readonly line: string; readonly row: string }>();
  const reportHere = useCallback((line: string) => {
    const row = document.activeElement?.closest<HTMLElement>("[data-node-row]")?.dataset.nodeId;
    setReport(line === "" || row === undefined ? undefined : { line, row });
  }, []);
  // The statements in the order they are drawn, which is the order the keys move through them,
  // and after them every other row the filter kept.
  const statements = useMemo(
    () => [
      ...groups.flatMap((group) =>
        group.runs.flatMap((run) => run.statements.map((statement) => statement.node))
      ),
      ...others,
    ],
    [groups, others]
  );
  const rows = useMemo<DrawnRow[]>(
    () => statements.map((node) => ({ kind: "node", level: 1, node, trail: [] })),
    [statements]
  );
  const takeFocus = useCallback(
    (node: TreeNode) => {
      setFocused(node.id);
      setStop(node.id);
      // A move to a statement is the reader going on from what a key reported.
      setReport((held) => (held?.row === node.id ? held : undefined));
      setFocusedNode({
        ...(node.author === undefined ? {} : { author: node.author }),
        id: node.id,
        ...(node.reference === undefined ? {} : { reference: node.reference }),
        title: node.text === "" ? node.id : firstLineOf(nodeWords(node)).line,
        type: node.type,
      });
    },
    [setFocusedNode]
  );
  // The focus leaving the body gives the panel back to the issue's own facts, unless it went
  // to Details, where a reader reads the node's (`IssueTree.tsx`).
  const releaseFocus = useCallback(
    (event: { currentTarget: HTMLElement; relatedTarget: EventTarget | null }) => {
      const to = event.relatedTarget;
      if (to instanceof Node && event.currentTarget.contains(to)) return;
      setFocused(undefined);
      if (to instanceof Element && to.closest(`[${DETAILS_ATTRIBUTE}]`) !== null) return;
      setFocusedNode(undefined);
    },
    [setFocusedNode]
  );
  // Puts the focus on the statement or the decision cell that draws node `id`.
  const goTo = useCallback((id: string): boolean => {
    const target = tree.current?.querySelector<HTMLElement>(
      `[data-node-id="${CSS.escape(id)}"][role="treeitem"]`
    );
    target?.focus();
    return target != null;
  }, []);
  // What the page asked for, each ask acted on once.
  const handled = useRef<number | undefined>(undefined);
  useEffect(() => {
    if (request === undefined || request.seq === handled.current) return;
    handled.current = request.seq;
    if (request.id !== undefined) goTo(request.id);
  }, [goTo, request]);
  // An address that names a statement puts the focus on it, once per address (R19): the row is
  // not on the page until the reads that hold it have answered, so this looks again on every
  // drawing of the columns.
  const taken = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (reveal === undefined || taken.current === reveal || statements.length === 0) return;
    if (goTo(reveal)) taken.current = reveal;
  }, [goTo, reveal, statements]);

  useTreeKeymap({
    container: tree,
    holdsEscape,
    onCycle: NOTHING,
    // Enter on a statement goes to it in the conversation, where it is edited: this filter
    // draws each statement and edits none.
    onEdit: (_row, node) => {
      const path = conversationPath(node);
      if (path !== undefined) navigate(path);
    },
    onEditEnd: NOTHING,
    onFold: NOTHING,
    onOpenDeeper: NOTHING,
    onPin: NOTHING,
    onReport: reportHere,
    // `w` and `q` can name a node this filter draws no row or cell for - a decision that shaped
    // no statement of the view, an open question - and the row says the key went nowhere, in
    // the words the outline says it in.
    onReveal: (id) => {
      if (goTo(id)) return;
      const decided = (decisions?.decisions ?? []).some((one) => nodeIdOf(one.ref) === id);
      reportHere(decided ? KEY_REPORTS.no_decision : KEY_REPORTS.no_next_question);
    },
    onZoom: NOTHING,
    oneNodeAtATime: false,
    rows,
    // The whole tree, which holds the decisions `w` moves to (`tree-keys.ts`'s `decisionBehind`).
    root,
    writer,
    zoom: undefined,
  });

  const tabStop =
    stop !== undefined && statements.some((node) => node.id === stop) ? stop : statements[0]?.id;
  return (
    <div className="mt-3.5 mb-[120px] flex w-full min-w-0 flex-col" data-spec-body="">
      <IssueThreeThings look="spec" reading={reading} summary={summary} />
      {decisionsRead.isError ? (
        <QueryError
          message={ANSWER_LINES.decisions_unreadable}
          onRetry={() => void decisionsRead.refetch()}
        />
      ) : null}
      <Legend />
      <div
        className={`sticky top-[var(--issue-identity-height,0px)] z-[6] grid grid-cols-[280px_minmax(0,1fr)] gap-x-5 border-b pt-2 pb-1.5 max-[900px]:hidden ${pageLineBorder} ${pagePaperBg}`}
        data-spec-head=""
      >
        <span className={CAP} {...pageChrome}>
          {SPEC_WORDS.decided_by}
        </span>
        <span className={`pl-[26px] ${CAP}`} {...pageChrome}>
          {sentenceCase(NODE_TYPE_NAMES.requirement)}
        </span>
      </div>
      {/* The outline the page draws under the Spec filter, which is this body's two columns:
          one tree, named as the tree every other filter draws, so a reader meets one outline
          on every address (R2, `IssueTree.tsx`). Its items are each statement and each
          decision beside them, which is what the focus moves between (R5's `w`); the layout
          between them carries no role of its own. */}
      <div
        aria-label={CONTROL_LABELS.tree}
        className="flex min-w-0 flex-col"
        data-spec-tree=""
        onBlur={releaseFocus}
        ref={tree}
        role="tree"
      >
        {groups.map((group) => (
          <Group
            context={context}
            focused={focused}
            group={group}
            key={group.parent?.id ?? group.runs[0]?.statements[0]?.node.id ?? ""}
            known={decisionsRead.isSuccess}
            onFocus={takeFocus}
            {...(report === undefined ? {} : { report })}
            tabStop={tabStop}
          />
        ))}
        {others.map((node) => (
          <OtherRow
            focused={focused === node.id}
            key={node.id}
            node={node}
            onFocus={() => takeFocus(node)}
            tabStop={tabStop === node.id}
          />
        ))}
      </div>
      {statements.length === 0 ? (
        // What the page says where the filter's query matched nothing under the issue, in the
        // words the outline says it in (`IssueTree.tsx`).
        <p className={`py-1 text-sm ${pageMutedText}`} data-spec-empty="" {...pageChrome}>
          {emptyLine}
        </p>
      ) : null}
    </div>
  );
}

/** A column heading, in the prototype's `.cap`. */
const CAP = `text-xs leading-4 font-semibold tracking-[0.08em] uppercase ${pageQuietText}`;

/** The colour each mark is drawn in, which repeats what its shape and the legend's word say
 *  (R6): met in the passed green, not met and no check in the failed red, unknown quiet. */
const MARK_TONES: Readonly<Record<SpecMark, string>> = {
  met: pageStateText.passed,
  no_check: pageStateText.failed,
  not_met: pageStateText.failed,
  unknown: pageStateText.quiet,
};

/** The four marks in the order the legend draws them. */
const MARK_ORDER: readonly SpecMark[] = ["met", "not_met", "unknown", "no_check"];

/** The shape of a mark: how a statement stands, or that no check serves it. */
function markToken(mark: SpecMark): string {
  return mark === "no_check" ? GLYPH_TOKENS.no_check : GLYPH_TOKENS.statements[mark];
}

/** The word of a mark, in the Verification filter's own words for how a statement stands. */
function markWord(mark: SpecMark): string {
  return mark === "no_check" ? SPEC_WORDS.no_check : VERIFICATION_WORDS.states[mark];
}

/** What each mark means, above the columns: a shape and the word for it, since neither colour
 *  nor shape alone tells a reader what one says (R6). */
function Legend(): ReactNode {
  return (
    <p
      className={`mb-1 ml-[300px] flex flex-wrap gap-[14px] text-xs leading-5 max-[900px]:ml-0 ${pageQuietText}`}
      data-spec-legend=""
    >
      {MARK_ORDER.map((mark) => (
        <span
          className="inline-flex items-center gap-[5px]"
          data-spec-legend-mark={mark}
          key={mark}
        >
          <NodeGlyph className={`size-3 shrink-0 ${MARK_TONES[mark]}`} token={markToken(mark)} />
          <span {...pageChrome}>{markWord(mark)}</span>
        </span>
      ))}
    </p>
  );
}

/** Who made something, by a name and a shape, as every other row of the page draws an author
 *  (R9): never an identifier, and the page's own words where the model holds none. */
function Author({ actor }: { actor: Actor | undefined }): ReactNode {
  const agentText = useAgentText();
  const kind = authorKind(actor);
  return (
    <span className="inline-flex items-center gap-1" data-spec-author="">
      <NodeGlyph className="size-3 shrink-0" token={GLYPH_TOKENS.authors[kind]} />
      <span {...(actor === undefined ? pageChrome : authorText)}>
        {actor === undefined ? CONTROL_LABELS.unknown_author : agentText.author(actor)}
      </span>
    </span>
  );
}

/** The statements under one parent: that parent's own line as the group's heading, then one row
 *  of the grid per run of statements one decision shaped. */
function Group({
  context,
  focused,
  group,
  known,
  onFocus,
  report,
  tabStop,
}: {
  context: ReadonlySet<string>;
  focused: string | undefined;
  group: SpecGroup;
  /** Whether the decisions read has answered, which is when a run with no decision can say
   *  that none shaped it. */
  known: boolean;
  onFocus: (node: TreeNode) => void;
  /** What a key reported, and the statement it was pressed on. */
  report?: { readonly line: string; readonly row: string };
  tabStop: string | undefined;
}): ReactNode {
  return (
    <section
      className="mt-[18px] min-w-0"
      data-parent-id={group.parent?.id}
      data-spec-group=""
      role="presentation"
    >
      {group.parent === undefined ? null : <GroupHeading parent={group.parent} />}
      {group.runs.map((run) => (
        <div
          className="grid min-w-0 grid-cols-[280px_minmax(0,1fr)] gap-x-5 max-[900px]:grid-cols-[minmax(0,1fr)]"
          data-spec-run=""
          key={run.statements[0]?.node.id ?? ""}
          role="presentation"
        >
          <DecisionCell
            decision={run.decision}
            known={known}
            parent={group.parent}
            statements={run.statements}
          />
          <div className="flex min-w-0 flex-col" role="presentation">
            {run.statements.map((statement) => (
              <Statement
                context={context.has(statement.node.id)}
                decision={run.decision}
                focused={focused === statement.node.id}
                key={statement.node.id}
                onFocus={() => onFocus(statement.node)}
                {...(report?.row === statement.node.id ? { report: report.line } : {})}
                statement={statement}
                tabStop={tabStop === statement.node.id}
              />
            ))}
          </div>
        </div>
      ))}
    </section>
  );
}

/** The node a group of statements hangs under, which the filter draws as no row (R2): its kind,
 *  who made it and when, and its first line. */
function GroupHeading({ parent }: { parent: SpecParent }): ReactNode {
  return (
    <div
      className="mb-1 ml-[300px] flex min-w-0 flex-col pl-[26px] max-[900px]:ml-0 max-[900px]:pl-0"
      data-spec-group-heading=""
      role="presentation"
    >
      <p className={`min-w-0 text-xs leading-[18px] [overflow-wrap:anywhere] ${pageQuietText}`}>
        <span {...pageChrome}>{NODE_TYPE_NAMES[parent.type]} · </span>
        <Author actor={parent.by} />
        {parent.at === undefined ? null : (
          <>
            <span {...pageChrome}> · </span>
            <span className="whitespace-nowrap" {...authorText}>
              {shortDay(parent.at)}
            </span>
          </>
        )}
      </p>
      <p
        className={`min-w-0 text-[15px] leading-6 font-semibold break-words ${pageInkText}`}
        {...authorText}
      >
        {parent.line}
      </p>
    </div>
  );
}

/**
 * The decision that shaped a run of statements, in the left column: the words it came from,
 * where they came from and when, and a link to the whole of it (R5). The bracket at its
 * trailing edge spans the statements it shaped, in the decisions' violet, dashed where an agent
 * decided; a run no decision shaped says so in words (R35), once the decisions read has
 * answered, and says nothing before that or where the read failed, since it does not know.
 *
 * The focus on the cell draws a person's whole answer character for character under its first
 * sentence (R5: "The focus on the line draws the whole answer word for word"); `w` on a
 * statement is how a keyboard reader puts it there.
 */
function DecisionCell({
  decision,
  known,
  parent,
  statements,
}: {
  decision: SpecDecision | undefined;
  known: boolean;
  parent: SpecParent | undefined;
  statements: readonly SpecStatement[];
}): ReactNode {
  const [focused, setFocused] = useState(false);
  const rule = decision === undefined ? pageHairBorder : "border-page-decision";
  const cap = decision === undefined ? "bg-page-hair" : "bg-page-decision";
  // The question the words answer, unless the statements hang under it already: there the
  // group's heading is that question, and the two are one line (R5).
  const question =
    decision?.question === undefined || decision.id === parent?.id ? undefined : decision.question;
  return (
    <div
      aria-level={1}
      className={`relative self-stretch rounded-l-[4px] border-r-2 pt-1 pr-3.5 pb-2 pl-0.5 text-xs leading-[18px] max-[900px]:my-1.5 max-[900px]:rounded-none max-[900px]:border-r-0 max-[900px]:border-l-2 max-[900px]:py-0.5 max-[900px]:pr-0 max-[900px]:pl-2.5 ${
        decision?.said === "agent" ? "border-dashed" : ""
      } ${rule} ${pageMutedText}`}
      data-decision-id={decision?.id}
      // The decision node the tie came from, which `w` moves the focus to from a statement
      // (`tree-keys.ts`'s `decisionBehind`): a person's decision is drawn by their answer, and
      // reached by the section of `decisions.md` that names it.
      data-node-id={decision?.node ?? decision?.id}
      data-said={decision?.said}
      data-spec-cell=""
      data-spec-statements={statements.map((statement) => statement.node.id).join(" ")}
      // An item of the outline, as the statements beside it are: it is where the focus goes
      // from a statement (R5's `w`), and the prototype's own cell takes the focus the same way.
      onBlur={() => setFocused(false)}
      onFocus={(event) => setFocused(event.target === event.currentTarget)}
      role="treeitem"
      tabIndex={-1}
    >
      <span
        aria-hidden="true"
        className={`absolute top-0 -right-0.5 h-0.5 w-2 max-[900px]:hidden ${cap}`}
      />
      <span
        aria-hidden="true"
        className={`absolute -right-0.5 bottom-0 h-0.5 w-2 max-[900px]:hidden ${cap}`}
      />
      <div className="sticky top-[calc(var(--issue-identity-height,0px)+44px)] flex min-w-0 flex-col gap-[3px] max-[900px]:static">
        {decision === undefined ? (
          known ? (
            <p data-spec-nothing="" {...pageChrome}>
              {statements.length === 1
                ? EMPTY_STATE_LINES.spec_no_decision_one
                : EMPTY_STATE_LINES.spec_no_decision_many}
            </p>
          ) : null
        ) : (
          <>
            <p className={`font-semibold ${pageDecisionText}`} data-spec-said="" {...pageChrome}>
              {decision.said === "agent"
                ? SPEC_WORDS.said_agent
                : decision.said === "you"
                  ? SPEC_WORDS.said_you
                  : SPEC_WORDS.said_person}
            </p>
            <p
              className={`min-w-0 text-sm leading-[21px] [overflow-wrap:anywhere] ${pageInkText}`}
              data-spec-quote=""
            >
              <span {...pageChrome}>“</span>
              <span {...authorText}>{decision.quote}</span>
              <span {...pageChrome}>”</span>
            </p>
            {focused && decision.whole !== undefined ? (
              <p
                className={`min-w-0 text-sm leading-[21px] whitespace-pre-wrap [overflow-wrap:anywhere] ${pageInkText}`}
                data-spec-whole-answer=""
                {...authorText}
              >
                {decision.whole}
              </p>
            ) : null}
            <p className={`min-w-0 [overflow-wrap:anywhere] ${pageQuietText}`} data-spec-source="">
              {question === undefined ? null : (
                <>
                  <span {...authorText}>{question}</span>
                  <span {...pageChrome}> · </span>
                </>
              )}
              <Author actor={decision.by} />
              {decision.at === undefined ? null : (
                <>
                  <span {...pageChrome}> · </span>
                  <span className="whitespace-nowrap" {...authorText}>
                    {shortDay(decision.at)}
                  </span>
                </>
              )}
              {decision.chose.length === 0 ? null : (
                <>
                  <span {...pageChrome}> · {SPEC_WORDS.chose} </span>
                  <span {...authorText}>{decision.chose.join(", ")}</span>
                </>
              )}
              {decision.state === undefined ? null : (
                <>
                  <span {...pageChrome}> · </span>
                  <span data-node-state={decision.state} {...pageChrome}>
                    {nodeStateWord(decision.state)}
                  </span>
                </>
              )}
              {decision.unsure ? (
                <>
                  <span {...pageChrome}> · </span>
                  <span data-node-unsure="" {...pageChrome}>
                    {CONTROL_LABELS.unsure}
                  </span>
                </>
              ) : null}
              {statements.some((statement) => statement.changedSince) ? (
                <>
                  <span {...pageChrome}> · </span>
                  <span data-spec-changed="" {...pageChrome}>
                    {SPEC_WORDS.changed_since}
                  </span>
                </>
              ) : null}
            </p>
            <WholeOf decision={decision} />
          </>
        )}
      </div>
    </div>
  );
}

/** The link to the whole of a decision: the answer a person wrote, or the decision itself. It
 *  goes to the node's own address, which is where every other link of this page goes (`Chip`),
 *  and is out of the tab order as every control inside a row is (R9). */
function WholeOf({ decision }: { decision: SpecDecision }): ReactNode {
  const route =
    decision.reference === undefined ? undefined : parseNodeReference(decision.reference);
  if (route === undefined) return null;
  return (
    <Link
      className={`dispatch-inline-link self-start text-left font-semibold underline decoration-page-hair underline-offset-[3px] ${pageInkText}`}
      data-spec-whole=""
      tabIndex={-1}
      to={buildReferencePath(route)}
      {...pageChrome}
    >
      {decision.type === "ask" ? SPEC_WORDS.whole_answer : SPEC_WORDS.whole_decision}{" "}
      <span aria-hidden="true">›</span>
    </Link>
  );
}

/**
 * One statement: its bullet, its words, the line naming the decision that shaped it, the chips
 * it carries, and its mark at the right of the row (R5, R17, R34). Its words are cut as an
 * agent's are on every row of the page, at six lines where more than 20 words would be hidden,
 * with "more · N words" opening the rest in place (ruling 1 and ruling 6 of build-rulings-md:
 * a statement of twenty lines opened every view on a wall of text); the focused statement
 * draws them whole.
 */
function Statement({
  context,
  decision,
  focused,
  onFocus,
  report,
  statement,
  tabStop,
}: {
  context: boolean;
  decision: SpecDecision | undefined;
  focused: boolean;
  onFocus: () => void;
  /** What a key pressed on this statement reported when it had nowhere to go. */
  report?: string;
  statement: SpecStatement;
  tabStop: boolean;
}): ReactNode {
  const node = statement.node;
  const row = useRef<HTMLDivElement | null>(null);
  const press = useRowOnScreen(row, focused);
  // Every gap and chip on the statement but the gap its mark already says: "No check" is the
  // mark at the right of the row and the word for it in the legend (R17), and a chip beside it
  // would say it twice. The rest are drawn as the outline draws them (R4, R10).
  const edged = { ...node, gaps: node.gaps.filter((gap) => gap !== "no_check") };
  const edges = edged.alsoUnder.length > 0 || edged.chips.length > 0 || edged.gaps.length > 0;
  return (
    <div
      aria-level={1}
      className={`grid min-w-0 grid-cols-[18px_minmax(0,1fr)_26px] items-start gap-x-2 rounded-md py-[3px] pr-1.5 outline-none max-[760px]:grid-cols-[16px_minmax(0,1fr)_22px] max-[760px]:gap-x-1.5 ${
        focused ? pageFocusedRow : ""
      }`}
      data-node-context={context ? "" : undefined}
      data-node-id={node.id}
      // The attributes every row of the outline carries, so the page's own readers find a
      // statement of this body as they find a row of the tree (`e2e/issue-page.ts`'s
      // `drawnRows` and `nodeRow`), and the outline's keys walk it (`useTreeKeymap.ts`).
      data-node-row=""
      data-node-type={node.type}
      data-spec-mark={statement.mark}
      data-spec-statement=""
      onFocus={(event) => {
        if (event.target === event.currentTarget) onFocus();
      }}
      onPointerDown={press}
      ref={row}
      role="treeitem"
      tabIndex={tabStop ? 0 : -1}
    >
      <span className="flex h-6 w-[18px] items-center justify-center max-[760px]:w-4">
        <i
          aria-hidden="true"
          className={`block size-[7px] rounded-full ${
            context ? pageKindBullet.context : pageKindBullet.statement
          }`}
        />
      </span>
      <div className="flex min-w-0 flex-col">
        <CutWords
          className={`text-[15px] leading-6 ${context ? pageQuietText : pageWordsText}`}
          whole={focused}
        >
          <RowText markdown={node.text} source={node.source} tree={node.tree} />
        </CutWords>
        <Because decision={decision} statement={statement} />
        {edges ? (
          <p
            className={`flex min-w-0 flex-wrap items-baseline gap-x-2 text-xs leading-5 ${pageQuietText}`}
            data-node-trail=""
          >
            <NodeEdges node={edged} />
          </p>
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
      <NodeGlyph
        className={`mx-auto mt-[5px] size-3.5 shrink-0 ${MARK_TONES[statement.mark]}`}
        token={markToken(statement.mark)}
      />
    </div>
  );
}

/** A row the filter kept that is no statement: a node an address named, which outranks the
 *  filter (R20). It is drawn by its kind and its words in the statements' column, with no mark
 *  and no decision beside it, so nothing the filter kept is lost (R19). */
function OtherRow({
  focused,
  node,
  onFocus,
  tabStop,
}: {
  focused: boolean;
  node: TreeNode;
  onFocus: () => void;
  tabStop: boolean;
}): ReactNode {
  const row = useRef<HTMLDivElement | null>(null);
  const press = useRowOnScreen(row, focused);
  return (
    <div
      aria-level={1}
      className={`mt-[18px] ml-[300px] min-w-0 rounded-md py-[3px] pr-1.5 pl-[26px] outline-none max-[900px]:ml-0 max-[900px]:pl-0 ${
        focused ? pageFocusedRow : ""
      }`}
      data-node-id={node.id}
      data-node-row=""
      data-node-type={node.type}
      data-spec-other=""
      onFocus={(event) => {
        if (event.target === event.currentTarget) onFocus();
      }}
      onPointerDown={press}
      ref={row}
      role="treeitem"
      tabIndex={tabStop ? 0 : -1}
    >
      <p className={`text-xs leading-5 ${pageQuietText}`} {...pageChrome}>
        {NODE_TYPE_NAMES[node.type]}
      </p>
      <CutWords className={`text-[15px] leading-6 ${pageWordsText}`} whole={focused}>
        <RowText markdown={node.text} source={node.source} tree={node.tree} />
      </CutWords>
    </div>
  );
}

/**
 * The one line under a statement that names the decision that shaped it (R5): the words it
 * came from, as the link to the whole of that decision, who decided and when, and then every
 * other decision that informed it, each a link to it. A statement no decision shaped draws no
 * line here: its cell says so beside it (R35).
 */
function Because({
  decision,
  statement,
}: {
  decision: SpecDecision | undefined;
  statement: SpecStatement;
}): ReactNode {
  if (decision === undefined && statement.alsoShapedBy.length === 0) return null;
  const route =
    decision?.reference === undefined ? undefined : parseNodeReference(decision.reference);
  const quoted =
    decision === undefined ? null : (
      <>
        <span {...pageChrome}>“</span>
        <span {...authorText}>{decision.quote}</span>
        <span {...pageChrome}>”</span>
      </>
    );
  return (
    <p
      className={`mb-0.5 min-w-0 text-xs leading-[18px] [overflow-wrap:anywhere] ${pageQuietText}`}
      data-spec-because=""
    >
      {decision === undefined ? null : (
        <>
          <span {...pageChrome}>{SPEC_WORDS.because} </span>
          {route === undefined ? (
            quoted
          ) : (
            // Out of the tab order, as every control inside a row is (R9): `f` steps to it.
            <Link
              className="dispatch-inline-link underline decoration-page-hair underline-offset-[3px]"
              data-spec-because-link=""
              tabIndex={-1}
              to={buildReferencePath(route)}
            >
              {quoted}
            </Link>
          )}
          <span {...pageChrome}> — </span>
          <Author actor={decision.by} />
          {decision.at === undefined ? null : (
            <>
              <span {...pageChrome}> · </span>
              <span className="whitespace-nowrap" {...authorText}>
                {shortDay(decision.at)}
              </span>
            </>
          )}
          {statement.changedSince ? <ChangedSince /> : null}
        </>
      )}
      {statement.alsoShapedBy.length === 0 ? null : (
        <>
          <span {...pageChrome}>
            {decision === undefined ? "" : " · "}
            {SPEC_WORDS.informed_by}{" "}
          </span>
          {statement.alsoShapedBy.map((informed, place) => (
            <span key={informed.decision.id}>
              {place === 0 ? null : <span {...pageChrome}>, </span>}
              <Informed decision={informed.decision} />
              {informed.changedSince ? <ChangedSince /> : null}
            </span>
          ))}
        </>
      )}
    </p>
  );
}

/** That the statement's own words changed after the decision beside these words was made
 *  (R5), read from the tie (`DecisionLink.to.changed_at`). */
function ChangedSince(): ReactNode {
  return (
    <>
      <span {...pageChrome}> · </span>
      <span data-spec-changed="" {...pageChrome}>
        {SPEC_WORDS.changed_since}
      </span>
    </>
  );
}

/** One other decision that shaped the statement, named by who decided it and when, as a link to
 *  it: the model holds no short name for a decision, and a block id is an identifier (R22). */
function Informed({ decision }: { decision: SpecDecision }): ReactNode {
  const route =
    decision.reference === undefined ? undefined : parseNodeReference(decision.reference);
  const named = (
    <>
      <Author actor={decision.by} />
      {decision.at === undefined ? null : (
        <>
          <span {...pageChrome}> · </span>
          <span className="whitespace-nowrap" {...authorText}>
            {shortDay(decision.at)}
          </span>
        </>
      )}
    </>
  );
  if (route === undefined) {
    return <span data-spec-informed="">{named}</span>;
  }
  return (
    <Link
      className="dispatch-inline-link underline decoration-page-hair underline-offset-[3px]"
      data-spec-informed=""
      tabIndex={-1}
      to={buildReferencePath(route)}
    >
      {named}
    </Link>
  );
}
