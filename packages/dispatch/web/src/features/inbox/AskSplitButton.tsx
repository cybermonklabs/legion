import { type ReactNode, useEffect, useId, useRef } from "react";

import { AnchoredPanel } from "../../components/AnchoredPanel";
import { ChevronIcon } from "../../components/DisclosureToggle";
import { useTooltips } from "../../components/tooltips";
import {
  borderDefault,
  card,
  kbdHint,
  menuItemActiveBg,
  menuItemRefusedText,
  menuRule,
  pageHairBorder,
  pageInkBorder,
  pageInkFill,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pagePaneBg,
  pagePaperBg,
  primaryButtonBg,
  primaryButtonEnabledHoverBg,
  splitButtonLiveFrame,
  splitButtonRefusedAction,
  splitButtonRefusedFrame,
  splitButtonRefusedLive,
  splitButtonRule,
  splitButtonRuleRefused,
  textSecondaryOnSurface,
} from "../../theme/classes";

/**
 * Each colour the split button draws inside `NoTooltips`, the new issue page's, by the role its
 * colour has on today's pages: live, the control is filled with the page's ink, as the page's one
 * filled control is, with a rule of the paper between its halves; while the action cannot be
 * pressed the control is the page's header control, its rule on the paper, the action in the
 * quiet grey and the chevron in the muted grey, the mark grey under the pointer. The menu is the
 * page's menu: its items in the ink, the mark grey under the pointer, a refused one in the quiet
 * grey, and each key in the muted grey on the pane.
 */
const ON_PAGE = {
  frame: `${pageLineBorder} ${pagePaperBg}`,
  item: `${pageInkText} not-refused:hover:bg-page-mark focus-visible:bg-page-mark refused:cursor-default refused:text-page-quiet`,
  key: `${pagePaneBg} ${pageMutedText}`,
  live: pageInkFill,
  liveFrame: pageInkBorder,
  menu: `${pageHairBorder} ${pagePaperBg}`,
  menuRule: "bg-page-line",
  refusedAction: "refused:cursor-not-allowed refused:text-page-quiet",
  refusedLive: `${pageMutedText} hover:bg-page-mark`,
  rule: "bg-page-paper/35",
  ruleRefused: "bg-page-line",
} as const;

/** One of the card's other actions, as its menu lists it: what it is called, the key that runs
 *  it with the card in hand, and why it cannot run now, which the item's tooltip says (inside
 *  `NoTooltips`, its description). */
export interface AskMenuItem {
  id: string;
  label: string;
  hotkey: string;
  /** Why the item cannot run now; set, the item is refused and says this as its tooltip, or
   *  inside `NoTooltips` as its accessible description. */
  refused?: string;
  run: () => void;
}

/** Each half is 42 px inside the frame's 1 px border: the 44 px tap target. */
const HALF = "min-h-[42px]";
/** The open menu's items, in the page: the panel is portalled out of the control. */
const MENU_ITEMS = "[data-ask-menu] [role=menuitem]";
const menuItems = () => [...document.querySelectorAll<HTMLElement>(MENU_ITEMS)];

/**
 * The card's one control for what it does: its action (Answer, or Ask back while the reader asks a
 * question back) and, on the same button, a chevron that opens every other action of the card in
 * one menu, each with its key. It is a split button: two halves of one shape with a thin light
 * rule between them.
 * While the action cannot be pressed the control is an outline with the action disabled, in the
 * disabled shade, and the chevron live, since the menu's actions do not wait on an answer; it
 * fills once the action can be pressed. The action is `disabled` rather than refused in words
 * alone, so the card hands the focus on when it disables under the reader (`AskCard`). Inside
 * `NoTooltips` a press whose write is out leaves the action refused with `aria-disabled` instead
 * (`actionRefusedHoldingFocus`), so it keeps the focus the press gave it, as every shared control
 * there does; it draws no `title` (outside it the chevron's is "More actions"), and a refused item
 * of the menu says why as its description.
 *
 * The menu groups its items (`groups`, each drawn under a rule from the one before) and is
 * walked with the arrow keys; each item's own key runs it from the open menu as well.
 */
