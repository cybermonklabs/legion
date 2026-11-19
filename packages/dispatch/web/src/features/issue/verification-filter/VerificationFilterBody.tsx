import { type ReactNode, useMemo, useState } from "react";

import type { Actor, CheckBlock } from "../../../api/types";
import {
  focusVisibleRing,
  pageFailedText,
  pageGuideBg,
  pageHairBorder,
  pageInkBg,
  pageInkBorder,
  pageInkText,
  pageKindBullet,
  pageKindTag,
  pageLineBorder,
  pageMutedText,
  pagePaneBg,
  pagePaperBg,
  pageQuietText,
  pageStateText,
  pageWordsText,
} from "../../../theme/classes";
import { useAgentText } from "../../refs/AgentName";
import { CutWords } from "../CutWords";
import type { NodeChip, TreeNode } from "../issue-tree";
import { NodeFacts } from "../NodeFacts";
import { NodeGlyph } from "../NodeGlyph";
import { chipWords } from "../node-row-chip";
import { shortDay } from "../node-time";
import { authorText, pageChrome } from "../page-structure";
import {
  authorKind,
  type CheckState,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  GAP_LINES,
  GLYPH_TOKENS,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  type StatementState,
  sentenceCase,
  VERIFICATION_WORDS,
} from "../page-words";
import { RowText } from "../RowText";
import {
  type CheckReading,
  STATEMENT_STATES,
  type StatementReading,
  shortCommit,
  statementCounts,
  type VerificationReading,
  verificationReading,
} from "./statement-state";

/**
 * The Verification filter's body (dispatch://INFRA-1196 R17, R2, R10, R22, the approved picture
 * `page-v3-verification-1440.png` and the prototype's `.ver`).
 *
 * It is the picture: a summary of how the issue stands, then each statement with the checks
 * that serve it beside it and each check's latest result beside that. From 900 px up the three
 * stand in three columns, as the prototype's table draws them; below that each statement is a
 * card and its checks stand under its words, as the prototype's `.vcards` draw them.
 *
 * What it says of a statement and of a check is `statement-state.ts`, which is pure. Four
 * rules of R17 are drawn here:
 *
 * - **One commit, drawn once.** The summary names the commit the issue's results are read
 *   against; no result draws a commit of its own on its line, and a result recorded against
 *   another commit says so in its check's word, "Out of date".
 * - **Seven states, each a word and a shape.** A check's state is the page's own
 *   (`NODE_STATE_WORDS.check`), drawn with the shape its token carries (`NodeGlyph`), so no
 *   state is told from another by colour alone (R6).
 * - **Covered, never proven.** The summary counts the statements that carry a check as
 *   covered; no count, legend or heading of this body reads proven or met for it, because a
 *   statement that has a check is not a statement that is true.
 * - **Every value inside its card.** Each cell is a column of its own that may be zero wide
 *   and breaks inside a word where it has nothing else to break on, and a check's attributes
 *   and a result's are the two-column grid `NodeFacts.tsx` draws, which is what keeps a value
 *   from reaching past its card or over another at any width (the old tab's defect).
 *
 * A check's four attributes and what its result recorded are its node's facts
 * (`check-facts.ts`), drawn in the body that opens when the reader's focus reaches the check
 * (R6): a `run` as a command with its own copy control, a screenshot as a picture, `when` as
 * words. A statement no check serves carries its gap in full, where the picture draws "No
 * check proves this" (R10).
 */
