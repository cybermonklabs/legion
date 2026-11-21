import { type ReactNode, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { SearchBox } from "../../components/SearchBox";
import {
  backdrop50,
  borderDefault,
  card,
  kbdHint,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { isPlainClick } from "../refs/picture-lightbox";
import {
  ALT_KEY_LABEL,
  ariaKeyshortcuts,
  type KeyBindingDescription,
  type KeymapScope,
  MOD_KEY_LABEL,
  stepsOf,
} from "./keymap";
import { useCloseOnNavigation, useDialog } from "./useDialog";

/** The id of Settings' Keyboard shortcuts section, where a person changes these keys (R14): the
 *  help links to it, and the section takes the focus when the address names it. */
export const SHORTCUTS_SECTION_ID = "keyboard-shortcuts";

/** The glyph of each part of a key that has one: `$mod` and `Alt` as the platform names them, the
 *  arrows as arrows, and Escape as `Esc`. Every other part is drawn as it is written. */
const KEY_GLYPHS: Record<string, string> = {
  $mod: MOD_KEY_LABEL,
  Alt: ALT_KEY_LABEL,
  ArrowDown: "↓",
  ArrowLeft: "←",
  ArrowRight: "→",
  ArrowUp: "↑",
  Escape: "Esc",
};

/** Each step of a chord as shown (`$mod+k` → `⌘+k`), keyed by its position so `g g` renders twice,
 *  with the glyph of each of its parts (`["⌘", "k"]`), which the rail runs together (`RailButton`):
 *  every surface that draws a key draws it with these glyphs. */
export function keyTokens(keys: string): { key: string; parts: string[]; token: string }[] {
  return stepsOf(keys).map((combo, position) => {
    const parts = combo.split("+").map((part) => KEY_GLYPHS[part] ?? part);
    return { key: `${position}:${combo}`, parts, token: parts.join("+") };
  });
}

/** Every scope's heading in `?`; the compiler checks this record covers `KeymapScope`. */
export const SCOPE_LABELS: Record<KeymapScope, string> = {
  agents: "Agents",
  architecture: "Architecture",
  board: "Board",
  delivery: "Delivery",
  dialog: "Dialog",
  document: "Document",
  global: "Global",
  inbox: "Inbox",
  issue: "Issue",
  project: "Project",
};

/**
 * Section order in `?`: the shell's own scope, then page scopes outermost first, then dialogs.
 * Registration order would do, except React runs child layout effects first, so a page that
 * mounts straight into its Board would list `board` above `project`.
 *
 * A scope left out here is drawn nowhere, neither in `?` nor in Settings' Keyboard shortcuts,
 * which lists its scopes in this order, and no type catches it: `ShortcutHelp.test.tsx` holds
 * this list to the scopes `SCOPE_LABELS` names.
 */
export const SCOPE_ORDER: readonly KeymapScope[] = [
  "global",
  "project",
  "architecture",
  "board",
  "issue",
  "document",
  "inbox",
  "agents",
  "delivery",
  "dialog",
];

/**
 * Every registered shortcut, grouped by scope and live-filtered. `snapshot` is the registry as
 * it stood when `?` was pressed — with focus still on the row the user was on, so per-row
 * bindings report the right availability — and `null` while closed. Inactive rows are greyed.
 * A route change closes it, like the search palette. Its foot links to Settings' Keyboard
 * shortcuts, where the person changes these keys.
 */
export function ShortcutHelp({
  onClose,
  snapshot,
}: {
  onClose: () => void;
  snapshot: readonly KeyBindingDescription[] | null;
}): ReactNode {
  const open = snapshot !== null;
  const [filter, setFilter] = useState("");
  const filterRef = useRef<HTMLInputElement>(null);
  const dialog = useDialog<HTMLDivElement>({ initialFocusRef: filterRef, onClose, open });
  useCloseOnNavigation(open, onClose);

  if (snapshot === null) {
    return null;
  }

  const needle = filter.trim().toLowerCase();
  // A palette-only action has no keys; `?` is the list of keys, so it has nothing to show here.
  const keyed = snapshot.filter((entry) => entry.keys.length > 0);
  const shown =
    needle === ""
      ? keyed
      : keyed.filter((entry) =>
          `${entry.label} ${entry.keys.join(" ")} ${entry.scope}`.toLowerCase().includes(needle)
        );
  const scopes = SCOPE_ORDER.filter((scope) => shown.some((entry) => entry.scope === scope));

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop50}`} onClick={onClose} />
      <div className="pointer-events-none fixed inset-0 z-50 flex items-start justify-center xl:p-4 xl:pt-20">
        <div
          aria-label="Keyboard shortcuts"
          aria-modal="true"
          className={`pointer-events-auto flex max-h-full w-full flex-col overflow-hidden border shadow-2xl xl:max-w-2xl xl:rounded-lg ${card} ${borderDefault}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <div className={`flex items-center gap-3 border-b p-3 ${borderDefault}`}>
            <h2 className={`text-base font-semibold ${textPrimaryOnSurface}`}>
              Keyboard shortcuts
            </h2>
            <SearchBox
              className="min-w-0 flex-1"
              label="Filter shortcuts"
              onChange={setFilter}
              placeholder="Filter"
              ref={filterRef}
              value={filter}
            />
            <button
              className={`min-h-11 rounded-lg px-3 text-sm font-medium ${textMutedOnSurface}`}
              onClick={onClose}
              type="button"
            >
              Close
            </button>
          </div>
          <div className="overflow-y-auto p-3">
            {shown.length === 0 ? (
              <p className={`px-2 py-3 text-sm ${textMutedOnSurface}`}>No shortcuts match</p>
            ) : (
              scopes.map((scope) => (
                <section aria-label={SCOPE_LABELS[scope]} className="mb-4 last:mb-0" key={scope}>
                  <h3
                    className={`mb-1 px-2 text-xs font-semibold tracking-wide uppercase ${textMutedOnSurface}`}
                  >
                    {SCOPE_LABELS[scope]}
                  </h3>
                  <ul>
                    {shown
                      .filter((entry) => entry.scope === scope)
                      .map((entry) => (
                        <li
                          className={`flex min-h-11 items-center justify-between gap-4 rounded-lg px-2 py-1.5 text-sm ${
                            entry.enabled
                              ? textPrimaryOnSurface
                              : `opacity-50 ${textMutedOnSurface}`
                          }`}
                          data-enabled={entry.enabled}
                          key={`${entry.scope}-${entry.id}`}
                        >
                          <span>{entry.label}</span>
                          <KeyHints keys={entry.keys} />
                        </li>
                      ))}
                  </ul>
                </section>
              ))
            )}
          </div>
          <p className={`border-t px-5 py-1 text-sm ${borderDefault}`}>
            {/* A plain click closes the help before the move to Settings, so the focus the help
                hands back as it closes lands before the section takes it; a click that opens a
                new tab leaves the help open here. */}
            <Link
              className={`inline-flex min-h-11 items-center font-medium ${linkText} ${linkHoverText}`}
              onClick={(event) => {
                if (isPlainClick(event.nativeEvent)) onClose();
              }}
              to={`/settings#${SHORTCUTS_SECTION_ID}`}
            >
              Change these keys in Settings
            </Link>
          </p>
        </div>
      </div>
    </>
  );
}

