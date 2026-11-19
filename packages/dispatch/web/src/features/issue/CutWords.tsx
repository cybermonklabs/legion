import { type ReactNode, useRef, useState } from "react";

import { pageQuietText } from "../../theme/classes";
import { useClamp } from "./NodeRow";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";

/**
 * A node's words as a filter's own body draws them, cut as the outline cuts an agent's words
 * (ruling 1 and ruling 6 of dispatch://INFRA-1196's build-rulings-md, the prototype's
 * `CUT_LINES`): at six lines where more than 20 words would be hidden, with "more · N words"
 * opening the rest in place, and whole while `whole` holds - the focused node, or one the
 * reader opened. The cut is measured by `NodeRow.tsx`'s `useClamp`, so a row of the outline and
 * a statement of the Spec or the Verification filter cut at one place.
 *
 * The control leaves the focus where it is, as the outline's own does: focusing the node would
 * draw the words whole under the pointer and move the control before the press ends.
 */
export function CutWords({
  children,
  className,
  whole,
}: {
  /** The words, drawn as the node's own text is (`RowText`). */
  children: ReactNode;
  /** The words' type and colour. */
  className: string;
  /** Whether the words are drawn whole whatever their length. */
  whole: boolean;
}): ReactNode {
  const words = useRef<HTMLDivElement | null>(null);
  const clamp = useClamp(words, true);
  const [opened, setOpened] = useState(false);
  const clamped = clamp !== undefined && !opened && !whole;
  return (
    <>
      <div
        className={`relative ${className} ${clamped ? "overflow-clip" : ""}`}
        data-node-words=""
        ref={words}
        style={clamped ? { maxHeight: clamp.height } : undefined}
      >
        {children}
        {clamped && !clamp.pictures ? (
          <span
            aria-hidden="true"
            className="pointer-events-none absolute inset-x-0 bottom-0 h-8 bg-[linear-gradient(transparent,var(--color-page-paper))]"
          />
        ) : null}
      </div>
      {clamp === undefined || whole ? null : (
        <button
          className={`dispatch-inline-link self-start text-xs leading-[22px] font-medium underline decoration-page-hair underline-offset-[3px] ${pageQuietText}`}
          data-node-more=""
          onClick={() => setOpened((held) => !held)}
          onMouseDown={(event) => event.preventDefault()}
          tabIndex={-1}
          type="button"
          {...pageChrome}
        >
          {opened ? (
            CONTROL_LABELS.read_less
          ) : (
            <>
              {CONTROL_LABELS.read_more} ·{" "}
              <span {...authorText}>{clamp.hidden.toLocaleString()}</span>{" "}
              {CONTROL_LABELS.read_more_words}
            </>
          )}
        </button>
      )}
    </>
  );
}
