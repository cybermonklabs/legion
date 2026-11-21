import { createContext, useContext, useLayoutEffect, useRef, useSyncExternalStore } from "react";

import {
  type CatalogueScope,
  catalogueEntry,
  type KeymapActionId,
  keysOfAction,
  NO_PERSON_KEYS,
  type PersonKeys,
} from "./keymap-catalogue";

/**
 * One keyboard registry for the SPA. Components register bindings under a scope
 * (`useKeymap`), pages and dialogs push their scope onto the shell-owned stack
 * (`useKeymapScope`), and a single `window` keydown listener (`KeymapProvider`) resolves each
 * key top-down through the stack: the first scope holding a matching binding wins, and while
 * a `dialog` scope is on the stack it is the only scope consulted. `keys` follows tinykeys' string
 * syntax — `"g i"` is a chord, `"$mod+k"` matches Meta or Control, `"Shift+P"` a shifted
 * letter, `"?"` the character an unshifted or shifted key produces. A combo is matched by the
 * character its key types, so a reader on AZERTY or Dvorak presses the letter a shortcut names
 * wherever it sits; on a Mac, one that holds `Alt` also matches by the key's position (`KeyM` for
 * `m`), because Option changes the character: `$mod+Alt+m` types `µ` there.
 *
 * Every page's binding names an action of the catalogue (`keymap-catalogue.ts`), which gives it
 * its id, its label and its keys, the person's where they changed them (`keymap-keys.ts`); only
 * a dialog's own Escape, the command bar's own close (on the person's keys for Search and
 * actions), the `y` that copies a focused link's address (`link-copy.ts`) and tests write a
 * binding out whole.
 */

/** A scope of the stack: each scope of the catalogue, and `dialog`, which every open dialog
 *  pushes and which alone is consulted while it is on the stack. */
export type KeymapScope = CatalogueScope | "dialog";

export interface KeyBinding {
  /** Stable identifier, unique within the registering component. */
  id: string;
  /**
   * One or more alternatives in tinykeys syntax, e.g. `["j", "ArrowDown"]`. `[]` is a
   * palette-only action: no key press ever matches it, and it is offered by `actions()` alone.
   */
  keys: string | readonly string[];
  label: string;
  run: (event: KeyboardEvent) => void;
  /** Whether the binding applies right now; `false` neither fires nor shadows lower scopes. */
  when?: () => boolean;
  /** Fires while a control that takes typed text has focus - a `TEXTAREA`, a `SELECT`, a
   *  contentEditable element, a text-entry `INPUT`, or an option list's trigger
   *  (`aria-haspopup="listbox"`, `MultiSelect`'s), open or closed - and while the focus is
   *  anywhere inside an open panel (`PANEL_ATTRIBUTE`). The key then reaches every such control,
   *  so a binding that belongs to only some of them says which in its `when`: the document's
   *  `$mod+Alt+m` runs only while its editor has focus and a selection, never from a comment
   *  composer or any other field. */
  inEditable?: boolean;
  /** Whether `⌘K` offers this binding as an action. The default is `true` for a binding with one
   *  key or none that is not `inEditable`, and `false` otherwise. A row presses only its first
   *  key, and a binding with several keys may pick what it does by the key pressed (the arrows,
   *  `1`–`9`), so one whose keys all do the same thing says `true`. An `inEditable` binding's key
   *  is one that has to work while the reader types - Escape, `$mod+k`, the document's
   *  `$mod+Alt+m` and `Alt+Enter` - and is a row only when it says `true`: Escape closes the
   *  palette, and `$mod+k` is its own key. Keys that walk a list with the row in hand set `false`,
   *  as does a second key for an action that already has a row; any other binding that sets it
   *  says why beside it. */
  palette?: boolean;
  /** Tried before every scope, the `dialog` scope included, and whatever the focus is on: the
   *  binding's `when` is its only guard. Only a press that completes one of its keys matches, so
   *  such a binding starts no chord. It is for the `y` that copies a focused link's address, which
   *  works on every page and in every dialog. */
  beforeScopes?: boolean;
}

/** A binding that names an action of the catalogue: the action gives its id, its label, its
 *  palette row and `inEditable`, and the person's keys or the defaults give its keys. */
export interface ActionBinding {
  action: KeymapActionId;
  run: (event: KeyboardEvent) => void;
  /** Whether the binding applies right now; `false` neither fires nor shadows lower scopes. */
  when?: () => boolean;
}

