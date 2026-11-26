import { dispatchWritingBox } from "@legion/contracts";
import { type FormEvent, type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import type { Actor, DisplayTree } from "../../api/types";
import { Dot, KindBullet } from "../../components/RowMarks";
import type { PageWords } from "../../components/tooltips";
import { compareTimestamps } from "../../lib/timestamps";
import {
  dangerText,
  decisionUnsureText,
  focusBorder,
  linkHoverText,
  linkText,
  pageFailedText,
  pageHairBorder,
  pageInkFill,
  pageInkText,
  pageKindBullet,
  pageKindTag,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
  pageWordsText,
  primaryButton,
  secondaryButtonCompact,
  textMutedOnSelectedCard,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { MentionField } from "../conversation/MentionField";
import { useAgentText } from "../refs/AgentName";
import { usePlainHeadline } from "../refs/plain-text";
import { Timestamp } from "../refs/Timestamp";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import {
  DECISION_ACT_WORDS,
  DECISION_KIND_WORDS,
  DECISION_LINE_WORDS,
  DECISION_LINK_WORDS,
  DECISION_STATE_WORDS,
} from "./decision-card-words";

/**
 * The colours of the card's own elements by the palette its caller hands (`CardPalette`), as
 * `issue-page/DecisionActions.tsx` keeps its own: `main`'s are the classes every surface of
 * today's pages draws, and the new issue page's take the role `main`'s colour has. The heading
 * and the Change form's label are the ink; a link - the heading's, a statement's, the ask it
 * answers - the ink, underlined, as the page draws a link; what stands beside the heading, a
 * statement's "Changed since" and "out of date", the level word and the state the quiet grey,
 * as the page's own row draws a decision's margin (`NodeRow.tsx`); the level's reason the muted
 * grey; the quoted words the page's words inside its rule, as the page's own Change quotes them;
 * a refusal the failed red. Keep, Change and Cancel are the page's compact secondary and Send
 * change its primary, each refused as the page refuses (`components/RefusableButton.tsx`'s
 * `PAGE_LOOKS`), and the Change box is the page's field, which keeps `main`'s focus rule. Each
 * pair is one `theme/classes.ts` registers for the page.
 */
const TONES = {
  main: {
    failed: dangerText,
    field: undefined,
    filled: primaryButton,
    hint: undefined,
    ink: textPrimaryOnSurface,
    level: decisionUnsureText,
    link: `${linkText} ${linkHoverText}`,
    meta: textMutedOnSelectedCard,
    quote: textSecondaryOnSurface,
    reason: textSecondaryOnSurface,
    secondary: secondaryButtonCompact,
  },
  page: {
    failed: pageFailedText,
    field: `${pageHairBorder} ${pagePaperBg} ${pageInkText} placeholder:text-page-quiet ${focusBorder}`,
    filled: `rounded-lg px-3 py-2 text-sm font-semibold ${pageInkFill} refused:cursor-not-allowed refused:bg-page-hair refused:text-page-muted`,
    hint: `text-xs ${pageQuietText}`,
    ink: pageInkText,
    level: pageQuietText,
    link: `underline ${pageInkText}`,
    meta: pageQuietText,
    quote: `${pageLineBorder} ${pageWordsText}`,
    reason: pageMutedText,
    secondary: `min-h-11 shrink-0 rounded-lg border px-3 py-1 text-xs font-medium whitespace-nowrap sm:min-h-7 sm:px-2 ${pageLineBorder} ${pagePaperBg} ${pageMutedText} refused:text-page-quiet`,
  },
} as const;

/** Whose colours the card draws (`TONES`): `main`'s, as every surface of today's pages draws
 *  it, or the new issue page's (dispatch://INFRA-3940 R13). */
export type CardPalette = keyof typeof TONES;

/** One statement a decision shaped, by the tie the decision holds to it. */
export interface CardLink {
  /** The statement's block, which the caller's `paths.statement` opens. */
  readonly block_id: string;
  /** When the statement last changed, where the read says. */
  readonly changed_at?: string | null;
  /** When the decision was tied to the statement. */
  readonly created_at: string;
  /** Whether the decision changed after the tie. */
  readonly stale: boolean;
  /** The statement's own `dispatch://` address, by which the caller reads its words where the
   *  read of the decision holds none (`withStatementTexts`). */
  readonly ref?: string;
  /** The statement's words, whose first line is the link: the read of the decision's, else the
   *  caller's batched read of the statements' names; none, or `""`, draws no line. */
  readonly text?: string;
  /** The server's reading of `text`, where the read that gave the words sent one. */
  readonly text_tree?: DisplayTree;
}

/** The change a person asked of the decision, which the caller found. */
export interface CardDecisionChange {
  readonly author: Actor;
}

/**
 * A decision as the card draws it (R19, R20): the card's own shape, so each read a caller holds
 * hands it its fields through an adapter (`adapters.ts`). Every field the card can draw that a
 * read may lack is optional, so a read that gains it hands it on with no change of the card.
 */
export interface CardDecision {
  /** A person's answer, which is a person's decision's text where the read holds it. */
  readonly answer?: string;
  /** The asks a person's decision answers, by id. */
  readonly asks: readonly string[];
  /** The change the caller found, with its author. */
  readonly change?: CardDecisionChange;
  /** When the decision last changed: its time. */
  readonly changed_at: string | null;
  /** Who made the decision, where the read says. */
  readonly decided_by?: Actor;
  /** The decision's heading: the words of its first line. */
  readonly heading: string;
  /** Who kept the decision, where a person kept it. */
  readonly kept_by?: Actor;
  readonly kind: "agent" | "person";
  /** The statements the decision shaped. */
  readonly links: readonly CardLink[];
  readonly replaced: boolean;
  /** The decision's text, its heading line included where the read writes one. */
  readonly text: string;
  /** The reading the server made of `text`, where it sent one. */
  readonly text_tree?: DisplayTree;
  /** The level of uncertainty, as the read holds it. */
  readonly uncertainty?: string;
  /** Why its maker was unsure, where the read holds it apart from the text. */
  readonly uncertainty_reason?: string;
  /** Whether its maker was unsure. */
  readonly unsure: boolean;
  readonly waiting_review: boolean;
}

/** The text the card hands its caller's renderer: the decision's text, or a person's answer. */
export interface DecisionText {
  readonly markdown: string;
  readonly tree?: DisplayTree;
}

/** Draws the decision's text. Each surface hands the card a renderer of its own (R21). */
export type DecisionTextRenderer = (text: DecisionText) => ReactNode;

/** Where the card's links go, which the caller knows and the decision does not. */
export interface CardPaths {
  /** The address of the ask `id`. */
  readonly ask: (id: string) => string;
  /** The decision's own place, which its heading links where the caller names one. */
  readonly decision?: string;
  /** The address that opens the statement block `blockId`. */
  readonly statement: (blockId: string) => string;
}

/** Keep, as the caller makes it. */
export interface CardKeepAct {
  readonly error: Error | null;
  readonly keep: () => void;
  readonly pending: boolean;
}

/** Change, as the caller makes it. */
export interface CardChangeAct {
  /** Sends the change `body`. `sent` runs once the write lands. */
  readonly change: (body: string, sent: () => void) => void;
  readonly error: Error | null;
  readonly pending: boolean;
  /** The decision's words the change is anchored on, which the form quotes. */
  readonly quote: string;
}

/** The decision's heading as its caller hands it to the card: drawn by the caller's renderer
 *  where the heading links nowhere, and as plain words where it words a link, names the Change
 *  form or names a stopped Keep, none of which holds markup (`useDecisionHeadingWords`). The card
 *  draws no heading of its own: a heading is the writer's own markdown, which only a renderer
 *  may draw. */
export interface CardHeading {
  /** The heading rendered for this surface (`MarkdownBody`), where it links nowhere. */
  readonly node: ReactNode;
  /** The heading as plain words (`decision-heading-words`). */
  readonly words: string;
}

export interface DecisionCardProps {
  /** R20's setting: on, the card draws the author and the level word the read holds. */
  readonly authorLine: boolean;
  /** Change, on a decision that waits for review; none where the caller writes none. */
  readonly change?: CardChangeAct;
  readonly decision: CardDecision;
  /** The decision's heading as its caller draws and words it (`CardHeading`). */
  readonly heading: CardHeading;
  /** The issue the decision is on, for the Change composer's reference picker. */
  readonly issueKey?: string;
  /** Keep, on a decision that waits for review; none where the caller writes none. */
  readonly keep?: CardKeepAct;
  /** The two marks of a host that divides its own words from an author's (`PageWords`): with
   *  them each element of the card that holds the card's own words - a label, a line, a control,
   *  the dot between two parts of a line - wears the mark of those words, which are the issue
   *  page's declared ones (`decision-card-words.ts`), and each value inside one - the author,
   *  the level the read holds and its reason, a statement's line, the name a state holds - the
   *  mark of a value, as the caller's renderer marks the heading and the text. The mark stands
   *  on each such element and never on a box that holds several: a reader of the page takes each
   *  marked element's words whole, with its values taken out, and the state and two controls in
   *  one marked box read as one word ("Waiting for review", "Keep" and "Change" as
   *  "reviewkeepchange"). The Change form's shared box, with its hint, and the words it quotes
   *  wear the mark of a value, and each of its actions the card's again. Dashboard hands them;
   *  no other host does, so every other surface draws the elements it always drew, with no mark. */
  readonly marks?: Pick<PageWords, "authorText" | "pageChrome">;
  /** The palette the card's own elements draw in (`TONES`): `main`'s unless the caller hands the
   *  page's. Dashboard on the new issue page hands `page`; no other host does, so every other
   *  surface, today's Dashboard among them, draws the classes it always drew. */
  readonly palette?: CardPalette;
  readonly paths: CardPaths;
  readonly renderText: DecisionTextRenderer;
}

/** The level word: off the setting, "Unsure" alone, for a decision its maker was unsure of or
 *  whose level is `high`; on it, the level the read holds, else "Unsure" for `unsure`. */
function levelWord(decision: CardDecision, authorLine: boolean): string | undefined {
  const level = decision.uncertainty;
  if (authorLine && level !== undefined && level !== "") return level;
  return decision.unsure || level === "high" ? DECISION_LINE_WORDS.unsure : undefined;
}

/** One statement the decision shaped, as a link by its first line, with "Changed since" and
 *  "out of date" where they apply. Its words are the link's own (`CardLink.text`), which the
 *  caller filled from one batched read of the statements' names where the decision's read holds
 *  none, which is how the server answers a tie today (`withStatementTexts`); a statement with no
 *  words is drawn as nothing, and never by its block's id. A link holds no markup, so the line
 *  is the first line of the statement's text as plain words, from the reading the server made of
 *  it where the read sent one (`usePlainHeadline`): the reading list's
 *  `decision-statement-line`. The line is the statement's own words, which a host that hands
 *  `marks` gets inside the mark of a value. */
function StatementLink({
  link,
  marks,
  palette,
  to,
}: {
  link: CardLink;
  marks: DecisionCardProps["marks"];
  palette: CardPalette;
  to: string;
}): ReactNode {
  const tones = TONES[palette];
  const { reads, text } = usePlainHeadline(link.text_tree, link.text, {
    firstLine: true,
    surface: "decision-statement-line",
  });
  // Until the engine has answered, the first line as written: the load state of
  // `decision-statement-line`.
  const asWritten = (link.text ?? "").trim().split("\n", 1)[0]?.replace(/\s+/gu, " ").trim() ?? "";
  const line = text?.text ?? asWritten;
  if (line === "") return null;
  return (
    <li>
      {reads}
      <Link className={tones.link} to={to} {...marks?.authorText}>
        {line}
      </Link>
      {link.changed_at != null && compareTimestamps(link.changed_at, link.created_at) > 0 ? (
        <span className={`ml-2 text-xs ${tones.meta}`} {...marks?.pageChrome}>
          {DECISION_LINK_WORDS.changed_since}
        </span>
      ) : null}
      {link.stale ? (
        <span className={`ml-2 text-xs ${tones.meta}`} {...marks?.pageChrome}>
          {DECISION_LINK_WORDS.out_of_date}
        </span>
      ) : null}
    </li>
  );
}

/**
 * The decision card (dispatch://INFRA-5702/spec#b-r-decision-card): one agent decision, or a
 * person's, on every surface that draws one. Its parts, in order: root R34's row - a bullet, the
 * heading, and at the right end of that line the kind, the author where the setting is on, and
 * the time; the text whole, through the caller's renderer; the level of uncertainty and its
 * reason; the statements it shaped, each by its first line as a link, with "Changed since" and
 * "out of date" where they apply; "The ask it answers" for each ask a person's decision answers;
 * the state word; and Keep and Change on a decision that waits for review. Its root carries
 * `data-decision-card`.
 *
 * It writes nothing of its own: Keep and Change call the acts its caller hands it, so a caller
 * hands it another write with no change of the card. It reads nothing of its own: a statement's
 * words are its caller's (`StatementLink`). It imports neither `DecisionActions` nor
 * `RowText` (R21), and no file under `features/issue-page/` (R22). The caller draws the frame
 * around it.
 */
export function DecisionCard({
  authorLine,
  change,
  decision,
  heading,
  issueKey,
  keep,
  marks,
  palette = "main",
  paths,
  renderText,
}: DecisionCardProps): ReactNode {
  const tones = TONES[palette];
  const agentText = useAgentText();
  const level = levelWord(decision, authorLine);
  // The level the read holds is a value; "Unsure" is the card's own word.
  const levelMark = level === decision.uncertainty ? marks?.authorText : marks?.pageChrome;
  const reason = decision.uncertainty_reason?.trim() ?? "";
  const author = !authorLine
    ? undefined
    : decision.decided_by === undefined
      ? DECISION_LINE_WORDS.author_unknown
      : agentText.author(decision.decided_by);
  // The name of who decided is a value; the word for an author the read does not name is the
  // card's own.
  const authorMark = decision.decided_by === undefined ? marks?.pageChrome : marks?.authorText;
  // How the decision stands: the state's words, and the name of the person it holds where it
  // holds one, which a host that hands `marks` draws inside the mark of a value.
  const state = decision.replaced
    ? { words: DECISION_STATE_WORDS.replaced }
    : authorLine && decision.change !== undefined
      ? { name: agentText.author(decision.change.author), words: DECISION_STATE_WORDS.changed_by }
      : decision.waiting_review
        ? { words: DECISION_STATE_WORDS.waiting_review }
        : decision.kept_by === undefined
          ? undefined
          : { name: agentText.author(decision.kept_by), words: DECISION_STATE_WORDS.kept_by };
  const text: DecisionText =
    decision.kind === "person" && decision.answer !== undefined
      ? { markdown: decision.answer }
      : { markdown: decision.text, tree: decision.text_tree };
  const acts = decision.waiting_review && !decision.replaced;
  // The dot between two parts of the line is the card's own mark, which `Dot` takes none of: a
  // host that hands `marks` gets it inside the mark of the card's words, in an element that
  // draws no box (`contents`), so the line is laid out as it is with no mark.
  const dot =
    marks === undefined ? (
      <Dot />
    ) : (
      <span className="contents" {...marks.pageChrome}>
        <Dot />
      </span>
    );
  return (
    // The bullet hangs left of the heading, and everything under it stands at the heading's left
    // edge: an 18 px column, its 7 px dot centred on the first line.
    <div className="flex gap-x-2" data-decision-card="">
      <KindBullet colour={pageKindBullet.decision} mark="data-decision-bullet" />
      <div className="min-w-0 flex-1 space-y-2">
        <div className="flex items-start justify-end gap-x-3">
          <h3 className={`min-w-0 flex-1 text-[15px] leading-6 font-medium ${tones.ink}`}>
            {paths.decision === undefined ? (
              heading.node
            ) : (
              <Link className={tones.link} to={paths.decision} {...marks?.authorText}>
                {/* A link's words hold no markup: the reading list's `decision-heading-words`. */}
                {heading.words}
              </Link>
            )}
          </h3>
          <p
            className={`flex min-w-0 max-w-[75%] shrink-0 items-center justify-end gap-x-1.5 text-xs leading-6 whitespace-nowrap ${tones.meta}`}
          >
            <span
              className={`shrink-0 font-semibold ${pageKindTag.decision}`}
              {...marks?.pageChrome}
            >
              {decision.kind === "person" ? DECISION_KIND_WORDS.person : DECISION_KIND_WORDS.agent}
            </span>
            {author === undefined ? null : (
              <>
                {dot}
                <span className="min-w-0 truncate" {...authorMark}>
                  {author}
                </span>
              </>
            )}
            {decision.changed_at === null ? null : (
              <>
                {dot}
                <Timestamp at={decision.changed_at} className="shrink-0" />
              </>
            )}
          </p>
        </div>
        <div className="space-y-2" data-decision-text="">
          {renderText(text)}
        </div>
        {level === undefined && reason === "" ? null : (
          <p className="flex flex-wrap gap-x-2 text-sm" data-decision-level="">
            {level === undefined ? null : (
              <span className={`font-semibold ${tones.level}`} {...levelMark}>
                {level}
              </span>
            )}
            {reason === "" ? null : (
              <span className={tones.reason} {...marks?.authorText}>
                {reason}
              </span>
            )}
          </p>
        )}
        {decision.links.length === 0 ? null : (
          <ul className="space-y-1 text-sm" data-decision-statements="">
            {decision.links.map((link, index) => (
              <StatementLink
                // biome-ignore lint/suspicious/noArrayIndexKey: one statement can be tied twice
                key={`${link.block_id}:${index}`}
                link={link}
                marks={marks}
                palette={palette}
                to={paths.statement(link.block_id)}
              />
            ))}
          </ul>
        )}
        {decision.asks.map((ask) => (
          <Link
            className={`block text-sm ${tones.link}`}
            key={ask}
            to={paths.ask(ask)}
            {...marks?.pageChrome}
          >
            {DECISION_LINK_WORDS.the_ask}
          </Link>
        ))}
        {state === undefined ? null : (
          <p
            className={`text-xs font-semibold ${tones.meta}`}
            data-decision-state=""
            {...marks?.pageChrome}
          >
            {state.name === undefined ? (
              state.words
            ) : marks === undefined ? (
              `${state.words} ${state.name}`
            ) : (
              <>
                {`${state.words} `}
                <span {...marks.authorText}>{state.name}</span>
              </>
            )}
          </p>
        )}
        {acts && (keep !== undefined || change !== undefined) ? (
          <DecisionActs
            change={change}
            issueKey={issueKey}
            keep={keep}
            marks={marks}
            name={heading.words}
            palette={palette}
          />
        ) : null}
      </div>
    </div>
  );
}

/** Keep and Change, and the form Change opens. */
function DecisionActs({
  change,
  issueKey,
  keep,
  marks,
  name,
  palette,
}: {
  change: CardChangeAct | undefined;
  issueKey: string | undefined;
  keep: CardKeepAct | undefined;
  marks: DecisionCardProps["marks"];
  /** The decision's heading as plain words, which a stopped Keep and the Change form are named
   *  after (`decision-heading-words`). */
  name: string;
  palette: CardPalette;
}): ReactNode {
  const [changing, setChanging] = useState(false);
  const { stopped } = DECISION_ACT_WORDS;
  const tones = TONES[palette];
  return (
    <div className="space-y-2">
      <div className="flex gap-2">
        {keep === undefined ? null : (
          <button
            className={tones.secondary}
            disabled={keep.pending}
            onClick={keep.keep}
            type="button"
            {...marks?.pageChrome}
          >
            {DECISION_ACT_WORDS.keep}
          </button>
        )}
        {change === undefined ? null : (
          <button
            aria-expanded={changing}
            className={tones.secondary}
            onClick={() => setChanging(!changing)}
            type="button"
            {...marks?.pageChrome}
          >
            {DECISION_ACT_WORDS.change}
          </button>
        )}
      </div>
      {keep === undefined || keep.error === null ? null : (
        <p className={`text-sm ${tones.failed}`} role="alert" {...marks?.pageChrome}>
          {/* A Keep the reader stopped may have been made, and Keep is a press of its own,
              under a key of its own. */}
          {keep.error instanceof SendStoppedError
            ? stoppedWriteNote(
                `${stopped.keep.before}${name}${stopped.keep.after}`,
                stopped.keep_done,
                { anew: stopped.keep_anew }
              )
            : keep.error.message}
        </p>
      )}
      {changing && change !== undefined ? (
        <ChangeForm
          change={change}
          issueKey={issueKey}
          marks={marks}
          name={name}
          onDone={() => setChanging(false)}
          palette={palette}
        />
      ) : null}
    </div>
  );
}

/** The change itself: the words it is anchored on, the reader's own, and Send change. The
 *  quoted words and the shared box are not the card's, so with `marks` the form wears the mark
 *  of a value and its actions the card's own. */
function ChangeForm({
  change,
  issueKey,
  marks,
  name,
  onDone,
  palette,
}: {
  change: CardChangeAct;
  issueKey: string | undefined;
  marks: DecisionCardProps["marks"];
  /** The decision's heading as plain words, which names the form. */
  name: string;
  onDone: () => void;
  palette: CardPalette;
}): ReactNode {
  const [body, setBody] = useState("");
  const { stopped } = DECISION_ACT_WORDS;
  const tones = TONES[palette];
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (body.trim() === "" || change.pending) return;
    change.change(body, () => {
      setBody("");
      onDone();
    });
  };
  return (
    <form
      // A form's name holds no markup: the reading list's `decision-heading-words`.
      aria-label={`${DECISION_ACT_WORDS.change} ${name}`}
      className="space-y-2"
      onSubmit={submit}
      {...marks?.authorText}
    >
      <blockquote className={`border-l-2 pl-2 text-sm ${tones.quote}`} data-testid="decision-quote">
        {/* The decision's own words, as they are: the reading list's `anchor-quote`. */}
        {change.quote}
      </blockquote>
      <MentionField
        body={body}
        fieldClassName={tones.field}
        hintClassName={tones.hint}
        issueKey={issueKey}
        label={DECISION_ACT_WORDS.your_change}
        labelClassName={`block text-sm ${tones.ink}`}
        onChange={(next) => setBody(next.body)}
        rows={3}
        writing={dispatchWritingBox("decisions-change", "comment")}
      />
      {change.error === null ? null : (
        <p className={`text-sm ${tones.failed}`} role="alert">
          {/* A change the reader stopped may have been posted, and Send change is a press of
              its own, under a key of its own. */}
          {change.error instanceof SendStoppedError
            ? stoppedWriteNote(stopped.change, stopped.change_done, { anew: stopped.change_anew })
            : change.error.message}
        </p>
      )}
      <div className="flex gap-2">
        <button
          className={tones.filled}
          disabled={change.pending || body.trim() === ""}
          type="submit"
          {...marks?.pageChrome}
        >
          {DECISION_ACT_WORDS.send_change}
        </button>
        <button className={tones.secondary} onClick={onDone} type="button" {...marks?.pageChrome}>
          {DECISION_ACT_WORDS.cancel}
        </button>
      </div>
    </form>
  );
}
