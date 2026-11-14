import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useLayoutEffect, useRef, useSyncExternalStore } from "react";
import { createPortal } from "react-dom";
import { Link } from "react-router-dom";

import { primarySpec } from "../../api/issue-cache";
import { issueQuery } from "../../api/queries";
import type { DisplayTree } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { LabelPill, StatusPill } from "../../components/Pill";
import {
  card,
  linkText,
  priorityBadge,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { statusText } from "../project/board-model";
import { AgentName } from "./AgentName";
import { type BlockItemKind, itemOfBlock } from "./block-reference";
import { MarkdownPreview } from "./MarkdownPreview";
import { REF_PREVIEW_CARD_ID, refPreviewCardContains } from "./ref-preview-card";
import {
  closeRefPreview,
  inHoverZone,
  type PreviewTarget,
  previewState,
  refPreview,
  subscribeToPreview,
  triggerOf,
} from "./ref-preview-state";
import {
  artifactTextQuery,
  blockText,
  blockTextTree,
  prefetchReference,
  useBlockDocument,
  useReferenceData,
  useReferencePath,
} from "./reference-target";
import {
  type DispatchReferenceRoute,
  isProjectRoute,
  parseNodeReference,
  shortForm,
} from "./routes";
import { referenceTargetKind } from "./target-id";
import { type TextSource, textSourceOf } from "./text-references";

/** The route a trigger names, read as a node's reference whatever the length of its target id: a
 *  `data-dispatch-ref`, which the dashboard writes for a route it already holds, or a Proof link
 *  mark's `data-dispatch-href`, the target its author named for the link (or the reference a
 *  picture's fallback link writes), which the card reads as the server reads a request. */
function routeOf(trigger: HTMLElement): DispatchReferenceRoute | undefined {
  const reference =
    trigger.getAttribute("data-dispatch-ref") ?? trigger.getAttribute("data-dispatch-href");
  return reference === null ? undefined : parseNodeReference(reference);
}

/** How long after the reader's last input a scroll still counts as theirs.
 *  A page that moves under a still pointer - a card above the reference settling its height, a
 *  picture landing, a long body formatting - scrolls the page under the pointer, and the
 *  browser reports that scroll with no input behind it. */
const READER_INPUT_GRACE_MS = 100;

/** `event.timeStamp` of the reader's last input that moves the pointer or the page: a pointer
 *  they moved, a wheel they turned, a key they pressed, a button they put down. Module-level
 *  beside the card's own state because the listener that judges a scroll (`RefPreviewHost`)
 *  reads it while the listeners of `attachTriggers` keep it: a boundary event is judged by
 *  whether the pointer's own position changed (`theReaderMoved`) and never by this. */
let lastInputAt = Number.NEGATIVE_INFINITY;

/** Delegated hover/focus triggers for every reference trigger in the document, present or
 * future — React-rendered links and the anchors inside Markdown bodies and the Proof editor
 * alike, which come and go outside React's knowledge. Returns the detach function. */
function attachTriggers(): () => void {
  /** The page took the hover zone out from under a pointer that has not moved, and the request
   *  was kept. The reader's next movement decides it: back in the zone it stays, anywhere else
   *  it goes. True from the start for a request this host made for a pointer it never saw
   *  arrive (the hover chain read below), since nothing of that pointer's has been seen to
   *  move. */
  let keptUnderAStillPointer = false;
  /** Where the pointer last was, so a pointer event the reader made can be told from one the
   *  page made for them. A `pointermove` the engine sends itself is at the place the reader
   *  left the pointer: WebKit and Firefox both send one after the page scrolls under it, to
   *  settle `:hover` on whatever is under it now (sjawhar/legion#2519). A boundary event is the
   *  same: the browser dispatches the pair at the pointer's own place when the page takes an
   *  element out from under it, and at the new place - the one the `pointermove` right behind
   *  it carries - when the reader moves away. The reader moving is the position changing. */
  let pointerWas: { x: number; y: number } | undefined;
  const theReaderMoved = (event: PointerEvent): boolean =>
    pointerWas === undefined || event.clientX !== pointerWas.x || event.clientY !== pointerWas.y;
  /** Whether `anchor` is what the pointer is on, asked the moment a request's delay runs out
   *  and again on the movements after that. A request is kept through whatever the page does
   *  to it (`onPointerOut`), so by the time the delay is up the reference may be anywhere: slid
   *  out from under the reader, or under a dialog, the inbox peek or a menu the page opened
   *  across it. A card drawn then previews a link that is not under their pointer, over
   *  whatever is - so it waits for them instead, pending rather than dropped.
   *
   *  Both halves are asked: the box, that the reference is where the pointer is, and
   *  `elementFromPoint`, that nothing of the page's stands over it there. Where this host has
   *  never seen the pointer there is no place to ask about, and the card opens as it always
   *  did - a pointer that rested on the reference before this host existed is on it by the
   *  browser's own reckoning, which is what the hover chain read below has just said. */
  const thePointerIsOn = (anchor: HTMLElement): boolean => {
    if (pointerWas === undefined) {
      return true;
    }
    const { x, y } = pointerWas;
    const box = anchor.getBoundingClientRect();
    if (x < box.left || x > box.right || y < box.top || y > box.bottom) {
      return false;
    }
    const at = document.elementFromPoint(x, y);
    return at !== null && (at === anchor || anchor.contains(at));
  };
  /** Anything the reader did that moves the pointer or the page, so a scroll after it is not
   *  read as the page moving by itself: a wheel they turned, a key they pressed (a space or an
   *  arrow scrolls), a button they put down (a drag of the scrollbar). */
  const onReaderInput = (event: Event) => {
    lastInputAt = event.timeStamp;
  };
  const onPointerMove = (event: PointerEvent) => {
    const moved = theReaderMoved(event);
    pointerWas = { x: event.clientX, y: event.clientY };
    if (!moved) {
      // The engine's own move, at the place the reader left the pointer: it says only that the
      // page moved under it, which is what the card is being kept for.
      return;
    }
    lastInputAt = event.timeStamp;
    if (keptUnderAStillPointer) {
      keptUnderAStillPointer = false;
      if (!inHoverZone(event.target)) {
        refPreview.leave();
        return;
      }
    }
    // The movement that decides a request left standing when its delay ran out somewhere the
    // reader was not pointing: where the reference is what they are on now, it waits that
    // delay again from here - arriving on a reference from over something else is the hover
    // the delay exists to tell from a reader passing through.
    refPreview.startTheDelayAgain(thePointerIsOn);
  };
  const onPointerOver = (event: PointerEvent) => {
    // Inside the zone there is nothing to open: a pointer arriving from outside it only cancels
    // the close its leaving armed, whatever kind of pointer it is.
    if (inHoverZone(event.target)) {
      if (!inHoverZone(event.relatedTarget)) {
        keptUnderAStillPointer = false;
        refPreview.reenter();
      }
      return;
    }
    // No card on touch (a tap follows the link), and none while a button is held (a
    // drag-selection passing over a link is not a hover).
    if (event.pointerType === "touch" || event.buttons !== 0) {
      return;
    }
    const trigger = triggerOf(event.target);
    if (trigger === null || trigger === triggerOf(event.relatedTarget)) {
      return;
    }
    const route = routeOf(trigger);
    if (route !== undefined) {
      keptUnderAStillPointer = false;
      refPreview.hoverStart(trigger, route, thePointerIsOn);
    }
  };
  const onPointerOut = (event: PointerEvent) => {
    if (!inHoverZone(event.target) || inHoverZone(event.relatedTarget)) {
      return;
    }
    // A card the reader asked for is not taken away because the page moved: where the hover
    // zone has slid out from under a pointer that never moved - a card above it settling its
    // height, a picture landing, a long body formatting, the issue page's own header, tab strip
    // and margin landing over the body they are pointing at - the card stays, measured where
    // the anchor is now, until the reader themselves moves away.
    //
    // A hover still waiting out its delay is kept on the same terms as a card already drawn,
    // because what is kept is the reader's request rather than the pixels it has put on the
    // screen: they pointed at the reference and held still, and the page moved it out from
    // under them before the delay was up. Abandoning it ended the hover for good - the pointer
    // never moved, so no crossing is coming to start a second one, and the card the reader
    // asked for never opened at all. That is `e2e/heavy-bodies.e2e.ts`'s hover card of a
    // comment of 8,000 list items: the page whose body is slow enough to be worth a card is the
    // page still drawing its header, its tab strip and its margin when the pointer reaches the
    // reference, and in CI's runner all three land inside the delay (run 42597442001, red in
    // chromium, firefox and webkit alike). `reposition` keeps the pending target, so the delay
    // runs on and the card opens where the anchor is now.
    //
    // THE POINTER'S OWN PLACE IS WHAT SAYS WHICH OF THE TWO MOVED, and it is the only thing
    // here that may say it. A clock cannot: a reader reaches a reference by moving onto it, so
    // their own last movement is always milliseconds old when the page slides it away, and
    // reading that arrival as a departure closed the card on the ordinary conversation page in
    // CI's container, deterministically and on both attempts (run 42628596511, shard 2,
    // `e2e/hover-preview.e2e.ts:72`). The ANCHOR'S MEASURED BOX cannot either: it says the page
    // moved only while the page is still moving it, and a request made for an anchor the page
    // has already moved - this host's chunk mounting in the very commit that lands the header,
    // which is where run 42743455211's one red attempt of that row left it: the card's own
    // reads went out 57 ms after the hover action, in the commit that landed the issue header
    // and the margin and took the anchor 11 px up from under a pointer resting at its middle,
    // leaving it 3 px below the reference, and no card ever opened - measures the same box
    // twice and reads the page's own boundary event as the reader leaving.
    //
    // So the question asked is the reader's: the boundary event carries the pointer's place,
    // and the page moving the zone away leaves that place exactly where the reader's last
    // movement left it. `keptUnderAStillPointer` is the correction - their next movement closes
    // a request kept over nothing, and a movement is what their own leaving always sends, since
    // the browser dispatches the move at the new place after the boundary pair. A boundary
    // event with nothing on the other side is not the page moving at all: the pointer has left
    // the window, or the anchor has left the document, and the request goes with it.
    //
    // An open card is kept on one further condition: the reference is still what the pointer is
    // on. A page that draws something over it - the palette's `fixed inset-0` backdrop, opened
    // from the keyboard under a resting pointer - sends exactly this boundary pair with the new
    // layer as its `relatedTarget`, and the card kept then stands above the dialog the reader
    // asked for, taking the first Escape from it (dispatch://INFRA-341). `thePointerIsOn` asks
    // both halves, the box and what is painted at that point, which is what tells the page
    // moving the reference from the page covering it.
    const kept = previewState();
    if (
      event.relatedTarget !== null &&
      (keptUnderAStillPointer || !theReaderMoved(event)) &&
      (kept?.phase !== "open" || thePointerIsOn(kept.target.anchor))
    ) {
      keptUnderAStillPointer = true;
      refPreview.reposition();
      return;
    }
    refPreview.leave();
  };
  const onFocusIn = (event: FocusEvent) => {
    const trigger = triggerOf(event.target);
    // `:focus-visible` is the browser's own keyboard-versus-pointer verdict: a mouse click or a
    // tap focuses a link without matching it, so only keyboard navigation opens the card.
    if (trigger === null || !trigger.matches(":focus-visible")) {
      return;
    }
    const route = routeOf(trigger);
    if (route !== undefined) {
      refPreview.focus(trigger, route);
    }
  };
  const onFocusOut = (event: FocusEvent) => {
    const trigger = triggerOf(event.target);
    // Focus moving from the anchor into its own card is not leaving: a press on the card's link
    // while the anchor holds focus (dnd-kit hands focus back to a board card's link after a
    // drop) must reach the link as a click, not unmount it under the pointer.
    if (trigger !== null && !refPreviewCardContains(event.relatedTarget)) {
      refPreview.blur(trigger);
    }
  };
  document.addEventListener("pointermove", onPointerMove, { passive: true });
  document.addEventListener("pointerover", onPointerOver);
  document.addEventListener("pointerout", onPointerOut);
  document.addEventListener("focusin", onFocusIn);
  document.addEventListener("focusout", onFocusOut);
  document.addEventListener("wheel", onReaderInput, { capture: true, passive: true });
  document.addEventListener("keydown", onReaderInput, { capture: true, passive: true });
  document.addEventListener("pointerdown", onReaderInput, { capture: true, passive: true });
  // The reader may already be on a trigger when these go up. This host is loaded beside the
  // route rather than in the entry chunk (`features/shell/deferred-shell.ts`) and mounted once
  // the route is drawn (`app.tsx`), so a page draws its references - and takes the reader's
  // pointer and their focus - before the listeners above exist. Neither a pointer resting on a
  // reference nor a focus already on one has a second event coming: the `pointerover` and the
  // `focusin` that put them there fired before this call, and a reader who holds still waits on
  // a card that never opens. A long body is where that window is widest, since the page the
  // reader is pointing at is the one keeping this chunk waiting
  // (`e2e/heavy-bodies.e2e.ts`'s hover card of a comment of 8,000 list items).
  //
  // So both are read where the browser keeps them: the hover chain, whose innermost element is
  // what the pointer is over, and `document.activeElement`. The pointer is read only where one
  // hovers at all - a touch screen leaves `:hover` on the last thing tapped, and a tap follows
  // the link rather than opening a card - and it opens on the delay a crossing would have
  // opened on, so a pointer merely passing over the reference is abandoned by its own
  // `pointerout` exactly as ever.
  if (window.matchMedia("(hover: hover)").matches) {
    const hovered = document.querySelectorAll<HTMLElement>(":hover");
    const resting = triggerOf(hovered.length === 0 ? null : hovered[hovered.length - 1]);
    const restingRoute = resting === null ? undefined : routeOf(resting);
    if (resting !== null && restingRoute !== undefined) {
      // Nothing of this pointer has been seen to move, so the page taking the reference out
      // from under it before the reader moves is the page's doing (`onPointerOut`).
      keptUnderAStillPointer = true;
      refPreview.hoverStart(resting, restingRoute, thePointerIsOn);
    }
  }
  const focused = triggerOf(document.activeElement);
  if (focused?.matches(":focus-visible") === true) {
    const route = routeOf(focused);
    if (route !== undefined) {
      refPreview.focus(focused, route);
    }
  }
  return () => {
    document.removeEventListener("pointermove", onPointerMove);
    document.removeEventListener("pointerover", onPointerOver);
    document.removeEventListener("pointerout", onPointerOut);
    document.removeEventListener("focusin", onFocusIn);
    document.removeEventListener("focusout", onFocusOut);
    document.removeEventListener("wheel", onReaderInput, { capture: true });
    document.removeEventListener("keydown", onReaderInput, { capture: true });
    document.removeEventListener("pointerdown", onReaderInput, { capture: true });
  };
}

function Heading({ children }: { children: ReactNode }): ReactNode {
  return <span className={`block font-semibold ${linkText}`}>{children}</span>;
}

/** The card's body: three rendered lines of what the target says, formatted, then cut. A `#id`
 *  in it names a question block of `source`, the document the target's own text belongs to. */
function Body({
  markdown,
  source,
  tree,
}: {
  markdown: string;
  source?: TextSource;
  tree: DisplayTree | undefined;
}): ReactNode {
  return (
    <MarkdownPreview
      className={`mt-1 ${textSecondaryOnSurface}`}
      lines={3}
      markdown={markdown}
      source={source}
      tree={tree}
    />
  );
}

/** The issue's full read, not the summary its links share: the card shows priority, labels, open
 *  asks and the spec, and holds that read only while it is shown. */
function IssuePreview({ route }: { route: DispatchReferenceRoute }): ReactNode {
  const key = "key" in route ? route.key : undefined;
  const { data: issue } = useQuery({ ...issueQuery(key), enabled: key !== undefined });
  const spec = primarySpec(issue);
  const specText = useQuery({
    ...artifactTextQuery(spec?.id, undefined),
    enabled: spec?.kind === "doc",
  });
  if (issue === undefined) {
    return <Heading>{shortForm(route)}</Heading>;
  }
  const specMarkdown =
    specText.data !== undefined && "markdown" in specText.data ? specText.data.markdown : undefined;
  const specTree =
    specText.data !== undefined && "markdown_tree" in specText.data
      ? specText.data.markdown_tree
      : undefined;
  return (
    <>
      <Heading>
        {issue.key}
        <span className={`ml-2 font-normal ${textPrimaryOnSurface}`}>{issue.title}</span>
      </Heading>
      <span className="mt-2 flex flex-wrap items-center gap-2">
        <StatusPill>{statusText(issue.status)}</StatusPill>
        {issue.priority === null ? null : (
          // Read-only on purpose: the card is a tooltip whose body is one link, so the editable
          // `PriorityControl` every other surface uses cannot live inside it.
          <span
            className={`inline-flex shrink-0 items-center rounded-sm px-1.5 py-0.5 text-xs font-semibold ${priorityBadge[issue.priority].bg} ${priorityBadge[issue.priority].text}`}
          >
            P{issue.priority}
          </span>
        )}
        {(issue.labels ?? []).map((label) => (
          <LabelPill key={label}>{label}</LabelPill>
        ))}
        {(issue.open_asks ?? []).length === 0 ? null : (
          <span className={`inline-flex items-center gap-1 text-xs ${textMutedOnSurface}`}>
            <AttentionBadge count={(issue.open_asks ?? []).length} />
            open {(issue.open_asks ?? []).length === 1 ? "ask" : "asks"}
          </span>
        )}
      </span>
      {specMarkdown === undefined || specMarkdown.trim() === "" ? null : (
        <Body markdown={specMarkdown} source={textSourceOf("artifact", spec?.id)} tree={specTree} />
      )}
    </>
  );
}

/** Who a document belongs to, as a card names it: an agent's conversation, a project, an issue.
 *  The card is a tooltip inside one link to the reference (never interactive), so an agent in it
 *  is named and marked by `AgentName` without a link of its own. */
function documentOwner(route: DispatchReferenceRoute): ReactNode {
  return route.kind === "agent-artifact" ? (
    <>
      agent <AgentName kind="static" sessionId={route.session} storedTitle={undefined} />
    </>
  ) : isProjectRoute(route) ? (
    route.project
  ) : (
    route.key
  );
}

function DocumentPreview({ route }: { route: DispatchReferenceRoute }): ReactNode {
  const { artifact, markdown, markdownTree } = useReferenceData(route, { text: true });
  if (artifact === undefined) {
    return <Heading>{shortForm(route)}</Heading>;
  }
  const owner = documentOwner(route);
  const latest = artifact.latest_version;
  return (
    <>
      <Heading>
        <span className={`font-normal ${textMutedOnSurface}`}>{owner} · </span>
        {artifact.name}
      </Heading>
      {latest === null ? null : (
        <span className={`mt-1 block text-xs ${textMutedOnSurface}`}>v{latest}</span>
      )}
      {markdown === undefined || markdown.trim() === "" ? null : (
        <Body
          markdown={markdown}
          source={textSourceOf("artifact", artifact.id)}
          tree={markdownTree}
        />
      )}
    </>
  );
}

/** The holder a block's card heads with, one label for each kind of item `itemOfBlock` names
 *  (`BLOCK_ITEM_KINDS`). */
const BLOCK_HOLDER_LABELS: Record<BlockItemKind, string> = {
  ask: "Ask",
  comment: "Comment",
  message: "Message",
};

/** A block of a document: its document, then for a pinned reference the version it pins and how
 *  the block stands since (`BlockPin.state`), then its text - the pinned version's for a pinned
 *  reference, its current text otherwise. The document's name is the card's own read of its
 *  summary, which no link to the block reads. A part of an ask, or a block of a comment or
 *  message, heads with its item and author in place of the document, and reads no summary
 *  (`useBlockDocument`), a comment's pin naming a revision of the comment and an ask part's a
 *  revision of the ask's parts; its text is the one its graph node carries, as a document
 *  block's is. */
function BlockPreview({ route }: { route: DispatchReferenceRoute }): ReactNode {
  const { block } = useReferenceData(route, { text: false });
  const artifact = useBlockDocument(route);
  const item = itemOfBlock(route);
  const holder = item === undefined ? artifact?.name : BLOCK_HOLDER_LABELS[item.kind];
  if (holder === undefined || block === undefined) {
    return <Heading>{shortForm(route)}</Heading>;
  }
  const { node } = block;
  const pin = node.pin;
  const text = blockText(node);
  const textTree = blockTextTree(node);
  const author = item === undefined ? undefined : node.made_by?.[0];
  const unit = item === undefined ? "version" : "revision";
  return (
    <>
      <Heading>
        <span className={`font-normal ${textMutedOnSurface}`}>{documentOwner(route)} · </span>
        {holder}
        {author === undefined ? null : (
          <>
            <span className={`font-normal ${textMutedOnSurface}`}> · </span>
            <AgentName actor={author} kind="static" />
          </>
        )}
      </Heading>
      {pin === undefined ? null : (
        <span className={`mt-1 block text-xs ${textMutedOnSurface}`}>
          <span className="block">
            {item === undefined ? "Version" : "Revision"} {pin.version}
          </span>
          <span className="block">
            {pin.state === "changed"
              ? `Changed in ${unit} ${node.changed_version}`
              : pin.state === "removed"
                ? `Removed in ${unit} ${pin.removed_version}`
                : `Unchanged since ${unit} ${pin.version}`}
          </span>
        </span>
      )}
      {text === undefined || text.trim() === "" ? null : (
        <Body markdown={text} source={textSourceOf("artifact", artifact?.id)} tree={textTree} />
      )}
    </>
  );
}

function AskPreview({ route }: { route: DispatchReferenceRoute }): ReactNode {
  const { ask } = useReferenceData(route, { text: false });
  if (ask === undefined) {
    return <Heading>{shortForm(route)}</Heading>;
  }
  const options = (ask.ask.options ?? []).length;
  return (
    <>
      <Heading>
        <MarkdownPreview
          lines={2}
          markdown={ask.ask.question}
          source={textSourceOf("ask", ask.ask.id)}
          tree={ask.ask.question_tree}
        />
      </Heading>
      <span className={`mt-1 block text-xs ${textMutedOnSurface}`}>
        {ask.ask.state}
        {options === 0 ? null : ` · ${options} ${options === 1 ? "option" : "options"}`}
      </span>
    </>
  );
}

function CommentPreview({ route }: { route: DispatchReferenceRoute }): ReactNode {
  const { comment } = useReferenceData(route, { text: false });
  if (comment === undefined) {
    return <Heading>{shortForm(route)}</Heading>;
  }
  return (
    <>
      <Heading>
        <AgentName actor={comment.comment.author} kind="static" />
      </Heading>
      <Body
        markdown={comment.comment.body}
        source={textSourceOf("comment", comment.comment.id)}
        tree={comment.comment.body_tree}
      />
    </>
  );
}

function MessagePreview({ route }: { route: DispatchReferenceRoute }): ReactNode {
  const { message } = useReferenceData(route, { text: false });
  if (message === undefined) {
    return <Heading>{shortForm(route)}</Heading>;
  }
  return (
    <>
      <Heading>
        <AgentName actor={message.message.author} kind="static" />
      </Heading>
      <Body
        markdown={message.message.body}
        source={textSourceOf("message", message.message.id)}
        tree={message.message.body_tree}
      />
    </>
  );
}

function PreviewContent({ route }: { route: DispatchReferenceRoute }): ReactNode {
  switch (referenceTargetKind(route)) {
    case "ask":
      return <AskPreview route={route} />;
    case "comment":
      return <CommentPreview route={route} />;
    case "message":
      return <MessagePreview route={route} />;
    case "document":
      return <DocumentPreview route={route} />;
    case "block":
      return <BlockPreview route={route} />;
    case "issue":
      return <IssuePreview route={route} />;
  }
}

const VIEWPORT_MARGIN_PX = 8;
const ANCHOR_GAP_PX = 6;

function PreviewCard({ target }: { target: PreviewTarget }): ReactNode {
  const element = useRef<HTMLDivElement>(null);
  const path = useReferencePath(target.route);
  // Re-measured on every render: the card grows when its data arrives, and its size decides
  // whether it fits below the anchor. Fixed positioning against the anchor's viewport rect is
  // the boring choice that works in Chromium and iPhone Safari alike — the native Popover API
  // would still need this same arithmetic (CSS anchor positioning is not yet in every engine
  // the e2e projects cover), and the card has no need for the top layer.
  useLayoutEffect(() => {
    const node = element.current;
    if (node === null) {
      return;
    }
    const rect = target.anchor.getBoundingClientRect();
    const height = node.offsetHeight;
    const width = node.offsetWidth;
    const below = rect.bottom + ANCHOR_GAP_PX;
    const above = rect.top - ANCHOR_GAP_PX - height;
    const top =
      below + height <= window.innerHeight - VIEWPORT_MARGIN_PX || above < VIEWPORT_MARGIN_PX
        ? below
        : above;
    const left = Math.max(
      VIEWPORT_MARGIN_PX,
      Math.min(rect.left, window.innerWidth - width - VIEWPORT_MARGIN_PX)
    );
    node.style.top = `${top}px`;
    node.style.left = `${left}px`;
  });
  return (
    <div
      className={`fixed top-0 left-0 z-[60] w-80 max-w-[calc(100vw-1rem)] rounded-xl border p-3 text-sm shadow-lg ${card}`}
      id={REF_PREVIEW_CARD_ID}
      ref={element}
      role="tooltip"
    >
      <Link className="block" tabIndex={-1} to={path}>
        <PreviewContent route={target.route} />
      </Link>
    </div>
  );
}

/** Mount once in the shell. Owns the document-level trigger listeners, renders the open card
 * into `document.body`, prefetches the target the moment a hover begins (so the card opens
 * populated), links the card to its anchor with `aria-describedby`, and closes it on Escape, on
 * any click (the click follows a link), when the anchor leaves the DOM, or on scroll — unless
 * the anchor holds keyboard focus (focusing it may itself scroll it into view), in which case
 * the card follows the anchor instead. */
export function RefPreviewHost(): ReactNode {
  const current = useSyncExternalStore(subscribeToPreview, previewState);
  const queryClient = useQueryClient();

  useEffect(() => {
    const detachTriggers = attachTriggers();
    // Capture phase, ahead of `useDialog`'s Escape: while a card is open it is the topmost
    // layer, so Escape dismisses it alone and leaves the dialog beneath (the search palette)
    // open. With no open card the key passes through untouched.
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape" && previewState()?.phase === "open") {
        event.stopPropagation();
        closeRefPreview();
      }
    };
    // Only an open card reacts to scroll. A pending hover is left alone — the pointer often
    // comes to rest on a link while the page is still settling (smooth scrolling, a
    // scrollIntoView), and the card is measured fresh when it opens anyway.
    const onScroll = (event: Event) => {
      const open = previewState();
      if (open?.phase !== "open") {
        return;
      }
      // The card follows its anchor rather than closing where the reader did nothing to move the
      // page: a long body above the reference formatting, a picture landing, the reader's place
      // being kept after a late paint all scroll the page under a resting pointer, and a card
      // the reader is reading must not be taken away by that. It is the same page movement the
      // `pointerout` rule above protects it from, reported as a scroll rather than as a boundary
      // event, and in WebKit and Firefox the scroll is the one that arrives first
      // (sjawhar/legion#2519). The reader's own scroll - a wheel, a key, a drag of the scrollbar
      // - still closes it, and so does any scroll once they have moved the pointer away, since
      // leaving the zone closes the card itself. An anchor that holds keyboard focus keeps its
      // card either way: focusing it may itself scroll it into view.
      if (
        document.activeElement === open.target.anchor ||
        event.timeStamp - lastInputAt > READER_INPUT_GRACE_MS
      ) {
        refPreview.reposition();
      } else {
        closeRefPreview();
      }
    };
    document.addEventListener("keydown", onKeyDown, true);
    // Bubble phase, after React's own delegated handlers (the card is portaled into `body`,
    // where React listens): a click on the card must still reach its link and navigate.
    window.addEventListener("click", closeRefPreview);
    document.addEventListener("scroll", onScroll, { capture: true, passive: true });
    return () => {
      detachTriggers();
      document.removeEventListener("keydown", onKeyDown, true);
      window.removeEventListener("click", closeRefPreview);
      document.removeEventListener("scroll", onScroll, { capture: true });
      closeRefPreview();
    };
  }, []);

  const target = current?.target;
  useEffect(() => {
    if (target === undefined) {
      return;
    }
    prefetchReference(queryClient, target.route);
    // No pointerout or focusout fires for an anchor that leaves the DOM (a Markdown body
    // re-rendering, a route change), so the card would outlive its anchor; watch for removal
    // for as long as this target is live.
    const observer = new MutationObserver(() => {
      if (!target.anchor.isConnected) {
        closeRefPreview();
      }
    });
    observer.observe(document.body, { childList: true, subtree: true });
    return () => observer.disconnect();
  }, [queryClient, target]);

  const openAnchor = current?.phase === "open" ? current.target.anchor : undefined;
  useEffect(() => {
    if (openAnchor === undefined) {
      return;
    }
    const previous = openAnchor.getAttribute("aria-describedby");
    openAnchor.setAttribute("aria-describedby", REF_PREVIEW_CARD_ID);
    return () => {
      if (previous === null) {
        openAnchor.removeAttribute("aria-describedby");
      } else {
        openAnchor.setAttribute("aria-describedby", previous);
      }
    };
  }, [openAnchor]);

  if (current?.phase !== "open") {
    return null;
  }
  return createPortal(<PreviewCard target={current.target} />, document.body);
}
