import { type FormEvent, type ReactNode, type Ref, useEffect, useRef } from "react";
import { Link } from "react-router-dom";

import { TruncatedText } from "../../components/TruncatedText";
import { useFocusHandOff } from "../../hooks/useFocusHandOff";
import { inputMethodKey } from "../../lib/input-method-key";
import {
  borderTransparent,
  dangerText,
  focusVisibleRing,
  inputClasses,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonText,
  surfaceMutedStrongBg,
  textMutedHoverToSecondary,
  textMutedOnSurface,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AgentName, useAgentText } from "../refs/AgentName";
import { buildIssuePath, sessionOfAddress } from "../refs/routes";
import type { IssueDrafts } from "./useIssueDrafts";

/** What the issue's message route is, said under its value in Details, with the id
 *  `routeHintId`, which the route's control and its editor each name. */
export const routeHint =
  "New asks, comments, and messages on this issue wake this agent or role; replies inside a thread reach their participants directly. It is where messages go, not who is working the issue — that is the claim.";
export const routeHintId = "issue-route-hint";

const saveButton = `min-h-11 rounded px-2 py-1 text-sm font-medium disabled:cursor-not-allowed md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonDisabledText}`;
const cancelButton = `min-h-11 rounded border px-2 py-1 text-sm font-medium md:min-h-8 ${borderTransparent} ${textMutedHoverToSecondary}`;
const fieldBox = `w-full rounded px-2 py-1 text-sm outline-none md:w-64 ${inputClasses(true)}`;

interface HeaderFormProps {
  drafts: IssueDrafts;
  isClosed: boolean;
  /** Gives the edit up and closes the editor: its Cancel and its Escape. */
  onCancel: () => void;
  /** Closes the editor after a save that was sent or queued. */
  onSaved: () => void;
  /** Whether the header's issue write is out, which holds Save. */
  saving: boolean;
}

/**
 * The input an editor opens on: the control that opened it unmounts with the press, so without
 * this the focus would drop to the body and a keyboard reader would have to find the form again.
 */
function useFocusOnOpen() {
  const input = useRef<HTMLInputElement>(null);
  useEffect(() => {
    input.current?.focus();
  }, []);
  return input;
}

/**
 * The focus of an editor of Details that its own save closes. Save route or Save parent, or Enter
 * in the editor's field, takes the form away with the focus inside it, and the close draws the
 * control that opened the editor again in its place: that control takes the focus
 * (`useFocusHandOff`), so a keyboard reader goes on from the row they saved rather than from the
 * page's body. Only a save hands it on: the editor notes it (`saved`) as its save closes it.
 * Answers the ref of the form, the ref of the control that opens the editor, and the editor's
 * `onSaved`.
 */
function useSavedEditorHandOff(onEditingChange: (editing: boolean) => void): {
  formRef: Ref<HTMLFormElement>;
  onSaved: () => void;
  opener: Ref<HTMLButtonElement>;
} {
  const opener = useRef<HTMLButtonElement>(null);
  const saved = useRef(false);
  const formRef = useFocusHandOff(() => opener.current, saved);
  const onSaved = () => {
    saved.current = true;
    onEditingChange(false);
  };
  return { formRef, onSaved, opener };
}

/**
 * The editor of the issue's message route. Its groups, in this order, wrapping whole: the route's
 * field, then Save route and Cancel, then the line that says why a route is refused, drawn only
 * while the draft is no route.
 */
export function IssueRouteForm({
  drafts,
  isClosed,
  onCancel,
  onSaved,
  ref,
  saving,
}: HeaderFormProps & { ref?: Ref<HTMLFormElement> }): ReactNode {
  const input = useFocusOnOpen();
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (drafts.requestRouteSubmit()) onSaved();
  };
  return (
    <form className="flex flex-wrap items-center gap-2" onSubmit={save} ref={ref}>
      <input
        aria-label="Message route"
        aria-describedby={`${routeHintId} issue-route-help`}
        className={fieldBox}
        disabled={isClosed}
        id="issue-route"
        onChange={(event) => drafts.writeRoute(event.target.value)}
        onKeyDown={(event) => {
          // The Escape that drops an input method's composition is no key of the reader's: it
          // gives up the composition, not the edit. Safari sends it after its compositionend, with
          // `isComposing` false and keyCode 229 (`inputMethodKey`).
          if (inputMethodKey(event.nativeEvent)) return;
          if (event.key === "Escape") onCancel();
        }}
        placeholder="role:legion-controller-core"
        ref={input}
        value={drafts.route}
      />
      {/* Save and Cancel are one group: the form wraps it whole onto the next line. */}
      <span className="inline-flex shrink-0 items-center gap-2">
        <button
          className={saveButton}
          disabled={isClosed || !drafts.routeIsValid || saving}
          type="submit"
        >
          Save route
        </button>
        <button className={cancelButton} onClick={onCancel} type="button">
          Cancel
        </button>
      </span>
      <span
        className={drafts.routeIsValid ? "sr-only" : `text-sm ${dangerText}`}
        id="issue-route-help"
      >
        A message route must be role:[a-z0-9-]+ or session:[0-9a-f-]{`{16,}`}.
      </span>
    </form>
  );
}

/**
 * The editor of the issue's parent, a key in `project` or in another project. Its groups, in this
 * order, wrapping whole: the key's field, then Save parent and Cancel.
 */