export function VerificationFilterBody({
  checks,
  context,
  emptyLine,
  view,
}: {
  /** The issue's checks with the results recorded against them, as the results read answers
   *  them (`GET /api/v1/issues/{key}/results`), which the page has already read for its tree. */
  checks: readonly CheckBlock[];
  /** The ids of the rows the filter draws as context, which under Verification is every
   *  statement (R2, `presets.ts`). */
  context: ReadonlySet<string>;
  /** What the body says where the filter's query left nothing to draw. */
  emptyLine: string;
  /** The tree as the Verification filter left it (`presetView`). */
  view: TreeNode;
}): ReactNode {
  const read = useMemo(() => verificationReading({ checks, view }), [checks, view]);
  // Which state the reader narrowed the body to, as the picture's chips do; nothing is
  // narrowed until they press one.
  const [only, setOnly] = useState<StatementState | undefined>(undefined);
  const counts = statementCounts(read.statements);
  const shown =
    only === undefined
      ? read.statements
      : read.statements.filter((statement) => statement.state === only);
  if (read.statements.length === 0 && read.loose.length === 0 && read.others.length === 0) {
    return (
      <div className="mt-[22px] mb-[120px] min-w-0" data-verification-body="">
        {/* The page holds one tree on every address (R2), which here holds no row: what the
            filter's query left nothing of is said in its place. */}
        <div aria-label={CONTROL_LABELS.tree} className="min-w-0" role="tree">
          <p className={`text-sm leading-5 ${pageQuietText}`} role="none" {...pageChrome}>
            {emptyLine}
          </p>
        </div>
      </div>
    );
  }
  return (
    <div className="mt-[22px] mb-[120px] flex w-full min-w-0 flex-col" data-verification-body="">
      <Summary
        checks={read}
        commit={read.commit}
        counts={counts}
        covered={read.covered}
        total={read.statements.length}
      />
      <Chips counts={counts} onlyState={only} onPress={setOnly} total={read.statements.length} />
      <div
        className={`min-w-0 overflow-hidden rounded-[10px] border ${pageLineBorder} ${pagePaperBg}`}
        data-verification-table=""
      >
        <Headings />
        {/* The table's rows are the outline the page draws under this filter: one tree, named
            as the outline every view draws, so a reader meets one on every address (R2); each
            statement is an item of it, and its checks' words are the controls that open their
            facts. */}
        <div aria-label={CONTROL_LABELS.tree} className="min-w-0" role="tree">
          {shown.map((statement) => (
            <Statement
              context={context.has(statement.node.id)}
              key={statement.node.id}
              statement={statement}
            />
          ))}
          {/* What the table says where the state the reader narrowed to holds no statement,
              rather than headings over nothing. */}
          {only !== undefined && shown.length === 0 ? (
            <p
              className={`${CELL} text-sm leading-5 ${pageQuietText}`}
              data-verification-none=""
              role="none"
              {...pageChrome}
            >
              {EMPTY_STATE_LINES.verification_none}
            </p>
          ) : null}
          {only === undefined
            ? read.loose.map((check) => (
                <div
                  aria-level={1}
                  className={`border-b last:border-b-0 ${pageLineBorder}`}
                  data-verification-loose=""
                  key={check.node.id}
                  role="treeitem"
                  tabIndex={-1}
                >
                  <Check check={check} />
                </div>
              ))
            : null}
          {only === undefined
            ? read.others.map((node) => <OtherRow key={node.id} node={node} />)
            : null}
        </div>
      </div>
    </div>
  );
}

/** The seven states a check can be in, in the order the page declares them. */
const CHECK_STATES = Object.keys(NODE_STATE_WORDS.check) as CheckState[];
/** A column heading, in the prototype's `.cap`. */
const CAP = `text-xs leading-4 font-semibold tracking-[0.08em] uppercase ${pageQuietText}`;

/** The statement column and the rest, as the prototype's table divides them (38% and the rest);
 *  below 900 px one column, where each statement is a card. */
const STATEMENT_COLUMNS =
  "grid min-w-0 grid-cols-[minmax(0,38%)_minmax(0,62%)] max-[900px]:grid-cols-[minmax(0,1fr)]";

/** The check column and its result, inside the rest (36% and 26% of the whole). */
const CHECK_COLUMNS =
  "grid min-w-0 grid-cols-[minmax(0,58%)_minmax(0,42%)] max-[900px]:grid-cols-[minmax(0,1fr)]";

/** One cell of the table, in the prototype's `.vt td`. */
const CELL = "min-w-0 px-3 py-[11px]";