/** A binding as `?` describes it: keys as typed, its scope, and whether it applies right now. */
export interface KeyBindingDescription {
  enabled: boolean;
  id: string;
  keys: readonly string[];
  label: string;
  scope: KeymapScope;
}

/** A binding the palette offers as a row: its label, the keys that also run it, and the run. */
export interface KeymapAction {
  id: string;
  keys: readonly string[];
  label: string;
  run: () => void;
  scope: KeymapScope;
}

export const DIALOG_SCOPE: KeymapScope = "dialog";
/** Whether the browser says it runs on a Mac, read once as the app loads: the registry matches an
 *  Alt combo by the key's place there, and the key hints draw the Mac's glyphs. */
export const IS_MAC = navigator.userAgent.includes("Mac");
/** How `$mod` is shown to the reader: the Command glyph on a Mac, `Ctrl` elsewhere. */
export const MOD_KEY_LABEL = IS_MAC ? "⌘" : "Ctrl";
/** How `Alt` is shown to the reader: the Option glyph on a Mac, `Alt` elsewhere. */
export const ALT_KEY_LABEL = IS_MAC ? "⌥" : "Alt";
/** How long a chord waits for its next key (`g` then `i`). */
export const CHORD_TIMEOUT_MS = 1000;

/** A chord the reader has started: the keys typed so far (`"g"`), and each key that can follow
 *  with the label of the binding it completes, innermost scope first. */
export interface PendingChord {
  readonly keys: string;
  readonly next: readonly { readonly key: string; readonly label: string }[];
}

const NO_CHORD: PendingChord = { keys: "", next: [] };

/** One press of a key as the registry reads it: `$mod+k` is `{ key: "k", mod: true, … }`. */
export interface KeyCombo {
  alt: boolean;
  control: boolean;
  key: string;
  meta: boolean;
  mod: boolean;
  shift: boolean;
}

interface KeyPress {
  altKey: boolean;
  /** The physical key (`KeyM`), whatever character the layout and modifiers make it type. */
  code: string;
  ctrlKey: boolean;
  key: string;
  metaKey: boolean;
  shiftKey: boolean;
}

interface Registration {
  binding: KeyBinding;
  chords: readonly (readonly KeyCombo[])[];
  scope: KeymapScope;
  /** Each chord's steps as written (`["g", "i"]`), which the chord hint shows. */
  steps: readonly (readonly string[])[];
}

const MODIFIER_KEYS: Record<string, true> = {
  Alt: true,
  AltGraph: true,
  Control: true,
  Meta: true,
  Shift: true,
};

function parseCombo(text: string): KeyCombo {
  const parts = text.split("+");
  const key = parts.pop();
  if (key === undefined || key === "") {
    throw new Error(`Keymap: "${text}" has no key`);
  }
  const combo: KeyCombo = {
    alt: false,
    control: false,
    key,
    meta: false,
    mod: false,
    shift: false,
  };
  for (const part of parts) {
    switch (part) {
      case "$mod":
        combo.mod = true;
        break;
      case "Control":
      case "Ctrl":
        combo.control = true;
        break;
      case "Meta":
        combo.meta = true;
        break;
      case "Alt":
        combo.alt = true;
        break;
      case "Shift":
        combo.shift = true;
        break;
      default:
        throw new Error(`Keymap: unknown modifier "${part}" in "${text}"`);
    }
  }
  return combo;
}

/** The steps of one key as written: `"g i"` is `["g", "i"]`, a chord of two presses. */
export function stepsOf(keys: string): string[] {
  return keys.trim().split(/\s+/);
}

/** The presses of one key as the registry matches them: `"g i"` is two. Throws on text that
 *  names no key or an unknown modifier, as `register` does. */
export function parseChord(keys: string): KeyCombo[] {
  return stepsOf(keys).map(parseCombo);
}

function keysOf(binding: KeyBinding): readonly string[] {
  return typeof binding.keys === "string" ? [binding.keys] : binding.keys;
}

/** The physical key a letter or digit names, by its place on a US keyboard: `KeyM` for `m`,
 *  `Digit1` for `1`. */
function physicalCode(key: string): string | undefined {
  if (/^[a-z]$/i.test(key)) {
    return `Key${key.toUpperCase()}`;
  }
  return /^[0-9]$/.test(key) ? `Digit${key}` : undefined;
}

/** Presses that name no key a binding can hold: a key that only changes the next press (a dead
 *  key, a lock), one the browser cannot name, and `+`, which the key text writes between a
 *  modifier and its key. */
