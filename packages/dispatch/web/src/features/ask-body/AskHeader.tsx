import type { ReactNode } from "react";

import type { Ask, AskResolution, AskUrgency } from "../../api/types";
import { Dot } from "../../components/RowMarks";
import { TruncatedText } from "../../components/TruncatedText";
import { useTooltips } from "../../components/tooltips";
import {
  askBlockPill,
  badgeBlocking,
  badgeHigh,
  badgeLow,
  badgeMed,
  pageFailedText,
  pageLabelChip,
  pagePaperBg,
  pageQuietText,
  pageStateText,
  surfaceBg,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
} from "../../theme/classes";
import type { AgentNames } from "../conversation/CachedAuthor";
import { askUrgency } from "../inbox/ask-urgency";
import { AgentName, useAgentText } from "../refs/AgentName";
import { askResolutionBadge } from "../refs/actor";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildDispatchReference, type DocumentOwner, itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { AskAnchorHeader } from "./AskAnchorLink";
import { AskAgent } from "./ask-agent";
import { ASK_LINE_WORDS, ASK_RECORD_WORDS, ASK_SHARED_WORDS } from "./ask-body-words";
import { type AskPalette, authored, ownPart, ownWords, type TextMarks } from "./ask-text";

/**
 * Where a header is drawn. Each place keeps its own layout, and every one of them reads the same
 * six fields from the same ask: `card` is the ask body's first line, which every surface draws
 * an open ask with (`AskBody`, inside `inbox/AskCard.tsx`'s card in both variants, a decision
 * block's footer and Dashboard), `answered` and `resolved` its records (`AskCompletionCard`),
 * `block` the decision block's own portalled header (`features/doc/AskBlockCard.tsx`).
 */
export type AskHeaderPlace = "answered" | "block" | "card" | "resolved";

/** What a decision block's own attributes say about its ask (`askBlockFacts`): what a block with
 *  no ask row - a past version, a block copied from another document - has to draw from. */
export interface AskHeaderBlock {
  /** The block cannot be answered as written, so its urgency mark gives way to `Malformed`. */
  readonly malformed: boolean;
  readonly state: string;
  readonly urgency: AskUrgency;
}

export interface AskHeaderProps {
  /** How the header names agents (`ask-agent.tsx`): `live` on today's surfaces and in the Inbox,
   *  as the ask body's host says (`AskBodyProps.agentNames`). */
  agentNames?: AgentNames;
  /** The indexed ask row. A decision block drawn from a past version, or copied from another
   *  document, has none and shows only the fields its own attributes hold. */
  ask?: Ask;
  /** What the hosting decision block's own attributes say, where the header is a block's. */
  block?: AskHeaderBlock;
  /** The decision block's footer draws a card of its own, and that card's header carries the
   *  provenance; the block's own header then draws neither it nor the state the card records. */
  carded?: boolean;
  /** What the host draws inside the header. On the ask body's first line, the question's words,
   *  which the line's end stands beside; on a record, everything between its anchor and its
   *  line. A block's header has none - the editor holds the question. */
  children?: ReactNode;
  /** A control the host keeps around the asker's name, drawn for a session asker alone:
   *  `AskCard`'s session-handles disclosure. */
  control?: (name: ReactNode) => ReactNode;
  /** The header is drawn inside its own decision block, which already draws the frame, the
   *  urgency mark, the question and the kind. */
  inBlock?: boolean;
  /** The classes of the state label, where the host draws in a palette of its own: a record's
   *  (`AskCompletionCard`'s `TONES`). */
  labelTone?: string;
  /** The host's own parts at the head of the line: the first line's approval mark, urgency word
   *  and kind, or the block's `Decision`. */
  leading?: ReactNode;
  /** The classes of a record's line, in the palette of the record that hosts it. */
  lineTone?: string;
  /** The marks of a host that divides its own words from an author's (`AskBodyProps.marks`):
   *  each word of the header's own, and each dot, wears the mark of the body's words, and the
   *  asker's cached name the mark of a value. */
  marks?: TextMarks;
  /** The project document that owns an ask with no issue, so the header can name the ask's
   *  `dispatch://` reference (only Inbox rows carry `document` themselves). */
  owner?: DocumentOwner;
  /** The palette a record's anchor draws in (`AskPalette`). */
  palette?: AskPalette;
  place: AskHeaderPlace;
  /** The host's own end of the line: the answered and resolved records' own sentences, the
   *  block's `Copied from`. */
  trailing?: ReactNode;
}

