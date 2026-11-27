import { useLayoutEffect, useRef, useSyncExternalStore } from "react";

import { inputMethodKey } from "../../hooks/inputMethodKey";

/**
 * One keyboard registry for the SPA. Components register bindings under a scope
 * (`useKeymap`), pages and dialogs push their scope onto the shell-owned stack
 * (`useKeymapScope`), and a single `window` keydown listener (`KeymapProvider`) resolves each
 * key top-down through the stack: the first scope holding a matching binding wins, and while
 * a `dialog` scope is on the stack it is the only scope consulted. Within a scope the binding
 * registered first that matches wins, one that is `askedLast` only where no other matches.
 * `keys` follows tinykeys' string syntax — `"g i"` is a chord, `"$mod+k"` matches Meta or
 * Control, `"Shift+P"` a shifted letter, `"?"` the character an unshifted or shifted key
 * produces. A combo is matched by the character its key types, so a reader on AZERTY or Dvorak
 * presses the letter a shortcut names wherever it sits; on a Mac, one that holds `Alt` also
 * matches by the key's position (`KeyM` for `m`), because Option changes the character:
 * `$mod+Alt+m` types `µ` there.
 */

export type KeymapScope =
  | "global"
  | "dialog"
  | "inbox"
  | "project"
  | "architecture"
  | "board"
  | "issue"
  | "document"
  | "agents";

/** What running a binding does to the reader: moves their place or their focus - a focus, a
 *  tab, a dialog or palette that takes the focus, another page - or moves neither. */
export type ShortcutEffect = "moves the reader" | "moves nothing";

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
  /**
   * What `run` does to the reader, which every binding declares where it binds its key: the
   * page's watches over the reader's place read a key the registry acts on by this and nothing
   * else (`effectOf`, read by the table of the reader's inputs in
   * `features/doc/readers-scroll.ts`). A binding that moves the focus, opens a dialog or a
   * palette that takes it, switches a tab or goes to another page moves the reader, and the
   * page lets go of their place for it; one that changes nothing their place or focus hangs on -
   * a write, a toggle, a copy - moves nothing, and the page goes on keeping their place through
   * it. Required, so a binding cannot be added without saying which.
   */
  effect: ShortcutEffect;
  /** Whether the binding applies right now; `false` neither fires nor shadows lower scopes. */
  when?: () => boolean;
  /** Fires while a control that takes typed text has focus: a `TEXTAREA`, a `SELECT`, a
   *  contentEditable element, or a text-entry `INPUT`. The key then reaches every such control,
   *  so a binding that belongs to only some of them says which in its `when`: the document's
   *  `$mod+Alt+m` runs only while its editor has focus and a selection, never from a comment
   *  composer or any other field. */
  inEditable?: boolean;
  /** Whether `⌘K` offers this binding as an action. The default is `true` for a binding with one
   *  key or none that is not `inEditable`, and `false` otherwise. A row presses only its first
   *  key, and a binding with several keys may pick what it does by the key pressed (the arrows,
   *  `1`–`9`), so one whose keys all do the same thing says `true`. An `inEditable` binding's key
   *  is one that has to work while the reader types - Escape, `$mod+k`, the document's
   *  `$mod+Alt+m` - and is a row only when it says `true`: Escape closes the palette, and `$mod+k`
   *  is its own key. Keys that walk a list with the row in hand set `false`, as does a second key
   *  for an action that already has a row; any other binding that sets it says why beside it. */
  palette?: boolean;
  /** Asked only where no other binding of its scope that applies matches the same press,
   *  whichever was registered first. Registration order is mount order, and a part that mounts
   *  after the component around it registers after it: a dialog's own Escape (`useDialog`) is
   *  the last level out of it, and the drawer's Inbox (`InboxDrawer.tsx`), whose chunk arrives
   *  after the drawer's frame the first time the drawer opens, takes Escape before it. */
  askedLast?: boolean;
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
const IS_MAC = navigator.userAgent.includes("Mac");
/** How `$mod` is shown to the reader: the Command glyph on a Mac, `Ctrl` elsewhere. */
export const MOD_KEY_LABEL = IS_MAC ? "⌘" : "Ctrl";
/** How `Alt` is shown to the reader: the Option glyph on a Mac, `Alt` elsewhere. */
export const ALT_KEY_LABEL = IS_MAC ? "⌥" : "Alt";
const CHORD_TIMEOUT_MS = 1000;