const UNNAMED_KEYS: Record<string, true> = {
  "+": true,
  CapsLock: true,
  Dead: true,
  Fn: true,
  FnLock: true,
  NumLock: true,
  Process: true,
  ScrollLock: true,
  Unidentified: true,
};

/**
 * The key text of one press, as a binding writes it and `parseCombo` reads it back: `Shift+B`,
 * `$mod+k`, `?`, `Space`. A modifier pressed alone, and a key that names nothing a binding can
 * hold (`UNNAMED_KEYS`, a space other than the space bar), is no press, and gives null. The
 * platform's command key is `$mod` (Cmd on a Mac, Ctrl elsewhere) and the other one is named. A
 * letter is lower case, and upper case with Shift; a character with no case already says whether
 * Shift was held (`?` is Shift+/), so its text leaves Shift out, as `matchesCombo` does. On a Mac
 * a press with Alt names its letter or digit by the key's place, as `matchesCombo` matches it,
 * because Option changes the character: Option+M types `µ`, and is `Alt+m`. That comes before
 * the test for a key with no name, since Option with E, I, U or N types a dead key
 * (`key: "Dead"`): Option+E is `Alt+e`.
 */
export function comboOf(
  press: Pick<KeyboardEvent, "altKey" | "code" | "ctrlKey" | "key" | "metaKey" | "shiftKey">,
  isMac: boolean = IS_MAC
): string | null {
  if (MODIFIER_KEYS[press.key] === true) return null;
  // The letter or digit the key's place names on a US keyboard (`KeyM` is `m`), which is
  // `physicalCode` read the other way.
  const placed =
    press.altKey && isMac
      ? (/^Key([A-Z])$/.exec(press.code)?.[1]?.toLowerCase() ??
        /^Digit([0-9])$/.exec(press.code)?.[1])
      : undefined;
  if (placed === undefined && UNNAMED_KEYS[press.key] === true) return null;
  let key = placed ?? (press.key === " " ? "Space" : press.key);
  if (/\s/.test(key)) return null;
  const cased = key.length === 1 && key.toLowerCase() !== key.toUpperCase();
  if (cased) key = press.shiftKey ? key.toUpperCase() : key.toLowerCase();
  const modifiers = [
    (isMac ? press.metaKey : press.ctrlKey) ? "$mod" : undefined,
    isMac && press.ctrlKey ? "Control" : undefined,
    !isMac && press.metaKey ? "Meta" : undefined,
    press.altKey ? "Alt" : undefined,
    press.shiftKey && (cased || key.length > 1) ? "Shift" : undefined,
  ];
  return [...modifiers.filter((part) => part !== undefined), key].join("+");
}

/** Whether `press` is a press of `combo`. Its Alt must equal the combo's, so on a layout that
 *  types `[` or `]` with AltGr or Option (German and French among them) the press can carry Alt
 *  and match no combo written without it, such as the issue page's `[` and `]`; the reader's page
 *  of keys (`docs/site/src/content/docs/dispatch/keyboard.md`) says to give those two actions
 *  other keys in Settings. */
function matchesCombo(combo: KeyCombo, press: KeyPress, isMac: boolean): boolean {
  const sameCharacter =
    press.key.length === 1
      ? press.key.toLowerCase() === combo.key.toLowerCase()
      : press.key === combo.key;
  const samePosition = combo.alt && isMac && press.code === physicalCode(combo.key);
  if (!(sameCharacter || samePosition) || combo.alt !== press.altKey) {
    return false;
  }
  // A single non-letter character (`?`, `/`, `1`) already encodes whether Shift was held.
  const shiftEncodedInKey =
    combo.key.length === 1 && combo.key.toLowerCase() === combo.key.toUpperCase();
  if (!shiftEncodedInKey && combo.shift !== press.shiftKey) {
    return false;
  }
  if (combo.mod) {
    return press.metaKey || press.ctrlKey;
  }
  return combo.control === press.ctrlKey && combo.meta === press.metaKey;
}

/** Whether two combos can be produced by one key press (`$mod+k` overlaps `Control+k`). */
export function sameCombo(a: KeyCombo, b: KeyCombo): boolean {
  if (a.key.toLowerCase() !== b.key.toLowerCase() || a.alt !== b.alt || a.shift !== b.shift) {
    return false;
  }
  if (a.mod || b.mod) {
    return (a.mod || a.control || a.meta) && (b.mod || b.control || b.meta);
  }
  return a.control === b.control && a.meta === b.meta;
}

