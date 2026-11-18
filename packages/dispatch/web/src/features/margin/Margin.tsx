import { type ReactNode, useEffect, useLayoutEffect, useRef } from "react";
import { useLocation } from "react-router-dom";

import {
  borderDefault,
  focusVisibleRing,
  pageHairBorder,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  railBg,
  railBorder,
  railHoverBg,
  railText,
} from "../../theme/classes";
import { DETAILS_ATTRIBUTE, type MarginDrawer, useMarginDrawer } from "../issue-page/margin-slot";
import { NO_HOVER_PREVIEW_ATTRIBUTE } from "../refs/ref-preview-card";
import { routeHasMargin } from "../refs/routes";
import { DIALOG_SCOPE, useKeymap } from "../shell/keymap";
import {
  COMPACT_VIEWPORT_QUERY,
  PHONE_VIEWPORT_QUERY,
  useDialog,
  useMediaQuery,
} from "../shell/useDialog";
import { MarginSheet, type MarginSheetModel } from "./MarginSheet";
import { useMargin } from "./margin-context";
import {
  clampMarginWidth,
  DEFAULT_MARGIN_WIDTH,
  MARGIN_WIDTH_STEP,
  MIN_MARGIN_WIDTH,
  maxMarginWidth,
} from "./margin-width";
import { useMarginSheet } from "./useMarginSheet";

export interface MarginProps {
  collapsed?: boolean;
  onCollapsedChange?(collapsed: boolean): void;
  onWidthChange?(width: number): void;
  width?: number;
}

