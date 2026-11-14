import { type ReactNode, useEffect, useRef } from "react";

import { AnchoredPanel } from "../../components/AnchoredPanel";
import { ChevronIcon } from "../../components/DisclosureToggle";
import {
  borderDefault,
  card,
  kbdHint,
  menuItemActiveBg,
  menuItemRefusedText,
  menuRule,
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

/** One of the card's other actions, as its menu lists it: what it is called, the key that runs
 *  it with the card in hand, and why it cannot run now, which the item's tooltip says. */
export interface AskMenuItem {
  id: string;
  label: string;
  hotkey: string;
  /** Why the item cannot run now; set, the item is refused and says this as its tooltip. */
  refused?: string;
  run: () => void;
}

/** Each half is 42 px inside the frame's 1 px border: the 44 px tap target. */
const HALF = "min-h-[42px]";
/** The open menu's items, in the page: the panel is portalled out of the control. */
const MENU_ITEMS = "[data-ask-menu] [role=menuitem]";

/**
 * The card's one control for what it does: its action (Answer, Ask back, Save answer) and, on
 * the same button, a chevron that opens every other action of the card in one menu, each with
 * its key. It is a split button: two halves of one shape with a thin light rule between them.
 * While the action cannot be pressed the control is an outline with the action in the disabled
 * shade and the chevron live, since the menu's actions do not wait on an answer; it fills once
 * the action can be pressed.
 *
 * The menu groups its items (`groups`, each drawn under a rule from the one before) and is
 * walked with the arrow keys; each item's own key runs it from the open menu as well.
 */
export function AskSplitButton({
  action,
  actionDescribedBy,
  actionRefused,
  actionTitle,
  groups,
  menuOpen,
  onMenuOpenChange,
}: {
  action: string;
  actionDescribedBy?: string;
  /** The action cannot be pressed now (no pick yet, a write out). */
  actionRefused: boolean;
  actionTitle?: string;
  groups: readonly (readonly AskMenuItem[])[];
  menuOpen: boolean;
  onMenuOpenChange: (open: boolean) => void;
}): ReactNode {
  const groupRef = useRef<HTMLDivElement>(null);
  const chevronRef = useRef<HTMLButtonElement>(null);
  const shown = groups.filter((group) => group.length > 0);
  const close = () => onMenuOpenChange(false);
  const run = (item: AskMenuItem) => {
    if (item.refused !== undefined) return;
    close();
    item.run();
  };
  const items = () => [...document.querySelectorAll<HTMLElement>(MENU_ITEMS)];
  // The panel focuses its first control as it opens; the menu moves that on to its first item
  // that can run, so Enter at once never presses a refused one.
  useEffect(() => {
    if (!menuOpen) return;
    [...document.querySelectorAll<HTMLElement>(MENU_ITEMS)]
      .find((item) => item.getAttribute("aria-disabled") !== "true")
      ?.focus();
  }, [menuOpen]);
  return (
    <div
      className={`relative inline-flex shrink-0 rounded-lg border shadow-xs ${actionRefused ? splitButtonRefusedFrame : splitButtonLiveFrame}`}
      ref={groupRef}
    >
      <button
        aria-describedby={actionDescribedBy}
        aria-disabled={actionRefused}
        className={`${HALF} rounded-l-[7px] pr-3.5 pl-4 text-sm font-semibold ${actionRefused ? splitButtonRefusedAction : `${primaryButtonBg} ${primaryButtonEnabledHoverBg}`}`}
        data-ask-action=""
        onClick={(event) => {
          if (actionRefused) event.preventDefault();
          close();
        }}
        title={actionTitle}
        type="submit"
      >
        {action}
      </button>
      <span
        aria-hidden="true"
        className={`w-px self-stretch ${actionRefused ? splitButtonRuleRefused : splitButtonRule}`}
      />
      <button
        aria-expanded={menuOpen}
        aria-haspopup="menu"
        aria-label="More actions"
        className={`${HALF} flex items-center rounded-r-[7px] px-2.5 ${actionRefused ? splitButtonRefusedLive : `${primaryButtonBg} ${primaryButtonEnabledHoverBg}`}`}
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
        title="More actions"
        type="button"
      >
        <ChevronIcon expanded={menuOpen} />
      </button>
      {menuOpen ? (
        <AnchoredPanel
          anchorRef={groupRef}
          className={`z-50 w-60 rounded-lg border p-1 shadow-lg outline-none ${card} ${borderDefault}`}
          flip
          label="More actions"
          onDismiss={close}
          role="menu"
        >
          {/* biome-ignore lint/a11y/noStaticElementInteractions: the keys of the menu around it (the panel is its `role="menu"`): arrows walk its items and each item's key runs it; the items are its buttons */}
          <div
            data-ask-menu=""
            onKeyDown={(event) => {
              const list = items();
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
                  <div aria-hidden="true" className={`mx-1 my-1 h-px ${menuRule}`} />
                )}
                {group.map((item) => (
                  <button
                    aria-disabled={item.refused !== undefined}
                    className={`group flex min-h-11 w-full items-center justify-between gap-6 rounded-md px-2.5 text-left text-sm outline-none sm:min-h-9 ${textSecondaryOnSurface} ${menuItemActiveBg} ${menuItemRefusedText}`}
                    data-ask-menu-item={item.id}
                    key={item.id}
                    onClick={() => run(item)}
                    role="menuitem"
                    title={item.refused}
                    type="button"
                  >
                    <span>{item.label}</span>
                    <kbd
                      className={`min-w-5 rounded px-1.5 py-0.5 text-center font-mono text-xs font-medium group-aria-disabled:opacity-50 ${kbdHint}`}
                    >
                      {item.hotkey}
                    </kbd>
                  </button>
                ))}
              </div>
            ))}
          </div>
        </AnchoredPanel>
      ) : null}
    </div>
  );
}
