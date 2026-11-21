import { isStrictPrefix, type KeyCombo, parseChord, sameCombo } from "./keymap";
import {
  catalogueEntry,
  KEYMAP_ACTIONS,
  type KeymapActionId,
  type KeySurface,
  keepsOwnKeys,
  keysOfAction,
  type PersonKeys,
} from "./keymap-catalogue";
import { LINK_COPY_KEY, LINK_COPY_LABEL } from "./link-copy";

/**
 * What the check says of a person's keys for one action (R14): refused, with the reason, which
 * names the action that holds the key; or accepted, with a notice for each global action the keys
 * stop on a page. The registry finds a prefix only among the bindings of the pages that are open
 * (`checkPrefixConflicts`, `keymap.ts`) and no duplicate at all, so this check reads the whole
 * catalogue: every action of every scope, whether or not its page is open.
 */
export type KeyVerdict =
  | { readonly kind: "accepted"; readonly notices: readonly string[] }
  | { readonly kind: "refused"; readonly message: string; readonly other?: KeymapActionId };

/** Each part of a page, as a notice names the page. */
const PAGE_NAMES: Record<KeySurface, string> = {
  agents: "the Agents page",
  architecture: "the Architecture page",
  board: "the Board",
  delivery: "the Delivery page",
  document: "a document",
  global: "every page",
  inbox: "the Inbox",
  issue: "the issue page",
  "issue-both": "the issue page",
  "issue-outline": "the new issue page",
  margin: "every page but the new issue page",
  "project-list": "a project's lists",
  "project-page": "a project's pages",
};

/** The surfaces drawn at the same time, whose keys a reader presses on one page: each issue page
 *  with the actions both issue pages draw and with its documents, and a project's own keys with
 *  each of its tabs. */
const DRAWN_TOGETHER: readonly (readonly [KeySurface, KeySurface])[] = [
  ["issue", "issue-both"],
  ["issue-outline", "issue-both"],
  ["issue", "document"],
  ["issue-outline", "document"],
  ["issue-both", "document"],
  ["project-page", "project-list"],
  ["project-page", "board"],
  ["project-page", "architecture"],
];

/**
 * The pairs of actions drawn on one page that share a key on purpose, since each applies only
 * while the focus is on a control of its own, so their `when`s never hold at once and a press runs
 * the one whose control has the focus (`resolve`, `keymap.ts`). A pair may share any key a person
 * gives both, since no key moves the focus. A key of one that is the first press of the other's
 * is still refused: the registry reads a prefix as the bindings register, before any `when`.
 *
 * INFRA-1196's decision 164 ("One key does one act on both issue pages",
 * dispatch://INFRA-1196/artifact/part-decisions-md) keeps `a` as Approve on both issue pages, and
 * on the new page Approve acts only on the header's approval chip, since elsewhere `a` answers the
 * question of a focused row. There Approve applies only while the focus is on an approval chip of
 * the header or on one of its actions (`issue-page/useHeaderKeys.ts`'s `focusedApprovalAction`),
 * and Answer only while it is on a row of the outline (`issue-page/useTreeKeymap.ts`'s
 * `focusedRow`): the two `when`s never hold at once. Today's page draws no Answer.
 */
const EXCLUSIVE_BY_FOCUS: readonly (readonly [KeymapActionId, KeymapActionId])[] = [
  ["issue.approve", "issue.tree-answer"],
];

/** Whether `one` and `two` are a pair of `EXCLUSIVE_BY_FOCUS`, in either order. */
function exclusiveByFocus(one: KeymapActionId, two: KeymapActionId): boolean {
  return EXCLUSIVE_BY_FOCUS.some(
    ([first, second]) => (first === one && second === two) || (first === two && second === one)
  );
}

/** The surfaces of the shell's keys, each drawn on every page but those it names: the shell's own
 *  keys on every page, and Toggle margin on every page but the new issue page, whose frame
 *  registers Details' key in its place (`app.tsx`). */
const ON_EVERY_PAGE_BUT: Partial<Record<KeySurface, readonly KeySurface[]>> = {
  global: [],
  margin: ["issue-outline"],
};

/** Whether `shell` is a surface of the shell's keys that is drawn on the page of `other`. */
function drawnOnPageOf(shell: KeySurface, other: KeySurface): boolean {
  const but = ON_EVERY_PAGE_BUT[shell];
  return but !== undefined && !but.includes(other);
}

/** Keys that keep their own actions: a bare digit, an arrow, Enter, Escape, Tab and Space, with
 *  or without Shift (a digit encodes its Shift in the character it types). */
