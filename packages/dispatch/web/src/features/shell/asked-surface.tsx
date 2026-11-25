import {
  type ComponentType,
  type ReactNode,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { flushSync } from "react-dom";

import { inputClasses, surfaceBg } from "../../theme/classes";
import { ChunkBoundary } from "./ChunkBoundary";
import type { Chunk } from "./chunk-component";
import { importWhenOnline, prefetchChunk, renderChunk } from "./chunk-loads";
import { ERROR_ACTION_CLASS, ErrorView } from "./ErrorBoundary";
import { DIALOG_SCOPE, inputMethodKey, useKeymapScope } from "./keymap";
import { useCloseOnNavigation, useDialog } from "./useDialog";

/** What a surface drawn through `askedSurface` takes, beside its own props. */
export interface AskedSurfaceProps {
  /** Closes the surface: what its host does when the reader closes it, and what this does when
   *  the reader cancels the open before the surface's chunk is in, and when the reader closes
   *  the alert that stands in the surface's place (`CouldNotOpen`) after its chunk could not
   *  load or a render under it threw. */
  readonly onClose: () => void;
  /** Whether the reader has the surface open. A host that draws its surface only while it is
   *  open - the Create issue dialog, the shortcut sheet - leaves it unset, and the surface is
   *  open while it is drawn. A host that keeps its surface mounted while closed - the search bar,
   *  which runs a chosen row once it has closed, and the Inbox peek, which keeps its filters for
   *  the next open - passes it. */
  readonly open?: boolean;
  /** What the reader typed into the placeholder while the surface's chunk was on its way, which
   *  the surface puts in its own text box as it mounts, so those keys arrive late rather than
   *  being lost. Empty when the chunk was in at the press. A surface whose keys after the open
   *  are shortcuts rather than words for a box - the Inbox peek, whose focus lands on its Close
   *  and whose letters walk and act on its rows - takes no `typed`, and the keys are dropped:
   *  replayed late, they would act on rows the reader has not seen. */
  readonly typed?: string;
}

/**
 * A surface the reader opens with a key or a press - the Create issue dialog
 * (`features/issue/CreateIssueDialogHost.tsx`), the shortcut sheet (`ShortcutHelpHost.tsx`),
 * the search bar (`features/search/SearchPaletteHost.tsx`) and the Inbox peek
 * (`features/inbox/InboxHost.tsx`) - whose code is a chunk of its own.
 *
 * WHY NOT `lazy` AND `Suspense`, OR `chunkComponent`. Each of these opens on a key, and the keys
 * a reader types straight after it are meant for the surface. Until a surface has mounted, no
 * `dialog` scope is on the keymap's stack (`useDialog.ts`), so the page's own shortcuts are live
 * and take those keys: on an issue page a digit writes the issue's priority and `e` puts what
 * follows into its title (`features/issue/IssuePage.tsx`). Behind `lazy` that window is at
 * least 300 ms on every first open, also with the chunk in (`chunk-component.tsx` says why), and
 * a chunk that is not in adds its download. Here a chunk that is in draws its surface in the
 * commit of the press, as the entry's own dialogs do; `chunkComponent` would do that too, but it
 * asks for its chunk as it mounts and draws a fixed fallback, and these need what follows.
 *
 * WHILE THE CHUNK IS NOT IN, A PLACEHOLDER HOLDS THE KEYS. The press draws a small box in the
 * entry's code, which takes the focus in the commit of the press and pushes the `dialog` scope,
 * so no key reaches the page's shortcuts or a field of the page. What the reader types into it
 * is handed to the surface as `typed` when the chunk lands - into its text box, or nowhere for
 * the Inbox peek, whose keys are shortcuts (`typed` says why) - and the surface then takes the
 * box's place and the focus. The reader can call the open off: Escape in the box, a press that
 * takes the focus out of it, or a move to another address each close it, and a chunk that lands
 * after that opens nothing and takes no focus - a dialog must not open on its own against a page
 * the reader has left or a field they went on to, which a download held by an outage would
 * otherwise do minutes later (`importWhenOnline`). A window that loses the focus to another
 * application cancels nothing: the box gets the focus back with the window. Tab and Shift+Tab
 * stay in the box, as they stay in an open dialog (`Placeholder` says what each key does there).
 *
 * WHEN THE CHUNK IS ASKED FOR. On every open while it is not in, under `renderChunk` and
 * `importWhenOnline`: the reader is waiting on it, so a download that fails online spends one of
 * the tab's recovery reloads (`DeploymentResilience.tsx`), and one that fails offline is asked
 * for again once the network is back. A download that fails online draws the alert in the box's
 * place, "Could not open <name>." with Close and Reload (`CouldNotOpen`). The alert takes the
 * focus and the keys from the box in the same commit, so no key reaches the page between the two.
 * The open stands until the reader closes the alert. Closed with no word, the open would give the
 * keys back to the page, and where no reload follows - the tab's reloads spent, say - the
 * reader's next key would reach the page's shortcuts: on an issue page a digit would write the
 * issue's priority. The alert's Close, Escape and a move to another address close the surface,
 * and the next press asks for the chunk again (`keptUntilRejected` keeps no failure). An open the
 * reader called off leaves its download to finish as a prefetch (`prefetchChunk`): nobody waits
 * on it any more, so its failure - after an outage the reader called the open off in, say -
 * reloads nothing under them. A host that wants its chunk in before the press, as most of these
 * do, starts it itself with `prefetchChunk`.
 *
 * Each surface draws under a `ChunkBoundary`, since every one of these hosts sits outside the
 * page's error boundary (`app.tsx`). A render that throws under it - a chunk the surface draws
 * from failing online, the decision card of a body in the peek among them
 * (`features/doc/TextAskCardHost.tsx`), or any other fault of the surface - draws an alert in
 * the surface's place, "Could not open <name>." with Close and Reload (`CouldNotOpen`), as the
 * boundary's `fallback`, which it draws for a chunk that could not load too, and the page stays
 * drawn and signed in, rather than reaching the app's top boundary (`main.tsx`). A render error
 * is never silent: a surface that closed with no word would look like a key that did nothing,
 * and the reader would press it again into the same fault. The surface's own chunk failing to
 * download is no throw in a render: it rejects the download above, and the same alert takes the
 * box's place, as "WHEN THE CHUNK IS ASKED FOR" says.
 *
 * `name` is the name the surface's own element carries - "Create issue", "Keyboard shortcuts",
 * "Search", "Inbox" - and names the box and the alert that stand in its place.
 */
export function askedSurface<P extends AskedSurfaceProps>(
  source: Chunk<P>,
  name: string
): ComponentType<P> {
  function AskedSurface(props: P): ReactNode {
    const { onClose } = props;
    const open = props.open !== false;
    const Component = source.loaded();
    // Set once the chunk lands, only to draw again: `source.loaded()` is what this reads.
    const [, landed] = useState<object>();
    // What the placeholder holds for the surface. A closed surface has nothing typed, so the next
    // open starts empty.
    const held = useRef<Held>({ opener: null, text: "" }).current;
    if (!open) held.text = "";
    // Whether this open's download failed online: the alert then stands in the box's place, and
    // holds the focus and the keys until the reader closes it. A closed surface keeps no failure,
    // so the next open asks for the chunk again, also when the host closed it.
    const [downloadFailed, setDownloadFailed] = useState(false);
    if (!open && downloadFailed) setDownloadFailed(false);
    // How many times the reader has closed the alert that stood in the surface's place: the
    // boundary's `reset`, so each close draws the boundary's children again with no failure kept,
    // and the next open draws the surface afresh.
    const [closes, closed] = useState(0);
    useEffect(() => {
      if (!open || Component !== undefined) {
        return;
      }
      let live = true;
      // The download, counted as a prefetch for as long as it is out, and as a render only
      // until it lands or the reader calls the open off (`off`), whichever comes first.
      const off = Promise.withResolvers<void>();
      const loading = importWhenOnline(source.load);
      prefetchChunk(() => loading);
      void renderChunk(() => Promise.race([loading, off.promise])).then(
        () => {
          if (!live) return;
          // The page as the press found it, and then the surface, in one step: the focus goes
          // back to the opener, and the surface draws and commits before the browser handles
          // another event, so no key lands in the opener between the two. The surface's first
          // render then reads the page the reader opened it over - the bar takes its rows from
          // what holds the focus (`appKeymap.actions()` in `features/search/SearchPalette.tsx`),
          // and with the box focused it would withdraw every row bound to the card or the page
          // the reader was on - and its `useDialog` records the opener as what the focus goes
          // back to when it closes. The box's blur on the way cancels nothing: its opener is
          // taken first. The page does not scroll to the opener: a reader who scrolled while the
          // box was up stays where they scrolled to.
          const { opener } = held;
          held.opener = null;
          if (opener instanceof HTMLElement) opener.focus({ preventScroll: true });
          flushSync(() => landed({}));
        },
        () => {
          // The alert takes the box's place. The box gives the focus back to the opener as it
          // goes, and the alert takes it in the same commit and records the opener as what the
          // focus goes back to when it closes (`useDialog`). The recovery asks for the tab's
          // reload; when none follows, the alert holds the keys until the reader closes it.
          if (live) setDownloadFailed(true);
        }
      );
      return () => {
        live = false;
        off.resolve();
      };
    }, [open, Component, held]);
    const alert = (
      <CouldNotOpen
        name={name}
        onClose={() => {
          closed(closes + 1);
          setDownloadFailed(false);
          onClose();
        }}
      />
    );
    return (
      <ChunkBoundary fallback={alert} reset={closes}>
        {Component !== undefined ? (
          <Component {...props} typed={held.text} />
        ) : open ? (
          downloadFailed ? (
            alert
          ) : (
            <Placeholder held={held} name={name} onCancel={onClose} />
          )
        ) : null}
      </ChunkBoundary>
    );
  }
  return AskedSurface;
}

/** Where the box and the alert stand: in the top middle of the window, over the page. */
const FLOATING =
  "fixed top-20 left-1/2 z-50 w-72 max-w-[calc(100vw-2rem)] -translate-x-1/2 shadow-2xl";

/**
 * What stands in a surface's place once its chunk could not download online, or once a render
 * under it has thrown: the reader is told that the surface could not open, and can close this or
 * reload the tab. It looks as a region that stopped rendering looks (`ErrorView`), over the page.
 * It is a dialog, as the surface was (`useDialog`): it takes the focus and the keys, so a key
 * typed after the failure reaches no shortcut of the page under it. Close and Escape close the
 * surface. A move to another address closes it too, as it closes the box and the surfaces
 * (`useCloseOnNavigation`): left open, its `dialog` scope would mask the shortcuts of the page
 * the reader moved to. The focus then goes back to where the open found it: the surface's
 * opener, which the box gives back as it goes (`Placeholder`), and which a surface that was open
 * hands to this as it goes (`handOverFocus` in `useDialog.ts`).
 */
function CouldNotOpen({ name, onClose }: { name: string; onClose: () => void }): ReactNode {
  const { containerRef } = useDialog<HTMLElement>({ onClose, open: true });
  useCloseOnNavigation(true, onClose);
  const heading = `Could not open ${name}.`;
  return (
    <ErrorView
      aria-label={heading}
      aria-modal="true"
      className={`${FLOATING} ${surfaceBg}`}
      heading={heading}
      ref={containerRef}
      reload="Reload"
      role="alertdialog"
    >
      <button className={`${ERROR_ACTION_CLASS} mr-3`} onClick={onClose} type="button">
        Close
      </button>
    </ErrorView>
  );
}

/** What the placeholder holds for the surface that takes its place: what held the focus at the
 *  press, until the surface lands, and what the reader typed into the box. */
interface Held {
  opener: Element | null;
  text: string;
}

/**
 * The box that holds the keys while an asked-for surface's chunk is on its way: the focus from
 * the commit of the press, and the `dialog` scope, which masks every shortcut beneath it
 * (`keymap.ts`). It is named for the surface it holds the keys for, "<name> (loading)", and it is
 * busy (`aria-busy`) until the surface takes its place.
 *
 * A key in the box does what it would do in the open surface, or nothing:
 * - Tab and Shift+Tab stay in the box, as they stay in an open dialog (`useDialog`), and move
 *   nothing, since the box is its one control. Let through, Shift+Tab would put the focus on the
 *   page, the box's blur would call the open off, and the next digit would write the issue's
 *   priority; Tab from the box, the shell's last control, would take the focus out of the
 *   document, cancel nothing, and let the surface open later while the reader's keys went
 *   elsewhere.
 * - Escape calls the open off, and goes no further, so a dialog the box sits over is not closed
 *   with it. An Escape that ends an input method's composition only ends it, and what the reader
 *   typed stays. `inputMethodKey` catches both `isComposing` and Safari's unmarked keyCode 229
 *   Escape, as Details' Escape rule (`escapeClosesDrawer` in `useDialog.ts`) does. An open
 *   surface's own Escape runs through `useDialog` and `keymap.ts`, which skips `isComposing`
 *   alone and not keyCode 229.
 * - Enter does nothing: the box is in no form. That is the safe choice: in the surface an Enter
 *   can submit, and a submit the reader made before they saw the surface is one they did not see.
 * - Every other key types into the box, and the surface takes what it holds as `typed`.
 *
 * Called off while it holds the focus - an Escape, a move to another address - it gives the focus
 * back to where the press found it as it goes, as a closing dialog does (`useDialog`), and the
 * page does not scroll to it. So does a box the alert takes the place of after its chunk failed,
 * and the alert, drawn in the same commit, takes the focus from there and gives it back there when
 * it closes (`CouldNotOpen`). That runs in the commit that removes the box, and React fires no
 * `onBlur` for a focus moved in a commit, so that move cancels nothing more. A box the focus has
 * already left gives nothing back: the reader is where they went.
 */
function Placeholder({
  held,
  name,
  onCancel,
}: {
  held: Held;
  name: string;
  onCancel: () => void;
}): ReactNode {
  const box = useRef<HTMLInputElement>(null);
  useKeymapScope(DIALOG_SCOPE);
  useCloseOnNavigation(true, onCancel);
  useLayoutEffect(() => {
    const input = box.current;
    held.opener = document.activeElement;
    input?.focus();
    return () => {
      const { opener } = held;
      if (document.activeElement === input && opener instanceof HTMLElement)
        opener.focus({ preventScroll: true });
    };
  }, [held]);
  return (
    <input
      aria-busy="true"
      aria-label={`${name} (loading)`}
      className={`${FLOATING} rounded-lg border px-3 py-2 text-sm ${inputClasses(false)}`}
      onBlur={() => {
        if (held.opener !== null && document.hasFocus()) onCancel();
      }}
      onChange={(event) => {
        held.text = event.target.value;
      }}
      onKeyDown={(event) => {
        if (event.key === "Tab") event.preventDefault();
        if (event.key === "Escape" && !inputMethodKey(event.nativeEvent)) {
          event.stopPropagation();
          onCancel();
        }
      }}
      placeholder="Loading…"
      ref={box}
    />
  );
}
