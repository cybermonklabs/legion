import { ISSUE_TITLE_MAX } from "@legion/contracts";
import { type ReactNode, useState } from "react";
import type { BarActionProps } from "../../components/BarAction";
import { textGlyph } from "../../components/bar-look";
import { controlPadding } from "../../components/control-names";
import {
  borderDefault,
  focusVisibleRing,
  surfaceBg,
  surfaceMutedHoverBg,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { usePriorityWrites } from "./priority-writes";

/** **New priority**'s glyph, drawn before its words where the control bar draws it. */
const PLUS_GLYPH = textGlyph("+");

/** Each button here, at the size of the toolbar it stands by (`controlPadding`'s `toolbar`), so
 *  each is the size of the controls it stands beside: from 1280 px up the one height of a
 *  toolbar's controls. */
const BUTTON = `rounded-lg border text-sm ${controlPadding.toolbar} ${borderDefault} ${surfaceMutedHoverBg}`;

/**
 * The one-line form a priority is added with: its title, **Add priority** and **Cancel**. Its
 * opener owns the write (`onAdd`), which outlives the form: the form closes as the write goes
 * out, and a closed form is no longer drawn. The form keeps a line of its own wherever it stands.
 */
function NewPriorityForm({
  onAdd,
  onClose,
}: {
  onAdd: (title: string) => void;
  onClose: () => void;
}): ReactNode {
  const [title, setTitle] = useState("");
  return (
    <form
      aria-label="New priority"
      className="flex basis-full flex-wrap items-center gap-2"
      onSubmit={(event) => {
        event.preventDefault();
        const named = title.trim();
        if (named === "") return;
        onAdd(named);
        onClose();
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
      <button className={BUTTON} onClick={onClose} type="button">
        Cancel
      </button>
    </form>
  );
}

/**
 * **New priority** in the priorities table's control bar: the action the bar draws (`TableToolbar`,
 * a quiet button of its line, and an item of **More** below 640 px of the bar's width), and the
 * form it opens, which the page draws under the bar, outside it. Only a reader who may write a
 * priority (`usePriorityWrites`) is offered the action; every other reader is offered none, and
 * the form is never drawn for them.
 */
export function useNewPriority(): { action?: BarActionProps; form: ReactNode } {
  const { add, canWrite } = usePriorityWrites();
  const [adding, setAdding] = useState(false);
  if (!canWrite) return { form: null };
  return {
    action: {
      icon: PLUS_GLYPH,
      label: "New priority",
      onPress: () => setAdding((was) => !was),
    },
    form: adding ? (
      <NewPriorityForm onAdd={(title) => add.mutate({ title })} onClose={() => setAdding(false)} />
    ) : null,
  };
}

/**
 * The way the first priority is added, on the page alone where no priority is written yet, which
 * is the one place a fresh deployment can write its first: a button that opens the one-line form
 * beside it, on a line of its own. It stands in no control bar. Drawn only for a reader who may
 * write a priority (`usePriorityWrites`); every other reader is shown nothing here.
 */
export function NewPriority(): ReactNode {
  const { action, form } = useNewPriority();
  if (action === undefined) return null;
  return (
    <>
      <button className={BUTTON} onClick={action.onPress} type="button">
        New priority
      </button>
      {form}
    </>
  );
}