const RESERVED_KEYS: Record<string, true> = {
  ArrowDown: true,
  ArrowLeft: true,
  ArrowRight: true,
  ArrowUp: true,
  Enter: true,
  Escape: true,
  Space: true,
  Tab: true,
};

/** The keys the browser keeps for itself with Ctrl or Cmd: close, new tab, new window, the
 *  address bar, reload and quit, and Tab, which moves between tabs. */
const BROWSER_KEYS: Record<string, true> = {
  l: true,
  n: true,
  q: true,
  r: true,
  t: true,
  tab: true,
  w: true,
};

function isReserved(combo: KeyCombo): boolean {
  if (combo.alt || combo.control || combo.meta || combo.mod) return false;
  return /^[0-9]$/.test(combo.key) || RESERVED_KEYS[combo.key] === true;
}

function isBrowsers(combo: KeyCombo): boolean {
  return (
    (combo.control || combo.meta || combo.mod) && BROWSER_KEYS[combo.key.toLowerCase()] === true
  );
}

/** Keys that edit a text field's text or move its caret, with Option on a Mac too: Option+Backspace
 *  deletes a word, Option+← moves the caret by a word, and Option+Space types a space of its own. */
const FIELD_KEYS: Record<string, true> = {
  ArrowDown: true,
  ArrowLeft: true,
  ArrowRight: true,
  ArrowUp: true,
  Backspace: true,
  Delete: true,
  End: true,
  Home: true,
  PageDown: true,
  PageUp: true,
  Space: true,
};

/** Whether a text field takes `combo` as its own, so that an `inEditable` binding of it would take
 *  the press from every field: a press with no Ctrl, Cmd or Meta, unless it holds Alt with a key
 *  that neither types a character nor edits the text (`Alt+Enter`). With Alt, a key of one
 *  character types one on a Mac (Option+M types `µ`). */
function actsInField(combo: KeyCombo): boolean {
  if (combo.control || combo.meta || combo.mod) return false;
  return !combo.alt || combo.key.length === 1 || FIELD_KEYS[combo.key] === true;
}

/** The bindings written out whole that are registered on every page: the `y` that copies a focused
 *  link's address (`link-copy.ts`). An action's key may neither begin with one of their keys nor
 *  be the first press of one, since the registry would find a prefix (`checkPrefixConflicts`,
 *  `keymap.ts`). The command bar's own close is the keys of Search and actions
 *  (`SearchPalette.tsx`), which the check reads as that action's. */
const WRITTEN_WHOLE: readonly { readonly key: string; readonly label: string }[] = [
  { key: LINK_COPY_KEY, label: LINK_COPY_LABEL },
];

function sameChord(a: readonly KeyCombo[], b: readonly KeyCombo[]): boolean {
  return a.length === b.length && a.every((combo, index) => sameCombo(combo, b[index] as KeyCombo));
}

/** Each key's presses as `chordOf` read them first: a check reads every action's keys, and the
 *  same text always gives the same presses. The keys are the catalogue's and the ones a person
 *  records, so the map stays small. */
const chords = new Map<string, readonly KeyCombo[] | null>();

/** The presses of `key`, or null where the text names no key. */
function chordOf(key: string): readonly KeyCombo[] | null {
  const known = chords.get(key);
  if (known !== undefined) return known;
  let chord: readonly KeyCombo[] | null;
  try {
    chord = parseChord(key);
  } catch {
    chord = null;
  }
  chords.set(key, chord);
  return chord;
}

function refused(message: string, other?: KeymapActionId): KeyVerdict {
  return { kind: "refused", message, other };
}

/**
 * Whether `action` may take `keys` in place of the keys it has, with every other action on its
 * keys in `personKeys` or its defaults: the check Settings runs on every key a person gives.
 * Refused: a key that is no key; a key that keeps its own action (`RESERVED_KEYS`, a digit) or
 * that the browser keeps; for an action that also works while the reader types (`inEditable`), a
 * key a text field takes as its own (`actsInField`), since the registry would take it from every
 * field; any key for an action whose keys all keep their own action, since its run reads the key
 * pressed; a key listed twice, or two of the action's own keys of which one is the first press of
 * the other (`g` beside `g i`, while `g i` beside `g a` passes); a key another action of the same
 * surface, or of a surface drawn with it, has, unless the two are a pair of `EXCLUSIVE_BY_FOCUS`;
 * a key that is the first press of another action's key, or whose first press is another action's
 * key, in any scope; and a key whose first press is the key of a binding written out whole
 * (`WRITTEN_WHOLE`). The shell's keys are drawn on every page but those `ON_EVERY_PAGE_BUT`
 * names: a key another action of the `global` scope has on a page where both are drawn is
 * refused, since one scope holds a key once, and a key a global action has, taken by a page's
 * action where the global one is drawn, or the other way round, is accepted with the notice that
 * the global action does not work on that page, as the Agents page's `/` and `i` are today. The
 * notice says more than the registry does: where the page's action does not apply (its `when` is
 * false), `resolve` in `keymap.ts` passes the key on to the global action.
 */
