import {
  type ClaimHolderLiveness,
  type ClaimHolding,
  claimHolds,
  claimTakeableAt,
  claimTakeableNow,
} from "@legion/contracts";
import { type ReactNode, type RefObject, useLayoutEffect, useRef, useState } from "react";

import { AGENT_LIST_REFILLING } from "../../api/client";
import type { IssueClaim } from "../../api/types";
import { pillClassName, pillToneClassName } from "../../components/Pill";
import { Tooltip, useTooltip } from "../../components/Tooltip";
import { TruncatedText } from "../../components/TruncatedText";
import { usePageWords, useTooltips } from "../../components/tooltips";
import { focusVisibleRing, pageInkText } from "../../theme/classes";
import { AgentName, useAgentText } from "../refs/AgentName";
import { type AgentRegistry, useAgentRegistry } from "../refs/agent-registry";
import { exactTimeLabel, Timestamp } from "../refs/Timestamp";

/**
 * What the registry says about a claim's holder, as `claimHolds` judges it. The registry lists
 * the unresponsive sessions beside the live ones and marks each row's state, so membership is
 * no longer the whole answer: a holder that has missed its heartbeats without ending still
 * holds the issue (`unresponsive`), and only a session the latest successful read does not list
 * at all has lapsed, which is exactly when the server lets any agent take the issue. A human's
 * claim never lapses - there is no session to end, and only a human releases or forces it.
 *
 * The current test is the whole point. A chip paints the stamped title before the registry
 * answers, so "the registry has not arrived" and "the holder is gone" look identical to a bare
 * lookup; the registry's `unknown` state (no successful read yet, or the latest read failed) is
 * never lapsed, or every claim on the page would read free for a moment, and free for good
 * whenever the listener is unreachable or refilling. So `claimHolds` gets the registry's titles
 * only once a read has landed and the latest one succeeded.
 */
export function claimHolding(
  claim: IssueClaim,
  registry: Pick<AgentRegistry, "state" | "titles">
): ClaimHolding {
  const state = registry.state(claim.actor.id);
  return claimHolds(claim, state === "unknown" ? undefined : registry.titles, {
    holder_state:
      state === "unresponsive" ? "unresponsive" : state === "not-running" ? "gone" : null,
  });
}

/**
 * The same judgement made against an issue read: the read's own verdict on the holder where it
 * gave one, and the live registry otherwise. One function, so the chip on the issue and the
 * bar's offer of that issue's agent never disagree about who is on it.
 */
export function claimHoldingFromRead(
  claim: IssueClaim,
  registry: Pick<AgentRegistry, "state" | "titles">,
  liveness: ClaimHolderLiveness | undefined
): ClaimHolding {
  return liveness?.holder_state == null
    ? claimHolding(claim, registry)
    : claimHolds(claim, registry.titles, liveness);
}

/**
 * The heartbeat an unresponsive holder was last seen at, as an RFC3339 stamp, or null when
 * nothing dates it: the issue read's own `last_seen` where it gave one, else the registry row's,
 * the order `claimTakeableAt` resolves the moment the claim comes free in.
 */
function holderLastSeen(
  liveness: ClaimHolderLiveness,
  lastSeen: number | undefined
): string | null {
  if (liveness.last_seen != null) {
    return liveness.last_seen;
  }
  return lastSeen === undefined ? null : new Date(lastSeen).toISOString();
}

/**
 * Whether another agent may take this claim now, the question the Unclaimed filter asks and the
 * chip's tone answers. It is the server's own rule (`claimBlockedBy` in
 * `internal/dispatch/api/issue_claim.go`): a holder the latest successful read does not list has
 * gone, so the claim is free; one that has only missed its heartbeats keeps the issue until
 * `claimTakeableAt` - its last registration plus the shared grace - which the server refuses a
 * take until, and grants one after, the strict comparison `claimTakeableNow` makes for both
 * callers of this rule. An unresponsive holder whose last registration nothing dates keeps the
 * claim, because the server refuses that take too.
 *
 * Takeable is not gone. Such a holder still reads "unresponsive" on the chip, still holds its
 * roles, and is still delivered to; this says only that a second agent asking for the issue now
 * would be given it, so the filter that lists pickable work must list it.
 */