/** The colour each state is drawn in, which repeats what its word and its shape already say
 *  (R6): met in the passed green, not met in the failed red, unknown quiet. */
const STATEMENT_TONES: Readonly<Record<StatementState, string>> = {
  met: pageStateText.passed,
  not_met: pageStateText.failed,
  unknown: pageStateText.quiet,
};

/** The fill each state takes in the summary's bar, which is decoration: the counts beside it
 *  say the same in words. */
const STATEMENT_FILLS: Readonly<Record<StatementState, string>> = {
  met: "bg-page-passed",
  not_met: "bg-page-failed",
  unknown: "bg-page-hair",
};

/** How a check's state is coloured: the passed green and the failed red the page already draws
 *  a check in, and the quiet grey for the five that are neither. */
function checkTone(state: CheckState): string {
  if (state === "passed") return pageStateText.passed;
  return state === "failed" ? pageStateText.failed : pageStateText.quiet;
}

/**
 * How the issue stands, in one card over the table (the prototype's `.vsum`): how many
 * statements carry a check, the bar of how they stand, the count in each state, the one commit
 * the page names for the issue's results, drawn here and nowhere else (R17), and how many of
 * the checks it draws stand in each of the seven states a check can be in, each by its word and
 * its shape, so the shapes the table draws are named once over it (R6).
 */
function Summary({
  checks,
  commit,
  counts,
  covered,
  total,
}: {
  /** The checks the body draws, each counted once however many statements it serves. */
  checks: Pick<VerificationReading, "loose" | "statements">;
  commit: string | undefined;
  counts: Readonly<Record<StatementState, number>>;
  covered: number;
  /** How many statements the filter drew, which the covered count is out of. */
  total: number;
}): ReactNode {
  const byState = new Map<string, CheckState | undefined>();
  for (const check of [
    ...checks.statements.flatMap((statement) => statement.checks),
    ...checks.loose,
  ]) {
    byState.set(check.node.id, check.state);
  }
  const inState = (state: CheckState): number =>
    [...byState.values()].filter((held) => held === state).length;
  return (
    <div
      className={`flex min-w-0 flex-wrap items-center gap-[18px] rounded-[10px] border px-[18px] py-3.5 ${pageLineBorder} ${pagePaperBg}`}
      data-verification-summary=""
    >
      <p className={`text-sm leading-5 font-semibold ${pageInkText}`} data-verification-covered="">
        <span {...authorText}>{covered}</span> <span {...pageChrome}>{VERIFICATION_WORDS.of}</span>{" "}
        <span {...authorText}>{total}</span>{" "}
        <span {...pageChrome}>{VERIFICATION_WORDS.covered}</span>
      </p>
      <span
        aria-hidden="true"
        className={`flex h-3 min-w-[220px] flex-1 overflow-hidden rounded-[6px] ${pageGuideBg}`}
        data-verification-bar=""
      >
        {STATEMENT_STATES.filter((state) => counts[state] > 0).map((state) => (
          <i
            className={`block h-full ${STATEMENT_FILLS[state]}`}
            key={state}
            style={{ width: `${(counts[state] / Math.max(total, 1)) * 100}%` }}
          />
        ))}
      </span>
      <p
        className={`flex min-w-0 flex-wrap gap-[14px] text-sm leading-5 ${pageMutedText}`}
        data-verification-legend=""
      >
        {STATEMENT_STATES.map((state) => (
          <span className="inline-flex items-center gap-[5px]" key={state}>
            <NodeGlyph
              className={`size-3 shrink-0 ${STATEMENT_TONES[state]}`}
              token={GLYPH_TOKENS.statements[state]}
            />
            <span className={pageInkText} {...authorText}>
              {counts[state]}
            </span>
            <span {...pageChrome}>{VERIFICATION_WORDS.states[state]}</span>
          </span>
        ))}
      </p>
      <p
        className={`flex min-w-0 basis-full flex-wrap gap-x-[14px] gap-y-1 text-xs leading-5 ${pageMutedText}`}
        data-verification-check-legend=""
      >
        {CHECK_STATES.map((state) => (
          <span
            className="inline-flex items-center gap-[5px]"
            data-check-legend-state={state}
            key={state}
          >
            <NodeGlyph
              className={`size-3 shrink-0 ${checkTone(state)}`}
              token={GLYPH_TOKENS.states.check[state]}
            />
            <span className={pageInkText} {...authorText}>
              {inState(state)}
            </span>
            <span {...pageChrome}>{NODE_STATE_WORDS.check[state]}</span>
          </span>
        ))}
      </p>
      {commit === undefined ? null : (
        <p className={`min-w-0 text-sm leading-5 ${pageMutedText}`} data-verification-commit="">
          <span {...pageChrome}>{VERIFICATION_WORDS.commit}</span>{" "}
          <code className={`font-mono break-all ${pageInkText}`} {...authorText}>
            {shortCommit(commit)}
          </code>
        </p>
      )}
    </div>
  );
}