interface KeyCombo {
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
function sameCombo(a: KeyCombo, b: KeyCombo): boolean {
  if (a.key.toLowerCase() !== b.key.toLowerCase() || a.alt !== b.alt || a.shift !== b.shift) {
    return false;
  }
  if (a.mod || b.mod) {
    return (a.mod || a.control || a.meta) && (b.mod || b.control || b.meta);
  }
  return a.control === b.control && a.meta === b.meta;
}

function isStrictPrefix(shorter: readonly KeyCombo[], longer: readonly KeyCombo[]): boolean {
  return (
    shorter.length < longer.length &&
    shorter.every((combo, index) => sameCombo(combo, longer[index] as KeyCombo))
  );
}

/** The `<input>` types that take typed text. Everything else an input can be - checkbox, radio,
 *  the button family, file, range, color - takes no text, so a letter or digit pressed while one
 *  has focus is a shortcut, not typing: ticking a checkbox with the pointer must not silence the
 *  page's keys. `HTMLInputElement.type` normalises a missing or unknown attribute to `text`. */
const TEXT_INPUT_TYPES: Record<string, true> = {
  date: true,
  "datetime-local": true,
  email: true,
  month: true,
  number: true,
  password: true,
  search: true,
  tel: true,
  text: true,
  time: true,
  url: true,
  week: true,
};

/** Whether `element` takes typed text: an editor, a `<textarea>`, or an `<input>` of a
 *  text-entry type (`TEXT_INPUT_TYPES`). A `<select>` takes none of its own, and neither does a
 *  checkbox, a radio or a button. */
export function takesTypedText(element: Element | null): boolean {
  if (!(element instanceof HTMLElement)) return false;
  if (element.isContentEditable) return true;
  if (element instanceof HTMLInputElement) return TEXT_INPUT_TYPES[element.type] === true;
  return element.tagName === "TEXTAREA";
}

/** Whether `target` is a control that takes typed text: a contentEditable element, a
 *  `TEXTAREA`, a `SELECT`, or a text-entry `INPUT`. The registry suspends every binding not
 *  marked `inEditable` there, and the table of the reader's inputs reads it as the field that
 *  takes Space, the arrows, Home and End for itself (`features/doc/readers-scroll.ts`). */
export function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) {
    return false;
  }
  // An `<input>` takes typed text only in the text-entry types; a checkbox, radio or button
  // takes none, so a page's single-key shortcuts keep working while one has focus. That is a
  // permission with an obligation attached: the registry stops guarding those controls, so a
  // page whose key writes something must say for itself where the reader may press it
  // (`IssuePage`'s `outsideInputsAndAskCards`, the Inbox's `outsideAskCard`).
  //
  // A `<select>` stays editable: its own keys type-ahead and step through the options, and the
  // Inbox's `h` hands it focus on purpose.
  return takesTypedText(target) || target.tagName === "SELECT";
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
   * `palette` is `true`, innermost scope first. Unset, `palette` is `false` for a binding with
   * several keys or one that fires in an editable (`KeyBinding.palette`). A row runs its binding
   * only if the binding is still registered and its `when()` still holds when the row is chosen.
   */
  actions(): KeymapAction[];
  /** Every registered binding with its `when()` evaluated now — the source for `?`. One component
   *  mounted twice registers its bindings twice (an issue's hidden Spec beside another of its
   *  documents), and they are one row, enabled when either instance's `when()` holds. */
  describe(): KeyBindingDescription[];
  /** What the binding a key press would run does to the reader (`KeyBinding.effect`), `moves
   *  nothing` for the first key of a chord the registry would hold, and `undefined` for a key it
   *  would leave to the browser. It runs nothing and moves no chord on, so a watch listening on
   *  the capture phase can read the press before the registry's own listener answers it. */
  effectOf(event: KeyboardEvent): ShortcutEffect | undefined;
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
  let pendingTimer: number | undefined;
  let pendingKeys = "";

  const notify = () => {
    pendingKeys = pending.map((press) => press.key).join(" ");
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
    // A modal owns the keyboard: nothing beneath it fires, however the scopes are ordered.
    const stack: KeymapScope[] = scopes.includes(DIALOG_SCOPE)
      ? [DIALOG_SCOPE]
      : ["global", ...scopes];
    for (let depth = stack.length - 1; depth >= 0; depth -= 1) {
      const scope = stack[depth];
      let exact: Registration | undefined;
      let askedLast: Registration | undefined;
      let partial = false;
      for (const registration of registrations) {
        const { binding } = registration;
        if (registration.scope !== scope || (editable && binding.inEditable !== true)) {
          continue;
        }
        for (const chord of registration.chords) {
          if (
            chord.length < presses.length ||
            !presses.every((press, index) => matchesCombo(chord[index] as KeyCombo, press, isMac))
          ) {
            continue;
          }
          if (chord.length > presses.length) {
            partial = true;
          } else if (binding.askedLast === true) {
            if (askedLast === undefined && binding.when?.() !== false) {
              askedLast = registration;
            }
          } else if (exact === undefined && binding.when?.() !== false) {
            exact = registration;
          }
        }
      }
      exact ??= askedLast;
      if (exact !== undefined || partial) {
        return { exact, partial };
      }
    }
    return { exact: undefined, partial: false };
  };

  /** The presses `event` completes and what the registry would do with them, or `undefined` for
   *  a press it leaves alone: one already handled, an input method's (`inputMethodKey`), a
   *  modifier on its own. It changes nothing, so `effectOf` reads a press exactly as
   *  `handleKeyDown` then answers it. */
  const readThePress = (
    event: KeyboardEvent
  ): { presses: KeyPress[]; resolution: Resolution } | undefined => {
    if (event.defaultPrevented || inputMethodKey(event) || MODIFIER_KEYS[event.key] === true) {
      return undefined;
    }
    const editable = isEditableTarget(event.target);
    const press: KeyPress = {
      altKey: event.altKey,
      code: event.code,
      ctrlKey: event.ctrlKey,
      key: event.key,
      metaKey: event.metaKey,
      shiftKey: event.shiftKey,
    };
    const presses = [...pending, press];
    const resolution = resolve(presses, editable);
    if (resolution.exact === undefined && !resolution.partial && pending.length > 0) {
      // A key that does not continue the chord is read afresh on its own.
      return { presses: [press], resolution: resolve([press], editable) };
    }
    return { presses, resolution };
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
        for (const registration of registrations) {
          const { binding } = registration;
          const keys = keysOf(binding);
          if (
            registration.scope !== scope ||
            !(binding.palette ?? (keys.length <= 1 && binding.inEditable !== true)) ||
            binding.when?.() === false
          ) {
            continue;
          }
          inScope.push({
            id: binding.id,
            keys,
            label: binding.label,
            // The rows are decided when the palette opens; by the time one is chosen its control
            // may have gone (another writer reopened the issue) or its component unmounted (the
            // page fell to its error view), so the row asks again.
            run: () => {
              if (registrations.includes(registration) && binding.when?.() !== false) {
                binding.run(new KeyboardEvent("keydown", { key: keys[0] ?? "" }));
              }
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
    effectOf(event) {
      const resolution = readThePress(event)?.resolution;
      if (resolution?.exact !== undefined) {
        return resolution.exact.binding.effect;
      }
      return resolution?.partial === true ? "moves nothing" : undefined;
    },
    handleKeyDown(event) {
      const read = readThePress(event);
      if (read === undefined) {
        return;
      }
      const { presses, resolution } = read;
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
      return pendingKeys;
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
      const incoming = bindings.map((binding) => ({
        binding,
        chords: keysOf(binding).map((keys) => keys.trim().split(/\s+/).map(parseCombo)),
        scope,
      }));
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

/** The application's registry; `KeymapProvider` binds it to `window`. Unit tests build their own.
 *  `import.meta.env` is read optionally because Playwright evaluates the e2e files under Node,
 *  where it is undefined, and an e2e file that imports a constant of the issue page loads this
 *  module through `readers-place.ts` (`e2e/issue-page-d.e2e.ts`'s `HEADER_SCROLLS_QUERY`). */
export const appKeymap = createKeymap({ strict: import.meta.env?.DEV === true });

/** Registers `bindings` under `scope` for the component's lifetime; closures stay current. */
export function useKeymap(scope: KeymapScope, bindings: readonly KeyBinding[]): void {
  const latest = useRef(bindings);
  latest.current = bindings;
  const signature = bindings.map((binding) => `${binding.id}=${keysOf(binding).join(",")}`).join();

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

/** The keys typed so far of an unfinished chord (`"g"`), or `""`. */
export function usePendingChord(): string {
  return useSyncExternalStore(appKeymap.subscribe, appKeymap.pending);
}