export function claimIsTakeable(
  claim: IssueClaim,
  registry: Pick<AgentRegistry, "lastSeen" | "state" | "titles">,
  now: number = Date.now()
): boolean {
  const holding = claimHolding(claim, registry);
  if (holding !== "unresponsive") {
    return holding === "lapsed";
  }
  return claimTakeableNow({ holder_state: "unresponsive" }, registry.lastSeen(claim.actor.id), now);
}

/**
 * "Claimed by <holder> · <age>" wherever an issue is shown: the issue header's state row, a
 * List row, a Board card. The holder is drawn by `AgentName`, as every agent is: its current
 * registry title, else the title stamped on the claim, else `session:<8>…`, with its running
 * state and a link to its live conversation. The age keeps itself current through the shared
 * `Timestamp` tick. The help is in the shared tooltip, on hover and on keyboard focus of the
 * chip or of the link inside it, so the chip is a Tab stop: who holds the claim, named whole,
 * and since when, then what a claim is, since the chip sits among status, priority and assignee
 * and nothing else on the page defines it, and whether its session still runs. The chip carries
 * no native title, cut or not: the tooltip's first line gives the whole name. An unclaimed issue
 * renders nothing at all, so a board of unclaimed work stays quiet.
 *
 * A holder the registry no longer lists reads "· not running" in the muted tone, beside its
 * name's own not-running mark, because that claim is free for the taking and a chip that looked
 * held would hide pickable work — the same judgement the Unclaimed filter makes through
 * `claimIsTakeable`. A holder that has only missed its heartbeats reads "· unresponsive since
 * <heartbeat>", the state and the moment R5 asks the chip for, and keeps the held tone while
 * the server would still refuse a take: the chip also names the moment the issue comes free
 * (`liveness`, the issue read's own `takeable_at`), so a reader waiting on it knows how long.
 * Past that moment the chip reads "· takeable now" and wears the free tone, because the server
 * now grants that take and a held-looking chip would hide the issue again — the holder is still
 * drawn as unresponsive, since it has not ended and may yet come back. Without a `liveness` - a
 * List row, a Board card, whose rows carry no such read - the state and the heartbeat are the
 * registry's and the takeable moment is that heartbeat plus the shared grace, the same sum the
 * server uses. The judgement is remade on each render, so it follows the registry poll and the
 * issue read rather than a timer of the chip's own.
 *
 * The chip is one group: "Claimed by", the holder, the age, then what the liveness adds (lapsed,
 * not running). A session title can be any length, and no name may widen the page.
 * `pillClassName` sets `shrink-0`, and no class reliably beats it: `shrink` is in the same
 * Tailwind group, and `[flex-shrink:1]` measured a computed `flex-shrink` of 0 on the built page,
 * so the shrink is an inline style, which cannot lose whatever the stylesheet order. The chip also
 * caps itself at the row's width. Only the holder's name ellipsizes, inside `TruncatedText`; the
 * chip's tooltip, which opens with "Claimed by" and the whole name, opens over the name too.
 * "Claimed by", the age and "not running" stay whole, because a chip reading "Claimed…" would
 * say nothing. The name is an inline link, so it keeps the chip's line box at a phone width.
 *
 * `stacked` is for a host as narrow as a Board card, where one line leaves the name a few
 * pixels: the chip takes the host's whole width and writes "Claimed by" and the holder's name,
 * with the login it acts for, on as many lines as they need, and the age and the state on a line
 * under them, so no part of the name is cut at the card's edge.
 *
 * Inside `NoTooltips` (`components/tooltips.tsx`, the new issue page) the chip draws no tooltip
 * and is no Tab stop, so everything a reader there acts on is drawn instead, in that page's
 * words with their marks: both instants after their ages (`Timestamp`'s `exact`), the moment the
 * issue comes free as "free for another agent from" and the date and time, and "· running state
 * unknown" where the registry could not be read at all, which is a claim nobody has judged
 * rather than a held one. There no part is cut: the holder's name wraps rather than ellipsizes,
 * in the pill too. There `author` names the holder as that page names an author (`AgentName`'s
 * `author`), and `inRow` draws the claim as the value of a row of Details, which draws the
 * label: one sentence in the ink, its parts divided by spaces, that wraps between words and cuts
 * no word, not a pill. Outside it, neither changes anything.
 *
 * `wraps` draws the pill in that page's shape and in this page's colours: no part is cut, the
 * holder's name wraps rather than ellipsizes, and the chip takes as many lines as its column's
 * width needs. A table that wraps its titles draws its claims so (`ListingTable`'s layout
 * `wrapTitles`): the priorities page's fault lists and a priority's work table, where a
 * session's name runs as long as its writer made it and no column could hold every one on a
 * line.
 *
 * In that shape, here and inside `NoTooltips`, the chip is two parts, as the stacked chip is:
 * "Claimed by" with the holder's name, and the age with the state. Where both fit on one line
 * the chip is a pill, one separator between them, as the pill that holds its line draws it.
 * Where they do not, it is the stacked chip's shape in the pill's colours, with 8 px corners so
 * its fill holds its words: the holder's part over the age and the state, which stand on a line
 * of their own with no separator, since a "·" that starts a line separates nothing and reads as
 * a list's bullet. Inside the age and state the words are text, not one flex item each: a
 * no-break space stands before each separator and a space after it, so they break after a
 * separator and never before one. A row of Details is one block of text, where that no-break
 * space is the whole of the rule.
 *
 * Which of the two the pill draws is read off the drawn chip (`useClaimLines`): no CSS knows
 * whether a box's words took one line, and none hides only a separator that starts a line but by
 * cutting it, which the layout rule reads as content lost. The read cannot flip back and forth.
 * The pill is drawn on one line first, and is split when that takes more than a line and a half,
 * before the frame is painted; it is drawn on one line again only once the room it stands in has
 * grown past the room it was split in, and each part, on one line of its own, fits beside the
 * other with the separator. At a room that does not change it splits once at most. What it draws
 * changing - another holder, a name the registry gave, a state - draws it on one line and reads
 * it afresh.
 */