/** The chips over the table (the prototype's `.vchips`): All, and one per state a statement can
 *  be in with how many stand in it. Pressing one draws those statements alone, pressing it
 *  again or pressing All draws them all, as the filter row's own controls work (R2). */
function Chips({
  counts,
  onlyState,
  onPress,
  total,
}: {
  counts: Readonly<Record<StatementState, number>>;
  onlyState: StatementState | undefined;
  onPress: (state: StatementState | undefined) => void;
  total: number;
}): ReactNode {
  const chip = (on: boolean): string =>
    `rounded-full border px-3 py-0.5 text-sm leading-5 font-semibold ${focusVisibleRing} ${
      on
        ? `${pageInkBg} ${pageInkBorder} text-page-paper`
        : `${pageHairBorder} ${pagePaperBg} ${pageMutedText}`
    }`;
  return (
    <div className="my-[14px] flex min-w-0 flex-wrap gap-1.5" data-verification-chips="">
      <button
        aria-pressed={onlyState === undefined}
        className={chip(onlyState === undefined)}
        data-verification-chip="all"
        onClick={() => onPress(undefined)}
        type="button"
        {...pageChrome}
      >
        {VERIFICATION_WORDS.all}{" "}
        <span className="text-xs font-medium" {...authorText}>
          {total}
        </span>
      </button>
      {STATEMENT_STATES.map((state) => (
        <button
          aria-pressed={onlyState === state}
          className={chip(onlyState === state)}
          data-verification-chip={state}
          // A chip with nothing in its state draws nothing, so it is no press to offer.
          disabled={counts[state] === 0 && onlyState !== state}
          key={state}
          onClick={() => onPress(onlyState === state ? undefined : state)}
          type="button"
          {...pageChrome}
        >
          {VERIFICATION_WORDS.states[state]}{" "}
          <span className="text-xs font-medium" {...authorText}>
            {counts[state]}
          </span>
        </button>
      ))}
    </div>
  );
}

/** The three column headings, in the prototype's `.vt th`; below 900 px the columns stack and
 *  each statement's card carries its own words, so the headings are drawn there. */
function Headings(): ReactNode {
  return (
    <div
      className={`${STATEMENT_COLUMNS} border-b max-[900px]:hidden ${pageLineBorder} ${pagePaneBg}`}
      data-verification-head=""
    >
      <span className={`${CELL} ${CAP}`} {...pageChrome}>
        {sentenceCase(NODE_TYPE_NAMES.requirement)}
      </span>
      <span className={CHECK_COLUMNS}>
        <span className={`${CELL} ${CAP}`} {...pageChrome}>
          {VERIFICATION_WORDS.checks}
        </span>
        <span className={`${CELL} ${CAP}`} {...pageChrome}>
          {VERIFICATION_WORDS.latest_result}
        </span>
      </span>
    </div>
  );
}

