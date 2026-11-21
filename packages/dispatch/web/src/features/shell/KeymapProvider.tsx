import { type PropsWithChildren, type ReactNode, useLayoutEffect, useMemo } from "react";

import { ChordIndicator } from "./ChordIndicator";
import { appKeymap, KeymapPersonContext } from "./keymap";
import { usePersonKeys } from "./keymap-keys";
import { useLinkCopy } from "./link-copy";

/**
 * Binds the application registry to `window` keydown, renders the chord indicator, gives every
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
  // The one window listener is attached in the commit that first draws the provider, before the
  // browser paints it or takes the next key, so no page under the provider is drawn before the
  // keymap listens. `AuthGate` draws the shell in a render its own passive effect starts
  // (`shellFor`), and a passive effect of the commit that render makes ran after the Inbox's
  // heading was in the page - 6 to 30 ms after it, and up to a second on a slow machine - so a key
  // pressed then reached no listener (dispatch://INFRA-2160/artifact/keymap-listener-order-md).
  // The bindings register in layout effects of their own (`useKeymap`) under the person's keys,
  // which `usePersonKeys` reads from the browser's storage as it renders, so that first key
  // matches the person's keys, or the defaults.
  useLayoutEffect(() => {
    const handleKeyDown = (event: KeyboardEvent) => appKeymap.handleKeyDown(event);
    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, []);
  useLinkCopy();

  return (
    <KeymapPersonContext.Provider value={person}>
      {children}
      <ChordIndicator />
    </KeymapPersonContext.Provider>
  );
}