const PILL_CLASS = "rounded-full px-2 py-0.5 text-[10px] font-semibold tracking-wide uppercase";

/** The urgency notch's text colour, matching the urgency badge of the same level. */
const URGENCY_NOTCH_TEXT: Record<AskUrgency, string> = {
  blocking: badgeBlocking.text,
  high: badgeHigh.text,
  low: badgeLow.text,
  med: badgeMed.text,
};

/**
 * Each colour the header draws inside `NoTooltips` (`components/tooltips.tsx`), the new issue
 * page's, by the role `main`'s colour has, as the ask body's own elements draw theirs: the first
 * line is the quiet words that qualify the question, the state label the page's label chip, and
 * the urgency notch stands on the page's paper in the colour of its level
 * (`PAGE_URGENCY_NOTCH_TEXT`).
 *
 * They stand in this table, not beside `main`'s classes, because `theme/text-on-background.test.ts`
 * pairs each text class with the nearest enclosing ground of the same file across both branches
 * of a ternary: a page colour written beside `main`'s would pair `main`'s words with the page's
 * paper and the page's words with `main`'s card.
 */
const ON_PAGE = {
  notch: pagePaperBg,
  quiet: pageQuietText,
  state: pageLabelChip,
} as const;

/** The urgency notch's words inside `NoTooltips`, matching `AskCard`'s `PAGE_URGENCY_ACCENT` on
 *  the page's paper. */
const PAGE_URGENCY_NOTCH_TEXT: Record<AskUrgency, string> = {
  blocking: pageFailedText,
  high: pageStateText.waiting,
  low: pageQuietText,
  med: "text-page-question",
};

/** Whether an ask draws its urgency notch, which hangs over the top edge of its card's own
 *  frame: that frame must be positioned and leave a row of room above it. A card inside a
 *  decision block draws none - the block's header has the urgency pill. */
export function hasUrgencyNotch(ask: Pick<Ask, "urgency">, inBlock: boolean): boolean {
  return !inBlock && (ask.urgency === "blocking" || ask.urgency === "high");
}

/** The urgency mark a card's frame wears (`hasUrgencyNotch`): the open card's (`AskCard`) and
 *  each record's (`AskCompletionCard`), drawn as a child of the positioned frame it hangs over. */
export function AskUrgencyNotch({
  ask,
  inBlock,
}: {
  ask: Pick<Ask, "urgency">;
  inBlock: boolean;
}): ReactNode {
  const tooltips = useTooltips();
  if (!hasUrgencyNotch(ask, inBlock)) return null;
  const urgency = askUrgency(ask.urgency);
  return (
    <span
      aria-hidden="true"
      className={`absolute -top-2 left-3 rounded px-1.5 text-[10px] leading-4 font-semibold tracking-wide uppercase ${tooltips ? `${surfaceBg} ${URGENCY_NOTCH_TEXT[urgency]}` : `${ON_PAGE.notch} ${PAGE_URGENCY_NOTCH_TEXT[urgency]}`}`}
      data-ask-urgency-notch=""
    >
      {ASK_SHARED_WORDS.urgency[urgency].toUpperCase()}
    </span>
  );
}

/** The asker's name, as the one naming rule spells it, and the tooltip the host puts on it. */
interface AskHeaderAsker {
  readonly label: string;
  readonly title: string;
}