/**
 * One statement with the checks that serve it (the prototype's `.vt tr` and its `.vcard` below
 * 900 px): how it stands at the commit the page names, its words, the node its words came from
 * as one line, and its gap where no check serves it (R1's ruling, R2, R10, R17). Its words are
 * cut as an agent's are on every row (ruling 6 of build-rulings-md: a twenty-line statement
 * opened this view on a wall of text).
 */
function Statement({
  context,
  statement,
}: {
  context: boolean;
  statement: StatementReading;
}): ReactNode {
  const node = statement.node;
  return (
    <div
      aria-level={1}
      className={`border-b last:border-b-0 ${STATEMENT_COLUMNS} ${pageLineBorder}`}
      data-node-context={context ? "" : undefined}
      data-node-id={node.id}
      data-node-type={node.type}
      data-statement-state={statement.state}
      data-verification-card=""
      role="treeitem"
      tabIndex={-1}
    >
      <div className={`flex min-w-0 flex-col ${CELL}`}>
        <p
          className={`mb-0.5 flex items-center gap-1.5 text-xs leading-5 font-semibold ${STATEMENT_TONES[statement.state]}`}
        >
          <NodeGlyph
            className="size-3.5 shrink-0"
            token={GLYPH_TOKENS.statements[statement.state]}
          />
          <span data-statement-state={statement.state} {...pageChrome}>
            {VERIFICATION_WORDS.states[statement.state]}
          </span>
        </p>
        <CutWords className={`min-w-0 text-[15px] leading-6 ${pageWordsText}`} whole={false}>
          <RowText markdown={node.text} source={node.source} tree={node.tree} />
        </CutWords>
        <ParentLine node={node} />
        <Gaps node={node} />
      </div>
      <div className="flex min-w-0 flex-col">
        {statement.checks.map((check) => (
          <Check check={check} key={`${check.node.id}:${check.latest?.id ?? ""}`} />
        ))}
      </div>
    </div>
  );
}

/**
 * One check beside its latest result (the prototype's `.vc` and `.vres`): how it stands, by a
 * word and a shape, its own words, and the four attributes its body draws (R15, R17). Its
 * earlier results are one line with their count, which opens them in place (ruling 6 of
 * build-rulings-md: the approved picture draws one result for each check), and the result the
 * check's state stands on is marked wherever it is drawn.
 *
 * The body opens while the reader's focus is on the check's words and while the reader has
 * pressed them open, and for no other check, which is the rule the outline draws a node's body
 * by (R6). A press while it is open closes it, so the control says what it does
 * (`aria-expanded`) and does it. The words are the control, so a reader reaches every fact of
 * a check and of its results by the keyboard alone and the table stays the table the picture
 * draws.
 */