export function Margin({
  collapsed = false,
  onCollapsedChange,
  onWidthChange,
  width,
}: MarginProps): ReactNode {
  const model = useMarginSheet();
  const isCompactViewport = useMediaQuery(COMPACT_VIEWPORT_QUERY);
  const { pathname, search } = useLocation();
  // The new issue page's Details drawer, where the shell draws that page's frame; undefined on
  // every other page, where the margin is the column it has always been.
  const drawer = useMarginDrawer();
  const resizePointer = useRef<
    { pointerId: number; startWidth: number; startX: number } | undefined
  >(undefined);
  const marginWidth = clampMarginWidth(width ?? DEFAULT_MARGIN_WIDTH);
  const maxWidth = maxMarginWidth();
  const setClampedMarginWidth = (nextWidth: number) => {
    onWidthChange?.(clampMarginWidth(nextWidth));
  };
  const hasMargin = routeHasMargin(pathname, search);
  // Collapsed to its rail, the margin stays mounted under it, hidden: an open composer's unsent
  // draft is still there when the margin comes back. A drawer has no rail: closed, it is gone
  // from the screen, and Details opens it.
  const railShown = drawer === undefined && !isCompactViewport && collapsed && hasMargin;
  // Each compose the margin shows - a new one, one a newer selection-bar action had to wait on,
  // or one held for the document the reader came back to - is the reader's to see, so the rail
  // opens for it; collapsing the rail with a compose open keeps it collapsed.
  // In the new issue page's frame a closed Details opens for it the same way.
  const { shownCompose } = useMargin();
  const handledCompose = useRef(shownCompose);
  useEffect(() => {
    if (shownCompose === handledCompose.current) return;
    handledCompose.current = shownCompose;
    if (shownCompose !== undefined && railShown) onCollapsedChange?.(false);
    if (shownCompose !== undefined && drawer !== undefined && !drawer.open) drawer.setOpen(true);
  }, [drawer, onCollapsedChange, railShown, shownCompose]);
  // In the drawer, a card the reader picks - a mark they pressed in a document, a link that names
  // a thread - is drawn in the margin, so a closed drawer opens for it as it does for a compose:
  // the card is what the reader asked to see. So is the card an address lands on beside its
  // document (`?mark=`, or an item at a pinned version), on the load that follows the link as on
  // a move: the drawer opens once per navigation, the first time the margin holds that card, and
  // a reader who closes it again keeps it closed. An address that names a node of the issue's
  // outline marks that item and is no pick: it lands on the node, and Details stays as it was
  // (R20). Every other page has no drawer, and nothing here runs there.
  const pickedCard = model.selection.selectedItemId;
  const routeMarked = model.selection.routeMarkedItemId;
  const handledCard = useRef(pickedCard);
  const landedCard = model.selection.landedCard;
  const handledLanding = useRef<string | undefined>(undefined);
  useEffect(() => {
    const picked =
      pickedCard !== handledCard.current && pickedCard !== undefined && pickedCard !== routeMarked;
    const landed = landedCard !== handledLanding.current && landedCard !== undefined;
    handledCard.current = pickedCard;
    handledLanding.current = landedCard;
    if ((picked || landed) && drawer !== undefined && !drawer.open) drawer.setOpen(true);
  }, [drawer, landedCard, pickedCard, routeMarked]);

  if (drawer !== undefined) {
    return (
      <DetailsDrawer
        drawer={drawer}
        model={model}
        onWidthChange={setClampedMarginWidth}
        width={marginWidth}
      />
    );
  }

  // A margin is a property of a document surface. On a route that has none - the Inbox, a
  // project, Settings, Agents - the desktop column takes no width at all, rather than spending
  // 384 px, a third of a 1280 px viewport, saying so in a sentence. The reader's own collapse
  // preference is untouched, so it returns as they left it on the next issue or document.
  // `routeHasMargin` is the one answer the shell reserves its gutter from too. A send the margin
  // had out when the reader left is the held-send store's, which keeps it for their return.
  if (!isCompactViewport && !hasMargin) {
    return null;
  }

  return (
    <>
      {railShown ? (
        <aside
          aria-label="Collapsed margin"
          className={`fixed inset-y-0 right-0 z-10 flex w-14 justify-center border-l p-2 ${railBorder} ${railBg} ${railText}`}
          data-testid="margin-rail"
        >
          <button
            aria-label="Show margin"
            className={`min-h-11 min-w-11 rounded-lg text-sm font-medium ${railHoverBg}`}
            onClick={() => onCollapsedChange?.(false)}
            type="button"
          >
            <span aria-hidden="true">‹</span>
          </button>
        </aside>
      ) : null}
      <div
        className={isCompactViewport ? "contents" : "relative order-3 shrink-0"}
        data-testid={isCompactViewport ? undefined : "desktop-margin-shell"}
        hidden={railShown || (!isCompactViewport && !hasMargin)}
        style={isCompactViewport ? undefined : { width: `${marginWidth}px` }}
      >
        <hr
          aria-controls="review-margin"
          aria-label="Resize margin"
          aria-orientation="vertical"
          aria-valuemax={maxWidth}
          aria-valuemin={MIN_MARGIN_WIDTH}
          aria-valuenow={marginWidth}
          className={
            isCompactViewport
              ? "hidden"
              : `absolute top-0 -left-1 z-20 h-full w-2 border-0 border-l cursor-col-resize focus-visible:outline-none focus-visible:ring-2 ${borderDefault} ${focusVisibleRing}`
          }
          onDoubleClick={() => setClampedMarginWidth(DEFAULT_MARGIN_WIDTH)}
          onKeyDown={(event) => {
            if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") {
              return;
            }
            event.preventDefault();
            setClampedMarginWidth(
              marginWidth + (event.key === "ArrowLeft" ? MARGIN_WIDTH_STEP : -MARGIN_WIDTH_STEP)
            );
          }}
          onPointerCancel={(event) => {
            if (resizePointer.current?.pointerId === event.pointerId) {
              resizePointer.current = undefined;
            }
          }}
          onPointerDown={(event) => {
            if (event.button !== 0) {
              return;
            }
            event.preventDefault();
            event.currentTarget.setPointerCapture(event.pointerId);
            resizePointer.current = {
              pointerId: event.pointerId,
              startWidth: marginWidth,
              startX: event.clientX,
            };
          }}
          onPointerMove={(event) => {
            const pointer = resizePointer.current;
            if (pointer?.pointerId !== event.pointerId) {
              return;
            }
            setClampedMarginWidth(pointer.startWidth + pointer.startX - event.clientX);
          }}
          onPointerUp={(event) => {
            if (resizePointer.current?.pointerId !== event.pointerId) {
              return;
            }
            resizePointer.current = undefined;
            event.currentTarget.releasePointerCapture(event.pointerId);
          }}
          tabIndex={0}
        />
        <MarginSheet
          collapsed={railShown}
          desktopControl={
            isCompactViewport || onCollapsedChange === undefined ? undefined : (
              <button
                aria-label="Hide margin"
                className={`min-h-11 min-w-11 rounded-lg text-sm font-medium ${railHoverBg}`}
                onClick={() => onCollapsedChange(true)}
                type="button"
              >
                <span aria-hidden="true">›</span>
              </button>
            )
          }
          model={model}
        />
      </div>
    </>
  );
}