/** The class of a `<kbd>` that draws a key: in the help, the palette, the chord hint and
 *  Settings. */
export const kbdClass = `rounded px-1.5 py-0.5 font-mono text-xs font-medium ${kbdHint}`;

/** The class of the `<kbd>` that draws the digit beside a menu's choice or a card's option. */
export const digitKbdClass = `rounded px-1.5 font-mono text-xs ${kbdHint}`;

/**
 * The keys that run a binding, as `?` and the palette both show them: a run of single keys in a
 * row (`["1", …, "9"]`, `["0", …, "3"]`) collapses to `1 – 9`, with a `0` after `9` read as the
 * tenth (`1 – 9, 0`); other alternatives are listed with "or", and no keys renders nothing.
 */
export function KeyHints({ keys }: { keys: readonly string[] }): ReactNode {
  // The digits of a list of options take `0` after `9`, as the tenth.
  const tenth = keys.length > 3 && keys[keys.length - 1] === "0" && keys[keys.length - 2] === "9";
  const run = tenth ? keys.slice(0, -1) : keys;
  const first = run[0];
  const last = run[run.length - 1];
  const consecutive =
    run.length > 2 &&
    first !== undefined &&
    last !== undefined &&
    run.every(
      (key, index) => key.length === 1 && key.charCodeAt(0) === first.charCodeAt(0) + index
    );
  if (consecutive) {
    return (
      <span className="flex shrink-0 items-center gap-1">
        <kbd className={kbdClass}>{first}</kbd>
        <span className={`text-xs ${textMutedOnSurface}`}>–</span>
        <kbd className={kbdClass}>{last}</kbd>
        {tenth ? (
          <>
            <span className={`text-xs ${textMutedOnSurface}`}>,</span>
            <kbd className={kbdClass}>0</kbd>
          </>
        ) : null}
      </span>
    );
  }
  return (
    <span className="flex shrink-0 items-center gap-2">
      {keys.map((chord, alternative) => (
        <span className="flex items-center gap-1" key={chord}>
          {alternative === 0 ? null : <span className={`text-xs ${textMutedOnSurface}`}>or</span>}
          {keyTokens(chord).map(({ key, token }) => (
            <kbd className={kbdClass} key={key}>
              {token}
            </kbd>
          ))}
        </span>
      ))}
    </span>
  );
}