/**
 * One ask's header, wherever the ask is drawn: its urgency, who asked, when, the control that
 * copies its `dispatch://` reference, and the label naming where it stands, beside the anchor
 * the host draws above it (`AskAnchorHeader`, the same component at every place). Every place
 * reads those fields from the same ask, so the ask body's first line, the answered record, the
 * resolved record and a document's decision block cannot disagree about them.
 *
 * Each place keeps its own layout. The ask body's first line is root R34's row
 * (dispatch://INFRA-5702/spec#b-r-ask-body): the question's words, which the host passes as
 * `children`, and at the right end of their first line the kind, who asked and when, then the
 * state label and the copy control; in a body narrower than 32rem that end stands under the
 * words. A record draws its anchor and the host's parts (`children`) above its one line,
 * "Asked … · Answered by …", which the label opens and the copy control ends. A block has no
 * content of its own, and its header is all it draws. The parts that are one place's alone - the
 * first line's kind, the records' own sentences, the block's `Decision`, "Answered by" and
 * `Copied from` - come in through `leading` and `trailing`.
 *
 * The state label is drawn at every place, as text, and its words are one function of where the
 * ask stands (`askStateLabel`): `Open`, `Answered`, and for a resolved ask the word naming how it
 * was resolved. A decision block reads its own attributes, which hold a state and no
 * resolution, and a block whose footer draws a card leaves the label to that card's header, so
 * one block draws one label; a malformed block, whose state cannot be read, draws none.
 * `data-ask-state` carries the state beside `data-ask-urgency`, `data-ask-asker`, `data-ask-at`
 * and `data-ask-copy`: the fields one browser row reads, with the label's own text, to hold the
 * four places to the same values. A field the place does not hold carries no attribute at all.
 *
 * Naming an asker reads the agent registry, which is a query: a header with no asker to name -
 * a decision block in a body of text, which has no ask row at all - is drawn by a surface that
 * need not have one, so only the branch that names one reads it. A block ask the server indexed
 * with no pending author has an empty login and no name to draw, and still says when it was
 * asked.
 */
export function AskHeader(props: AskHeaderProps): ReactNode {
  const { ask, carded = false } = props;
  return ask !== undefined && !carded && (ask.author.kind === "session" || ask.author.id !== "") ? (
    <NamedAskHeader {...props} ask={ask} />
  ) : (
    <DrawnAskHeader {...props} asker={undefined} />
  );
}

function NamedAskHeader(props: AskHeaderProps & { ask: Ask }): ReactNode {
  const agentText = useAgentText();
  const { author } = props.ask;
  // `author` is `label` everywhere but inside `NoTooltips`, where it is the name `AgentName`
  // draws there: a session nothing names is the page's word for an agent, never its short id.
  const label = agentText.author(author);
  return (
    <DrawnAskHeader
      {...props}
      asker={{
        label,
        // A session's tooltip also carries its full id, since the innermost title is the one a
        // browser shows, and an untitled session's name is the short form `session:<8>…` that
        // sessions started in the same minute share.
        title: author.kind === "session" ? `${label} · ${author.id}` : label,
      }}
    />
  );
}

/**
 * The words of an ask's state label, the same wherever the ask is drawn: `Open`, `Answered`, and
 * for a resolved ask the badge naming how it was resolved (`askResolutionBadge`), or `Resolved`
 * where no resolution is known - a decision block's attributes hold none. A state this build
 * does not know reads `Closed`, as a decision block has always drawn it: the ask is no longer
 * open, and the label claims no more than that.
 */
export function askStateLabel(state: string, resolution: AskResolution | undefined): string {
  switch (state) {
    case "open":
      return ASK_LINE_WORDS.state.open;
    case "answered":
      return ASK_LINE_WORDS.state.answered;
    case "resolved":
      return resolution === undefined
        ? ASK_LINE_WORDS.state.resolved
        : askResolutionBadge(resolution);
    default:
      return ASK_LINE_WORDS.state.closed;
  }
}

