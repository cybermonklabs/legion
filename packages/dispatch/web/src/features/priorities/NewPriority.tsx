import { ISSUE_TITLE_MAX } from "@legion/contracts";
import { type ReactNode, type RefObject, useState } from "react";
import { controlPadding } from "../../components/control-names";
import { useFocusBack } from "../../hooks/useFocusBack";
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
 * reader is shown nothing here. The form opens beside the button in the control row, whose slot
 * keeps one line from `sm` up, and takes a line of its own in a row that wraps. Its submit - Add
 * priority, or Enter in its title - and its Cancel each end it with the focus back on the
 * button, which the form's going would leave on the page's body (`useFocusBack`). The first
 * priority, added from the page alone, takes that button off the page as it lands, and the page
 * gives the focus on to the table's, so long as the add's end draws one (`onAdd`, `opener`).
 */
export function NewPriority({
  onAdd,
  opener,
}: {
  /** Told as the form sends a priority, with the add's end: once Dispatch has answered it, landed
   *  or not, and the read of the priorities it set off has answered (`usePriorityWrites`' `add`).
   *  The page's empty state takes it, whose New priority the first priority's landing takes off
   *  the page (`PrioritiesPage`). */
  readonly onAdd?: (ended: Promise<void>) => void;
  /** Where the page reaches this New priority: the table's, which takes the focus the empty
   *  state's going leaves on the page's body (`PrioritiesPage`). */
  readonly opener?: RefObject<HTMLButtonElement | null>;
} = {}): ReactNode {
  const { add, canWrite } = usePriorityWrites();
  const [adding, setAdding] = useState(false);
  const [title, setTitle] = useState("");
  const focusBack = useFocusBack(adding);
  if (!canWrite) return null;
  const closeForm = () => {
    focusBack.handBack();
    setTitle("");
    setAdding(false);
  };
  return (
    <>
      <button
        className={BUTTON}
        onClick={() => setAdding((was) => !was)}
        ref={(node) => {
          focusBack.opener.current = node;
          if (opener !== undefined) opener.current = node;
        }}
        type="button"
      >
        New priority
      </button>
      {adding ? (
        <form
          aria-label="New priority"
          className="flex basis-full flex-wrap items-center gap-2"
          onSubmit={(event) => {
            event.preventDefault();
            const named = title.trim();
            if (named === "") return;
            // A refusal is the shell's line's to say (`useKeyedWrite`), so the end is told
            // alike whether the add landed or not.
            const ended = add.mutateAsync({ title: named }).then(
              () => undefined,
              () => undefined
            );
            onAdd?.(ended);
            closeForm();
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
          <button className={BUTTON} onClick={closeForm} type="button">
            Cancel
          </button>
        </form>
      ) : null}
    </>
  );
}
