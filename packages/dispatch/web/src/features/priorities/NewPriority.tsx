import { type ReactNode, useState } from "react";
import {
  borderDefault,
  focusVisibleRing,
  surfaceBg,
  surfaceMutedHoverBg,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { usePriorityWrites } from "./priority-writes";

/**
 * The way a priority is added: a button that opens a one-line form for its title. It stands in
 * the priorities table's toolbar, and on the page alone where no priority is written yet, which
 * is the one place a fresh deployment can write its first. Drawn only for a reader who may write
 * a priority (`usePriorityWrites`); every other reader is shown nothing here. The form takes a
 * line of its own in the flex row it is drawn in.
 */
export function NewPriority(): ReactNode {
  const { add, canWrite } = usePriorityWrites();
  const [adding, setAdding] = useState(false);
  const [title, setTitle] = useState("");
  if (!canWrite) return null;
  return (
    <>
      <button
        className={`rounded-lg border px-3 py-1.5 text-sm ${borderDefault} ${surfaceMutedHoverBg}`}
        onClick={() => setAdding((was) => !was)}
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
            setTitle("");
            setAdding(false);
          }}
        >
          <input
            aria-label="Title of the new priority"
            className={`min-w-64 flex-1 rounded-lg border px-3 py-1.5 text-sm ${borderDefault} ${surfaceBg} ${textPrimaryOnCanvas}`}
            onChange={(event) => setTitle(event.target.value)}
            placeholder="What the new priority is"
            value={title}
          />
          <button
            className={`min-h-11 rounded-lg border px-3 text-sm disabled:opacity-40 ${borderDefault} ${surfaceMutedHoverBg} ${focusVisibleRing}`}
            disabled={title.trim() === ""}
            type="submit"
          >
            Add priority
          </button>
          <button
            className={`min-h-11 rounded-lg border px-3 text-sm ${borderDefault} ${surfaceMutedHoverBg}`}
            onClick={() => {
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
