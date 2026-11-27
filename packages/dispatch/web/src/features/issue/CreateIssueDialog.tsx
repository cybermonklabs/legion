import { dispatchWritingBox, ISSUE_TITLE_MAX } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { ApiError, api, type WriteBounds } from "../../api/client";
import { projectsQuery } from "../../api/queries";
import type { CreateIssueInput, Issue } from "../../api/types";
import { heldField } from "../../components/held-press";
import { MultiSelect } from "../../components/MultiSelect";
import { QueryError } from "../../components/QueryError";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  backdrop50,
  borderDefault,
  card,
  inputClasses,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { MentionField } from "../conversation/MentionField";
import { buildIssuePath, routeProjectOf } from "../refs/routes";
import { useDialog } from "../shell/useDialog";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import { RelatedItems } from "./RelatedItems";

const createIssue = (input: CreateIssueInput, bounds: WriteBounds): Promise<Issue> =>
  api.createIssue(input, bounds);

/**
 * The `c` shortcut's minimal create-issue dialog: project (preselecting the route's project),
 * title, and an optional first spec line. Mounted only while open, so its draft and error state
 * start fresh each time and `useDialog` restores focus to the caller on close. A server
 * `POSSIBLE_DUPLICATE` answer is shown with its candidate and can be overridden with
 * **Create anyway** (`force`), the same path an agent takes. Success opens the new issue. While
 * a person types a title, the dialog lists the related items the check finds for it, so a
 * duplicate is seen before it is filed.
 *
 * The project is the shared list's single pick (`MultiSelect`), whose text box narrows the
 * projects as the reader types (`KEY · name`), held while the create is out. Its panel is
 * portalled out of the dialog and stands in the layer over it (`AnchoredPanel`), so its options
 * take the press rather than the backdrop, and Escape and Tab in the list are the list's: they
 * close it and leave the reader on its trigger, inside the dialog, whose own Escape and Tab
 * cycle then take over. It says the projects are loading while their read is out, and that they
 * could not be loaded where it failed, as the dialog does beside it.
 *
 * While the create is out, the reader's press stays where they made it. Title and Spec are held:
 * `aria-disabled` and read-only, never disabled. Create is held by `aria-disabled`. So the field
 * or the button the reader submitted from keeps the focus, and a submit then sends nothing. A
 * create that lands with no answer empties the title, and Create stays held while it has no
 * title, with the focus still on it. Create is disabled only while no project is picked, which
 * nothing the dialog sends changes. Create anyway goes at its own press, with the refusal it
 * answers, so it has nothing to hold.
 */