/**
 * The facts every place draws from the same ask, computed once: the attributes one browser row
 * reads, the copy control, the urgency mark, the state label and the time it was asked. Pure,
 * and the only place that reads `carded`, `inBlock` and the block's own fields - so a change to
 * one place below edits one branch and nothing else.
 */
interface AskHeaderFacts {
  readonly askedAt: ReactNode;
  readonly attributes: Record<string, string>;
  /** The copy control, or nothing where this header draws none, with the class the place puts
   *  on its button (`CopyRefButton`'s own `className`) to stand it where its layout needs it. */
  readonly copy: (className?: string) => ReactNode;
  readonly notch: ReactNode;
  /** Whether this header draws who asked, when, and the copy control: the card below a decision
   *  block draws all three, so the block's own header draws none of them. */
  readonly provenance: boolean;
  /** The state label's words and its attributes, which each place draws in a span of its own
   *  classes, or null where this header draws no label. */
  readonly stateLabel: {
    readonly attributes: Record<string, string>;
    readonly words: string;
  } | null;
  readonly urgencyPill: ReactNode;
}

function askHeaderFacts(
  { ask, block, carded = false, inBlock = false, marks, owner }: AskHeaderProps,
  asker: AskHeaderAsker | undefined
): AskHeaderFacts {
  const reference = ask === undefined ? undefined : itemRoute("ask", ask, owner);
  const urgency = block?.urgency ?? ask?.urgency;
  const state = block?.state ?? ask?.state;
  const provenance = ask !== undefined && !carded;
  const copied = provenance ? reference : undefined;

  const attributes: Record<string, string> = { "data-ask-header": "" };
  if (urgency !== undefined) attributes["data-ask-urgency"] = urgency;
  if (state !== undefined) attributes["data-ask-state"] = state;
  if (provenance && ask !== undefined) attributes["data-ask-at"] = ask.created_at;
  if (asker !== undefined) attributes["data-ask-asker"] = asker.label;
  if (copied !== undefined) attributes["data-ask-copy"] = buildDispatchReference(copied);

  const urgencyPill =
    block === undefined ? null : block.malformed ? (
      <span className={`${PILL_CLASS} ${badgeBlocking.bg} ${badgeBlocking.text}`}>
        {ASK_LINE_WORDS.malformed}
      </span>
    ) : (
      <span
        className={`${PILL_CLASS} ${askBlockPill[block.urgency].bg} ${askBlockPill[block.urgency].text}`}
        data-dispatch-ask-pill={block.urgency}
      >
        {ASK_SHARED_WORDS.urgency[block.urgency]}
      </span>
    );
  const notch = ask === undefined ? null : <AskUrgencyNotch ask={ask} inBlock={inBlock} />;
  // A block reads its own attributes alone, so a later withdrawal never shows on a saved version;
  // every other place reads the ask row, whose resolution names how a resolved ask ended.
  const resolution = block === undefined ? ask?.resolution : undefined;
  const labelAttributes: Record<string, string> = { "data-ask-state-label": "" };
  // The handle the rows that read how an ask ended have always found its badge by.
  if (state === "resolved" && resolution !== undefined) {
    labelAttributes["data-testid"] = "ask-resolution-badge";
  }
  const stateLabel =
    state === undefined || carded || block?.malformed === true
      ? null
      : {
          attributes: { ...labelAttributes, ...marks?.pageChrome },
          words: askStateLabel(state, resolution),
        };

  return {
    askedAt: ask === undefined ? null : <Timestamp at={ask.created_at} />,
    attributes,
    copy: (className) =>
      copied === undefined ? null : <CopyRefButton className={className} route={copied} />,
    notch,
    provenance,
    stateLabel,
    urgencyPill,
  };
}

type PlaceProps = AskHeaderProps & {
  asker: AskHeaderAsker | undefined;
  facts: AskHeaderFacts;
  /** False inside `NoTooltips` (`components/tooltips.tsx`). */
  tooltips: boolean;
};