/** Whether the presses of `shorter` begin `longer` (`g` begins `g i`). */
export function isStrictPrefix(shorter: readonly KeyCombo[], longer: readonly KeyCombo[]): boolean {
  return (
    shorter.length < longer.length &&
    shorter.every((combo, index) => sameCombo(combo, longer[index] as KeyCombo))
  );
}

/** The attribute every open panel's element carries (`AnchoredPanel` writes it): what tells the
 *  registry a key was pressed inside one (`isEditableTarget`). */
export const PANEL_ATTRIBUTE = "data-anchored-panel";
const IN_PANEL = `[${PANEL_ATTRIBUTE}]`;
/** An option list's trigger: the button that names the `listbox` it opens (`MultiSelect`). */
const LIST_TRIGGER = '[aria-haspopup="listbox"]';

/** The `<input>` types whose value is typed text alone, so that their `change` is the commit on
 *  blur of what the reader typed, which Chromium fires as an update takes the focused field away.
 *  `TEXT_INPUT_TYPES` holds these and the date and time types besides, since a digit typed in
 *  one of those edits its value too; but a date or time box fires its `change` at the reader's
 *  pick - in its picker, or with the key that completes the value - as a `<select>` or a
 *  checkbox does, and not on blur (`commitsOnBlur`). */
const COMMIT_ON_BLUR_TYPES: Record<string, true> = {
  email: true,
  number: true,
  password: true,
  search: true,
  tel: true,
  text: true,
  url: true,
};

/** The `<input>` types that take typed text: those whose value is typed text alone
 *  (`COMMIT_ON_BLUR_TYPES`), and the date and time types, whose value a digit typed edits.
 *  Everything else an input can be - checkbox, radio, the button family, file, range, color -
 *  takes no text, so a letter or digit pressed while one has focus on the page is a shortcut, not
 *  typing: ticking a checkbox with the pointer must not silence the page's keys.
 *  `HTMLInputElement.type` normalises a missing or unknown attribute to `text`. */
const TEXT_INPUT_TYPES: Record<string, true> = {
  ...COMMIT_ON_BLUR_TYPES,
  date: true,
  "datetime-local": true,
  month: true,
  time: true,
  week: true,
};

/** Whether `element` takes typed text: a content-editable element, an `<input>` of a text-entry
 *  type (`TEXT_INPUT_TYPES`), or a `<textarea>`. A key pressed on one is the control's own
 *  (`isEditableTarget`), and the page's focus rescue reads a character typed in one as no press
 *  of the reader's (`focus-place.ts`'s `isPress`), since it only edits the field. */
export function takesText(element: Element): boolean {
  if (element instanceof HTMLElement && element.isContentEditable) {
    return true;
  }
  // An `<input>` takes typed text only in the text-entry types; a checkbox, radio or button
  // takes none.
  if (element instanceof HTMLInputElement) {
    return TEXT_INPUT_TYPES[element.type] === true;
  }
  return element.tagName === "TEXTAREA";
}

/** Whether a `change` on `element` is the commit on blur of the text typed in it, rather than the
 *  reader's pick: on an `<input>` whose value is typed text alone (`COMMIT_ON_BLUR_TYPES`), a
 *  `<textarea>`, or a content-editable element. Every other control fires its `change` at the
 *  pick - a `<select>`, a checkbox, a radio, a file, range or color input, a date or time box -
 *  so the page's focus rescue reads that `change` as a press of the reader's (`focus-place.ts`'s
 *  `isPress`). An `<input>` of the button family or of type hidden fires no `change`; it is
 *  answered false, so a `change` on one would read as a pick. */
export function commitsOnBlur(element: Element): boolean {
  if (element instanceof HTMLInputElement) {
    return COMMIT_ON_BLUR_TYPES[element.type] === true;
  }
  // Off an `<input>`, an element takes typed text (`takesText`) exactly where a `change` on it is
  // the commit on blur: a `<textarea>` or a content-editable element.
  return takesText(element);
}

/**
 * Whether a key pressed on `target` is the control's own, so that only the bindings that fire
 * while the reader types (`inEditable`) may take it: a control that takes typed text
 * (`takesText`), a `<select>`, the trigger of an option list, or anything at all inside an open
 * panel.
 *
 * A panel - the display menu, a filter panel and the lists opened from it, a column filter -
 * stands over the page in place of the selects it replaced, and its keys are its own whatever
 * control of it holds the focus: Tab and Shift+Tab walk it, the arrows walk a group of radios,
 * Space ticks a box, and Escape closes it (`AnchoredPanel` takes Escape before this registry
 * hears it). A radio or a checkbox takes no text, so without this rule `j` on the display menu's
 * radio would walk the table behind the panel, and the Inbox's single keys would act on its
 * rows. Only the key's own target counts: a panel standing open while the focus is on the page -
 * the Board's, restored on load - masks nothing.
 *
 * An option list's trigger (`MultiSelect`'s, the one control that names a `listbox` popup) stands
 * in for the `<select>` or the text box it replaced, closed as much as open, so it takes the keys
 * a select takes: a digit on the issue header's Assignee is no priority, and `c` on a Settings
 * project list opens no Create issue.
 */
