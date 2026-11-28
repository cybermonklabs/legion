import { ISSUE_TITLE_MAX } from "@legion/contracts";
import { type ReactNode, useRef, useState } from "react";
import { controlPadding } from "../../components/control-names";
import { landWhole } from "../../components/focus-place";
import {
  borderDefault,
  focusVisibleRing,
  surfaceBg,
  surfaceMutedHoverBg,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { usePriorityWrites } from "./priority-writes";

/** Each button here, at the size of the toolbar it stands in (`controlPadding`'s `toolbar`), so
 *  each is the size of the controls it stands beside: from 1280 px up the one height of a
 *  toolbar's controls. */
const BUTTON = `rounded-lg border text-sm ${controlPadding.toolbar} ${borderDefault} ${surfaceMutedHoverBg}`;

/**
 * The way a priority is added: a button that opens a one-line form for its title. It stands in
 * the priorities table's control row as the page's own control (`TableToolbar`), and on the page
 * alone where no priority is written yet, which is the one place a fresh deployment can write
 * its first. Drawn only for a reader who may write a priority (`usePriorityWrites`); every other
 * reader is shown nothing here. Open, the form takes a line of its own at the end of the flex
 * line it is drawn in (`order-last basis-full`), at every width, so that line never has to hold
 * the title box, Add priority and Cancel beside its other controls. In the control row the form
 * is marked as the row's own line (`data-row-line`): while it is drawn, the row's first line
 * wraps and the slot of the page's own controls draws no box, so the form stands under the row's
 * other controls and the row never runs past its width (`ControlRow`). Its title box, Add priority
 * and Cancel wrap inside the form where the line cannot hold them. Cancel puts the keyboard focus
 * back on New priority, the button that opened the form, before the form goes, with no scroll of
 * the engine's own (`landWhole`), so the focus never falls to the page's body and the row's
 * hand-off has no loss to answer.
 */
export function NewPriority(): ReactNode {
  const { add, canWrite } = usePriorityWrites();
  const [adding, setAdding] = useState(false);
  const [title, setTitle] = useState("");
  const opener = useRef<HTMLButtonElement>(null);
  if (!canWrite) return null;
  return (
    <>
      <button
        className={BUTTON}
        onClick={() => setAdding((was) => !was)}
        ref={opener}
        type="button"
      >
        New priority
      </button>
      {adding ? (
        <form
          aria-label="New priority"
          className="order-last flex basis-full flex-wrap items-center gap-2"
          data-row-line=""
          onSubmit={(event) => {
            event.preventDefault();
            const named = title.trim();
            if (named === "") return;
            add.mutate({ title: named });
            setTitle("");
            setAdding(false);
          }}
        >
          <input
            aria-label="Title of the new priority"
            className={`w-full min-w-0 flex-none rounded-lg border text-sm sm:w-auto sm:min-w-64 sm:flex-1 ${controlPadding.toolbar} ${borderDefault} ${surfaceBg} ${textPrimaryOnCanvas}`}
            maxLength={ISSUE_TITLE_MAX}
            onChange={(event) => setTitle(event.target.value)}
            placeholder="What the new priority is"
            value={title}
          />
          <button
            className={`${BUTTON} disabled:opacity-40 ${focusVisibleRing}`}
            disabled={title.trim() === ""}
            type="submit"
          >
            Add priority
          </button>
          <button
            className={BUTTON}
            onClick={() => {
              setAdding(false);
              setTitle("");
              // The form goes in the commit after this handler, so New priority takes the focus
              // while Cancel still holds it, and the landing's second look follows that commit.
              if (opener.current !== null) landWhole(opener.current);
            }}
            type="button"
          >
            Cancel
          </button>
        </form>
      ) : null}
    </>
  );
}
