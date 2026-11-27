/**
 * How Dispatch writes a key for its reader: the one formatter (`keyTokens`, `keyText`) and its one
 * table of glyphs (`KEY_GLYPHS`). Every text a reader sees that names a modifier is drawn from
 * them - `?`, Settings' Keyboard shortcuts, the rail, a control's key hint, a hint under a text
 * box, a placeholder, a label, a refusal message, a `title` and an `aria-label` - and no other
 * text writes `Ctrl`, `Cmd`, `Shift`, `Alt`, `Option`, `⌘`, `⌃`, `⌥` or `⇧`. `aria-keyshortcuts`
 * names its keys as ARIA does, `Control`, `Meta`, `Shift`, `Alt` (`ariaKeyshortcuts`,
 * `keymap.ts`). The reader's guide (`docs/site/src/content/docs/dispatch/keyboard.md`) states the
 * same table. The module also holds `listed`, which writes a list of words as a sentence does,
 * as a refusal names the keys it lists.
 *
 * The module imports nothing: `page-words.ts` reads it, which a browser spec's own Node process
 * loads, so that process reads how a key is written without loading the keymap.
 */

/** Whether the browser says it runs on a Mac, read once as the app loads: there `$mod` is Cmd,
 *  the registry matches an Alt combo by the key's place (`keymap.ts`), and a key is drawn in the
 *  Mac's glyphs. In a process with no browser it is false. */
export const IS_MAC = typeof navigator !== "undefined" && navigator.userAgent.includes("Mac");

/** One platform's glyphs, by the part of a key as a binding writes it (`$mod`, `Shift`). */
type KeyGlyphs = Readonly<Record<string, string>>;

/**
 * The glyph of each part of a key that has one, on a Mac and elsewhere. On a Mac a modifier is
 * drawn as its keycap prints it: Control `⌃`, Shift `⇧`, Option `⌥`, and Cmd `⌘`, which `$mod` and
 * Meta are there. Elsewhere it is a word: `Ctrl`, which `$mod` and Control are there, `Shift`,
 * `Alt`, and `Meta` for the Meta key. On both the arrows are arrows and Escape is `Esc`. Every
 * other part is drawn as it is written.
 */
const KEY_GLYPHS: { readonly mac: KeyGlyphs; readonly other: KeyGlyphs } = {
  mac: {
    $mod: "⌘",
    Alt: "⌥",
    ArrowDown: "↓",
    ArrowLeft: "←",
    ArrowRight: "→",
    ArrowUp: "↑",
    Control: "⌃",
    Ctrl: "⌃",
    Escape: "Esc",
    Meta: "⌘",
    Shift: "⇧",
  },
  other: {
    $mod: "Ctrl",
    Alt: "Alt",
    ArrowDown: "↓",
    ArrowLeft: "←",
    ArrowRight: "→",
    ArrowUp: "↑",
    Control: "Ctrl",
    Ctrl: "Ctrl",
    Escape: "Esc",
    Meta: "Meta",
    Shift: "Shift",
  },
};

/** The steps of one key as written: `"g i"` is `["g", "i"]`, a chord of two presses. */
export function stepsOf(keys: string): string[] {
  return keys.trim().split(/\s+/);
}

/**
 * Each step of a key as drawn, keyed by its position so `g g` renders twice, with the glyph of
 * each of its parts: `$mod+k` is `⌘+k` (`["⌘", "k"]`) on a Mac and `Ctrl+k` elsewhere. `?`,
 * Settings and a control's key hint draw each step's `token`; the rail joins its `parts` itself
 * (`RailButton`). `isMac`, the browser's own unless a caller names one, picks the glyphs.
 */
export function keyTokens(
  keys: string,
  isMac: boolean = IS_MAC
): { key: string; parts: string[]; token: string }[] {
  const glyphs = isMac ? KEY_GLYPHS.mac : KEY_GLYPHS.other;
  return stepsOf(keys).map((combo, position) => {
    const parts = combo.split("+").map((part) => glyphs[part] ?? part);
    return { key: `${position}:${combo}`, parts, token: parts.join("+") };
  });
}

/** A key as one run of text, as `?` draws it: each step as `keyTokens` writes it, with a space
 *  between the two presses of a key of two (`g i`). A sentence that names a key writes it so:
 *  `⌘+Enter` on a Mac and `Ctrl+Enter` elsewhere for `$mod+Enter`, `⇧` and `Shift` for `Shift`. */
export function keyText(keys: string, isMac: boolean = IS_MAC): string {
  return keyTokens(keys, isMac)
    .map(({ token }) => token)
    .join(" ");
}

/** `words` as a sentence lists them: a comma between two, and `conjunction` before the last
 *  (`B, D and E`). */
export function listed(words: readonly string[], conjunction: "and" | "or"): string {
  return words.length < 2
    ? words.join("")
    : `${words.slice(0, -1).join(", ")} ${conjunction} ${words[words.length - 1]}`;
}