/** The attribute a link in Details carries, or an element around it, where following that link
 *  leaves the phone's Details sheet open (`DetailsDrawer`'s `closeOnLink`): a history turn's time
 *  link, for one. Every other link followed inside the sheet closes it. */
export const KEEPS_DETAILS_ATTRIBUTE = "data-keeps-details";

/**
 * The new issue page's panel as a drawer, Details, which opens over the right of the page rather
 * than keeping a column of its own (R14, the prototype's `#details`), so
 * the page's own column is the window less the navigation rail. It keeps what the margin's
 * column had: the width the reader gave it, kept across loads, and the handle that resizes it,
 * at its leading edge; its collapse is the drawer closed, as every load opens it. It casts the
 * prototype's edge shadow at every width. Below the phone's width it is the phone's sheet, the
 * whole screen wide, and holds the focus while it is open as any sheet does; at 760 px and below
 * the prototype's shade covers the page under it, and a press on the shade closes it.
 *
 * Its head names it and closes it, in the words the page's root hands the shell
 * (`MarginDrawer.page`), and draws nothing before the root has; Escape closes it too. Under its
 * head it draws only what the page put in the margin's slot (`MarginSheet`'s `drawer`).
 */
function DetailsDrawer({
  drawer,
  model,
  onWidthChange,
  width,
}: {
  drawer: MarginDrawer;
  model: MarginSheetModel;
  onWidthChange: (width: number) => void;
  width: number;
}): ReactNode {
  const isPhoneViewport = useMediaQuery(PHONE_VIEWPORT_QUERY);
  const closeButton = useRef<HTMLButtonElement>(null);
  const box = useRef<HTMLElement | null>(null);
  const { open, page, setOpen } = drawer;
  // The shell hands a new `setOpen` whenever its drawer changes - a new `page` among them - so
  // the effects below read the latest through this ref and run on `open` alone: a drawer that
  // stays open neither moves the focus nor records a new opener.
  const setOpenNow = useRef(setOpen);
  setOpenNow.current = setOpen;
  // The phone's sheet covers the page, so it is a dialog: the focus stays inside it, the page
  // under it does not scroll, and Escape closes it and gives the focus back (`useDialog`).
  const sheet = useDialog<HTMLElement>({
    initialFocusRef: closeButton,
    onClose: () => setOpenNow.current(false),
    open: open && isPhoneViewport,
  });
  // An open dialog masks every global binding, `Shift+M` among them, so the sheet binds its own
  // in the dialog's scope: it closes Details, as the global one does at every other width, while
  // Details is the innermost dialog the focus is in. A dialog over it - another layer, or a
  // thread's own dialog drawn inside it - holds the focus, and the key leaves Details open.
  useKeymap(
    DIALOG_SCOPE,
    open && isPhoneViewport
      ? [
          {
            id: "details.close",
            keys: "Shift+M",
            label: "Open or close Details",
            run: () => setOpenNow.current(false),
            when: () => document.activeElement?.closest('[role="dialog"]') === box.current,
          },
        ]
      : []
  );
  // From the phone's width up the drawer sits beside the page the reader keeps working in, so it
  // holds no focus of its own: opening it puts the focus on its Close, and closing it - by Close,
  // by Escape, or by the page's own control - gives the focus back to where it was taken from
  // when that focus is still in the drawer or nowhere. Both happen in the commit that opens or
  // closes it, so a focus the reader moves the moment it has closed is never taken back.
  const opener = useRef<HTMLElement | null>(null);
  useLayoutEffect(() => {
    if (!open || isPhoneViewport) return;
    // The drawer as it is now: the commit that closes it detaches the `aside`'s ref before this
    // effect's cleanup runs, so the cleanup cannot read `box`.
    const drawerBox = box.current;
    const active = document.activeElement;
    opener.current =
      active instanceof HTMLElement && active !== document.body && !drawerBox?.contains(active)
        ? active
        : null;
    closeButton.current?.focus({ preventScroll: true });
    // Escape closes the drawer before the page's own Escape does anything else: the press is the
    // drawer's alone. A field keeps its own Escape, which leaves the field, wherever it is -
    // inside the drawer or beside it. So does a layer over the drawer or inside it: a dialog the
    // focus is in - the navigation's sheet, the list of keys - closes on its own Escape and
    // leaves the drawer open under it, and a menu open in the drawer closes before the drawer
    // does, since Escape closes the innermost layer.
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || event.defaultPrevented) return;
      const target = event.target;
      const field =
        target instanceof HTMLElement &&
        (target.isContentEditable ||
          target.tagName === "SELECT" ||
          target.tagName === "TEXTAREA" ||
          (target instanceof HTMLInputElement &&
            !["button", "checkbox", "radio", "reset", "submit"].includes(target.type)));
      if (field) return;
      if (target instanceof Element && target.closest('[role="dialog"]') !== null) return;
      if (box.current?.querySelector('[role="menu"]') != null) return;
      event.preventDefault();
      setOpenNow.current(false);
    };
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("keydown", closeOnEscape);
      const focus = document.activeElement;
      const lost = focus === null || focus === document.body || drawerBox?.contains(focus) === true;
      if (lost && opener.current?.isConnected === true) {
        opener.current.focus({ preventScroll: true });
      }
    };
  }, [isPhoneViewport, open]);
  // On a phone the sheet covers the whole page, so a link followed inside it - a pin, an entry
  // of the history, a file - would land under it: the sheet closes as the link is followed. A
  // link that carries `KEEPS_DETAILS_ATTRIBUTE`, or stands inside an element that does, leaves
  // it open. The listener is the DOM's own, in the capture phase, because a panel the page draws
  // into the drawer through a portal is outside the drawer's React tree, and a React handler here
  // never sees its clicks.
  useEffect(() => {
    const drawerBox = box.current;
    if (drawerBox === null || !open || !isPhoneViewport) return;
    const closeOnLink = (event: MouseEvent) => {
      if (!(event.target instanceof Element)) return;
      const link = event.target.closest("a[href]");
      if (
        link !== null &&
        !link.hasAttribute("download") &&
        link.getAttribute("target") === null &&
        link.closest(`[${KEEPS_DETAILS_ATTRIBUTE}]`) === null
      ) {
        setOpenNow.current(false);
      }
    };
    drawerBox.addEventListener("click", closeOnLink, true);
    return () => drawerBox.removeEventListener("click", closeOnLink, true);
  }, [isPhoneViewport, open]);

  return (
    <>
      {open ? (
        // The prototype's `#scrim`, at its `isNarrow` width alone.
        <div
          aria-hidden="true"
          className="fixed inset-0 z-40 hidden bg-[rgba(28,25,23,0.25)] max-[761px]:block"
          data-testid="details-shade"
          onClick={() => setOpen(false)}
        />
      ) : null}
      {/* biome-ignore lint/a11y/useAriaPropsSupportedByRole: aria-modal only applies while the phone's sheet is an open dialog */}
      <aside
        aria-label={page?.words.details}
        aria-modal={open && isPhoneViewport ? true : undefined}
        className={`fixed inset-y-0 right-0 z-40 flex max-w-full flex-col border-l shadow-[-12px_0_30px_rgba(28,25,23,0.14)] ${pagePaperBg} ${pageLineBorder} ${pageInkText} ${
          isPhoneViewport ? "w-full" : ""
        }`}
        data-testid="desktop-margin-shell"
        {...{ [DETAILS_ATTRIBUTE]: "", [NO_HOVER_PREVIEW_ATTRIBUTE]: "" }}
        hidden={!open}
        ref={(element) => {
          box.current = element;
          sheet.containerRef.current = element;
        }}
        role={isPhoneViewport ? "dialog" : undefined}
        style={isPhoneViewport ? undefined : { width: `${width}px` }}
        // On the phone the sheet is a modal dialog, so a press on its text keeps the focus in it:
        // with no focusable box there the press moves the focus to the body, and the sheet's own
        // `Shift+M` above, which acts only while the focus is inside, no longer closes it.
        tabIndex={isPhoneViewport ? -1 : undefined}
      >
        {isPhoneViewport ? null : (
          <DetailsResizeHandle onWidthChange={onWidthChange} width={width} />
        )}
        {page === undefined ? null : (
          <div
            className={`flex shrink-0 items-center gap-2 border-b px-4 pt-3.5 pb-2.5 ${pageLineBorder}`}
          >
            <h2 className="flex-1 text-sm font-semibold" {...page.chrome}>
              {page.words.details}
            </h2>
            <button
              className={`rounded-[7px] border px-3 py-[3px] text-sm font-medium ${pageHairBorder} ${pagePaperBg} ${pageMutedText} ${focusVisibleRing}`}
              data-testid="details-close"
              onClick={() => setOpen(false)}
              ref={closeButton}
              type="button"
              {...page.chrome}
            >
              {page.words.close}
            </button>
          </div>
        )}
        <MarginSheet drawer model={model} />
      </aside>
    </>
  );
}

