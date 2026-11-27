// biome-ignore lint/style/noRestrictedImports: user-event cannot mark a key as an input method's (`isComposing`, keyCode 229); every row that sends such a key takes its form from here, and every one but the outline's, which dispatches its keys into the row's editor itself (`IssueTree.test.tsx`'s `pressIn`), sends it here
import { fireEvent } from "@testing-library/react";

/** A key pressed in a composition, as Chromium and Firefox send it: marked `isComposing`. */
export const COMPOSING = { isComposing: true } as const;

/** A key sent unmarked with keyCode 229, as Safari sends the key that ends a composition, after
 *  its `compositionend` (WebKit bug 165004). */
export const KEY_CODE_229 = { isComposing: false, keyCode: 229 } as const;

/** What each `keyDown` of `pressAsInputMethod` returned, as `dispatchEvent` answers it: `false`
 *  where a listener called `preventDefault`. */
export interface InputMethodPresses {
  /** The key pressed in a composition (`COMPOSING`). */
  readonly composing: boolean;
  /** The same key, plain. */
  readonly plain: boolean;
  /** The key that ends a composition as Safari sends it (`KEY_CODE_229`). */
  readonly safari: boolean;
}

/**
 * Sends `key` to `target` as an input method's key, in `form` - one of the two above - with what
 * `held` holds, a modifier pressed with it, and answers what its `keyDown` returned. For a row
 * that reads one form alone; `pressAsInputMethod` sends both and then the plain key.
 */
export function pressInputMethodKey(
  target: Document | Element | Window,
  key: string,
  form: typeof COMPOSING | typeof KEY_CODE_229,
  held: KeyboardEventInit = {}
): boolean {
  // user-event cannot mark a key as an input method's, so the key is fired.
  return fireEvent.keyDown(target, { ...held, ...form, key });
}

/**
 * Sends `key` to `target` as an input method's key, in both of the forms `inputMethodKey` reads
 * (`hooks/inputMethodKey.ts`), then plain, each with what `held` holds - a modifier pressed with
 * it - and answers what each `keyDown` returned. `beforePlain` runs between the two and the plain
 * key, where a row reads what the input method's keys left.
 *
 * A reader typing Chinese, Japanese or Korean confirms a word with Enter and drops a candidate with
 * Escape, and those keys are the input method's: a handler that reads the predicate leaves them
 * alone and acts on the plain key. A handler that calls `preventDefault` as it acts answers
 * `{ composing: true, plain: false, safari: true }`; with its predicate taken out, the input
 * method's keys go the same way as the plain one and answer `false` too.
 */
export function pressAsInputMethod(
  target: Document | Element | Window,
  key: string,
  {
    beforePlain,
    held = {},
  }: { readonly beforePlain?: () => void; readonly held?: KeyboardEventInit } = {}
): InputMethodPresses {
  const composing = pressInputMethodKey(target, key, COMPOSING, held);
  const safari = pressInputMethodKey(target, key, KEY_CODE_229, held);
  beforePlain?.();
  // The plain key goes the way the input method's keys went, so only the composition mark differs.
  const plain = fireEvent.keyDown(target, { ...held, key });
  return { composing, plain, safari };
}