export function ClaimChip(props: ClaimChipProps): ReactNode {
  return props.claim === null ? null : <HeldClaimChip {...props} claim={props.claim} />;
}

interface ClaimChipProps {
  author?: boolean;
  claim: IssueClaim | null;
  inRow?: boolean;
  liveness?: ClaimHolderLiveness;
  /** Write the holder's name over its age and state, for a host too narrow for one line. */
  stacked?: boolean;
  wraps?: boolean;
}

/** The room across that `pill`'s host gives it: the content box of the box it stands in. */
function roomFor(pill: HTMLElement): number {
  const host = pill.parentElement;
  if (host === null) return 0;
  const style = getComputedStyle(host);
  return host.clientWidth - pixels(style.paddingLeft) - pixels(style.paddingRight);
}

/** A computed length in pixels, and 0 where none is computed (happy-dom, which lays nothing out). */
function pixels(value: string): number {
  return Number.parseFloat(value) || 0;
}

/** The room the separator takes between the holder and the age on one line: its two 4 px gaps
 *  and its dot, with the rest spare for the dot's width in any font and for rounding. */
const SEPARATOR_ROOM_PX = 24;

/**
 * Whether the pill that wraps (`on`) draws its age and state under its holder (`split`) rather
 * than beside it, read off the drawn chip (`ref`) and its two parts (`holder`, `state`), as
 * `ClaimChip` says. Read before the first paint, again before the paint after each change of
 * `split` or of what the chip draws (`drawn`), and each time the pill or its host changes size, in
 * the render that change schedules. Not `on`, or where nothing is laid out (happy-dom, which has
 * no `ResizeObserver` either), the pill is on one line.
 */