function Check({ check }: { check: CheckReading }): ReactNode {
  const [focused, setFocused] = useState(false);
  // The reader's own press: open, shut, or neither, in which case the focus decides.
  const [pressed, setPressed] = useState<boolean | undefined>(undefined);
  const [earlierShown, setEarlierShown] = useState(false);
  const node = check.node;
  const surface = node.facts.find((fact) => fact.name === "surface")?.value ?? "";
  const opened = pressed ?? focused;
  return (
    <div
      className={`${CHECK_COLUMNS} border-t first:border-t-0 ${pageLineBorder}`}
      data-node-id={node.id}
      data-node-state={check.state}
      data-node-type={node.type}
      data-verification-check=""
    >
      <div className={CELL}>
        <button
          aria-expanded={opened}
          className={`flex w-full min-w-0 items-baseline gap-2 text-left text-sm leading-5 ${pageWordsText} ${focusVisibleRing}`}
          data-verification-check-words=""
          onBlur={() => {
            setFocused(false);
            setPressed((held) => (held === false ? undefined : held));
          }}
          onClick={() => setPressed(!opened)}
          onFocus={() => setFocused(true)}
          // A pointer's press is the press alone: focusing the words on its way down would open
          // the body before the press, and the press would then shut it.
          onMouseDown={(event) => event.preventDefault()}
          type="button"
        >
          <span
            aria-hidden="true"
            className={`mt-1.5 block size-[7px] shrink-0 rounded-full ${pageKindBullet.check}`}
          />
          <span className="min-w-0 break-words" data-node-words="" {...authorText}>
            {node.text}
          </span>
        </button>
        {surface === "" ? null : (
          <p
            className={`mt-0.5 text-xs leading-[18px] font-semibold ${pageKindTag.check}`}
            data-check-surface=""
            {...authorText}
          >
            {surface}
          </p>
        )}
        <AlsoUnder chips={node.alsoUnder} />
        <Gaps node={node} />
        {opened ? (
          <div className="mt-1 min-w-0" data-node-body="">
            <NodeFacts facts={node.facts} />
          </div>
        ) : null}
      </div>
      <div className={CELL}>
        {check.state === undefined ? null : (
          <p
            className={`flex items-center gap-1.5 text-xs leading-5 font-semibold ${checkTone(check.state)}`}
          >
            <NodeGlyph
              className="size-3.5 shrink-0"
              token={GLYPH_TOKENS.states.check[check.state]}
            />
            <span data-node-state={check.state} {...pageChrome}>
              {NODE_STATE_WORDS.check[check.state]}
            </span>
          </p>
        )}
        {check.latest === undefined ? null : (
          <LatestResult
            opened={opened}
            result={check.latest}
            stands={check.standsOn === check.latest.id}
          />
        )}
        {check.earlier.length === 0 ? null : (
          <button
            aria-expanded={earlierShown}
            className={`dispatch-inline-link mt-1 text-xs leading-5 font-medium underline decoration-page-hair underline-offset-[3px] ${pageQuietText}`}
            data-verification-earlier=""
            onClick={() => setEarlierShown(!earlierShown)}
            type="button"
            {...pageChrome}
          >
            <span {...authorText}>{check.earlier.length.toLocaleString()}</span>{" "}
            {check.earlier.length === 1
              ? VERIFICATION_WORDS.earlier_one
              : VERIFICATION_WORDS.earlier_many}
          </button>
        )}
        {earlierShown
          ? check.earlier.map((result) => (
              <LatestResult
                key={result.id}
                opened={opened}
                result={result}
                stands={check.standsOn === result.id}
              />
            ))
          : null}
      </div>
    </div>
  );
}

/** One result of a check: who recorded it and when, what it observed, whether the check's state
 *  stands on it, and what it holds that no line has room for, in the body the check's focus
 *  opens (R6, R9, R15). The commit it ran at is the one the summary names, drawn there and not
 *  here (R17). */
function LatestResult({
  opened,
  result,
  stands,
}: {
  opened: boolean;
  result: TreeNode;
  stands: boolean;
}): ReactNode {
  return (
    <div
      className="mt-1 min-w-0"
      data-node-id={result.id}
      data-node-type={result.type}
      data-result-stands={stands ? "" : undefined}
      data-verification-result=""
    >
      <p className={`min-w-0 text-xs leading-[18px] [overflow-wrap:anywhere] ${pageQuietText}`}>
        {stands ? (
          <>
            <span className={`font-semibold ${pageInkText}`} {...pageChrome}>
              {VERIFICATION_WORDS.stands}
            </span>
            <span {...pageChrome}> · </span>
          </>
        ) : null}
        <Author actor={result.author} />
        {result.at === undefined ? null : (
          <>
            <span {...pageChrome}> · </span>
            <span className="whitespace-nowrap" data-node-time="" {...authorText}>
              {shortDay(result.at)}
            </span>
          </>
        )}
      </p>
      {result.text === "" ? null : (
        <p
          className={`mt-0.5 min-w-0 text-sm leading-5 break-words ${pageWordsText}`}
          data-node-words=""
          {...authorText}
        >
          {result.text}
        </p>
      )}
      {opened ? (
        <div className="mt-1 min-w-0" data-node-body="">
          <NodeFacts facts={result.facts} />
        </div>
      ) : null}
    </div>
  );
}

