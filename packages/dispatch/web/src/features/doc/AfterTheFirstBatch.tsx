import { type ReactNode, useLayoutEffect, useRef, useState } from "react";

import { useAfterTheFirstBatch } from "./first-batch";
import { keepWhatStandsUnder } from "./readers-place";

/**
 * The two ways a part of the page the reader is not waiting for is drawn after the editor's
 * first batch (`first-batch.ts`, dispatch://INFRA-341 R11).
 *
 * The reads the first editable block needs none of wait for that batch already. Their panels'
 * own React does not: measured on this pod, the shell's render and its commits cost the main
 * thread 1,062 ms on the way to the first editable block of a 28-section document, and the
 * issue page's header, tab strip and artifact chrome 522 ms of React inside a 1,065 ms window.
 * The route has to render; the sidebar, the inbox peek, the live pill, the needs-you and
 * replies counts and the page's own chrome do not, and they are drawn after the batch instead.
 * The document's text is on the screen about 0.6 s into the load, drawn by the server
 * (`first-screen.ts`), so the reader is reading rather than waiting while that happens -
 * drawing the content before the chrome is the pattern the server's own first screen follows.
 *
 * Nothing changes on a page with no document: `useAfterTheFirstBatch` answers true there from
 * the first render, so every part below is drawn in the commit that would have drawn it
 * anyway. Nor is a part held from a reader who reaches for it - the navigation they opened,
 * the inbox they peeked at, the chrome they pressed - which is what `reached` says.
 */

/**
 * `children` once the first batch is in, `space` until then, and no box of its own: for a part
 * whose arrival moves nothing a reader can see - a column beside the document, a corner fixed
 * over it, a control drawn at the size it already stood at.
 *
 * `space` is what that part draws today before its own reads answer, so the reader is never
 * shown less than they are shown now and the box the answer paints into is already there.
 */
export function AfterTheFirstBatch({
  children,
  reached = false,
  space,
}: {
  children: ReactNode;
  reached?: boolean;
  space: ReactNode;
}): ReactNode {
  return useAfterTheFirstBatch(reached) ? children : space;
}

/**
 * The same, for chrome that stands over the document: the issue's header, its tab strip and an
 * artifact's own header, each of which pushes the document down the page when it lands.
 *
 * How tall each will be is the issue's own labels, chips, claim, route markers and components
 * laid out, or the artifact's name, approval and versions - none of it known until the part
 * renders, so no placeholder can be the right size and the page re-places the reader instead
 * (`packages/dispatch/AGENTS.md`, "A panel that arrives after the reader"). `keepWhatStandsUnder`
 * is that re-placing: the box this draws is watched for as long as it is on the page, and the
 * page moves by whatever it grows above the top of the reader's screen, so the words under it
 * stay on the line they are on. It is every change of the box rather than one commit of it,
 * because a part arrives over as many commits as its chunk needs - the chunk's own fallback
 * first, the part itself after it.
 *
 * A reader who presses or tabs into the space has reached for the chrome, and gets it then:
 * the press is `onPointerDownCapture` below and the Tab is the reach this draws, a control of
 * its own standing first in the box. A keyboard reader who presses an issue binding instead
 * reaches it the same way from the page's own keymap (`features/issue/IssuePage.tsx`'s
 * `reachingTheChrome`).
 *
 * The reach is drawn here rather than left to whatever the space holds, because a space holds
 * what the part it keeps room for looks like and not a tab stop: the issue's tab strip keeps a
 * row of `aria-hidden` spans, so a reader with no pointer tabbed from the header straight past
 * the strip and into the document, with no way to ask for either (dispatch://INFRA-341).
 * `label` names what that one Tab brings, so the reader is told what they have reached.
 */
export function ChromeAfterTheFirstBatch({
  children,
  label,
  space,
}: {
  children: ReactNode;
  /** What this box is keeping, as the reach reads it out: "Show the issue header". */
  label: string;
  space: ReactNode;
}): ReactNode {
  const [reached, setReached] = useState(false);
  const here = useAfterTheFirstBatch(reached);
  const box = useRef<HTMLDivElement>(null);
  const tookTheFocus = useRef(false);
  useLayoutEffect(() => {
    const element = box.current;
    return element === null ? undefined : keepWhatStandsUnder(element);
  }, []);
  // The handover unmounts whatever the reader is standing on: the space and the chrome are
  // different element types in the same slot, so React takes the whole space off the page in
  // the commit `setReached` makes, the reach inside it with it. Focus falls to `<body>` then
  // and the reader's next Tab starts again at the top of the page - the same failure this
  // branch fixed for the sidebar by hoisting its control out of the swap
  // (`features/sidebar/Sidebar.tsx`). This box is the one element that stands through every
  // commit of the handover - the swap, the chunk's own fallback, and the part after it - so
  // the reader is put on it, and their next Tab goes on into the chrome they asked for.
  useLayoutEffect(() => {
    // The commit that brought the chrome, and no other: `here` is what the swap turned on.
    if (!here || !tookTheFocus.current) {
      return;
    }
    tookTheFocus.current = false;
    const element = box.current;
    if (element !== null && !element.contains(document.activeElement)) {
      // `preventScroll`, because this box is the one the page is holding the reader's place
      // under: a focus that scrolled it into view would move the document by the very pixels
      // `keepWhatStandsUnder` is answering for, and the reader is standing here already.
      element.focus({ preventScroll: true });
    }
  }, [here]);
  const reach = here ? undefined : () => setReached(true);
  const reachedByFocus =
    reach === undefined
      ? undefined
      : () => {
          tookTheFocus.current = true;
          reach();
        };
  return (
    // `flow-root`, so what the box measures is the whole of what stands there. A margin on the
    // part inside it would otherwise collapse out through this box and be counted in neither
    // reading: the space's `mb-6` and the header's own `mb-3` differ by 12 px, which is 12 px
    // of the document's own place lost on every correction.
    //
    // `tabIndex={-1}` is the restore above and nothing else: the box takes a `focus()` and
    // never a Tab, so it is no stop of its own in the reader's way once the chrome is here.
    <div
      className="flow-root"
      onFocusCapture={reachedByFocus}
      onPointerDownCapture={reach}
      ref={box}
      tabIndex={-1}
    >
      {/* Standing first in the box, so one Tab reaches the chrome whatever the space holds,
          and `sr-only`, so the box it stands in is the height of the part it is keeping room
          for and not a pixel more. */}
      {here ? null : (
        <button className="sr-only" type="button">
          {label}
        </button>
      )}
      {here ? children : space}
    </div>
  );
}
