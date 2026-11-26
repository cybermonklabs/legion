import { ISSUE_TITLE_MAX } from "@legion/contracts";
import { type ReactNode, useState } from "react";
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
 * button, which the form's going would leave on the page's body (`useFocusBack`).
 */
export function NewPriority(): ReactNode {
  const { add, canWrite } = usePriorityWrites();
  const [adding, setAdding] = useState(false);
  const [title, setTitle] = useState("");
  const focusBack = useFocusBack(adding);
  if (!canWrite) return null;
  return (
    <>
      <button
        className={BUTTON}
        onClick={() => setAdding((was) => !was)}
        ref={focusBack.opener}
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
            add.mutate({ title: named });
            focusBack.handBack();
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
              focusBack.handBack();
              setAdding(false);
              setTitle("");
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
