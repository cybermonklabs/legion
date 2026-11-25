import { createContext, useContext, useLayoutEffect, useRef, useSyncExternalStore } from "react";

import {
  editsText,
  MOVING_KEYS,
  PANEL_ATTRIBUTE,
  takesText,
  typedOnMenuTrigger,
} from "../../components/key-targets";
import { inputMethodKey } from "../../lib/input-method-key";

import { IS_MAC, stepsOf } from "./key-text";
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
 * syntax — `"g i"` is a chord, `"$mod+k"` matches Cmd+K on a Mac and Ctrl+K elsewhere (a Mac's
 * text field keeps Control+K), `"Shift+P"` a shifted letter, `"?"` the character an unshifted or
 * shifted key produces. A combo is matched by the character its key types, so a reader on AZERTY
 * or Dvorak presses the letter a shortcut names wherever it sits; on a Mac, one that holds `Alt`
 * also matches by the key's position (`KeyM` for `m`), because Option changes the character:
 * `$mod+Alt+m` types `µ` there.
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
   *  (`aria-haspopup="listbox"`, `MultiSelect`'s), open or closed, but for a character typed on a
   *  menu's closed trigger, which is the page's key (`typedOnMenuTrigger`) - and while the focus
   *  is anywhere inside an open panel (`PANEL_ATTRIBUTE`). The key then reaches every such
   *  control, so a binding that belongs to only some of them says which in its `when`: the
   *  document's `$mod+Alt+m` runs only while its editor has focus and a selection, never from a
   *  comment composer or any other field. */
  inEditable?: boolean;
  /** Whether a held key's repeats run it too: an action that steps (`CatalogueEntry.repeats`),
   *  as a held `j` walks a list. Every other binding runs once per press, and the registry keeps
   *  its key's repeats from the page (`Keymap.captureKeyDown`). */
  repeats?: boolean;
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
  /** The catalogue action the binding stands for, which `actionBinding` names; a binding written
   *  out whole names none. `describe()` lists the binding under the action's own scope, whatever
   *  scope registered it: the Inbox drawer registers the Inbox's actions under `dialog`. */
  catalogueAction?: KeymapActionId;
}

/** A binding that names an action of the catalogue: the action gives its id, its label, its
 *  palette row and `inEditable`, and the person's keys or the defaults give its keys. */
export interface ActionBinding {
  action: KeymapActionId;
  run: (event: KeyboardEvent) => void;
  /** Whether the binding applies right now; `false` neither fires nor shadows lower scopes. */
  when?: () => boolean;
}

/** A binding as `?` lists it (`Keymap.describe`): its keys as typed, the scope it is listed
 *  under, and whether a press of its keys would run it right now. */
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
  return press.ctrlKey === holdsControl(combo, isMac) && press.metaKey === holdsMeta(combo, isMac);
}

/** Whether a press of `combo` holds Ctrl on the platform: `$mod` is Ctrl away from a Mac. */
function holdsControl(combo: KeyCombo, isMac: boolean): boolean {
  return combo.control || (combo.mod && !isMac);
}

/** Whether a press of `combo` holds Cmd (Meta) on the platform: `$mod` is Cmd on a Mac, as
 *  tinykeys reads it, since a Mac's text field keeps its Control keys (Control+K deletes to the
 *  end of the line). */
function holdsMeta(combo: KeyCombo, isMac: boolean): boolean {
  return combo.meta || (combo.mod && isMac);
}

/** Whether a key press holds the platform's `$mod` and not the other of Ctrl and Cmd: Cmd on a
 *  Mac, Ctrl elsewhere, as `matchesCombo` reads `$mod`. For a handler outside the registry that
 *  takes a `$mod` key itself, so a Mac text field keeps its Control keys there too. */
export function modHeld(
  press: { readonly ctrlKey: boolean; readonly metaKey: boolean },
  isMac: boolean = IS_MAC
): boolean {
  return isMac ? press.metaKey && !press.ctrlKey : press.ctrlKey && !press.metaKey;
}

