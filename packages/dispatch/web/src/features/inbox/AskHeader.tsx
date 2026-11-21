import type { ReactNode } from "react";

import type { Ask, AskResolution, AskUrgency, Comment } from "../../api/types";
import { TruncatedText } from "../../components/TruncatedText";
import { useTooltips } from "../../components/tooltips";
import {
  askBlockPill,
  badgeBlocking,
  badgeHigh,
  badgeLow,
  badgeMed,
  calloutSuccessTimestampText,
  pageFailedText,
  pageLabelChip,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
  pageStateText,
  surfaceBg,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AgentName, useAgentText } from "../refs/AgentName";
import { askResolutionBadge } from "../refs/actor";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildDispatchReference, type DocumentOwner, itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { AskAnchorHeader } from "./AskAnchorLink";
import { askTurnHolder } from "./ask-turn";
import { askUrgency, URGENCY_LABELS } from "./ask-urgency";

/**
 * Where a header is drawn. Each place keeps its own grammar and tone, and every one of them
 * reads the same six fields from the same ask: `card` is the Inbox card (both variants),
 * `answered` and `resolved` its records (`AskCompletionCard`), `block` the decision block's own
 * portalled header (`features/doc/AskBlockCard.tsx`).
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
  /** The indexed ask row. A decision block drawn from a past version, or copied from another
   *  document, has none and shows only the fields its own attributes hold. */
  ask?: Ask;
  /** What the hosting decision block's own attributes say, where the header is a block's. */
  block?: AskHeaderBlock;
  /** The decision block's footer draws a card of its own, and that card's header carries the
   *  provenance; the block's own header then draws neither it nor the state the card records. */
  carded?: boolean;
  /** What the host draws between its anchor and its provenance line: its question, and whatever
   *  else is that place's alone. A block's header has none - the editor holds the question. */
  children?: ReactNode;
  /** A control the host keeps around the asker's name, drawn for a session asker alone:
   *  `AskCard`'s session-handles disclosure. */
  control?: (name: ReactNode) => ReactNode;
  /** The card is hosted inside its own decision block, which already draws the frame, the
   *  urgency mark and the anchor. */
  inBlock?: boolean;
  /** The ask's newest reply, which decides whose turn it is. */
  lastReply?: Pick<Comment, "author">;
  /** The host's own parts at the head of the provenance line: the block's `Decision`. */
  leading?: ReactNode;
  /** The project document that owns an ask with no issue, so the header can name the ask's
   *  `dispatch://` reference (only Inbox rows carry `document` themselves). */
  owner?: DocumentOwner;
  place: AskHeaderPlace;
  /** The host's own end of the provenance line: the answered and resolved records' own
   *  sentences, `AskCard`'s session handles, the block's `Copied from`. */
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
 * Each colour the card's header draws inside `NoTooltips` (`components/tooltips.tsx`), the new
 * issue page's, by the role `main`'s colour has, as `AskCard`'s own elements draw theirs: the
 * provenance line is the quiet words that qualify the question, whose turn it is the muted grey
 * a reader acts on, the state label the page's label chip, and the urgency notch stands on the
 * page's paper in the colour of its level (`PAGE_URGENCY_NOTCH_TEXT`).
 *
 * They stand in this table, not beside `main`'s classes, because `theme/text-on-background.test.ts`
 * pairs each text class with the nearest enclosing ground of the same file across both branches
 * of a ternary: a page colour written beside `main`'s would pair `main`'s words with the page's
 * paper and the page's words with `main`'s card.
 */
