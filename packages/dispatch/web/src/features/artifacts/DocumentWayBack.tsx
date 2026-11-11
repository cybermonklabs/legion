import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { type ArtifactOwner, api, apiErrorMessage } from "../../api/client";
import type { Artifact } from "../../api/types";
import { RefusableButton } from "../../components/RefusableButton";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { dangerText, inputClasses, linkHoverText, linkText } from "../../theme/classes";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { refreshOwnerArtifacts } from "./ArtifactUpload";

/** Why a closed issue takes no correction. Its document is read-only there, as the page says,
 *  and its title is part of what is read. */
export const CLOSED_ISSUE_RENAME_REFUSAL = "This issue is closed. Reopen it to correct the title.";

/** Why a closed issue takes no removal. One sentence covers both ways to the control: the row in
 *  the issue's document list, and the document's own page. */
export const CLOSED_ISSUE_REMOVE_REFUSAL =
  "This issue is closed. Reopen it to remove this document.";

/** Who holds `artifact`: its issue, its project, or the agent whose conversation holds it. */
function artifactOwner(artifact: Artifact): ArtifactOwner {
  if (artifact.session_id !== null) {
    return { session: artifact.session_id };
  }
  return artifact.issue_key === null
    ? { project: artifact.project }
    : { issue: artifact.issue_key };
}

/**
 * Refreshes every read that shows the document's title or its row, so a correction and a removal
 * both land on the open page with no reload: its owner's document list and, for an issue, the
 * issue read the artifact page's header draws from; the document's own read; and the project
 * document page, which reads it by `project/slug`. The slug does not move with a correction, so
 * the same keys name the document after it.
 *
 * The live stream refreshes the same reads from the event the write appends
 * (`api/event-refresh.ts`), which is how one reader's correction reaches another's page; this is
 * what makes the writer's own page move whether or not that stream is up.
 */
function refreshArtifact(queryClient: QueryClient, artifact: Artifact): void {
  refreshOwnerArtifacts(queryClient, artifactOwner(artifact));
  void queryClient.invalidateQueries({ queryKey: ["artifact", artifact.id] });
  void queryClient.invalidateQueries({
    queryKey: ["artifact-ref", `${artifact.project}/${artifact.slug}`],
  });
}

/**
 * The control that corrects a document's title, on the document's own page: a `Correct title`
 * button, then the title as it stands and a submit. The write carries the name alone, so the
 * slug, the reference key and the role do not move and every citation of the document holds.
 *
 * Every refusal is the server's own sentence, shown as it came: the name rules a new document's
 * name is held to, a title the owner already holds, and the owner's own refusals. With a
 * `refusal` - the issue is closed - nothing is corrected, and the button says why: on the line
 * the host already shows it on (`refusalShownBy`), or on a hidden line of the button's own.
 */
export function CorrectTitle({
  artifact,
  refusal = null,
  refusalShownBy,
}: {
  readonly artifact: Artifact;
  readonly refusal?: string | null;
  readonly refusalShownBy?: string;
}): ReactNode {
  const [correcting, setCorrecting] = useState(false);
  const [title, setTitle] = useState(artifact.name);
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const rename = useKeyedWrite({
    inline: true,
    label: (name: string) => `Rename ${artifact.name} to ${name}`,
    write: (name: string, bounds) => api.renameArtifact(artifact.id, { name }, bounds),
    onSettled: () => submitGuard.release(),
    onSuccess: () => {
      setCorrecting(false);
      refreshArtifact(queryClient, artifact);
    },
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const name = title.trim();
    if (name === "" || name === artifact.name || refusal !== null) {
      setCorrecting(false);
      return;
    }
    submitGuard.guard(() => rename.mutate(name));
  };
  const problem = rename.isError
    ? apiErrorMessage(rename.error, "Could not correct the title.")
    : undefined;

  return (
    // Idle this is one button in the host's strip; open it takes the line it is on.
    <div
      className={`flex min-w-0 flex-wrap items-center gap-2 ${correcting ? "w-full" : "shrink-0"}`}
    >
      {correcting ? (
        <form
          aria-label="Correct title"
          className="flex w-full min-w-0 flex-wrap items-center gap-2"
          onSubmit={submit}
        >
          <label className="block min-w-48 flex-1">
            <span className="sr-only">Title</span>
            <input
              aria-label="Title"
              className={`block min-h-11 w-full rounded-lg border px-3 py-2 text-sm ${inputClasses(true)}`}
              onChange={(event) => setTitle(event.target.value)}
              placeholder="Title"
              required
              value={title}
            />
          </label>
          <RefusableButton
            busy={rename.isPending ? "Saving…" : undefined}
            className="min-h-11 shrink-0"
            refusal={refusal}
            refusalShownBy={refusalShownBy}
            type="submit"
          >
            Save
          </RefusableButton>
          <button
            className={`min-h-11 shrink-0 text-sm font-medium ${linkText} ${linkHoverText}`}
            disabled={rename.isPending}
            onClick={() => {
              setCorrecting(false);
              setTitle(artifact.name);
            }}
            type="button"
          >
            Cancel
          </button>
          {problem === undefined ? null : (
            <p className={`w-full text-sm ${dangerText}`} role="alert">
              {problem}
            </p>
          )}
        </form>
      ) : (
        <RefusableButton
          look="secondary-compact"
          onPress={() => {
            setTitle(artifact.name);
            setCorrecting(true);
          }}
          refusal={refusal}
          refusalShownBy={refusalShownBy}
        >
          Correct title
        </RefusableButton>
      )}
    </div>
  );
}

/**
 * The control that removes a document, on its row in the issue's document list and on its own
 * page. It asks for no confirmation: Dispatch removes only a document that holds nothing and that
 * nothing names, and refuses every other one with a sentence that names the test it failed, which
 * is shown here, where the person pressed.
 *
 * `onRemoved` is the host's own way back from the page of a document that has gone.
 */
export function RemoveDocument({
  artifact,
  onRemoved,
  refusal = null,
  refusalShownBy,
}: {
  readonly artifact: Artifact;
  readonly onRemoved?: () => void;
  readonly refusal?: string | null;
  readonly refusalShownBy?: string;
}): ReactNode {
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const remove = useKeyedWrite({
    inline: true,
    // The document's name is what the press is of, as the indicator names it; the route takes
    // its id, so a retry of one press keeps that press's key (`sameVariables`).
    label: (name: string) => `Remove ${name}`,
    write: (_name: string, bounds) => api.deleteArtifact(artifact.id, bounds),
    onSettled: () => submitGuard.release(),
    onSuccess: () => {
      refreshArtifact(queryClient, artifact);
      onRemoved?.();
    },
  });
  const problem = remove.isError
    ? apiErrorMessage(remove.error, "Could not remove the document.")
    : undefined;

  return (
    <div className="flex min-w-0 flex-wrap items-center gap-2">
      <RefusableButton
        busy={remove.isPending ? "Removing…" : undefined}
        look="secondary-compact"
        onPress={() => submitGuard.guard(() => remove.mutate(artifact.name))}
        refusal={refusal}
        refusalShownBy={refusalShownBy}
      >
        Remove
      </RefusableButton>
      {problem === undefined ? null : (
        <p className={`text-sm ${dangerText}`} role="alert">
          {problem}
        </p>
      )}
    </div>
  );
}