function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) {
    return false;
  }
  if (takesText(target) || target.closest(IN_PANEL) !== null) {
    return true;
  }
  // An `<input>` that takes no text - a checkbox, radio or button - leaves its keys to the page,
  // so a page's single-key shortcuts keep working while one has focus. That is a permission with
  // an obligation attached: the registry stops guarding those controls, so a page whose key
  // writes something must say for itself where the reader may press it (`IssuePage`'s
  // `outsideInputsAndAskCards`, the Inbox's `outsideAskCard`).
  if (target instanceof HTMLInputElement) {
    return false;
  }
  // A `<select>` stays editable: its own keys type-ahead and step through the options. A list's
  // trigger is read as one.
  return target.tagName === "SELECT" || target.matches(LIST_TRIGGER);
}

export interface KeymapOptions {
  /** Whether Alt combos may match a physical key; production follows the browser's Mac identity. */
  isMac?: boolean;
  /** Timer seam for the chord timeout; tests drive it without real waits. */
  clearTimeout?: (handle: number) => void;
  setTimeout?: (callback: () => void, ms: number) => number;
  /** Throw on a prefix conflict (development); otherwise report it and keep going. */
  strict?: boolean;
}

export interface Keymap {
  /**
   * The palette's rows: every enabled binding of the scopes beneath the innermost dialog whose
   * `palette` is `true` and that is not `beforeScopes`, innermost scope first. Unset, `palette`
   * is `false` for a binding with several keys or one that fires in an editable
   * (`KeyBinding.palette`). A row runs its binding only if the binding is still registered and
   * its `when()` still holds when the row is chosen.
   * A binding several mounted lists register under one scope and id (the project row keys of
   * `/priorities`) is one row, which runs the registration that applies when it is chosen.
   */
  actions(): KeymapAction[];
  /** The unfinished chord, or one with no keys; the same object until the chord changes. */
  chord(): PendingChord;
  /** Every registered binding with its `when()` evaluated now — the source for `?`. One component
   *  mounted twice registers its bindings twice (an issue's hidden Spec beside another of its
   *  documents), and they are one row, enabled when either instance's `when()` holds. */
  describe(): KeyBindingDescription[];
  handleKeyDown(event: KeyboardEvent): void;
  /** The keys typed so far of an unfinished chord (`"g"`), or `""`. */
  pending(): string;
  pushScope(scope: KeymapScope): () => void;
  register(scope: KeymapScope, bindings: readonly KeyBinding[]): () => void;
  subscribe(listener: () => void): () => void;
}

interface Resolution {
  exact: Registration | undefined;
  partial: boolean;
}

