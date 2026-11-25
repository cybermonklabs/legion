import { type PropsWithChildren, type ReactNode, useLayoutEffect, useMemo } from "react";

import { ChordIndicator } from "./ChordIndicator";
import { appKeymap, KeymapPersonContext } from "./keymap";
import { usePersonKeys } from "./keymap-keys";
import { useLinkCopy } from "./link-copy";

/**
 * Binds the application registry to `window` - the keydown that runs a binding, and before every
 * control the capture-phase keydown and keyup, and the window's own blur, that keep a held key's
 * repeats from the page (`Keymap.captureKeyDown`) - renders the chord indicator, gives every
 * registration below it the keys `login` changed (`keymap-keys.ts`), and registers the one key of
 * no page, the `y` that copies a focused link's address (`link-copy.ts`). With no `login` -
 * nobody signed in, or a unit test - every action takes its defaults.
 */
export function KeymapProvider({
  children,
  login,
}: PropsWithChildren<{ login?: string }>): ReactNode {
  const personKeys = usePersonKeys(login);
  const person = useMemo(() => ({ login, personKeys }), [login, personKeys]);
  // The keydown listener that runs a binding is attached in the commit that first draws the
  // provider, before the browser paints it or takes the next key, so no page under the provider
  // is drawn before the keymap listens. `AuthGate` draws the shell in a render its own passive
  // effect starts (`shellFor`), and a passive effect of the commit that render makes ran after
  // the Inbox's heading was in the page - 6 to 30 ms after it, and up to a second on a slow
  // machine - so a key pressed then reached no listener
  // (dispatch://INFRA-2160/artifact/keymap-listener-order-md).
  // The bindings register in layout effects of their own (`useKeymap`) under the person's keys,
  // which `usePersonKeys` reads from the browser's storage as it renders, so that first key
  // matches the person's keys, or the defaults.
  // A held key's repeats are read in the same commit, in the window's capture phase, so that a
  // repeat that would act reaches no control and no listener that acts: the document's listeners
  // (`AnchoredPanel`'s Escape) and React's come after the window's, and the window's capture
  // listeners of the pages under it are attached later, in their passive effects. The one attached
  // before it, at start (`installChunkFailureRecovery`), only notes that the reader is still here.
  // A key's release, and the window's loss of the focus, which takes the releases elsewhere, end
  // its hold. The listeners go as the provider does, and so do the holds they noted.
  useLayoutEffect(() => {
    const listening = new AbortController();
    const { signal } = listening;
    const capture = { capture: true, signal };
    window.addEventListener("keydown", (event) => appKeymap.captureKeyDown(event), capture);
    window.addEventListener("keyup", (event) => appKeymap.handleKeyUp(event), capture);
    window.addEventListener("blur", () => appKeymap.releaseKeys(), { signal });
    window.addEventListener("keydown", (event) => appKeymap.handleKeyDown(event), { signal });
    return () => {
      listening.abort();
      appKeymap.releaseKeys();
    };
  }, []);
  useLinkCopy();

  return (
    <KeymapPersonContext.Provider value={person}>
      {children}
      <ChordIndicator />
    </KeymapPersonContext.Provider>
  );
}