const ON_PAGE = {
  muted: pageMutedText,
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

/** Whether the header draws its urgency notch, which hangs over the top edge of the card's own
 *  frame: that frame must be positioned and leave a row of room above it. A card inside a
 *  decision block draws none - the block's header has the urgency pill. */
export function hasUrgencyNotch(ask: Pick<Ask, "urgency">, inBlock: boolean): boolean {
  return !inBlock && (ask.urgency === "blocking" || ask.urgency === "high");
}

/** The asker's name, as the one naming rule spells it, and the tooltip the host puts on it. */
interface AskHeaderAsker {
  readonly label: string;
  readonly title: string;
}

/**
 * One ask's header, wherever the ask is drawn: its urgency mark, its anchor, who asked, when,
 * the control that copies its `dispatch://` reference, and the label naming where it stands.
 * Every place reads those six fields from the same ask, so the Inbox card, the answered record,
 * the resolved record and a document's decision block cannot disagree about them.
 *
 * Each place keeps its own layout. The anchor belongs above the host's question and the
 * provenance line below it (chrome never sits between a reader and the primary content), so the
 * host passes what it draws between them as `children`; a block has no content of its own, and
 * its header is all it draws. The parts that are one place's alone - `AskCard`'s session
 * handles, the records' own sentences, the block's `Decision`, "Answered by" and `Copied from` -
 * come in through `leading` and `trailing`.
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
      return "Open";
    case "answered":
      return "Answered";
    case "resolved":
      return resolution === undefined ? "Resolved" : askResolutionBadge(resolution);
    default:
      return "Closed";
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
  readonly stateLabel: ReactNode;
  readonly urgencyPill: ReactNode;
}

/** `tooltips` is false inside `NoTooltips`, where the notch and the state label draw in the new
 *  issue page's colours (`ON_PAGE`). */
function askHeaderFacts(
  { ask, block, carded = false, inBlock = false, owner }: AskHeaderProps,
  asker: AskHeaderAsker | undefined,
  tooltips: boolean
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
      <span className={`${PILL_CLASS} ${badgeBlocking.bg} ${badgeBlocking.text}`}>Malformed</span>
    ) : (
      <span
        className={`${PILL_CLASS} ${askBlockPill[block.urgency].bg} ${askBlockPill[block.urgency].text}`}
        data-dispatch-ask-pill={block.urgency}
      >
        {URGENCY_LABELS[block.urgency]}
      </span>
    );
  const notch =
    ask === undefined || !hasUrgencyNotch(ask, inBlock) ? null : (
      <span
        aria-hidden="true"
        className={`absolute -top-2 left-3 rounded px-1.5 text-[10px] leading-4 font-semibold tracking-wide uppercase ${tooltips ? `${surfaceBg} ${URGENCY_NOTCH_TEXT[askUrgency(ask.urgency)]}` : `${ON_PAGE.notch} ${PAGE_URGENCY_NOTCH_TEXT[askUrgency(ask.urgency)]}`}`}
        data-ask-urgency-notch=""
      >
        {URGENCY_LABELS[askUrgency(ask.urgency)].toUpperCase()}
      </span>
    );
  // A block reads its own attributes alone, so a later withdrawal never shows on a saved version;
  // every other place reads the ask row, whose resolution names how a resolved ask ended.
  const resolution = block === undefined ? ask?.resolution : undefined;
  const stateLabel =
    state === undefined || carded || block?.malformed === true ? null : (
      <span
        className={`inline-block rounded-full px-2.5 py-1 text-xs font-medium ${tooltips ? `${badgeLow.bg} ${badgeLow.text}` : ON_PAGE.state}`}
        data-ask-state-label=""
        // The handle the rows that read how an ask ended have always found its badge by.
        data-testid={
          state === "resolved" && resolution !== undefined ? "ask-resolution-badge" : undefined
        }
      >
        {askStateLabel(state, resolution)}
      </span>
    );

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

/** The parts every place draws, in the order every place draws them: the urgency, the place's
 *  own `leading` part, the state label and the place's provenance. Each place draws its copy
 *  control (`facts.copy`) and its `trailing` parts after them, in the boxes its layout needs.
 *  A place that wraps the urgency, its `leading` part and the label as one group names that
 *  group's box as `lead`. */
function AskHeaderRow({
  facts,
  lead,
  leading,
  provenance,
}: {
  facts: AskHeaderFacts;
  lead?: string;
  leading: ReactNode;
  provenance: ReactNode;
}): ReactNode {
  const opening = (
    <>
      {facts.urgencyPill}
      {leading}
      {facts.stateLabel}
    </>
  );
  return (
    <>
      {lead === undefined ? opening : <span className={lead}>{opening}</span>}
      {provenance}
    </>
  );
}

type PlaceProps = AskHeaderProps & {
  asker: AskHeaderAsker | undefined;
  facts: AskHeaderFacts;
  /** False inside `NoTooltips` (`components/tooltips.tsx`). */
  tooltips: boolean;
};

function DrawnAskHeader(props: AskHeaderProps & { asker: AskHeaderAsker | undefined }): ReactNode {
  const tooltips = useTooltips();
  const facts = askHeaderFacts(props, props.asker, tooltips);
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
function BlockAskHeader({ ask, asker, facts, leading, trailing }: PlaceProps): ReactNode {
  const author = ask?.author;
  return (
    <header
      className={`@container mb-3 flex flex-wrap items-center gap-x-2 gap-y-1 text-sm ${textMutedOnSurfaceMuted}`}
      {...facts.attributes}
    >
      <span className="flex min-w-0 items-center gap-x-2 whitespace-nowrap @max-ask-block-line:contents">
        <AskHeaderRow
          facts={facts}
          lead="flex shrink-0 items-center gap-x-2 @max-ask-block-line:flex-1 @max-ask-block-line:flex-wrap @max-ask-block-line:gap-y-1"
          leading={leading}
          provenance={
            !facts.provenance || author === undefined ? null : (
              <span
                className="flex min-w-0 items-center gap-x-2 @max-ask-block-line:order-1 @max-ask-block-line:basis-full"
                data-dispatch-ask-asked=""
              >
                {asker === undefined ? (
                  <span className="shrink-0">Asked</span>
                ) : (
                  <>
                    <TruncatedText className="min-w-0" title={asker.title}>
                      Asked by <AgentName actor={author} inline />
                    </TruncatedText>
                    <span className="shrink-0" aria-hidden="true">
                      ·
                    </span>
                  </>
                )}
                <span className="shrink-0">{facts.askedAt}</span>
              </span>
            )
          }
        />
        {facts.copy("@max-ask-block-line:self-start")}
      </span>
      {trailing}
    </header>
  );
}

/** The Inbox card, in both variants: whose turn it is, who asked and when, in one group whose
 *  names truncate. Inside `NoTooltips` no title gives back what an ellipsis cut, and a name and
 *  an instant are values, which wrap there: each name breaks anywhere inside its own part of the
 *  group, which still holds its one line, so no separator opens a line there either. */
function CardAskHeader({
  ask,
  asker,
  children,
  control,
  facts,
  inBlock = false,
  lastReply,
  leading,
  tooltips,
  trailing,
}: PlaceProps): ReactNode {
  const author = ask?.author;
  const session = author?.kind === "session" ? author : undefined;
  const turnHolder = ask === undefined ? null : askTurnHolder(ask, lastReply);
  // The asker's name, cut with its tooltip where one is drawn; inside `NoTooltips`, where no
  // tooltip gives back what an ellipsis cut, it is a value that breaks anywhere instead.
  const askerName = (name: ReactNode) =>
    tooltips ? (
      <TruncatedText className="min-w-0" title={asker?.title ?? "asked"}>
        {name}
      </TruncatedText>
    ) : (
      <span className="min-w-0 [overflow-wrap:anywhere]">{name}</span>
    );
  return (
    <div {...facts.attributes}>
      {facts.notch}
      {ask === undefined ? null : (
        <AskAnchorHeader ask={ask} inBlock={inBlock} quoteSpacing="roomy" tone="surface" />
      )}
      {children}
      <div
        className={`@container mt-1.5 flex flex-wrap items-center gap-x-1.5 text-xs ${tooltips ? textMutedOnSurface : ON_PAGE.quiet}`}
        data-ask-card-row=""
      >
        {/* The label and the provenance are one group whose names truncate, and the copy
            control stands beside the group, so the control stays on the label's line at every
            width. A separator that wrapped left a line opening - or ending - on a bare "·", and
            the agents are the only parts that can give up width ("Waiting on you" never does).
            Every choice below reads the row's own width (`@container`), never the viewport: one
            card stands in the Inbox, its reading pane, a margin, a phone's review sheet and a
            decision block, and a reader narrows the pane and the margin. So each layout holds
            the copy control and the live link at both of their sizes, 32 px and the 44 px touch
            boxes every button is below 1280 px (`styles.css`).
            From 27.5rem of row (`ask-card-line`, named in `styles.css`'s `@theme` with the two
            other widths) the group is one line. An agent's turn gives up its width faster
            than the asker (`shrink-[1.5]`), so the asker's live link and time keep clear of each
            other down to that width with touch boxes, where "Waiting on you" beside the asker
            needs 436 px. From 19.5rem (`ask-card-two-lines`) to 27.5rem (the 323 px of a margin
            at its default width) the group breaks in two beside the control: the label, as tall
            as the line's text, and the turn share the first line, and the asker and time take
            the next, with no separator - two lines no taller than the control's 32 px box, so
            the card is no taller than it was before it drew a label. An agent's turn there gives
            up all of its width, `flex-1` from a zero basis, so its name truncates instead of
            pushing what follows it off the line. Below 19.5rem (a phone's 306 px, the narrowest
            margin and reading pane) the group gives way to one wrapping line: the control ends
            the first line, and the asker's line is the card's whole width, which is what reads a
            name whole beside its live link (`e2e/agent-name.e2e.ts`).
            `e2e/ask-header-sweep.e2e.ts` sweeps the row from 216 px up at both control sizes,
            and holds the control to the label's line in all three layouts at the widths a reader
            meets them (the Inbox at 1280 px, the margin, a phone), reading the line and the
            asker by their own marks (`data-ask-card-row`, `data-ask-card-asked`). */}
        <span className="flex min-w-0 items-center gap-x-1.5 @max-ask-card-two-lines:flex-wrap">
          <span
            className={`flex min-w-0 items-center gap-x-1.5 ${tooltips ? "whitespace-nowrap " : ""}@max-ask-card-line:flex-wrap @max-ask-card-line:[&>[data-ask-state-label]]:py-0 @max-ask-card-two-lines:contents`}
          >
            <AskHeaderRow
              facts={facts}
              leading={leading}
              provenance={
                !facts.provenance || ask === undefined || author === undefined ? null : (
                  <>
                    {turnHolder === null ? null : (
                      <span
                        className={`${turnHolder === "you" ? "shrink-0" : `min-w-0 shrink-[1.5] ${tooltips ? "truncate" : "[overflow-wrap:anywhere]"} @max-ask-card-line:flex-1`} font-semibold ${tooltips ? textSecondaryOnSurface : ON_PAGE.muted}`}
                        data-testid={`turn-${ask.id}`}
                      >
                        Waiting on{" "}
                        {turnHolder === "you" ? "you" : <AgentName actor={turnHolder} inline />}
                      </span>
                    )}
                    {turnHolder === null ? null : (
                      <span className="shrink-0 @max-ask-card-line:hidden" aria-hidden="true">
                        ·
                      </span>
                    )}
                    <span
                      className="flex min-w-0 items-center gap-x-1.5 @max-ask-card-line:basis-full @max-ask-card-two-lines:order-1"
                      data-ask-card-asked=""
                    >
                      {session === undefined || control === undefined ? (
                        askerName(asker === undefined ? "asked" : <AgentName actor={author} />)
                      ) : (
                        // The asker's control keeps its name and opens the session's copy
                        // handles; the component's live link sits beside it, outside the
                        // control. `whitespace-pre` keeps the space before an attribution
                        // suffix, which a flex item would drop; inside `NoTooltips`
                        // `whitespace-pre-wrap` keeps it too, and lets the name wrap.
                        <AgentName
                          actor={session}
                          className={`flex min-w-0 items-center ${tooltips ? "whitespace-pre" : "whitespace-pre-wrap"}`}
                          control={(name) => control(askerName(name))}
                          kind="control"
                        />
                      )}
                      <span className="shrink-0" aria-hidden="true">
                        ·
                      </span>
                      {facts.askedAt}
                    </span>
                  </>
                )
              }
            />
          </span>
          {facts.copy("@max-ask-card-two-lines:ml-auto")}
        </span>
        {trailing}
      </div>
    </div>
  );
}

/** The answered and resolved records (`AskCompletionCard`): one sentence of provenance, in the
 *  tone of the frame it stands in, with the copy control at the end of its first line. The
 *  sentence wraps as text does, in a box of its own beside the control, so the control stays
 *  on the label's line at every width rather than following the time onto whichever line the
 *  time wraps to. */
function RecordAskHeader({
  ask,
  asker,
  children,
  facts,
  inBlock = false,
  leading,
  place,
  trailing,
}: PlaceProps): ReactNode {
  const author = ask?.author;
  const answered = place === "answered";
  return (
    <div {...facts.attributes}>
      {facts.notch}
      {ask === undefined ? null : (
        <AskAnchorHeader
          ask={ask}
          inBlock={inBlock}
          quoteSpacing="compact"
          tone={answered ? "success" : "surface"}
        />
      )}
      {children}
      <div
        className={`mt-2 flex items-start gap-x-1.5 text-xs ${answered ? calloutSuccessTimestampText : textMutedOnSurface}`}
        data-ask-record-row=""
      >
        <span className="min-w-0 [&>[data-ask-state-label]]:mr-1.5">
          <AskHeaderRow
            facts={facts}
            leading={leading}
            provenance={
              !facts.provenance || author === undefined ? null : (
                <>
                  {asker === undefined ? null : (
                    <>
                      <AgentName actor={author} inline />
                      {" · "}
                    </>
                  )}
                  Asked {facts.askedAt}
                </>
              )
            }
          />
          {trailing}
        </span>
        {facts.copy("shrink-0")}
      </div>
    </div>
  );
}