export function createKeymap(options: KeymapOptions = {}): Keymap {
  const isMac = options.isMac ?? IS_MAC;
  const schedule = options.setTimeout ?? ((callback, ms) => window.setTimeout(callback, ms));
  const unschedule = options.clearTimeout ?? ((handle) => window.clearTimeout(handle));
  const strict = options.strict ?? false;

  const registrations: Registration[] = [];
  const scopes: KeymapScope[] = [];
  const listeners = new Set<() => void>();
  let pending: KeyPress[] = [];
  /** Whether the press that started the chord was the control's own (`isEditableTarget`), which
   *  decides, as it does for that press, which bindings the hint lists as its next keys. */
  let pendingEditable = false;
  let pendingTimer: number | undefined;
  let chord: PendingChord = NO_CHORD;

  const matches = (combos: readonly KeyCombo[], presses: readonly KeyPress[]) =>
    presses.every((press, index) => matchesCombo(combos[index] as KeyCombo, press, isMac));

  /** The scopes a key is read through, outermost first. A modal owns the keyboard: nothing
   *  beneath it fires, however the scopes are ordered. */
  const consulted = (): readonly KeymapScope[] =>
    scopes.includes(DIALOG_SCOPE) ? [DIALOG_SCOPE] : ["global", ...scopes];

  /** The keys that can follow `presses`, over the scopes `resolve` reads, innermost first, one
   *  entry for each next key: the innermost binding's label, as the innermost one would fire. */
  const nextSteps = (presses: readonly KeyPress[], editable: boolean): PendingChord["next"] => {
    const stack = consulted();
    const next: { key: string; label: string }[] = [];
    const listed = new Set<string>();
    for (let depth = stack.length - 1; depth >= 0; depth -= 1) {
      for (const { binding, chords, scope, steps } of registrations) {
        if (
          scope !== stack[depth] ||
          binding.beforeScopes === true ||
          (editable && binding.inEditable !== true)
        ) {
          continue;
        }
        // The keys this binding's chords take next, read before its `when`: few bindings
        // continue the chord pressed, and a `when` reads the page.
        const keys: string[] = [];
        chords.forEach((combos, alternative) => {
          const key = steps[alternative]?.[presses.length];
          if (key !== undefined && combos.length > presses.length && matches(combos, presses)) {
            keys.push(key);
          }
        });
        if (keys.length === 0 || binding.when?.() === false) {
          continue;
        }
        for (const key of keys) {
          if (listed.has(key)) continue;
          listed.add(key);
          next.push({ key, label: binding.label });
        }
      }
    }
    return next;
  };

  const notify = () => {
    chord =
      pending.length === 0
        ? NO_CHORD
        : {
            keys: pending.map((press) => press.key).join(" "),
            next: nextSteps(pending, pendingEditable),
          };
    for (const listener of listeners) {
      listener();
    }
  };

  const clearPending = () => {
    if (pendingTimer !== undefined) {
      unschedule(pendingTimer);
      pendingTimer = undefined;
    }
    if (pending.length > 0) {
      pending = [];
      notify();
    }
  };

  const reportConflict = (message: string) => {
    if (strict) {
      throw new Error(message);
    }
    console.error(message);
  };

  const checkPrefixConflicts = (incoming: readonly Registration[]) => {
    const existing = [...registrations, ...incoming];
    for (const candidate of incoming) {
      for (const other of existing) {
        if (other === candidate) {
          continue;
        }
        for (const chord of candidate.chords) {
          for (const otherChord of other.chords) {
            if (isStrictPrefix(chord, otherChord) || isStrictPrefix(otherChord, chord)) {
              reportConflict(
                `Keymap: "${candidate.binding.id}" (${candidate.scope}) and "${other.binding.id}" (${other.scope}) conflict — one key sequence is a prefix of the other`
              );
            }
          }
        }
      }
    }
  };

  const resolve = (presses: readonly KeyPress[], editable: boolean): Resolution => {
    // Before every scope: a binding that holds on every page and in every dialog, guarded by its
    // `when` alone (`KeyBinding.beforeScopes`).
    for (const registration of registrations) {
      const { binding } = registration;
      if (
        binding.beforeScopes === true &&
        registration.chords.some(
          (combos) => combos.length === presses.length && matches(combos, presses)
        ) &&
        binding.when?.() !== false
      ) {
        return { exact: registration, partial: false };
      }
    }
    const stack = consulted();
    for (let depth = stack.length - 1; depth >= 0; depth -= 1) {
      const scope = stack[depth];
      let exact: Registration | undefined;
      let partial = false;
      for (const registration of registrations) {
        const { binding } = registration;
        if (
          registration.scope !== scope ||
          binding.beforeScopes === true ||
          (editable && binding.inEditable !== true)
        ) {
          continue;
        }
        for (const chord of registration.chords) {
          if (chord.length < presses.length || !matches(chord, presses)) {
            continue;
          }
          if (chord.length > presses.length) {
            partial = true;
          } else if (exact === undefined && binding.when?.() !== false) {
            exact = registration;
          }
        }
      }
      if (exact !== undefined || partial) {
        return { exact, partial };
      }
    }
    return { exact: undefined, partial: false };
  };

  return {
    actions() {
      // Everything beneath the innermost dialog: the palette is itself a dialog, so this is the
      // page the reader is looking at. A scope pushed twice contributes one set of rows.
      const stack: KeymapScope[] = ["global", ...scopes];
      const dialog = stack.lastIndexOf(DIALOG_SCOPE);
      const beneath = dialog === -1 ? stack : stack.slice(0, dialog);
      const offered: KeymapAction[] = [];
      const seen = new Set<KeymapScope>();
      for (let depth = beneath.length - 1; depth >= 0; depth -= 1) {
        const scope = beneath[depth] as KeymapScope;
        if (seen.has(scope)) {
          continue;
        }
        seen.add(scope);
        const inScope: KeymapAction[] = [];
        // The registrations each row stands for: one, or each list that registers the same
        // action, of which the row runs the one that applies when it is chosen.
        const rows = new Map<string, Registration[]>();
        for (const registration of registrations) {
          const { binding } = registration;
          const keys = keysOf(binding);
          if (
            registration.scope !== scope ||
            binding.beforeScopes === true ||
            !(binding.palette ?? (keys.length <= 1 && binding.inEditable !== true)) ||
            binding.when?.() === false
          ) {
            continue;
          }
          const same = rows.get(binding.id);
          if (same !== undefined) {
            same.push(registration);
            continue;
          }
          const standsFor = [registration];
          rows.set(binding.id, standsFor);
          inScope.push({
            id: binding.id,
            keys,
            label: binding.label,
            // The rows are decided when the palette opens; by the time one is chosen its control
            // may have gone (another writer reopened the issue) or its component unmounted (the
            // page fell to its error view), so the row asks again.
            run: () => {
              const live = standsFor.find(
                (candidate) =>
                  registrations.includes(candidate) && candidate.binding.when?.() !== false
              );
              live?.binding.run(
                new KeyboardEvent("keydown", { key: keysOf(live.binding)[0] ?? "" })
              );
            },
            scope,
          });
        }
        // Registration order is mount timing — the same page reached by a navigation and by a
        // reload registers its scopes in a different order — so which row the palette highlights
        // first would follow the load path. Within a scope the rows read alphabetically instead:
        // collated, since a code-unit compare puts every capital before every lowercase letter
        // ("Go to Settings" before "Go to project…"). English collation, as the labels are, so the
        // order does not move with the reader's locale.
        inScope.sort((a, b) => a.label.localeCompare(b.label, "en"));
        offered.push(...inScope);
      }
      return offered;
    },
    chord() {
      return chord;
    },
    describe() {
      const described = new Map<string, KeyBindingDescription>();
      for (const { binding, scope } of registrations) {
        const enabled = binding.when?.() !== false;
        const key = `${scope}\u0000${binding.id}`;
        const seen = described.get(key);
        if (seen === undefined) {
          described.set(key, {
            enabled,
            id: binding.id,
            keys: keysOf(binding),
            label: binding.label,
            scope,
          });
        } else {
          seen.enabled ||= enabled;
        }
      }
      return [...described.values()];
    },
    handleKeyDown(event) {
      if (event.defaultPrevented || event.isComposing || MODIFIER_KEYS[event.key] === true) {
        return;
      }
      const editable = isEditableTarget(event.target);
      const press: KeyPress = {
        altKey: event.altKey,
        code: event.code,
        ctrlKey: event.ctrlKey,
        // The space bar types `" "`, which no key text can hold (a chord splits on whitespace)
        // and which would read as a key that holds its Shift already, as `?` does: as `Space` it
        // has a name of its own, and Space and Shift+Space are two keys.
        key: event.key === " " ? "Space" : event.key,
        metaKey: event.metaKey,
        shiftKey: event.shiftKey,
      };
      let presses = [...pending, press];
      let resolution = resolve(presses, editable);
      if (resolution.exact === undefined && !resolution.partial && pending.length > 0) {
        // A key that does not continue the chord is read afresh on its own.
        presses = [press];
        resolution = resolve(presses, editable);
      }
      if (resolution.exact !== undefined) {
        event.preventDefault();
        clearPending();
        resolution.exact.binding.run(event);
        return;
      }
      if (resolution.partial) {
        event.preventDefault();
        if (pendingTimer !== undefined) {
          unschedule(pendingTimer);
        }
        if (presses.length === 1) {
          pendingEditable = editable;
        }
        pending = presses;
        pendingTimer = schedule(() => {
          pendingTimer = undefined;
          clearPending();
        }, CHORD_TIMEOUT_MS);
        notify();
        return;
      }
      clearPending();
    },
    pending() {
      return chord.keys;
    },
    pushScope(scope) {
      scopes.push(scope);
      return () => {
        const index = scopes.lastIndexOf(scope);
        if (index !== -1) {
          scopes.splice(index, 1);
        }
      };
    },
    register(scope, bindings) {
      const incoming = bindings.map((binding) => {
        const steps = keysOf(binding).map(stepsOf);
        return { binding, chords: steps.map((chord) => chord.map(parseCombo)), scope, steps };
      });
      checkPrefixConflicts(incoming);
      registrations.push(...incoming);
      return () => {
        for (const registration of incoming) {
          const index = registrations.indexOf(registration);
          if (index !== -1) {
            registrations.splice(index, 1);
          }
        }
      };
    },
    subscribe(listener) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
  };
}