function DrawnAskHeader(props: AskHeaderProps & { asker: AskHeaderAsker | undefined }): ReactNode {
  const tooltips = useTooltips();
  const facts = askHeaderFacts(props, props.asker);
  if (props.place === "block") {
    return <BlockAskHeader {...props} facts={facts} tooltips={tooltips} />;
  }
  if (props.place === "card") {
    return <CardAskHeader {...props} facts={facts} tooltips={tooltips} />;
  }
  return <RecordAskHeader {...props} facts={facts} tooltips={tooltips} />;
}

/** A decision block's own portalled header, with nothing of its own between the anchor and the
 *  provenance - the editor holds the question. Its layout is chosen by the header's own width
 *  (`@container`), not the viewport's: one block stands in a document's column and in the
 *  Inbox's reading pane, which a reader can narrow to 30% of its two columns at a desktop
 *  width. From 36rem of header (`ask-block-line`, named in `styles.css`'s `@theme`) it is the
 *  one wrapping row it was before it drew a label: the urgency, the block's `Decision`, the
 *  state label, who asked and when, and the copy control stand in one line that never wraps -
 *  the asker's name truncates, with the whole name in its tooltip - and the host's "Answered
 *  by" (`AskBlockCard`) follows on that row where it fits, on the next where it does not. Only
 *  the name gives up width, so a narrower line would cut it to nothing, then draw the time over
 *  the copy control and push the control out of the header. Below 36rem the line gives way to
 *  rows instead, as a phone's does: the urgency, `Decision` and the label, then the copy control
 *  at the end of their row, who asked and when on the next (`order-1`), and "Answered by" on the
 *  row after it (`order-2`). The urgency, `Decision` and the label are one group that takes the
 *  row from a zero basis and wraps in itself, so where even those three and the copy control do
 *  not fit on one line (under about 290 px of header) the label takes a second line of the group
 *  and the copy control stays at the end of the first, rather than wrapping to a line of its
 *  own. */
function BlockAskHeader({ ask, asker, facts, leading, tooltips, trailing }: PlaceProps): ReactNode {
  const author = ask?.author;
  return (
    <header
      className={`@container mb-3 flex flex-wrap items-center gap-x-2 gap-y-1 text-sm ${textMutedOnSurfaceMuted}`}
      {...facts.attributes}
    >
      <span className="flex min-w-0 items-center gap-x-2 whitespace-nowrap @max-ask-block-line:contents">
        <span className="flex shrink-0 items-center gap-x-2 @max-ask-block-line:flex-1 @max-ask-block-line:flex-wrap @max-ask-block-line:gap-y-1">
          {facts.urgencyPill}
          {leading}
          {facts.stateLabel === null ? null : (
            <span
              className={`inline-block rounded-full px-2.5 py-1 text-xs font-medium ${tooltips ? `${badgeLow.bg} ${badgeLow.text}` : ON_PAGE.state}`}
              {...facts.stateLabel.attributes}
            >
              {facts.stateLabel.words}
            </span>
          )}
        </span>
        {!facts.provenance || author === undefined ? null : (
          <span
            className="flex min-w-0 items-center gap-x-2 @max-ask-block-line:order-1 @max-ask-block-line:basis-full"
            data-dispatch-ask-asked=""
          >
            {asker === undefined ? (
              <span className="shrink-0">{ASK_RECORD_WORDS.asked}</span>
            ) : (
              <>
                <TruncatedText className="min-w-0" title={asker.title}>
                  {`${ASK_LINE_WORDS.asked_by} `}
                  <AgentName actor={author} inline />
                </TruncatedText>
                <span className="shrink-0" aria-hidden="true">
                  ·
                </span>
              </>
            )}
            <span className="shrink-0">{facts.askedAt}</span>
          </span>
        )}
        {facts.copy("@max-ask-block-line:self-start")}
      </span>
      {trailing}
    </header>
  );
}

