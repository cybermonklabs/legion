import type { ReactNode } from "react";

import type { Ask, AskResolution, AskUrgency, Comment } from "../../api/types";
import { TruncatedText } from "../../components/TruncatedText";
import {
  askBlockPill,
  badgeBlocking,
  badgeHigh,
  badgeLow,
  badgeMed,
  calloutSuccessTimestampText,
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
  const label = agentText.label(author);
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
  readonly copy: ReactNode;
  readonly notch: ReactNode;
  /** Whether this header draws who asked, when, and the copy control: the card below a decision
   *  block draws all three, so the block's own header draws none of them. */
  readonly provenance: boolean;
  readonly stateLabel: ReactNode;
  readonly urgencyPill: ReactNode;
}

function askHeaderFacts(
  { ask, block, carded = false, inBlock = false, owner }: AskHeaderProps,
  asker: AskHeaderAsker | undefined
): AskHeaderFacts {
  const reference = ask === undefined ? undefined : itemRoute("ask", ask, owner);
  const urgency = block?.urgency ?? ask?.urgency;
  const state = block?.state ?? ask?.state;
  const provenance = ask !== undefined && !carded;
  const copy = provenance && reference !== undefined ? <CopyRefButton route={reference} /> : null;

  const attributes: Record<string, string> = { "data-ask-header": "" };
  if (urgency !== undefined) attributes["data-ask-urgency"] = urgency;
  if (state !== undefined) attributes["data-ask-state"] = state;
  if (provenance && ask !== undefined) attributes["data-ask-at"] = ask.created_at;
  if (asker !== undefined) attributes["data-ask-asker"] = asker.label;
  if (copy !== null && reference !== undefined) {
    attributes["data-ask-copy"] = buildDispatchReference(reference);
  }

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
        className={`absolute -top-2 left-3 rounded px-1.5 text-[10px] leading-4 font-semibold tracking-wide uppercase ${surfaceBg} ${URGENCY_NOTCH_TEXT[askUrgency(ask.urgency)]}`}
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
        className={`inline-block rounded-full px-2.5 py-1 text-xs font-medium ${badgeLow.bg} ${badgeLow.text}`}
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
    copy,
    notch,
    provenance,
    stateLabel,
    urgencyPill,
  };
}

/** The row every place draws, in the order every place draws it; what differs is the provenance
 *  each writes and the box it stands in. `line` is a place's class for one box around all of it
 *  but `trailing`, for a place whose row must not wrap before its copy control; `group` is the
 *  class of a box inside it around all of it but the copy control, for a place that breaks that
 *  part into lines of its own beside the control. */
function AskHeaderRow({
  facts,
  group,
  leading,
  line,
  provenance,
  trailing,
}: {
  facts: AskHeaderFacts;
  group?: string;
  leading: ReactNode;
  line?: string;
  provenance: ReactNode;
  trailing: ReactNode;
}): ReactNode {
  const parts = (
    <>
      {facts.urgencyPill}
      {leading}
      {facts.stateLabel}
      {provenance}
    </>
  );
  const head = (
    <>
      {group === undefined ? parts : <span className={group}>{parts}</span>}
      {facts.copy}
    </>
  );
  return (
    <>
      {line === undefined ? head : <span className={line}>{head}</span>}
      {trailing}
    </>
  );
}

type PlaceProps = AskHeaderProps & {
  asker: AskHeaderAsker | undefined;
  facts: AskHeaderFacts;
};

function DrawnAskHeader(props: AskHeaderProps & { asker: AskHeaderAsker | undefined }): ReactNode {
  const facts = askHeaderFacts(props, props.asker);
  if (props.place === "block") {
    return <BlockAskHeader {...props} facts={facts} />;
  }
  if (props.place === "card") {
    return <CardAskHeader {...props} facts={facts} />;
  }
  return <RecordAskHeader {...props} facts={facts} />;
}

/** A decision block's own portalled header, with nothing of its own between the anchor and the
 *  provenance - the editor holds the question. Its first row is the urgency, the block's
 *  `Decision`, the state label and, at that row's right end, the copy control; who asked and
 *  when takes the next row (`order-1`), the asker's name truncating with the whole name in its
 *  tooltip, and the host's "Answered by" the row after it (`AskBlockCard`). Beside the label and
 *  a phone's 44 px copy control a first row has no room for the provenance as well: sharing it,
 *  a long name, or a phone, wrapped the copy control onto a row of its own. */