/** The application's registry; `KeymapProvider` binds it to `window`. Unit tests build their own. */
export const appKeymap = createKeymap({ strict: import.meta.env.DEV });

/** Who the keys are for: the signed-in login, and the keys they changed. `KeymapProvider` gives
 *  it; with no provider (a unit test) nobody is signed in and every action takes its defaults. */
interface KeymapPerson {
  readonly login: string | undefined;
  readonly personKeys: PersonKeys;
}

export const KeymapPersonContext = createContext<KeymapPerson>({
  login: undefined,
  personKeys: NO_PERSON_KEYS,
});

/** The whole binding an action stands for, under `personKeys`' keys for it. An action that writes
 *  (`CatalogueEntry.writes`) runs once per press: a held key's repeats run nothing, and the
 *  registry's `preventDefault` keeps them from the page too. A palette row's keydown is no
 *  repeat, so the row runs it. */
export function actionBinding(binding: ActionBinding, personKeys: PersonKeys): KeyBinding {
  const entry = catalogueEntry(binding.action);
  const { run } = binding;
  return {
    id: entry.id,
    inEditable: entry.inEditable === true,
    keys: keysOfAction(binding.action, personKeys),
    label: entry.label,
    palette: entry.palette,
    run:
      entry.writes === true
        ? (event) => {
            if (!event.repeat) run(event);
          }
        : run,
    when: binding.when,
  };
}