/**
 * Details' resize handle: dragged, pressed with the arrow keys, or double-pressed back to the
 * default width, as the margin column's own handle is. It is fixed to the screen at the drawer's
 * leading edge, four pixels of it over the page to its left, so it is grabbable however far the
 * page under the drawer is scrolled.
 */
function DetailsResizeHandle({
  onWidthChange,
  width,
}: {
  onWidthChange: (width: number) => void;
  width: number;
}): ReactNode {
  const resizePointer = useRef<
    { pointerId: number; startWidth: number; startX: number } | undefined
  >(undefined);
  return (
    <hr
      aria-controls="review-margin"
      aria-label="Resize margin"
      aria-orientation="vertical"
      aria-valuemax={maxMarginWidth()}
      aria-valuemin={MIN_MARGIN_WIDTH}
      aria-valuenow={width}
      className={`fixed top-0 z-50 h-dvh w-2 border-0 border-l cursor-col-resize focus-visible:outline-none focus-visible:ring-2 ${borderDefault} ${focusVisibleRing}`}
      style={{ right: `${width - 4}px` }}
      onDoubleClick={() => onWidthChange(DEFAULT_MARGIN_WIDTH)}
      onKeyDown={(event) => {
        if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") {
          return;
        }
        event.preventDefault();
        onWidthChange(width + (event.key === "ArrowLeft" ? MARGIN_WIDTH_STEP : -MARGIN_WIDTH_STEP));
      }}
      onPointerCancel={(event) => {
        if (resizePointer.current?.pointerId === event.pointerId) {
          resizePointer.current = undefined;
        }
      }}
      onPointerDown={(event) => {
        if (event.button !== 0) {
          return;
        }
        event.preventDefault();
        event.currentTarget.setPointerCapture(event.pointerId);
        resizePointer.current = {
          pointerId: event.pointerId,
          startWidth: width,
          startX: event.clientX,
        };
      }}
      onPointerMove={(event) => {
        const pointer = resizePointer.current;
        if (pointer?.pointerId !== event.pointerId) {
          return;
        }
        onWidthChange(pointer.startWidth + pointer.startX - event.clientX);
      }}
      onPointerUp={(event) => {
        if (resizePointer.current?.pointerId !== event.pointerId) {
          return;
        }
        resizePointer.current = undefined;
        event.currentTarget.releasePointerCapture(event.pointerId);
      }}
      tabIndex={0}
    />
  );
}