/** Whether two combos are one key press on the platform, as `matchesCombo` reads a press: on a
 *  Mac `$mod+k` is `Meta+k` and not `Control+k`, and elsewhere the other way round. */
export function sameCombo(a: KeyCombo, b: KeyCombo, isMac: boolean): boolean {
  return (
    a.key.toLowerCase() === b.key.toLowerCase() &&
    a.alt === b.alt &&
    a.shift === b.shift &&
    holdsControl(a, isMac) === holdsControl(b, isMac) &&
    holdsMeta(a, isMac) === holdsMeta(b, isMac)
  );
}

/** Whether the presses of `shorter` begin `longer` (`g` begins `g i`) on the platform. */
export function isStrictPrefix(
  shorter: readonly KeyCombo[],
  longer: readonly KeyCombo[],
  isMac: boolean
): boolean {
  return (
    shorter.length < longer.length &&
    shorter.every((combo, index) => sameCombo(combo, longer[index] as KeyCombo, isMac))
  );
}

/** Inside an open panel (`PANEL_ATTRIBUTE`, which `AnchoredPanel` writes): what tells the
 *  registry a key was pressed inside one (`isEditableTarget`). */
const IN_PANEL = `[${PANEL_ATTRIBUTE}]`;
/** An option list's trigger: the button that names the `listbox` it opens (`MultiSelect`). */
const LIST_TRIGGER = '[aria-haspopup="listbox"]';

/**
 * Whether `event`, a key pressed on its target, is the control's own, so that only the bindings
 * that fire while the reader types (`inEditable`) may take it: a key on a control that takes
 * typed text (`takesText`), a `<select>`, the trigger of an option list, or anything at all
 * inside an open panel.
 *
 * A panel - the display menu, a filter panel and the lists opened from it, a column filter, a
 * control's own list such as Edit labels' - stands over the page, and its keys are its own
 * whatever control of it holds the focus: Tab and Shift+Tab walk it, the arrows walk a group of radios,
 * Space ticks a box, and Escape closes it (`AnchoredPanel` takes Escape before this registry
 * hears it). A radio or a checkbox takes no text, so without this rule `j` on the display menu's
 * radio would walk the table behind the panel, and the Inbox's single keys would act on its
 * rows. Only the key's own target counts: a panel standing open while the focus is on the page -
 * the Board's, restored on load - masks nothing.
 *
 * An option list's trigger (`MultiSelect`'s, the one control that names a `listbox` popup) takes
 * the keys a select takes, open or closed; a typed character goes to its list's search. Which
 * keys open a closed list is `MultiSelect`'s to say. So a digit on the issue header's Assignee is
 * no priority, and `c` on a Settings project list opens no Create issue. A key the trigger leaves
 * alone, such as a letter on a busy trigger, fires only the bindings that fire while the reader
 * types (`inEditable`). A menu's trigger - the Snooze, the priority, the status - is a menu's
 * button and no select: a character typed on it while its menu is closed is the page's key, as
 * on its row (`typedOnMenuTrigger`), so `b` there opens the Snooze menu and `s` the status menu,
 * while its Enter, Space and arrows stay its own.
 */
function isEditableTarget(event: KeyboardEvent): boolean {
  const { target } = event;
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
  // trigger is read as one, but for a character typed on a menu's closed trigger.
  return (
    target.tagName === "SELECT" ||
    (target.matches(LIST_TRIGGER) && !typedOnMenuTrigger(target, event))
  );
}

/** The press of a key still held down (`Keymap.captureKeyDown`): the element it was pressed on,
 *  and the press itself, whose `defaultPrevented`, once the press has reached every listener,
 *  says whether a control or a binding took it. */
