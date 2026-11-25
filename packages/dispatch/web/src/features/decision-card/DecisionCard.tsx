import { useQuery } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";
import { Link } from "react-router-dom";

import type { Actor, DisplayTree } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import {
  dangerText,
  decisionUnsureText,
  inputClasses,
  linkHoverText,
  linkText,
  pageKindBullet,
  pageKindTag,
  primaryButton,
  secondaryButtonCompact,
  textMutedOnSelectedCard,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useAgentText } from "../refs/AgentName";
import { blockQuery, blockText, blockTextTree } from "../refs/reference-target";
import { Timestamp } from "../refs/Timestamp";
import { wordsOf } from "../refs/tree-fragment";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import {
  DECISION_ACT_WORDS,
  DECISION_KIND_WORDS,
  DECISION_LINE_WORDS,
  DECISION_LINK_WORDS,
  DECISION_STATE_WORDS,
} from "./decision-card-words";

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
  /** The statement's own `dispatch://` address, whose read names it where `text` is absent. */
  readonly ref?: string;
  /** The statement's words, whose first line is the link, where the read of the decision holds
   *  them. */
  readonly text?: string;
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

export interface DecisionCardProps {
  /** R20's setting: on, the card draws the author and the level word the read holds. */
  readonly authorLine: boolean;
  /** Change, on a decision that waits for review; none where the caller writes none. */
  readonly change?: CardChangeAct;
  readonly decision: CardDecision;
  /** Keep, on a decision that waits for review; none where the caller writes none. */
  readonly keep?: CardKeepAct;
  readonly paths: CardPaths;
  readonly renderText: DecisionTextRenderer;
}

/** The mark between two parts of a line, which a screen reader does not read. */
function Dot(): ReactNode {
  return (
    <span aria-hidden="true" className="shrink-0">
      ·
    </span>
  );
}

/** The level word: off the setting, "Unsure" alone, for a decision its maker was unsure of or
 *  whose level is `high`; on it, the level the read holds, else "Unsure" for `unsure`. */
function levelWord(decision: CardDecision, authorLine: boolean): string | undefined {
  const level = decision.uncertainty;
  if (authorLine && level !== undefined && level !== "") return level;
  return decision.unsure || level === "high" ? DECISION_LINE_WORDS.unsure : undefined;
}

/** The words of the first block of a reading that holds any, as a row of the new issue page
 *  draws a block's line; undefined for no reading, and "" for one none of whose blocks holds a
 *  word. */
function firstBlockWords(reading: DisplayTree | undefined): string | undefined {
  const content = reading?.content;
  if (!Array.isArray(content)) return undefined;
  for (const block of content as DisplayTree[]) {
    const words = wordsOf(block);
    if (words.trim() !== "") return words;
  }
  return "";
}

/** One statement the decision shaped, as a link by its first line, with "Changed since" and
 *  "out of date" where they apply. Where the decision's read holds no words for it, which is
 *  how the server answers a tie today, its line is read from the statement's own reference read
 *  (`blockQuery`), the read every reference to it on the page shares; a statement with no
 *  words is drawn as nothing, and never by its block's id. */