export function IssueParentForm({
  drafts,
  isClosed,
  onCancel,
  onSaved,
  project,
  ref,
  saving,
}: HeaderFormProps & { project: string; ref?: Ref<HTMLFormElement> }): ReactNode {
  const input = useFocusOnOpen();
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (drafts.requestParentSubmit()) onSaved();
  };
  return (
    <form className="flex flex-wrap items-center gap-2" onSubmit={save} ref={ref}>
      <label className="sr-only" htmlFor="issue-parent">
        Parent
      </label>
      <input
        aria-describedby="issue-parent-help"
        className={fieldBox}
        disabled={isClosed}
        id="issue-parent"
        onChange={(event) => drafts.writeParent(event.target.value)}
        onKeyDown={(event) => {
          // The Escape that drops an input method's composition is no key of the reader's.
          if (inputMethodKey(event.nativeEvent)) return;
          if (event.key === "Escape") onCancel();
        }}
        placeholder={`${project}-12`}
        ref={input}
        value={drafts.parent}
      />
      <span className="inline-flex shrink-0 items-center gap-2">
        <button className={saveButton} disabled={isClosed || saving} type="submit">
          Save parent
        </button>
        <button className={cancelButton} onClick={onCancel} type="button">
          Cancel
        </button>
      </span>
      <span className="sr-only" id="issue-parent-help">
        Parent issue key, in {project} or another project; leave empty to clear the parent.
      </span>
    </form>
  );
}

/** Whether an editor of Details is open, held by the header, which closes it with Details. */
interface DetailsEditorProps extends Omit<HeaderFormProps, "onSaved"> {
  editing: boolean;
  onEditingChange: (editing: boolean) => void;
}

/**
 * The value of the Message route row of today's issue header's Details. Its groups, in this
 * order: the route - a control that opens its editor, which a `session:` route draws as the agent
 * it names, with that agent's live link - or, while the reader edits it, the editor
 * (`IssueRouteForm`); then what a route is, on a line of its own (`routeHint`).
 */
export function IssueRouteDetails({
  editing,
  onEditingChange,
  ...form
}: DetailsEditorProps): ReactNode {
  const { drafts, isClosed } = form;
  const agentText = useAgentText();
  const handOff = useSavedEditorHandOff(onEditingChange);
  // A `session:` route is an agent: its button names it as `AgentName` does, with the agent's own
  // live link beside it. A role route is its address.
  const routeSession = sessionOfAddress(drafts.route);
  const routeLabel = `Messages default to ${
    drafts.route === ""
      ? "no route"
      : routeSession === undefined
        ? drafts.route
        : agentText.sessionText(routeSession, undefined)
  }`;
  const routeButton = (name: ReactNode) => (
    <button
      aria-describedby={routeHintId}
      aria-label={routeLabel}
      className={`inline-flex min-h-11 min-w-0 max-w-full items-center truncate rounded-full px-3 py-2 text-left text-sm outline-none focus-visible:ring-2 xl:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary} ${focusVisibleRing}`}
      disabled={isClosed}
      onClick={() => onEditingChange(true)}
      ref={handOff.opener}
      title={drafts.route === "" ? undefined : drafts.route}
      type="button"
    >
      <TruncatedText>{name}</TruncatedText>
    </button>
  );
  return (
    <>
      {editing ? (
        <IssueRouteForm {...form} onSaved={handOff.onSaved} ref={handOff.formRef} />
      ) : routeSession === undefined ? (
        routeButton(drafts.route === "" ? "No route" : drafts.route)
      ) : (
        <AgentName
          className="inline-flex min-w-0 max-w-full items-center"
          control={routeButton}
          kind="control"
          sessionId={routeSession}
          storedTitle={undefined}
        />
      )}
      <p className={`mt-1 text-xs ${textMutedOnSurface}`} id={routeHintId}>
        {routeHint}
      </p>
    </>
  );
}

/**
 * The value of the Parent row of today's issue header's Details. Its groups, in this order,
 * wrapping whole: the parent's key as a link, when it has one, then the control that opens the
 * editor ("None" or "Edit"); or, while the reader edits it, the editor (`IssueParentForm`).
 */
export function IssueParentDetails({
  editing,
  onEditingChange,
  parent,
  project,
  ...form
}: DetailsEditorProps & { parent: string | null; project: string }): ReactNode {
  const handOff = useSavedEditorHandOff(onEditingChange);
  if (editing) {
    return (
      <IssueParentForm
        {...form}
        onSaved={handOff.onSaved}
        project={project}
        ref={handOff.formRef}
      />
    );
  }
  return (
    <span className="flex min-w-0 flex-wrap items-center gap-2">
      {parent === null ? null : (
        <Link
          className={`dispatch-inline-link underline ${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: parent, kind: "issue" })}
        >
          {parent}
        </Link>
      )}
      <button
        // The name starts with the word on the button - "None" or "Edit" - because a name that
        // does not contain the visible text leaves the control out of reach of a voice user
        // (WCAG 2.5.3, axe's `label-content-name-mismatch`).
        aria-label={parent === null ? "None, set parent issue" : "Edit parent issue"}
        className={`inline-flex min-h-11 shrink-0 items-center rounded-full px-3 py-2 text-left text-sm outline-none focus-visible:ring-2 xl:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary} ${focusVisibleRing}`}
        disabled={form.isClosed}
        onClick={() => onEditingChange(true)}
        ref={handOff.opener}
        type="button"
      >
        {parent === null ? "None" : "Edit"}
      </button>
    </span>
  );
}