/**
 * Registers `bindings` under `scope` for the component's lifetime; closures stay current. Each
 * binding names a catalogue action (`ActionBinding`), whose keys are the signed-in person's, so a
 * change to them registers the bindings again. The registration scope is `scope` whatever the
 * action's own: the Inbox's drawer registers the Inbox's actions, with the Inbox's keys, under
 * `dialog`. A binding written out whole (`KeyBinding`) is a dialog's own Escape, the command
 * bar's own close (on the person's keys for Search and actions), the `y` that copies a focused
 * link's address (`link-copy.ts`), or a test's; a key change of one is a new signature, which
 * registers it again.
 */
export function useKeymap(
  scope: KeymapScope,
  bindings: readonly (ActionBinding | KeyBinding)[]
): void {
  const { personKeys } = useContext(KeymapPersonContext);
  const resolved = bindings.map((binding) =>
    "action" in binding ? actionBinding(binding, personKeys) : binding
  );
  const latest = useRef(resolved);
  latest.current = resolved;
  const signature = resolved.map((binding) => `${binding.id}=${keysOf(binding).join(",")}`).join();

  // biome-ignore lint/correctness/useExhaustiveDependencies: `signature` stands in for `bindings`, whose closures are read through `latest`
  useLayoutEffect(() => {
    const proxies = latest.current.map((binding, index) => ({
      ...binding,
      run: (event: KeyboardEvent) => latest.current[index]?.run(event),
      when: () => latest.current[index]?.when?.() !== false,
    }));
    return appKeymap.register(scope, proxies);
  }, [scope, signature]);
}

/** Pushes `scope` onto the stack while mounted; `null` pushes nothing (a closed dialog). */
export function useKeymapScope(scope: KeymapScope | null): void {
  useLayoutEffect(() => (scope === null ? undefined : appKeymap.pushScope(scope)), [scope]);
}

/** The unfinished chord, and the keys that can follow it. */
export function usePendingChord(): PendingChord {
  return useSyncExternalStore(appKeymap.subscribe, appKeymap.chord);
}

/**
 * `keys` as `aria-keyshortcuts` writes them: `$mod+k` is `Control+K Meta+K`, a letter is upper
 * case, and a key of two presses is left out, since the attribute names one press per key.
 */
export function ariaKeyshortcuts(keys: readonly string[]): string {
  return keys
    .map((key) => key.trim())
    .filter((key) => !/\s/.test(key))
    .flatMap((key) => {
      const parts = key.split("+");
      const last = parts.pop() ?? "";
      const named = last.length === 1 ? last.toUpperCase() : last;
      const modifiers = parts.map((part) => (part === "Ctrl" ? "Control" : part));
      if (!modifiers.includes("$mod")) {
        return [[...modifiers, named].join("+")];
      }
      const rest = modifiers.filter((part) => part !== "$mod");
      return [["Control", ...rest, named].join("+"), ["Meta", ...rest, named].join("+")];
    })
    .join(" ");
}