/** Who made something, by a name and a shape, as every other row of the page draws an author
 *  (R9): never an identifier, and the page's own words where the model holds none. */
function Author({ actor }: { actor: Actor | undefined }): ReactNode {
  const agentText = useAgentText();
  const kind = authorKind(actor);
  return (
    <span className="inline-flex min-w-0 items-center gap-1" data-node-author="">
      <NodeGlyph className="size-3 shrink-0" token={GLYPH_TOKENS.authors[kind]} />
      <span className="min-w-0 break-words" {...(actor === undefined ? pageChrome : authorText)}>
        {actor === undefined ? CONTROL_LABELS.unknown_author : agentText.author(actor)}
      </span>
    </span>
  );
}

/** The other statements a check is drawn under, each named by its first words as every chip of
 *  the page names its target (R4, `node-row-chip.ts`): a check serves several statements and
 *  this body draws it under each. A target the reference read has not named draws nothing. */
function AlsoUnder({ chips }: { chips: readonly NodeChip[] }): ReactNode {
  const named = chips.filter((chip) => chip.name !== "");
  if (named.length === 0) return null;
  return (
    <p
      className={`mt-0.5 min-w-0 text-xs leading-[18px] [overflow-wrap:anywhere] ${pageQuietText}`}
      data-node-trail=""
    >
      <span {...pageChrome}>{CONTROL_LABELS.also_under} </span>
      {named.map((chip, place) => (
        <span data-node-chip="" key={chip.ref}>
          {place === 0 ? null : <span {...pageChrome}>, </span>}
          <span {...authorText}>{chipWords(chip.name)}</span>
        </span>
      ))}
    </p>
  );
}

/** Each gap the node carries, said in full where the picture draws "No check proves this"
 *  (R10): a statement no check serves, and a check whose `serves` names no statement. */
function Gaps({ node }: { node: TreeNode }): ReactNode {
  if (node.gaps.length === 0) return null;
  return (
    <>
      {node.gaps.map((gap) => (
        <p
          className={`mt-1 min-w-0 text-xs leading-[18px] font-semibold ${pageFailedText}`}
          data-gap=""
          data-gap-kind={gap}
          key={gap}
          {...pageChrome}
        >
          {GAP_LINES[gap]}
        </p>
      ))}
    </>
  );
}

/** The one line a row draws for the node its words came from, where the filter draws that node
 *  as no row (R2, R1's first-line rule): its kind and its first line. */
function ParentLine({ node }: { node: TreeNode }): ReactNode {
  if (node.parentLine === undefined) return null;
  return (
    <p
      className={`mt-0.5 flex min-w-0 items-baseline gap-1 text-xs leading-5 ${pageQuietText}`}
      data-node-parent-line=""
      data-parent-id={node.parentLine.id}
      data-parent-type={node.parentLine.type}
    >
      <span className="shrink-0" {...pageChrome}>
        {NODE_TYPE_NAMES[node.parentLine.type]}
      </span>
      <span className="min-w-0 break-words" {...authorText}>
        {node.parentLine.line}
      </span>
    </p>
  );
}

/** A row the filter kept that is neither a statement nor a check: a node an address named,
 *  which outranks the filter (R20). It is drawn by its kind and its words, so nothing the
 *  filter drew is lost (R19). */
function OtherRow({ node }: { node: TreeNode }): ReactNode {
  return (
    <div
      aria-level={1}
      className={`${CELL} border-b last:border-b-0 ${pageLineBorder}`}
      data-node-id={node.id}
      data-node-type={node.type}
      data-verification-other=""
      role="treeitem"
      tabIndex={-1}
    >
      <p className={`text-xs leading-5 ${pageQuietText}`} {...pageChrome}>
        {NODE_TYPE_NAMES[node.type]}
      </p>
      <div className={`min-w-0 text-[15px] leading-6 ${pageWordsText}`} data-node-words="">
        <RowText markdown={node.text} source={node.source} tree={node.tree} />
      </div>
      <ParentLine node={node} />
    </div>
  );
}