function useClaimLines(
  on: boolean,
  drawn: string
): {
  readonly holder: RefObject<HTMLSpanElement | null>;
  readonly ref: RefObject<HTMLSpanElement | null>;
  readonly split: boolean;
  readonly state: RefObject<HTMLSpanElement | null>;
} {
  const ref = useRef<HTMLSpanElement>(null);
  const holder = useRef<HTMLSpanElement>(null);
  const state = useRef<HTMLSpanElement>(null);
  const [split, setSplit] = useState(false);
  // The room the pill stood in when it was last split, which it must outgrow to be drawn on one
  // line again: at the room it split in it does not fit there.
  const splitAt = useRef(0);
  // What the chip draws changed: it is drawn on one line again, and read afresh.
  const [readOf, setReadOf] = useState(drawn);
  if (readOf !== drawn) {
    setReadOf(drawn);
    setSplit(false);
  }
  // biome-ignore lint/correctness/useExhaustiveDependencies: `drawn` is a trigger the read does not use: a change of what the chip draws is read before it is painted, not a frame later on the resize it causes.
  useLayoutEffect(() => {
    const pill = ref.current;
    if (!on || pill === null) return;
    const read = () => {
      const style = getComputedStyle(pill);
      const line = Number.parseFloat(style.lineHeight);
      if (!split) {
        const height =
          pill.getBoundingClientRect().height -
          pixels(style.paddingTop) -
          pixels(style.paddingBottom) -
          pixels(style.borderTopWidth) -
          pixels(style.borderBottomWidth);
        if (height > line * 1.5) {
          splitAt.current = roomFor(pill);
          setSplit(true);
        }
        return;
      }
      const room = roomFor(pill);
      if (room <= splitAt.current + 1) return;
      const parts = [holder.current, state.current].map((part) => part?.getBoundingClientRect());
      const edges =
        pixels(style.paddingLeft) +
        pixels(style.paddingRight) +
        pixels(style.borderLeftWidth) +
        pixels(style.borderRightWidth);
      let width = SEPARATOR_ROOM_PX + edges;
      for (const part of parts) {
        if (part === undefined || part.height > line * 1.5) return;
        width += part.width;
      }
      if (width <= room) setSplit(false);
    };
    read();
    if (typeof ResizeObserver === "undefined") return;
    const sizes = new ResizeObserver(read);
    sizes.observe(pill);
    if (pill.parentElement !== null) sizes.observe(pill.parentElement);
    return () => sizes.disconnect();
  }, [on, split, drawn]);
  return { holder, ref, split: on && split, state };
}