/**
 * The ask body's first line (root R34's row, dispatch://INFRA-5702/spec#b-r-ask-body): the
 * question's words (`children`), and at the right end of their first line what the host leads
 * with (`leading`: an approval request's mark and the urgency word a caller asked for beside the
 * kind, the kind in its colour, `NodeRow.tsx`'s `RowMargin`), then who asked and when, then the
 * state label and the copy control. In a document's ask block the block's frame draws the
 * question and the kind, so the line holds the rest alone.
 *
 * Outside `NoTooltips` the end is one line, and the asker's name is what gives up width, cut
 * with its tooltip; inside it no title gives back what an ellipsis cut, and a name and an
 * instant are values, which wrap. In a body narrower than 32rem - the comments margin at its
 * default 384 px, the Inbox's drawer, a phone - the end stands under the words, in the same order
 * from the left, and each of its parts moves to a line of its own before it gives up width. The
 * rule reads the body's own box (`@container/ask-body` on the ask body's root), never the
 * window's: at 1440 px the margin's body is about 320 px wide, and beside the words there the end
 * left them some 90 px, which broke a word such as "troubleshooting" over two lines, and the
 * asker's name a few letters. Beside the words the end takes up to three quarters of the row,
 * so a body of 32rem still leaves the words a column of some 270 px. Who asked and when are one
 * part, narrow and wide (`data-ask-card-asked`): the time never opens a line of its own. Outside
 * `NoTooltips` the time is held whole and the asker's name gives up the width the time needs;
 * inside, the time is a value that breaks anywhere as the name does, and the part's one flex line
 * keeps the two together. Each word of the body's on it, and each dot between two parts, carries
 * the mark of the body's words on its own element (`AskHeaderProps.marks`); the line itself
 * carries none.
 */
function CardAskHeader({
  agentNames = "live",
  ask,
  asker,
  children,
  control,
  facts,
  leading,
  marks,
  tooltips,
}: PlaceProps): ReactNode {
  const agentText = useAgentText();
  const author = ask?.author;
  const session = author?.kind === "session" ? author : undefined;
  const dot = ownPart(marks, <Dot />);
  // The asker's name, cut with its tooltip where one is drawn, and never cut to nothing: beside
  // the words the end leaves it little room. In a narrow body, where the end stands under the
  // words and the name shares its line with the time, it wraps onto a second line before it is
  // cut, so a name two lines hold stays whole. Inside `NoTooltips`, where no tooltip gives back
  // what an ellipsis cut, it is a value that breaks anywhere instead.
  const askerName = (name: ReactNode) =>
    tooltips ? (
      <TruncatedText
        className="min-w-8 @max-[32rem]/ask-body:line-clamp-2 @max-[32rem]/ask-body:whitespace-normal @max-[32rem]/ask-body:[overflow-wrap:anywhere]"
        title={asker?.title ?? ASK_LINE_WORDS.asked}
      >
        {name}
      </TruncatedText>
    ) : (
      <span className="min-w-0 [overflow-wrap:anywhere]">{name}</span>
    );
  // Who asked: a session's name as the host's control where the host hands one (the card's
  // toggle of its copy handles), else the agent alone. The asker's control keeps its name and
  // opens what the host folds behind it; the component's live link sits beside it, outside the
  // control. `whitespace-pre` keeps the space before an attribution suffix, which a flex item
  // would drop; inside `NoTooltips` `whitespace-pre-wrap` keeps it too, and lets the name wrap.
  const named =
    author === undefined ? null : session === undefined || control === undefined ? (
      askerName(
        asker === undefined ? (
          ownWords(marks, ASK_LINE_WORDS.asked)
        ) : (
          <AskAgent actor={author} agentNames={agentNames} marks={marks} />
        )
      )
    ) : agentNames === "live" ? (
      <AgentName
        actor={session}
        className={`flex min-w-0 items-center ${tooltips ? "whitespace-pre" : "whitespace-pre-wrap"}`}
        control={(name) => control(askerName(name))}
        kind="control"
      />
    ) : (
      control(askerName(authored(marks, agentText.label(session))))
    );
  const label = facts.stateLabel;
  return (
    <div
      className="flex items-start justify-end gap-x-3 @max-[32rem]/ask-body:flex-wrap @max-[32rem]/ask-body:justify-start"
      data-ask-card-row=""
      {...facts.attributes}
    >
      {children}
      <div
        className={`flex min-w-0 max-w-[75%] items-center justify-end gap-x-1.5 text-xs leading-6 @max-[32rem]/ask-body:max-w-full @max-[32rem]/ask-body:basis-full @max-[32rem]/ask-body:flex-wrap @max-[32rem]/ask-body:justify-start ${tooltips ? `shrink-0 whitespace-nowrap ${textMutedOnSurface}` : `flex-wrap ${ON_PAGE.quiet}`}`}
        data-ask-card-end=""
      >
        {leading}
        {!facts.provenance || ask === undefined ? null : (
          <span className="flex min-w-0 items-center gap-x-1.5" data-ask-card-asked="">
            {named}
            {dot}
            <Timestamp at={ask.created_at} className={tooltips ? "shrink-0" : undefined} />
          </span>
        )}
        {/* The label and the copy control are one group that never breaks, so the control
            stays on the label's line however the end wraps under the words. */}
        {label === null && !facts.provenance ? null : (
          <span className="flex shrink-0 items-center gap-x-1.5">
            {label === null ? null : (
              <>
                {dot}
                <span
                  className={`inline-block shrink-0 rounded-full px-2 text-xs leading-5 font-medium ${tooltips ? `${badgeLow.bg} ${badgeLow.text}` : ON_PAGE.state}`}
                  {...label.attributes}
                >
                  {label.words}
                </span>
              </>
            )}
            {facts.copy("shrink-0")}
          </span>
        )}
      </div>
    </div>
  );
}