export function AskSplitButton({
  action,
  actionDescribedBy,
  actionRefused,
  actionRefusedHoldingFocus = false,
  actionTitle,
  groups,
  menuOpen,
  onMenuOpenChange,
}: {
  action: string;
  actionDescribedBy?: string;
  /** The action cannot be pressed now (no pick yet, nothing typed, an upload out, and outside
   *  `NoTooltips` a write out): it is `disabled`. */
  actionRefused: boolean;
  /** Inside `NoTooltips`, the card refuses the action while its write is out and until the ask
   *  is read again after it: `aria-disabled`, not `disabled`, so the action keeps the focus. A
   *  press of it still submits the form, which sends nothing then (`AskCard`). */
  actionRefusedHoldingFocus?: boolean;
  actionTitle?: string;
  groups: readonly (readonly AskMenuItem[])[];
  menuOpen: boolean;
  onMenuOpenChange: (open: boolean) => void;
}): ReactNode {
  const tooltips = useTooltips();
  const reasonId = useId();
  const groupRef = useRef<HTMLDivElement>(null);
  const chevronRef = useRef<HTMLButtonElement>(null);
  const shown = groups.filter((group) => group.length > 0);
  const close = () => onMenuOpenChange(false);
  const run = (item: AskMenuItem) => {
    if (item.refused !== undefined) return;
    close();
    item.run();
  };
  // The panel focuses its first control as it opens; the menu moves that on to its first item
  // that can run, so Enter at once never presses a refused one.
  useEffect(() => {
    if (!menuOpen) return;
    menuItems()
      .find((item) => item.getAttribute("aria-disabled") !== "true")
      ?.focus();
  }, [menuOpen]);
  // Disabled or refused holding the focus, the action cannot be pressed, and the control is
  // drawn as such.
  const cannotPress = actionRefused || actionRefusedHoldingFocus;
  const frameLook = tooltips
    ? cannotPress
      ? splitButtonRefusedFrame
      : splitButtonLiveFrame
    : cannotPress
      ? ON_PAGE.frame
      : ON_PAGE.liveFrame;
  const actionLook = tooltips
    ? cannotPress
      ? splitButtonRefusedAction
      : `${primaryButtonBg} ${primaryButtonEnabledHoverBg}`
    : cannotPress
      ? ON_PAGE.refusedAction
      : ON_PAGE.live;
  const ruleLook = tooltips
    ? cannotPress
      ? splitButtonRuleRefused
      : splitButtonRule
    : cannotPress
      ? ON_PAGE.ruleRefused
      : ON_PAGE.rule;
  const chevronLook = tooltips
    ? cannotPress
      ? splitButtonRefusedLive
      : `${primaryButtonBg} ${primaryButtonEnabledHoverBg}`
    : cannotPress
      ? ON_PAGE.refusedLive
      : ON_PAGE.live;
  return (
    <div
      className={`relative inline-flex shrink-0 rounded-lg border shadow-xs ${frameLook}`}
      ref={groupRef}
    >
      <button
        // While nothing can be sent it is `disabled` alone, on both sides of `NoTooltips`.
        aria-disabled={actionRefused || !actionRefusedHoldingFocus ? undefined : true}
        aria-describedby={actionDescribedBy}
        className={`${HALF} rounded-l-[7px] pr-3.5 pl-4 text-sm font-semibold ${actionLook}`}
        data-ask-action=""
        disabled={actionRefused}
        onClick={close}
        title={tooltips ? actionTitle : undefined}
        type="submit"
      >
        {action}
      </button>
      <span aria-hidden="true" className={`w-px self-stretch ${ruleLook}`} />
      <button
        aria-expanded={menuOpen}
        aria-haspopup="menu"
        aria-label="More actions"
        className={`${HALF} flex items-center rounded-r-[7px] px-2.5 ${chevronLook}`}
        data-ask-menu-button=""
        onClick={() => onMenuOpenChange(!menuOpen)}
        onKeyDown={(event) => {
          if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
          event.preventDefault();
          onMenuOpenChange(true);
        }}
        // A press here must not take the reader's selection of words, which Define reads.
        onMouseDown={(event) => event.preventDefault()}
        ref={chevronRef}
        title={tooltips ? "More actions" : undefined}
        type="button"
      >
        <ChevronIcon expanded={menuOpen} />
      </button>
      {menuOpen ? (
        <AnchoredPanel
          anchorRef={groupRef}
          className={`z-50 w-60 rounded-lg border p-1 shadow-lg outline-none ${tooltips ? `${card} ${borderDefault}` : ON_PAGE.menu}`}
          flip
          label="More actions"
          onDismiss={close}
          role="menu"
        >
          {/* biome-ignore lint/a11y/noStaticElementInteractions: the keys of the menu around it (the panel is its `role="menu"`): arrows walk its items and each item's key runs it; the items are its buttons */}
          <div
            data-ask-menu=""
            onKeyDown={(event) => {
              const list = menuItems();
              const at = event.target instanceof HTMLElement ? list.indexOf(event.target) : -1;
              if (event.key === "ArrowDown" || event.key === "ArrowUp") {
                event.preventDefault();
                const step = event.key === "ArrowDown" ? 1 : -1;
                list[(at + step + list.length) % list.length]?.focus();
                return;
              }
              if (event.key === "Home" || event.key === "End") {
                event.preventDefault();
                (event.key === "Home" ? list[0] : list.at(-1))?.focus();
                return;
              }
              if (event.key === "Tab") {
                close();
                return;
              }
              const item = shown.flat().find((one) => one.hotkey === event.key);
              if (item !== undefined && !event.metaKey && !event.ctrlKey && !event.altKey) {
                event.preventDefault();
                event.stopPropagation();
                run(item);
              }
            }}
          >
            {shown.map((group, index) => (
              <div key={group[0]?.id ?? index}>
                {index === 0 ? null : (
                  <div
                    aria-hidden="true"
                    className={`mx-1 my-1 h-px ${tooltips ? menuRule : ON_PAGE.menuRule}`}
                  />
                )}
                {group.map((item) => {
                  // Inside `NoTooltips` why a refused item cannot run is its description, said
                  // after its name, in place of its tooltip.
                  const described = !tooltips && item.refused !== undefined;
                  return (
                    <button
                      aria-describedby={described ? `${reasonId}-${item.id}` : undefined}
                      aria-disabled={item.refused !== undefined}
                      // The key is drawn beside the label and told to a screen reader as the
                      // item's shortcut, so the item's name is its label alone.
                      aria-keyshortcuts={item.hotkey === "Esc" ? "Escape" : item.hotkey}
                      aria-label={item.label}
                      className={`group flex min-h-11 w-full items-center justify-between gap-6 rounded-md px-2.5 text-left text-sm outline-none sm:min-h-9 ${tooltips ? `${textSecondaryOnSurface} ${menuItemActiveBg} ${menuItemRefusedText}` : ON_PAGE.item}`}
                      data-ask-menu-item={item.id}
                      key={item.id}
                      onClick={() => run(item)}
                      role="menuitem"
                      title={tooltips ? item.refused : undefined}
                      type="button"
                    >
                      <span>{item.label}</span>
                      <kbd
                        className={`min-w-5 rounded px-1.5 py-0.5 text-center font-mono text-xs font-medium group-aria-disabled:opacity-50 ${tooltips ? kbdHint : ON_PAGE.key}`}
                      >
                        {item.hotkey}
                      </kbd>
                      {described ? (
                        <span className="sr-only" id={`${reasonId}-${item.id}`}>
                          {item.refused}
                        </span>
                      ) : null}
                    </button>
                  );
                })}
              </div>
            ))}
          </div>
        </AnchoredPanel>
      ) : null}
    </div>
  );
}