function StatementLink({ link, to }: { link: CardLink; to: string }): ReactNode {
  const read = useQuery({
    ...blockQuery(link.ref),
    enabled: link.text === undefined && link.ref !== undefined,
  });
  const node = read.data?.node;
  const words =
    link.text ??
    (node === undefined ? undefined : (firstBlockWords(blockTextTree(node)) ?? blockText(node)));
  const line = (words ?? "").trim().split("\n", 1)[0]?.replace(/\s+/gu, " ").trim() ?? "";
  if (line === "") return null;
  return (
    <li>
      <Link className={`${linkText} ${linkHoverText}`} to={to}>
        {line}
      </Link>
      {link.changed_at != null && compareTimestamps(link.changed_at, link.created_at) > 0 ? (
        <span className={`ml-2 text-xs ${textMutedOnSelectedCard}`}>
          {DECISION_LINK_WORDS.changed_since}
        </span>
      ) : null}
      {link.stale ? (
        <span className={`ml-2 text-xs ${textMutedOnSelectedCard}`}>
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
 * hands it another write with no change of the card. Its one read is a statement's name, where
 * the decision's read holds none (`StatementLink`). It imports neither `DecisionActions` nor
 * `RowText` (R21), and no file under `features/issue-page/` (R22). The caller draws the frame
 * around it.
 */
export function DecisionCard({
  authorLine,
  change,
  decision,
  keep,
  paths,
  renderText,
}: DecisionCardProps): ReactNode {
  const agentText = useAgentText();
  const level = levelWord(decision, authorLine);
  const reason = decision.uncertainty_reason?.trim() ?? "";
  const author = !authorLine
    ? undefined
    : decision.decided_by === undefined
      ? DECISION_LINE_WORDS.author_unknown
      : agentText.author(decision.decided_by);
  const state = decision.replaced
    ? DECISION_STATE_WORDS.replaced
    : authorLine && decision.change !== undefined
      ? `${DECISION_STATE_WORDS.changed_by} ${agentText.author(decision.change.author)}`
      : decision.waiting_review
        ? DECISION_STATE_WORDS.waiting_review
        : decision.kept_by === undefined
          ? undefined
          : `${DECISION_STATE_WORDS.kept_by} ${agentText.author(decision.kept_by)}`;
  const text: DecisionText =
    decision.kind === "person" && decision.answer !== undefined
      ? { markdown: decision.answer }
      : { markdown: decision.text, tree: decision.text_tree };
  const acts = decision.waiting_review && !decision.replaced;
  return (
    // The bullet hangs left of the heading, and everything under it stands at the heading's left
    // edge: an 18 px column, its 7 px dot centred on the first line.
    <div className="flex gap-x-2" data-decision-card="">
      <span
        aria-hidden="true"
        className="flex h-6 w-[18px] shrink-0 items-center justify-center"
        data-decision-bullet=""
      >
        <i className={`block size-[7px] rounded-full ${pageKindBullet.decision}`} />
      </span>
      <div className="min-w-0 flex-1 space-y-2">
        <div className="flex items-start justify-end gap-x-3">
          <h3
            className={`min-w-0 flex-1 text-[15px] leading-6 font-medium ${textPrimaryOnSurface}`}
          >
            {paths.decision === undefined ? (
              decision.heading
            ) : (
              <Link className={`${linkText} ${linkHoverText}`} to={paths.decision}>
                {decision.heading}
              </Link>
            )}
          </h3>
          <p
            className={`flex min-w-0 max-w-[75%] shrink-0 items-center justify-end gap-x-1.5 text-xs leading-6 whitespace-nowrap ${textMutedOnSelectedCard}`}
          >
            <span className={`shrink-0 font-semibold ${pageKindTag.decision}`}>
              {decision.kind === "person" ? DECISION_KIND_WORDS.person : DECISION_KIND_WORDS.agent}
            </span>
            {author === undefined ? null : (
              <>
                <Dot />
                <span className="min-w-0 truncate">{author}</span>
              </>
            )}
            {decision.changed_at === null ? null : (
              <>
                <Dot />
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
              <span className={`font-semibold ${decisionUnsureText}`}>{level}</span>
            )}
            {reason === "" ? null : <span className={textSecondaryOnSurface}>{reason}</span>}
          </p>
        )}
        {decision.links.length === 0 ? null : (
          <ul className="space-y-1 text-sm" data-decision-statements="">
            {decision.links.map((link, index) => (
              <StatementLink
                // biome-ignore lint/suspicious/noArrayIndexKey: one statement can be tied twice
                key={`${link.block_id}:${index}`}
                link={link}
                to={paths.statement(link.block_id)}
              />
            ))}
          </ul>
        )}
        {decision.asks.map((ask) => (
          <Link
            className={`block text-sm ${linkText} ${linkHoverText}`}
            key={ask}
            to={paths.ask(ask)}
          >
            {DECISION_LINK_WORDS.the_ask}
          </Link>
        ))}
        {state === undefined ? null : (
          <p className={`text-xs font-semibold ${textMutedOnSelectedCard}`} data-decision-state="">
            {state}
          </p>
        )}
        {acts && (keep !== undefined || change !== undefined) ? (
          <DecisionActs change={change} heading={decision.heading} keep={keep} />
        ) : null}
      </div>
    </div>
  );
}

/** Keep and Change, and the form Change opens. */
function DecisionActs({
  change,
  heading,
  keep,
}: {
  change: CardChangeAct | undefined;
  heading: string;
  keep: CardKeepAct | undefined;
}): ReactNode {
  const [changing, setChanging] = useState(false);
  const { stopped } = DECISION_ACT_WORDS;
  return (
    <div className="space-y-2">
      <div className="flex gap-2">
        {keep === undefined ? null : (
          <button
            className={secondaryButtonCompact}
            disabled={keep.pending}
            onClick={keep.keep}
            type="button"
          >
            {DECISION_ACT_WORDS.keep}
          </button>
        )}
        {change === undefined ? null : (
          <button
            aria-expanded={changing}
            className={secondaryButtonCompact}
            onClick={() => setChanging(!changing)}
            type="button"
          >
            {DECISION_ACT_WORDS.change}
          </button>
        )}
      </div>
      {keep === undefined || keep.error === null ? null : (
        <p className={`text-sm ${dangerText}`} role="alert">
          {/* A Keep the reader stopped may have been made, and Keep is a press of its own,
              under a key of its own. */}
          {keep.error instanceof SendStoppedError
            ? stoppedWriteNote(
                `${stopped.keep.before}${heading}${stopped.keep.after}`,
                stopped.keep_done,
                { anew: stopped.keep_anew }
              )
            : keep.error.message}
        </p>
      )}
      {changing && change !== undefined ? (
        <ChangeForm change={change} heading={heading} onDone={() => setChanging(false)} />
      ) : null}
    </div>
  );
}

/** The change itself: the words it is anchored on, the reader's own, and Send change. */
function ChangeForm({
  change,
  heading,
  onDone,
}: {
  change: CardChangeAct;
  heading: string;
  onDone: () => void;
}): ReactNode {
  const [body, setBody] = useState("");
  const fieldId = useId();
  const { stopped } = DECISION_ACT_WORDS;
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
      aria-label={`${DECISION_ACT_WORDS.change} ${heading}`}
      className="space-y-2"
      onSubmit={submit}
    >
      <blockquote
        className={`border-l-2 pl-2 text-sm ${textSecondaryOnSurface}`}
        data-testid="decision-quote"
      >
        {change.quote}
      </blockquote>
      <label className={`block text-sm ${textPrimaryOnSurface}`} htmlFor={fieldId}>
        {DECISION_ACT_WORDS.your_change}
      </label>
      <textarea
        className={`block w-full rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
        id={fieldId}
        onChange={(event) => setBody(event.target.value)}
        rows={3}
        value={body}
      />
      {change.error === null ? null : (
        <p className={`text-sm ${dangerText}`} role="alert">
          {/* A change the reader stopped may have been posted, and Send change is a press of
              its own, under a key of its own. */}
          {change.error instanceof SendStoppedError
            ? stoppedWriteNote(stopped.change, stopped.change_done, { anew: stopped.change_anew })
            : change.error.message}
        </p>
      )}
      <div className="flex gap-2">
        <button
          className={primaryButton}
          disabled={change.pending || body.trim() === ""}
          type="submit"
        >
          {DECISION_ACT_WORDS.send_change}
        </button>
        <button className={secondaryButtonCompact} onClick={onDone} type="button">
          {DECISION_ACT_WORDS.cancel}
        </button>
      </div>
    </form>
  );
}
