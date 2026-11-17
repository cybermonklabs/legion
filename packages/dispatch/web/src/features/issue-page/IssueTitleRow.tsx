import { ISSUE_TITLE_MAX } from "@legion/contracts";
import { type ReactNode, useEffect, useRef, useState } from "react";

import {
  bgTransparent,
  borderStrongHover,
  borderTransparent,
  focusBorder,
  pageInkText,
} from "../../theme/classes";
import type { IssueDrafts } from "../issue/useIssueDrafts";
import { authorText } from "./page-structure";

/**
 * Row two of the header: the issue's title, which is the issue as a person names it, and the
 * editor it becomes when the reader presses it: a click, Enter or Space on the heading, or the
 * header's `e` key, which clicks it (`useHeaderKeys.ts`).
 *
 * The type is the prototype's `.title`: 600 weight, 22 px on 29 px from 761 px up, held to two
 * lines there, and 20 px on 26 px at 760 px and below, where nothing clamps it. It keeps that
 * size while the identity block is stuck to the top of the screen, as the approved picture of
 * the scrolled page draws it.
 *
 * The heading and the editor are one box. Both sit in one grid cell, and while the reader
 * edits, the heading stays in that cell unseen, holding the draft and drawn whole, so the
 * editor - a `textarea` with the heading's font and wrapping - is exactly the heading's size at
 * every length and the reader reads the whole title as they write it. An `<input>` was one line
 * whatever the title: ending the edit of a title that wraps grew the row back by a line between
 * the mousedown that blurred the input and the mouseup, so the press on a control under the
 * title landed on nothing. A title longer than the clamp is drawn whole while it is edited, so
 * the read view keeps that height until the press that ended the edit has been delivered, and
 * only then clamps it again.
 *
 * Escape and Enter end the edit from the keyboard, and give the focus back to the heading the
 * reader opened the editor from, so the keyboard reader is where they were. A focus alone
 * never opens the editor: the page gives the focus back to the heading on its own - Details
 * closing onto the control that opened it, a return to the window - and an editor opened then
 * would take the next shortcut key as text of the title.
 */
export function IssueTitleRow({
  drafts,
  isClosed,
}: {
  drafts: IssueDrafts;
  isClosed: boolean;
}): ReactNode {
  const [editing, setEditing] = useState(false);
  // Whether a mouse button is down right now, read by the editor's blur: a blur a press caused
  // keeps the heading at the editor's height until that press is released and its click has
  // been dispatched. A tap's mousedown comes after its pointerup, so the mouse's own events are
  // the ones that bracket the press on every input. They are listened to while the row is
  // drawn, not only while it edits: the blur a press causes ends the edit inside the press's
  // mousedown, so a listener taken away with the editor missed the mouseup, and the next edit a
  // key ended read a press that was over and held the title unclamped.
  const pressing = useRef(false);
  const [held, setHeld] = useState(false);
  const heading = useRef<HTMLHeadingElement>(null);
  // Set by a key that ends the edit, and read once the heading is drawn again: the focus goes
  // back to it.
  const returning = useRef(false);
  useEffect(() => {
    if (editing || !returning.current) return;
    heading.current?.focus({ preventScroll: true });
    returning.current = false;
  }, [editing]);
  useEffect(() => {
    const down = () => {
      pressing.current = true;
    };
    const up = () => {
      pressing.current = false;
    };
    window.addEventListener("mousedown", down, true);
    window.addEventListener("mouseup", up, true);
    return () => {
      window.removeEventListener("mousedown", down, true);
      window.removeEventListener("mouseup", up, true);
    };
  }, []);

  const type = "text-xl leading-[26px] min-[761px]:text-[22px] min-[761px]:leading-[29px]";
  // The box both views share, to the pixel: the font, the wrapping and the rule the editor
  // draws on focus, which the heading keeps transparent. The rule and the padding inside it
  // are 5 px on each side and 1 px above and below, which the wrapper's negative margins take
  // back, so the words have the header's whole column - 362 px at 390 px, the prototype's own -
  // and the row is the prototype's height: 2 px above the words and none below.
  const box = `[grid-area:1/1] min-w-0 rounded-md border px-1 font-semibold tracking-[-0.012em] whitespace-pre-wrap [overflow-wrap:anywhere] ${type} ${pageInkText}`;
  const whole = editing || held;

  return (
    <div className="-mx-[5px] mt-px -mb-px grid min-w-0">
      <h1
        aria-hidden={editing ? true : undefined}
        className={`${box} ${borderTransparent} ${whole ? "" : "min-[761px]:line-clamp-2"} ${
          editing ? "invisible" : isClosed ? "" : `cursor-text ${borderStrongHover}`
        }`}
        onClick={() => {
          if (!isClosed) setEditing(true);
        }}
        onKeyDown={(event) => {
          if (!isClosed && (event.key === "Enter" || event.key === " ")) {
            event.preventDefault();
            setEditing(true);
          }
        }}
        ref={heading}
        tabIndex={isClosed || editing ? -1 : 0}
        {...authorText}
      >
        {/* A trailing space or an empty draft still holds a line, as the editor does. */}
        {drafts.title === "" || drafts.title.endsWith(" ") ? `${drafts.title}\u200b` : drafts.title}
      </h1>
      {editing ? (
        <textarea
          aria-describedby={drafts.titleError === null ? undefined : "issue-title-help"}
          aria-label="Issue title"
          className={`${box} resize-none overflow-hidden outline-none ${borderTransparent} ${bgTransparent} ${borderStrongHover} ${focusBorder}`}
          disabled={isClosed}
          maxLength={ISSUE_TITLE_MAX}
          onBlur={() => {
            drafts.requestTitleSubmit();
            if (pressing.current) {
              setHeld(true);
              // The click follows the mouseup in the same task, so a task queued from the mouseup
              // runs once the click has been dispatched to the control under the pointer.
              window.addEventListener("mouseup", () => window.setTimeout(() => setHeld(false), 0), {
                capture: true,
                once: true,
              });
            }
            setEditing(false);
          }}
          // A title is one line of words: a line break typed or pasted is a space.
          onChange={(event) => drafts.writeTitle(event.target.value.replace(/\s*\n\s*/gu, " "))}
          onKeyDown={(event) => {
            if (event.key === "Escape") {
              drafts.discardTitle();
              returning.current = true;
              setEditing(false);
              return;
            }
            if (event.key === "Enter") {
              event.preventDefault();
              returning.current = true;
              event.currentTarget.blur();
            }
          }}
          ref={(node) => node?.focus()}
          rows={1}
          value={drafts.title}
        />
      ) : null}
    </div>
  );
}