export function refusalOf(
  action: KeymapActionId,
  keys: readonly string[],
  personKeys: PersonKeys
): KeyVerdict {
  const entry = catalogueEntry(action);
  if (keys.length > 0 && keepsOwnKeys(entry)) {
    return refused(`${entry.label} keeps its own keys.`);
  }
  const taken: { chord: readonly KeyCombo[]; key: string }[] = [];
  for (const key of keys) {
    const chord = chordOf(key);
    if (chord === null) return refused(`${key} is not a key.`);
    if (chord.some(isReserved)) {
      return refused(
        `${key} is not free: the digits, the arrows, Enter, Escape, Tab and Space keep their own actions.`
      );
    }
    if (chord.some(isBrowsers)) return refused(`${key} is the browser's own key.`);
    if (entry.inEditable === true && chord.some(actsInField)) {
      return refused(
        `${key} would be taken from every text field: ${entry.label} also works while you type, so its key needs Ctrl or Cmd, or Alt with a key that types nothing, such as Alt+Enter.`
      );
    }
    for (const earlier of taken) {
      if (sameChord(earlier.chord, chord)) return refused(`${key} is listed twice.`);
      if (isStrictPrefix(earlier.chord, chord)) {
        return refused(`${earlier.key} is the first press of ${key}, both keys of ${entry.label}.`);
      }
      if (isStrictPrefix(chord, earlier.chord)) {
        return refused(`${key} is the first press of ${earlier.key}, both keys of ${entry.label}.`);
      }
    }
    taken.push({ chord, key });
  }

  const notices = new Set<string>();
  for (const other of KEYMAP_ACTIONS) {
    if (other === action) continue;
    const otherEntry = catalogueEntry(other);
    for (const otherKey of keysOfAction(other, personKeys)) {
      const otherChord = chordOf(otherKey);
      if (otherChord === null) continue;
      for (const { chord, key } of taken) {
        if (isStrictPrefix(chord, otherChord)) {
          return refused(`${key} is the first press of ${otherKey}, ${otherEntry.label}.`, other);
        }
        if (isStrictPrefix(otherChord, chord)) {
          return refused(`${otherKey}, ${otherEntry.label}, is the first press of ${key}.`, other);
        }
        if (!sameChord(chord, otherChord) || exclusiveByFocus(action, other)) continue;
        if (
          entry.surface === otherEntry.surface ||
          DRAWN_TOGETHER.some(
            ([one, two]) =>
              (one === entry.surface && two === otherEntry.surface) ||
              (one === otherEntry.surface && two === entry.surface)
          )
        ) {
          return refused(`${key} is already the key of ${otherEntry.label}.`, other);
        }
        if (
          !drawnOnPageOf(entry.surface, otherEntry.surface) &&
          !drawnOnPageOf(otherEntry.surface, entry.surface)
        ) {
          continue;
        }
        // One of the two is a key of the shell, drawn on the other's page. A page's scope is read
        // before the `global` scope (`resolve`, `keymap.ts`), which holds a key once.
        const shell = entry.scope === "global";
        if (shell === (otherEntry.scope === "global")) {
          return refused(`${key} is already the key of ${otherEntry.label}.`, other);
        }
        if (shell) {
          notices.add(`${entry.label} does not work on ${PAGE_NAMES[otherEntry.surface]}.`);
        } else {
          notices.add(`${otherEntry.label} does not work on ${PAGE_NAMES[entry.surface]}.`);
        }
      }
    }
  }
  for (const whole of WRITTEN_WHOLE) {
    const wholeChord = chordOf(whole.key);
    if (wholeChord === null) continue;
    for (const { chord, key } of taken) {
      if (isStrictPrefix(wholeChord, chord)) {
        return refused(`${whole.key}, ${whole.label}, is the first press of ${key}.`);
      }
      if (isStrictPrefix(chord, wholeChord)) {
        return refused(`${key} is the first press of ${whole.key}, ${whole.label}.`);
      }
    }
  }
  return { kind: "accepted", notices: [...notices] };
}