interface HeldPress {
  readonly event: KeyboardEvent;
  readonly target: EventTarget | null;
  /** The id of the stepping binding (`KeyBinding.repeats`) the press ran, which its repeats run
   *  again (`handleKeyDown`). */
  steps?: string;
}

/** The physical key a keydown or a keyup is of, which its press, its repeats and its release
 *  share whatever Shift makes it type: `KeyR` for `r` and `R`. An event that names no physical
 *  key, as a script's may not, is read by its character. */
function physicalKey(event: KeyboardEvent): string {
  return event.code === "" ? event.key : event.code;
}

/** `event` as the registry matches it. The space bar types `" "`, which no key text can hold (a
 *  chord splits on whitespace) and which would read as a key that holds its Shift already, as
 *  `?` does: as `Space` it has a name of its own, and Space and Shift+Space are two keys. */
function pressOf(event: KeyboardEvent): KeyPress {
  return {
    altKey: event.altKey,
    code: event.code,
    ctrlKey: event.ctrlKey,
    key: event.key === " " ? "Space" : event.key,
    metaKey: event.metaKey,
    shiftKey: event.shiftKey,
  };
}

export interface KeymapOptions {
  /** Whether the browser runs on a Mac, where `$mod` is Cmd and an Alt combo may match a physical
   *  key; production follows the browser's Mac identity. */
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
  /**
   * Reads a keydown in the window's capture phase, before every control and every other listener
   * of the page hears it (`KeymapProvider`): notes each press of a key until the key is let go
   * (`handleKeyUp`, `releaseKeys`), and keeps from the whole page each repeat of a held key that
   * would act, and no other (`repeatGoesOn`). Each key that acts - opens a field, opens or closes
   * a layer, presses a button, sends, picks, ticks - acts once for each press however long it is
   * held, but for a binding that steps (`KeyBinding.repeats`), and its repeats type nothing in a
   * field its press moved the focus to, and press nothing on a control there. Every other repeat
   * reaches the browser untouched: a key that moves, typing in the field that had the focus
   * before the press, and a key that no binding names and nothing took, such as the browser's
   * zoom or a character typed with AltGr. A press itself goes on untouched, so a control still
   * takes its own keys before the registry reads them (`handleKeyDown`), and so does an input
   * method's key.
   */
  captureKeyDown(event: KeyboardEvent): void;
  /** The unfinished chord, or one with no keys; the same object until the chord changes. */
  chord(): PendingChord;
  /**
   * Every registered binding, the source for `?`. A binding that names a catalogue action is
   * listed under the action's own scope (`catalogueEntry`), whatever scope registered it; any
   * other binding is listed under the scope that registered it. So the Inbox's actions, which
   * the Inbox drawer registers under `dialog`, are listed under Inbox.
   *
   * A binding is enabled when a press of its keys would reach it now: a key is read through the
   * scope that registered it (`consulted`), and its `when()` is not false. While a dialog is
   * open, the bindings of the shell and of the page beneath the dialog are disabled. A
   * `beforeScopes` binding is read before every scope, so its `when()` alone decides.
   *
   * Bindings with the same id under the same listed scope are one row, enabled when any of them
   * is: an action that the Inbox page and its drawer both register, or the bindings of one
   * component mounted twice (an issue's hidden Spec beside another of its documents).
   */
  describe(): KeyBindingDescription[];
  handleKeyDown(event: KeyboardEvent): void;
  /** Forgets the press of the key `event` lets go of (`captureKeyDown`). */
  handleKeyUp(event: KeyboardEvent): void;
  pushScope(scope: KeymapScope): () => void;
  register(scope: KeymapScope, bindings: readonly KeyBinding[]): () => void;
  /** Forgets every press of a key held down, as the window loses the focus and its keys' releases
   *  go elsewhere (`captureKeyDown`). */
  releaseKeys(): void;
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
  /** The press of each key held down now, by its physical key (`physicalKey`), from the press to
   *  the key's release or the window's loss of the focus (`captureKeyDown`). */
  const held = new Map<string, HeldPress>();