/** The answered and resolved records (`AskCompletionCard`): one sentence of provenance, in the
 *  tone of the record it stands in (`lineTone`), with the copy control at the end of its first
 *  line. The sentence wraps as text does, in a box of its own beside the control, so the control
 *  stays on the label's line at every width rather than following the time onto whichever line
 *  the time wraps to. */
function RecordAskHeader({
  agentNames = "live",
  ask,
  asker,
  children,
  facts,
  inBlock = false,
  labelTone = `${badgeLow.bg} ${badgeLow.text}`,
  lineTone = textMutedOnSurface,
  palette = "main",
  place,
  trailing,
}: PlaceProps): ReactNode {
  const author = ask?.author;
  return (
    <div {...facts.attributes}>
      {facts.notch}
      {ask === undefined ? null : (
        <AskAnchorHeader
          ask={ask}
          inBlock={inBlock}
          palette={palette}
          tone={place === "answered" ? "success" : "surface"}
        />
      )}
      {children}
      <div className={`mt-2 flex items-start gap-x-1.5 text-xs ${lineTone}`} data-ask-record-row="">
        <span className="min-w-0 [&>[data-ask-state-label]]:mr-1.5">
          {facts.stateLabel === null ? null : (
            <span
              className={`inline-block rounded-full px-2.5 py-1 text-xs font-medium ${labelTone}`}
              {...facts.stateLabel.attributes}
            >
              {facts.stateLabel.words}
            </span>
          )}
          {!facts.provenance || author === undefined ? null : (
            <>
              {asker === undefined ? null : (
                <>
                  <AskAgent actor={author} agentNames={agentNames} inline />
                  {" · "}
                </>
              )}
              {`${ASK_RECORD_WORDS.asked} `}
              {facts.askedAt}
            </>
          )}
          {trailing}
        </span>
        {facts.copy("shrink-0")}
      </div>
    </div>
  );
}