/** `ClaimChip` of an issue someone holds. */
function HeldClaimChip({
  author = false,
  claim,
  inRow = false,
  liveness,
  stacked = false,
  wraps = false,
}: ClaimChipProps & { claim: IssueClaim }): ReactNode {
  const registry = useAgentRegistry(claim.actor.kind === "session");
  const tooltips = useTooltips();
  const page = usePageWords();
  const text = useAgentText();
  const tooltip = useTooltip();
  const inDetails = page !== null && inRow;
  // Inside `NoTooltips`, and where the host asks it to (`wraps`), nothing of the claim stops its
  // line from wrapping, in a row of Details and in the pill alike: in the pill each part may
  // shrink, and in both a long part breaks inside itself only where no break between words is
  // left, since the holder's name and the instants are values, which wrap there. Elsewhere each
  // part stays whole, as on `main`, and the holder's name ellipsizes.
  const wrapping = page !== null || wraps;
  const part = wrapping ? "min-w-0 [overflow-wrap:anywhere]" : "shrink-0";
  // The pill in the shape that wraps, as against a row of Details and the stacked form.
  const wrapsPill = wrapping && !inDetails && !stacked;
  // The issue read judged the holder against its own record; a row without one is judged by the
  // registry, which lists every session the listener still holds, live or unresponsive, and
  // whose heartbeat then dates the wait.
  const read: ClaimHolderLiveness = liveness?.holder_state == null ? {} : liveness;
  const judged = read.holder_state == null;
  const holding = claimHoldingFromRead(claim, registry, liveness);
  const lapsed = holding === "lapsed";
  const unresponsive = holding === "unresponsive";
  const lastSeen = registry.lastSeen(claim.actor.id);
  const heard: ClaimHolderLiveness = judged ? { holder_state: "unresponsive" } : read;
  const heardAt = !unresponsive ? null : holderLastSeen(heard, lastSeen);
  const takeableAt = !unresponsive ? null : claimTakeableAt(heard, lastSeen);
  // One moment decides the tone and the wording, and it is the server's: at it the take is still
  // refused, after it granted, which is why `claimTakeableNow` compares strictly.
  const takeable = lapsed || (unresponsive && claimTakeableNow(heard, lastSeen));
  // A session claim the registry could not judge says so, rather than pass for a held one.
  const unchecked = claim.actor.kind === "session" && registry.isError;
  // Whether the pill that wraps draws its age and state under its holder, read afresh on one line
  // whenever what it draws changes: the holder, the claim's moment and the holder's state.
  const drawn = [
    text.label(claim.actor),
    claim.at,
    holding,
    takeable,
    unchecked,
    heardAt,
    takeableAt,
  ];
  const lines = useClaimLines(wrapsPill, drawn.join("\n"));
  // What the chip's tooltip says, built only where a tooltip is drawn: who holds the claim,
  // named whole, and since when, then what a claim is and whether its session still runs.
  const claimHelp = () => {
    const since = `Claimed by ${text.label(claim.actor)} · since ${new Date(claim.at).toLocaleString()}.`;
    const explains =
      "A claim marks who is implementing this issue, so two agents never take the same work.";
    const lapsedNote = lapsed ? " That session is no longer running, so anyone may claim it." : "";
    const unresponsiveNote = !unresponsive
      ? ""
      : ` That session has missed its heartbeats without ending${
          heardAt === null ? "" : `, last seen ${new Date(heardAt).toLocaleString()}`
        }. ${
          takeableAt === null
            ? "It keeps this issue; another agent may take it once the listener gives it up."
            : takeable
              ? `It kept this issue until ${new Date(takeableAt).toLocaleString()}, so another agent may take it now. It has not ended, and may yet answer.`
              : `It keeps this issue until ${new Date(takeableAt).toLocaleString()}, when another agent may take it.`
        }`;
    const uncheckedNote = unchecked
      ? ` Couldn't check whether that session is still running: ${
          registry.refilling ? AGENT_LIST_REFILLING : "Envoy listener unreachable."
        }`
      : "";
    return (
      <>
        <p>{since}</p>
        <p className="mt-1">
          {explains}
          {lapsedNote}
          {unresponsiveNote}
          {uncheckedNote}
        </p>
      </>
    );
  };
  // Where the claim's words are text - a row of Details, which is one sentence, and the age and
  // state of the pill that wraps - a no-break space stands before each separator and a space
  // after it, where the pill's gap stands: a line breaks after a separator as between any two
  // words, and never before one, so no line starts with "·". The pill that holds its line and the
  // stacked chip lay each part out as a flex item, and draw neither, as on `main`.
  const words = inDetails || wrapsPill;
  const dot = (
    <>
      {words ? "\u00a0" : null}
      <span aria-hidden="true" className={part} {...page?.pageChrome}>
        ·
      </span>
      {words ? " " : null}
    </>
  );
  // The claim's age and the holder's state, which follow the name on one line, and stand on a
  // line under it stacked.
  const state = (
    <>
      <Timestamp at={claim.at} className={part} exact />
      {lapsed ? (
        <>
          {dot}
          <span className={part} data-testid="issue-claim-lapsed" {...page?.pageChrome}>
            {page?.CONTROL_LABELS.not_running ?? "not running"}
          </span>
        </>
      ) : null}
      {unresponsive ? (
        <>
          {dot}
          <span className={part} data-testid="issue-claim-unresponsive" {...page?.pageChrome}>
            {page?.CONTROL_LABELS.unresponsive ?? "unresponsive"}
            {heardAt === null ? null : (
              <span data-testid="issue-claim-unresponsive-since">
                {` ${page?.CONTROL_LABELS.unresponsive_since ?? "since"} `}
                <Timestamp at={heardAt} exact />
              </span>
            )}
            {takeableAt === null ? null : (
              <span data-testid="issue-claim-takeable">
                {takeable ? (
                  `, ${page?.CONTROL_LABELS.takeable_now ?? "takeable now"}`
                ) : page === null ? (
                  <>
                    , takeable <Timestamp at={takeableAt} />
                  </>
                ) : (
                  <>
                    {`, ${page.CONTROL_LABELS.takeable} `}
                    {/* The moment itself, and no relative age: the words before it end in
                        "from", and a `Timestamp` would finish the sentence with "in 3 minutes"
                        or, inside the last minute of the wait, with "just now" - which reads as
                        free while the server still refuses the take. */}
                    <time className={part} dateTime={takeableAt} {...page.authorText}>
                      {exactTimeLabel(new Date(takeableAt))}
                    </time>
                  </>
                )}
              </span>
            )}
          </span>
        </>
      ) : null}
      {/* Where a tooltip carries it, the tooltip's `uncheckedNote` says it; inside `NoTooltips`
          the chip says it, because a claim nobody could judge is not a held one and a reader on
          a phone or on the keyboard has no other way to that. */}
      {unchecked && page !== null ? (
        <>
          {dot}
          <span className={part} data-testid="issue-claim-unchecked" {...page.pageChrome}>
            {page.CONTROL_LABELS.unchecked}
          </span>
        </>
      ) : null}
    </>
  );
  const tone = takeable ? "label" : "selected-label";
  // Where a tooltip is drawn the chip is a Tab stop, and the tooltip opens on its hover and its
  // keyboard focus. Inside `NoTooltips` the chip draws what the tooltip says and takes no focus.
  const focusRing = tooltips ? ` outline-none focus-visible:ring-2 ${focusVisibleRing}` : "";
  // The tooltip's content is built only while it is open: it is drawn nowhere else.
  const help =
    tooltips && tooltip.open ? (
      <Tooltip anchorRef={lines.ref} trigger={tooltip}>
        {claimHelp()}
      </Tooltip>
    ) : null;
  // What the chip's root carries in each of its forms. The Tab stop opens the tooltip that says
  // who holds the claim and what a claim is.
  const root = {
    ...(tooltips ? tooltip.props : {}),
    "data-testid": "issue-claim",
    ref: lines.ref,
    tabIndex: tooltips ? 0 : undefined,
  };
  if (stacked)
    return (
      <span
        {...root}
        className={`flex w-full min-w-0 flex-col items-start gap-0.5 rounded-lg px-2 py-1 text-xs font-medium${focusRing} ${pillToneClassName(tone)}`}
        data-claim-stacked
      >
        <span className="block w-full min-w-0 [overflow-wrap:anywhere]">
          Claimed by <AgentName actor={claim.actor} inline />
        </span>
        <span className="flex w-full min-w-0 flex-wrap items-center gap-1">{state}</span>
        {help}
      </span>
    );
  if (wrapsPill) {
    // Inside `NoTooltips` the pill's tone is the new issue page's (`pillClassName`'s `onPage`),
    // and it wraps there as where the host asks it to. Its two parts, and when it draws one under
    // the other, are the doc comment's.
    const holder = (
      <span className="min-w-0" ref={lines.holder}>
        <span {...page?.pageChrome}>{page?.PROPERTY_LABELS.claim ?? "Claimed by"}</span>{" "}
        <AgentName actor={claim.actor} author={author} inline />
      </span>
    );
    const ageAndState = (
      <span className="min-w-0" ref={lines.state}>
        {state}
      </span>
    );
    return lines.split ? (
      <span
        {...root}
        className={`inline-flex min-w-0 max-w-full flex-col items-start gap-0.5 rounded-lg px-2 py-1 text-xs font-medium [overflow-wrap:anywhere] ${pillToneClassName(tone, page !== null)}${focusRing}`}
        style={{ flexShrink: 1 }}
      >
        {holder}
        {ageAndState}
        {help}
      </span>
    ) : (
      <span
        {...root}
        className={`${pillClassName(tone, page !== null, true)} min-w-0 max-w-full gap-1${focusRing}`}
        style={{ flexShrink: 1 }}
      >
        {holder}
        <span aria-hidden="true" className={part} {...page?.pageChrome}>
          ·
        </span>
        {ageAndState}
        {help}
      </span>
    );
  }
  return (
    <span
      {...root}
      className={
        inDetails
          ? // A block of text, not a flex box, so a part that does not fit breaks across lines as
            // a sentence does rather than moving whole to a line of its own. Below 1280 px the
            // holder's name, an inline link, takes 6 px of padding above and below for a finger
            // (`styles.css`'s `.dispatch-inline-link`), which moves no line: the 28 px line keeps
            // that padded box off the letters of the lines above and below it, which text-sm's
            // own 20 px line would let it reach into. From 1280 px the link has no padding, and
            // the line is text-sm's own.
            `block min-w-0 max-w-full text-sm leading-7 [overflow-wrap:anywhere] xl:leading-5 ${pageInkText}`
          : `${pillClassName(tone)} min-w-0 max-w-full gap-1${focusRing}`
      }
      style={{ flexShrink: 1 }}
    >
      {inDetails ? null : <span className="shrink-0">Claimed by</span>}
      {inDetails ? (
        <AgentName actor={claim.actor} author={author} inline />
      ) : (
        <TruncatedText>
          <AgentName actor={claim.actor} author={author} className="min-w-0 truncate" inline />
        </TruncatedText>
      )}
      {dot}
      {state}
      {help}
    </span>
  );
}
