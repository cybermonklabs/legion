import { type FormEvent, type ReactNode, useEffect, useRef } from "react";

import { inputMethodKey } from "../../hooks/inputMethodKey";
import {
  borderTransparent,
  dangerText,
  inputClasses,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonText,
  textMutedHoverToSecondary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import type { IssueDrafts } from "./useIssueDrafts";

/** What the issue's message route is, said beside its value and its editor. The header draws it
 *  once, as `issue-route-hint`, which each of them names. */
export const routeHint =
  "New asks, comments, and messages on this issue wake this agent or role; replies inside a thread reach their participants directly. It is where messages go, not who is working the issue — that is the claim.";

const saveButton = `min-h-11 rounded px-2 py-1 text-sm font-medium disabled:cursor-not-allowed md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonDisabledText}`;
const cancelButton = `min-h-11 rounded border px-2 py-1 text-sm font-medium md:min-h-8 ${borderTransparent} ${textMutedHoverToSecondary}`;
const fieldBox = `w-full rounded px-2 py-1 text-sm outline-none md:w-64 ${inputClasses(true)}`;

interface HeaderFormProps {
  drafts: IssueDrafts;
  isClosed: boolean;
  /** Closes the editor: after a save that was sent or queued, a Cancel or an Escape. */
  onClose: () => void;
  /** Whether the header's issue write is out, which holds Save. */
  saving: boolean;
}

/**
 * The input an editor opens on: the trigger that opened it unmounts with the press, so without
 * this the focus would drop to the body and a keyboard reader would have to find the form again.
 */
function useFocusOnOpen() {
  const input = useRef<HTMLInputElement>(null);
  useEffect(() => {
    input.current?.focus();
  }, []);
  return input;
}

/** The issue header's editor of the message route. */
export function IssueRouteForm({ drafts, isClosed, onClose, saving }: HeaderFormProps): ReactNode {
  const input = useFocusOnOpen();
  const discard = () => {
    drafts.discardRoute();
    onClose();
  };
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (drafts.requestRouteSubmit()) onClose();
  };
  return (
    <form className="mt-2 flex flex-wrap items-center gap-2" onSubmit={save}>
      <label
        aria-describedby="issue-route-hint"
        className={`text-sm font-medium ${textSecondaryOnSurface}`}
        htmlFor="issue-route"
        title={routeHint}
      >
        Message route:
      </label>
      <input
        aria-label="Message route"
        aria-describedby="issue-route-hint issue-route-help"
        className={fieldBox}
        disabled={isClosed}
        id="issue-route"
        onChange={(event) => drafts.writeRoute(event.target.value)}
        onKeyDown={(event) => {
          // The Escape that ends an input method's composition is the input method's.
          if (event.key === "Escape" && !inputMethodKey(event)) discard();
        }}
        placeholder="role:legion-controller-core"
        ref={input}
        value={drafts.route}
      />
      <button
        className={saveButton}
        disabled={isClosed || !drafts.routeIsValid || saving}
        type="submit"
      >
        Save route
      </button>
      <button className={cancelButton} onClick={discard} type="button">
        Cancel
      </button>
      <span
        className={drafts.routeIsValid ? "sr-only" : `text-sm ${dangerText}`}
        id="issue-route-help"
      >
        A message route must be role:[a-z0-9-]+ or session:[0-9a-f-]{`{16,}`}.
      </span>
    </form>
  );
}

/** The issue header's editor of the parent, a key in `project` or in another project. */
export function IssueParentForm({
  drafts,
  isClosed,
  onClose,
  project,
  saving,
}: HeaderFormProps & { project: string }): ReactNode {
  const input = useFocusOnOpen();
  const discard = () => {
    drafts.discardParent();
    onClose();
  };
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (drafts.requestParentSubmit()) onClose();
  };
  return (
    <form className="mt-2 flex flex-wrap items-center gap-2" onSubmit={save}>
      <label className="sr-only" htmlFor="issue-parent">
        Parent
      </label>
      <span aria-hidden="true" className={`text-sm font-medium ${textSecondaryOnSurface}`}>
        Parent:
      </span>
      <input
        aria-describedby="issue-parent-help"
        className={fieldBox}
        disabled={isClosed}
        id="issue-parent"
        onChange={(event) => drafts.writeParent(event.target.value)}
        onKeyDown={(event) => {
          // The Escape that ends an input method's composition is the input method's.
          if (event.key === "Escape" && !inputMethodKey(event)) discard();
        }}
        placeholder={`${project}-12`}
        ref={input}
        value={drafts.parent}
      />
      <button className={saveButton} disabled={isClosed || saving} type="submit">
        Save parent
      </button>
      <button className={cancelButton} onClick={discard} type="button">
        Cancel
      </button>
      <span className="sr-only" id="issue-parent-help">
        Parent issue key, in {project} or another project; leave empty to clear the parent.
      </span>
    </form>
  );
}