function BlockAskHeader({ ask, asker, facts, leading, trailing }: PlaceProps): ReactNode {
  const author = ask?.author;
  return (
    <header
      className={`mb-3 flex flex-wrap items-center gap-x-2 gap-y-1 text-sm [&>[data-copy-ref]>button]:ml-auto ${textMutedOnSurfaceMuted}`}
      {...facts.attributes}
    >
      <AskHeaderRow
        facts={facts}
        leading={leading}
        provenance={
          !facts.provenance || author === undefined ? null : (
            <span
              className="order-1 flex min-w-0 basis-full items-center gap-x-2 whitespace-nowrap"
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
        trailing={trailing}
      />
    </header>
  );
}

/** The Inbox card, in both variants: whose turn it is, who asked and when, in one group whose
 *  names truncate. */
function CardAskHeader({
  ask,
  asker,
  children,
  control,
  facts,
  inBlock = false,
  lastReply,
  leading,
  trailing,
}: PlaceProps): ReactNode {
  const author = ask?.author;
  const session = author?.kind === "session" ? author : undefined;
  const turnHolder = ask === undefined ? null : askTurnHolder(ask, lastReply);
  return (
    <div {...facts.attributes}>
      {facts.notch}
      {ask === undefined ? null : (
        <AskAnchorHeader ask={ask} inBlock={inBlock} quoteSpacing="roomy" tone="surface" />
      )}
      {children}
      <div
        className={`@container mt-1.5 flex flex-wrap items-center gap-x-1.5 text-xs ${textMutedOnSurface}`}
      >
        <AskHeaderRow
          facts={facts}
          // The label and the provenance are one group whose names truncate, and the copy
          // control stands beside the group, so the line holding both never wraps and the
          // control stays at the first line's right end at every width the card is drawn at.
          // A separator that wrapped left a line opening - or ending - on a bare "·", and the
          // agents are the only parts that can give up width ("Waiting on you" never does).
          // Below 24rem of line (a phone, a margin) the group breaks in two: the label, as tall
          // as the line's text, and the turn share the first line, and the asker and time take
          // the next, with no separator - beside the turn, and beside the label, the asker's
          // name kept a few letters. Below `md` (a phone, its review sheet) the copy control and
          // the live link are 44 px touch boxes, so the group gives way to one wrapping line:
          // the control ends the first line, and the asker's line is the card's whole width,
          // which is what reads a name whole beside its live link (`e2e/agent-name.e2e.ts`).
          // From `md` (the 280 px margin) the control's 32 px box stands beside both lines, so
          // the card is no taller than it was before it drew a label.
          group="flex min-w-0 items-center gap-x-1.5 whitespace-nowrap @max-sm:flex-wrap @max-sm:[&>[data-ask-state-label]]:py-0 max-md:@max-sm:contents"
          leading={leading}
          line="flex min-w-0 items-center gap-x-1.5 max-md:@max-sm:flex-wrap max-md:@max-sm:[&>[data-copy-ref]>button]:ml-auto"
          provenance={
            !facts.provenance || ask === undefined || author === undefined ? null : (
              <>
                {turnHolder === null ? null : (
                  <span
                    className={`${turnHolder === "you" ? "shrink-0" : "min-w-0 truncate"} font-semibold ${textSecondaryOnSurface}`}
                    data-testid={`turn-${ask.id}`}
                  >
                    Waiting on{" "}
                    {turnHolder === "you" ? "you" : <AgentName actor={turnHolder} inline />}
                  </span>
                )}
                {turnHolder === null ? null : (
                  <span className="shrink-0 @max-sm:hidden" aria-hidden="true">
                    ·
                  </span>
                )}
                <span className="flex min-w-0 items-center gap-x-1.5 @max-sm:basis-full max-md:@max-sm:order-1">
                  {session === undefined || control === undefined ? (
                    <TruncatedText className="min-w-0" title={asker?.title ?? "asked"}>
                      {asker === undefined ? "asked" : <AgentName actor={author} />}
                    </TruncatedText>
                  ) : (
                    // The asker's control keeps its name and opens the session's copy handles;
                    // the component's live link sits beside it, outside the control.
                    // `whitespace-pre` keeps the space before an attribution suffix, which a
                    // flex item would drop.
                    <AgentName
                      actor={session}
                      className="flex min-w-0 items-center whitespace-pre"
                      control={(name) =>
                        control(
                          <TruncatedText className="min-w-0" title={asker?.title ?? "asked"}>
                            {name}
                          </TruncatedText>
                        )
                      }
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
          trailing={trailing}
        />
      </div>
    </div>
  );
}

/** The answered and resolved records (`AskCompletionCard`): one sentence of provenance, in the
 *  tone of the frame it stands in. */
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
        className={`mt-2 text-xs [&>[data-ask-state-label]]:mr-1.5 ${answered ? calloutSuccessTimestampText : textMutedOnSurface}`}
      >
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
          trailing={trailing}
        />
      </div>
    </div>
  );
}
