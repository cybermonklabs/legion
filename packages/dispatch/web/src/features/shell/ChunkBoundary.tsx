import { Component, type ReactNode } from "react";

import { ChunkNotLoaded } from "./chunk-component";

interface ChunkBoundaryProps {
  readonly children: ReactNode;
  /** What draws in the place of `children` once a render under them has thrown: nothing, unless
   *  a host names something. A chunk that could not load draws it too, unless the host names
   *  `notLoaded`. */
  readonly fallback?: ReactNode;
  /** What draws in the place of `children` once a chunk under them could not load
   *  (`ChunkNotLoaded`, `chunkComponent`): `fallback`, unless a host names something else. */
  readonly notLoaded?: ReactNode;
  /** For a host whose part draws with no reader action (the two lists of writes, the
   *  reference-preview layer): a value that changes whenever the part may be wanted again - a
   *  change of which writes a list shows or of where one stands, pressed by the reader or
   *  answered by Dispatch (`features/writes/write-hosts.tsx`); a card the reader asks for with a
   *  pointer over a reference, a keyboard focus on one, or the caret entering a link of an
   *  editable document where a hover opens no card (`features/refs/RefPreviewLayer.tsx`). For
   *  these hosts, never a move to another address: no one waits on the download that it would
   *  start. A boundary that has caught a throw draws `children` again once the value changes, so
   *  the part mounts afresh and asks for its chunk again (`chunkComponent` keeps no failed
   *  download), rather than drawing nothing for as long as its host stays mounted: no recovery
   *  reload need follow the failure (the comment below says when none does), and the reader can
   *  stay on that page for as long as they like. For a surface the reader opens with a key
   *  (`askedSurface`): the count of the reader's closes of its alert, a close on a move to
   *  another address included. A new count only draws a closed surface again, which asks for
   *  nothing. A boundary that has not failed draws nothing anew for a change, so the value may
   *  change often. */
  readonly reset?: unknown;
}

/** What a boundary caught, if anything: a chunk that could not load, or a render that threw. */
type Failure = "none" | "chunk" | "render";

/**
 * The error boundary of a host the shell draws outside the page's own boundary (`app.tsx`'s
 * `ErrorBoundary region="this page"`), around a part whose code is a chunk of its own: the
 * pending-writes indicator (`features/writes/write-hosts.tsx`), drawn in the status corner of
 * every signed-in page; the reference-preview layer (`features/refs/RefPreviewLayer.tsx`); and
 * the surfaces a reader opens with a key - the Create issue dialog, the shortcut sheet, the
 * search bar and the Inbox peek (`askedSurface`); the bodies in the peek draw their decision
 * cards from a chunk of their own too (`features/doc/TextAskCardHost.tsx`). The sign-in page's
 * list of kept writes is in the entry, with no chunk, and its host draws it under one of these
 * all the same, for the alert a throw of its render draws and the `reset` that draws it again.
 *
 * WHY EACH SUCH HOST NEEDS ONE. A chunk that fails to download while the browser is online is
 * thrown into the render that waits on it (`ChunkNotLoaded`, `chunk-component.tsx`), and the
 * nearest boundary catches it. Outside the page's boundary that is the app's top one
 * (`main.tsx`), which replaces the whole app: `AuthGate` unmounts, and with it this tab's
 * sign-in channel listener and its sign-in check, so the tab stops answering another tab's
 * sign-out and stops showing the sign-in page on a 401 while its writes go on. The recovery's
 * reload, which a failure like this asks for (`DeploymentResilience.tsx`), does not always follow
 * to bring the page back: the page can still count as being left after a navigation that never
 * went, until the reader presses on it or it is shown again, the reader can stay at the leave
 * prompt a write out raises, a page asks for one reload only, and the tab's budget of reloads can
 * be spent. So the failure stops here, and the rest of the page stays drawn and signed in. This
 * boundary only stops the throw: the reload is still asked for, as it is for any chunk a reader
 * waits on.
 *
 * Why not `ErrorBoundary`: that one tells the reader which region failed, with a reload button,
 * in the region's own place, for every throw. Here a chunk that could not load is told from a
 * render that threw. A host names what stands in its part's place for a render that threw
 * (`fallback`). A chunk that could not load draws the same, unless the host names something
 * else for it (`notLoaded`). So a host that names only `fallback` is never silent when its chunk
 * fails. A host that names neither draws nothing in its part's place. React logs the failure
 * itself (`onCaughtError`). Each host's own comment says what it names, and why.
 */
export class ChunkBoundary extends Component<ChunkBoundaryProps, { failed: Failure }> {
  state: { failed: Failure } = { failed: "none" };

  static getDerivedStateFromError(error: unknown): { failed: Failure } {
    return { failed: error instanceof ChunkNotLoaded ? "chunk" : "render" };
  }

  componentDidUpdate({ reset }: ChunkBoundaryProps): void {
    // Only a boundary that has failed draws `children` again. One that has not draws them
    // already, and a state set for nothing would cost a render at every change of `reset`, which
    // a list of writes changes whenever which writes it shows, or where one stands, changes.
    if (this.state.failed !== "none" && reset !== this.props.reset) {
      this.setState({ failed: "none" });
    }
  }

  render(): ReactNode {
    const { failed } = this.state;
    if (failed === "none") return this.props.children;
    return failed === "chunk" ? (this.props.notLoaded ?? this.props.fallback) : this.props.fallback;
  }
}