  const matches = (combos: readonly KeyCombo[], presses: readonly KeyPress[]) =>
    presses.every((press, index) => matchesCombo(combos[index] as KeyCombo, press, isMac));

  /** The scopes a key is read through, outermost first, which `describe()` reads too. A modal
   *  owns the keyboard: nothing beneath it fires, however the scopes are ordered. */
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
            if (
              isStrictPrefix(chord, otherChord, isMac) ||
              isStrictPrefix(otherChord, chord, isMac)
            ) {
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

  /**
   * Whether a repeat of a held key goes on to the page. A held key's repeats are no presses (R5,
   * R8), so a repeat is kept from the page only where it would act there, but for a binding that
   * repeats by design (`KeyBinding.repeats`), and for a text field's own Backspace and Delete and
   * a multi-line field's or an editor's Enter, held with no Ctrl or Cmd (no Alt for Enter), on the
   * element the press was made in, which repeat as typing does. `press` is the key's held press
   * (`captureKeyDown`), none once the page has lost its release.
   *
   * Tab is kept, so that a fresh press is needed to leave the next layer, and a key that moves
   * goes on to whatever has the focus now (`MOVING_KEYS`). A key a binding names goes on only to
   * run again the binding that steps (`KeyBinding.repeats`) which its press ran: that binding took
   * the press, and repeats by design. Any other repeat, of a key no binding names, is kept only
   * where it would act:
   * - its press was taken - a binding ran on it, or a control's handler or a panel's Escape
   *   (`AnchoredPanel`) took it - so that its repeat would act again, but for the Backspace, the
   *   Delete and an editor's Enter of a field the press was made in, which repeat as typing does;
   * - its press moved the focus, or the page holds no press of it, and it would type or delete in
   *   the field it lands in (`editsText`);
   * - it is an Enter anywhere but a field of several lines, where it would press a button, follow a
   *   link or send a form again, on its press's own element too.
   * Every other repeat reaches the browser untouched: its zoom, a Mac text field's Control keys, a
   * character typed with AltGr or with no modifier in the field that had the focus before the
   * press, and Space where nothing took its press, which scrolls the page.
   */
  const repeatGoesOn = (event: KeyboardEvent, press: HeldPress | undefined): boolean => {
    if (event.key === "Tab") return false;
    if (MOVING_KEYS[event.key] === true) return true;
    const { exact, partial } = resolve([pressOf(event)], isEditableTarget(event));
    if (exact !== undefined || partial) {
      return exact?.binding.repeats === true && exact.binding.id === press?.steps;
    }
    const { target } = event;
    const field = target instanceof Element && takesText(target);
    const editor = target instanceof HTMLElement && target.isContentEditable;
    if (event.key === "Enter" && !editor && !(target instanceof HTMLTextAreaElement)) return false;
    if (press === undefined || press.target !== target) {
      return press?.event.defaultPrevented !== true && !(field && editsText(event));
    }
    if (!press.event.defaultPrevented) return true;
    // The one way on from a press a control took: a text field's own Backspace and Delete, and an
    // editor's Enter, held with no Ctrl or Cmd. An editor takes Backspace at a paragraph's start
    // to join it to the one before, and Enter to break a paragraph, and each repeat does it again,
    // as typing does; a field's Ctrl or Cmd key that its handler took, such as the one that sends,
    // acts once.
    return (
      field &&
      !event.ctrlKey &&
      !event.metaKey &&
      (event.key === "Backspace" ||
        event.key === "Delete" ||
        (event.key === "Enter" && !event.altKey && editor))
    );
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
    captureKeyDown(event) {
      // An input method's key is the input method's, a repeat or not, and a modifier alone acts
      // on nothing: Shift held for Shift+R repeats as long as R does.
      if (inputMethodKey(event) || MODIFIER_KEYS[event.key] === true) return;
      if (!event.repeat) {
        held.set(physicalKey(event), { event, target: event.target });
        return;
      }
      if (repeatGoesOn(event, held.get(physicalKey(event)))) return;
      // A repeat that would act is kept from the browser's own action - a character typed in the
      // field its press moved the focus to, a button pressed, a form sent - and from every listener
      // after this one: the window's own, a panel's Escape at the document's capture phase
      // (`AnchoredPanel`), React's handlers and this registry's.
      event.preventDefault();
      event.stopImmediatePropagation();
    },
    chord() {
      return chord;
    },
    describe() {
      const read = consulted();
      const described = new Map<string, KeyBindingDescription>();
      for (const { binding, scope } of registrations) {
        const listedUnder =
          binding.catalogueAction === undefined
            ? scope
            : catalogueEntry(binding.catalogueAction).scope;
        const enabled =
          (binding.beforeScopes === true || read.includes(scope)) && binding.when?.() !== false;
        const key = `${listedUnder}\u0000${binding.id}`;
        const seen = described.get(key);
        if (seen === undefined) {
          described.set(key, {
            enabled,
            id: binding.id,
            keys: keysOf(binding),
            label: binding.label,
            scope: listedUnder,
          });
        } else {
          seen.enabled ||= enabled;
        }
      }
      return [...described.values()];
    },
    handleKeyDown(event) {
      // A key already taken, a modifier alone, and an input method's key (`inputMethodKey`) - one
      // pressed in a composition, or the one that ends it, which Safari sends with `isComposing`
      // false and keyCode 229 - run no binding: the last is the input method's, and the Escape that
      // ends a composition closes no dialog and leaves no field.
      if (event.defaultPrevented || inputMethodKey(event) || MODIFIER_KEYS[event.key] === true) {
        return;
      }
      const editable = isEditableTarget(event);
      const press = pressOf(event);
      // A held key's repeats are no presses (R5, R8): each is read alone, outside any chord, and
      // runs only a binding that steps (`KeyBinding.repeats`), as a held `j` walks a list. Any
      // other binding its key names ran on the press and runs on no repeat - a held `1` that
      // snoozed an ask ticks no option of the question handed on to, a held `b` opens its menu
      // once - and the repeats of its key are kept from the page all the same: most never reach
      // this listener (`captureKeyDown`), and those of a key that moves, which go on to the
      // control the focus is on, reach no binding but one that steps (Home on the outline's rows).
      if (event.repeat) {
        const { exact, partial } = resolve([press], editable);
        if (exact === undefined && !partial) return;
        event.preventDefault();
        if (exact?.binding.repeats === true) exact.binding.run(event);
        return;
      }
      let presses = [...pending, press];
      let resolution = resolve(presses, editable);
      if (resolution.exact === undefined && !resolution.partial && pending.length > 0) {
        // A key that does not continue the chord is read afresh on its own.
        presses = [press];
        resolution = resolve(presses, editable);
      }
      if (resolution.exact !== undefined) {
        const heldPress = held.get(physicalKey(event));
        if (
          heldPress?.event === event &&
          presses.length === 1 &&
          resolution.exact.binding.repeats === true
        ) {
          heldPress.steps = resolution.exact.binding.id;
        }
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
    handleKeyUp(event) {
      held.delete(physicalKey(event));
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
    releaseKeys() {
      held.clear();
    },
    subscribe(listener) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
  };
}

/** The application's registry; `KeymapProvider` binds it to `window`. Unit tests build their own.
 *  Strict in Vite's development build. A module loaded outside Vite has no `import.meta.env`:
 *  the browser rows import names from the app's control modules in Node (`e2e/issue-page-h.e2e.ts`
 *  imports `repoControlName`), and those modules reach this one through the shared list
 *  (`MultiSelect`, whose trigger draws its key hint from `ShortcutHelp`). */
export const appKeymap = createKeymap({ strict: import.meta.env?.DEV === true });

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

/**
 * A condition every binding registered beneath it also needs (`useKeymap`): a binding there holds,
 * and `?` shows it enabled, only while the gate and its own `when` both hold. The Inbox drawer
 * sets it to "no dialog stands over the drawer", so no key of its tree acts behind a dialog
 * standing over it: the `?` help, or the Decline box a card of it opens. `null`, the default,
 * gates nothing.
 */
export const KeymapGateContext = createContext<(() => boolean) | null>(null);

/** How `useKeymap` registers its bindings. */
export interface UseKeymapOptions {
  /** Leaves the bindings out of the gate around the caller (`KeymapGateContext`). For a dialog's
   *  own Escape (`useDialog`), whose `when` - this dialog is the innermost open one - is the whole
   *  rule: a dialog opened inside the drawer closes on it while the drawer's keys stand down. */
  readonly outsideGate?: boolean;
}

/** The whole binding an action stands for, under `personKeys`' keys for it. An action that steps
 *  (`CatalogueEntry.repeats`) runs on a held key's repeats too; every other action runs once per
 *  press (`KeyBinding.repeats`). A palette row's keydown is no repeat, so the row runs it. */
export function actionBinding(binding: ActionBinding, personKeys: PersonKeys): KeyBinding {
  const entry = catalogueEntry(binding.action);
  return {
    catalogueAction: binding.action,
    id: entry.id,
    inEditable: entry.inEditable === true,
    keys: keysOfAction(binding.action, personKeys),
    label: entry.label,
    palette: entry.palette,
    repeats: entry.repeats === true,
    run: binding.run,
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
 * registers it again. Beneath a gate (`KeymapGateContext`) each binding holds only while the gate
 * does too, unless `options.outsideGate` leaves it out.
 */
export function useKeymap(
  scope: KeymapScope,
  bindings: readonly (ActionBinding | KeyBinding)[],
  options: UseKeymapOptions = {}
): void {
  const { personKeys } = useContext(KeymapPersonContext);
  const contextGate = useContext(KeymapGateContext);
  const resolved = bindings.map((binding) =>
    "action" in binding ? actionBinding(binding, personKeys) : binding
  );
  const latest = useRef(resolved);
  latest.current = resolved;
  // Read as a key is pressed, as `latest` is, so a gate drawn anew registers nothing again.
  const gate = useRef<(() => boolean) | null>(null);
  gate.current = options.outsideGate === true ? null : contextGate;
  const signature = resolved.map((binding) => `${binding.id}=${keysOf(binding).join(",")}`).join();

  // biome-ignore lint/correctness/useExhaustiveDependencies: `signature` stands in for `bindings`, whose closures are read through `latest`
  useLayoutEffect(() => {
    const proxies = latest.current.map((binding, index) => ({
      ...binding,
      run: (event: KeyboardEvent) => latest.current[index]?.run(event),
      when: () =>
        (gate.current === null || gate.current()) && latest.current[index]?.when?.() !== false,
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
 * `keys` as `aria-keyshortcuts` writes them: `$mod` is the platform's one key, `Meta` on a Mac and
 * `Control` elsewhere, as the registry matches it (`matchesCombo`), a letter is upper case, and a
 * key of two presses is left out, since the attribute names one press per key.
 */
export function ariaKeyshortcuts(keys: readonly string[], isMac: boolean = IS_MAC): string {
  return keys
    .map((key) => key.trim())
    .filter((key) => !/\s/.test(key))
    .map((key) => {
      const parts = key.split("+");
      const last = parts.pop() ?? "";
      const named = last.length === 1 ? last.toUpperCase() : last;
      const modifiers = parts.map((part) =>
        part === "Ctrl" ? "Control" : part === "$mod" ? (isMac ? "Meta" : "Control") : part
      );
      return [...modifiers, named].join("+");
    })
    .join(" ");
}