/** The class of a `<kbd>` of a control's key hint: `kbdClass`, with its key drawn by its
 *  `::before` from its `data-key`. */
const hintKbdClass = `${kbdClass} before:content-[attr(data-key)]`;

/**
 * The first key of the action a control runs, drawn inside the control beside its label (the
 * reading pane's head, R9), so a reader sees which key also presses it: the person's own key as
 * the host reads it (`useActionKeys`), and nothing for an action with no key. It is for the eye
 * alone: the control names each of its keys in its `aria-keyshortcuts` (`keyshortcutsOf`), so the
 * hint is hidden from assistive tech and stays out of the control's accessible name.
 *
 * Each key is CSS generated content, drawn from its `kbd`'s `data-key`, and no text of the page:
 * the words a control holds, `aria-hidden` ones too, are what WCAG 2.5.3 (Label in Name) holds to
 * its name, as axe's `label-content-name-mismatch` reads them, so a control named by its
 * `aria-label` ("Snooze CORE-1") holds its label's words alone. The browser puts generated
 * content into a name computed from content, so the hint's `aria-hidden` is what keeps the key
 * out of a name such as Approve's.
 */
export function ControlKeyHint({ keys }: { keys: readonly string[] | undefined }): ReactNode {
  const first = keys?.[0];
  if (first === undefined) return null;
  return (
    <span aria-hidden="true" className="ml-1.5 inline-flex shrink-0 items-center gap-1">
      {keyTokens(first).map(({ key, token }) => (
        <kbd className={hintKbdClass} data-key={token} key={key} />
      ))}
    </span>
  );
}

/** The `aria-keyshortcuts` of a control that `keys` also press, or nothing where no key does:
 *  the attribute with an empty value names no key, so it is left off. */
export function keyshortcutsOf(keys: readonly string[] | undefined): string | undefined {
  const named = keys === undefined ? "" : ariaKeyshortcuts(keys);
  return named === "" ? undefined : named;
}