export function CreateIssueDialog({
  onClose,
  createIssue: create = createIssue,
}: {
  onClose: () => void;
  /** Write seam for tests; defaults to the real API. */
  createIssue?: (input: CreateIssueInput, bounds: WriteBounds) => Promise<Issue>;
}): ReactNode {
  const location = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const titleRef = useRef<HTMLInputElement>(null);
  const dialog = useDialog<HTMLFormElement>({ initialFocusRef: titleRef, onClose, open: true });
  const projects = useQuery(projectsQuery());
  const routeProject = routeProjectOf(location.pathname);
  const [project, setProject] = useState<string | undefined>(undefined);
  const [projectListOpen, setProjectListOpen] = useState(false);
  const [title, setTitle] = useState("");
  const [spec, setSpec] = useState("");
  const submitGuard = useSubmitGuard();
  // The dialog shows a refusal itself, beside the draft it was refused over.
  const mutation = useKeyedWrite({
    inline: true,
    // An issue's title goes in as the characters it is: the reading list's `write-label`
    // (`DISPATCH_READING_SURFACES`) says why.
    label: (input: CreateIssueInput) => `New issue “${input.title}”`,
    onSettled: () => submitGuard.release(),
    method: "POST",
    write: create,
    onSuccess: (issue) => {
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
      void queryClient.invalidateQueries({ queryKey: projectsQuery().queryKey });
      if (issue !== null) {
        onClose();
        navigate(buildIssuePath({ key: issue.key, kind: "issue" }));
        return;
      }
      // Made, with no answer to open: the draft goes, so a second press makes nothing, and the
      // dialog says what was made (`mutation.unanswered`).
      setTitle("");
      setSpec("");
    },
  });

  const available = projects.data ?? [];
  const projectLabels = new Map(
    available.map((candidate) => [candidate.key, `${candidate.key} · ${candidate.name}`])
  );
  const selectedProject =
    project ??
    available.find((candidate) => candidate.key === routeProject)?.key ??
    available[0]?.key ??
    "";
  const trimmedTitle = title.trim();
  const trimmedSpec = spec.trim();
  const canSubmit = selectedProject !== "" && trimmedTitle !== "" && !mutation.isPending;
  const submit = (force: boolean) =>
    submitGuard.guard(() =>
      mutation.mutate({
        project: selectedProject,
        title: trimmedTitle,
        ...(trimmedSpec === "" ? {} : { spec: trimmedSpec }),
        ...(force ? { force } : {}),
      })
    );
  const handleSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (canSubmit) {
      submit(false);
    }
  };
  const duplicate =
    mutation.error instanceof ApiError && mutation.error.code === "POSSIBLE_DUPLICATE"
      ? mutation.error
      : undefined;

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop50}`} onClick={onClose} />
      <div className="pointer-events-none fixed inset-0 z-50 flex items-start justify-center xl:p-4 xl:pt-20">
        {/* The dialog scrolls: the related section grows with what the check finds, and below
            `xl` the dialog is a full-bleed sheet, so without this the Create row - and the
            refusal's Create anyway with it - paints past the bottom edge of a phone window with
            nothing to scroll. The cap at `xl` leaves the wrapper's own `p-4 pt-20` outside it. */}
        <form
          aria-label="Create issue"
          aria-modal="true"
          className={`pointer-events-auto max-h-[100dvh] w-full space-y-3 overflow-y-auto border p-4 shadow-2xl xl:max-h-[calc(100dvh-6rem)] xl:max-w-lg xl:rounded-lg ${card} ${borderDefault}`}
          onSubmit={handleSubmit}
          ref={dialog.containerRef}
          role="dialog"
        >
          <h2 className={`text-base font-semibold ${textPrimaryOnSurface}`}>Create issue</h2>
          <div className={`text-sm font-medium ${textSecondaryOnSurface}`}>
            <span aria-hidden="true">Project</span>
            <MultiSelect
              disabled={mutation.isPending}
              emptyMessage={
                projects.isPending
                  ? "Loading projects…"
                  : projects.isError || available.length > 0
                    ? "No project matches that."
                    : "No projects yet"
              }
              error={projects.isError ? "Could not load projects." : null}
              label="Project"
              mode="single"
              onOpenChange={setProjectListOpen}
              onPick={setProject}
              open={projectListOpen}
              optionLabel={(key) => projectLabels.get(key)}
              options={available.map((candidate) => candidate.key)}
              searchLabel="Search projects"
              triggerClassName={`mt-1 block min-h-11 w-full rounded-lg border px-3 py-2 text-left text-sm font-normal outline-none ${inputClasses(true)} ${textPrimaryOnSurface}`}
              value={selectedProject === "" ? undefined : selectedProject}
            >
              {selectedProject === ""
                ? projects.isPending
                  ? "Loading projects…"
                  : "Choose a project"
                : undefined}
            </MultiSelect>
          </div>
          {projects.isSuccess && available.length === 0 ? (
            <p className={`text-sm ${textMutedOnSurface}`}>
              No projects yet — create one under Settings first.
            </p>
          ) : null}
          {projects.isError ? (
            <QueryError
              message="Could not load projects."
              onRetry={() => void projects.refetch()}
              retrying={projects.isFetching}
            />
          ) : null}
          <label className={`block text-sm font-medium ${textSecondaryOnSurface}`}>
            Title
            <input
              {...heldField(mutation.isPending)}
              className={`mt-1 block min-h-11 w-full rounded-lg border px-3 py-2 text-sm font-normal outline-none ${inputClasses(true)}`}
              maxLength={ISSUE_TITLE_MAX}
              onChange={(event) => {
                mutation.forgetUnanswered();
                setTitle(event.target.value);
              }}
              ref={titleRef}
              type="text"
              value={title}
            />
          </label>
          <MentionField
            body={spec}
            className="text-sm"
            held={mutation.isPending}
            label="Spec (optional)"
            onChange={(change) => setSpec(change.body)}
            onKeyDown={(event) => submitOnModifiedEnter(event)}
            project={selectedProject}
            readOnly={mutation.isPending}
            rows={3}
            writing={dispatchWritingBox("create-issue-spec", "issue-spec")}
          />
          <RelatedItems project={selectedProject} title={title} />
          {mutation.unanswered === undefined ? null : (
            <LandedWithoutAnswer what={`The issue “${mutation.unanswered.variables.title}”`} />
          )}
          {mutation.isError ? (
            duplicate === undefined ? (
              <QueryError
                message={
                  // A create the reader stopped may have made the issue, and Retry makes that
                  // very press again (`retryLast`); Create is a press of its own.
                  stoppedNote(mutation, "the new issue", "made") ??
                  (mutation.error instanceof ApiError
                    ? mutation.error.message
                    : "Could not create the issue.")
                }
                onRetry={() => submitGuard.retryLast(mutation)}
                retrying={mutation.isPending}
              />
            ) : (
              <p className={`text-sm ${textSecondaryOnSurface}`} role="alert">
                {duplicate.message}
              </p>
            )
          ) : null}
          <div className="flex items-center justify-end gap-2">
            <button
              className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              onClick={onClose}
              type="button"
            >
              Cancel
            </button>
            {duplicate === undefined ? null : (
              <button
                className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
                disabled={!canSubmit}
                onClick={() => submit(true)}
                type="button"
              >
                Create anyway
              </button>
            )}
            <button
              aria-disabled={!canSubmit || undefined}
              className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
              disabled={selectedProject === ""}
              type="submit"
            >
              {mutation.isPending ? "Creating…" : "Create"}
            </button>
          </div>
        </form>
      </div>
    </>
  );
}
