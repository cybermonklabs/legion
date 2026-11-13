import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { type ArtifactOwner, api, apiErrorMessage, isArtifactNotFound } from "../../api/client";
import type { Artifact } from "../../api/types";
import { RefusableButton } from "../../components/RefusableButton";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { dangerText } from "../../theme/classes";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { refreshOwnerArtifacts } from "./ArtifactUpload";
import { TitleForm } from "./TitleForm";

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

/** What the correction form says of a correction the reader stopped: Dispatch may have saved it,
 *  and the heading is read again from Dispatch (`refreshArtifact`), so it shows which title holds.
 *  Save is a press of its own, under a key of its own (`useKeyedWrite`), and sends nothing where
 *  the field holds the title the page shows (`CorrectTitle`). */
export function stoppedCorrection(title: string): string {
  return stoppedWriteNote(`the title “${title}”`, "saved", {
    anew: "The heading reads the title again from Dispatch: where it does not show this one, Save sends it again as a new correction.",
  });
}

/**
 * The control that corrects a document's title, on the document's own page: a `Correct title`
 * button, then the title as it stands and a submit. The write carries the name alone, so the
 * slug, the reference key and the role do not move and every citation of the document holds.
 *
 * Every refusal is the server's own sentence, shown as it came: the name rules a new document's
 * name is held to, a title the owner already holds, and the owner's own refusals. With a
 * `refusal` - the issue is closed - nothing is corrected, and the button says why, on a hidden
 * line of its own (`RefusableButton`).
 */
export function CorrectTitle({
  artifact,
  refusal = null,
}: {
  readonly artifact: Artifact;
  readonly refusal?: string | null;
}): ReactNode {
  const [correcting, setCorrecting] = useState(false);
  const [title, setTitle] = useState(artifact.name);
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const rename = useKeyedWrite({
    inline: true,
    label: (name: string) => `Rename ${artifact.name} to ${name}`,
    method: "PATCH",
    write: (name: string, bounds) => api.renameArtifact(artifact.id, { name }, bounds),
    onSettled: () => submitGuard.release(),
    onSuccess: () => {
      setCorrecting(false);
      refreshArtifact(queryClient, artifact);
    },
    // A correction the reader stopped may have landed, and the heading still shows the title the
    // page held before it. Reading the document again is all this does - the write is settled and
    // no attempt of it is left to make - so the heading shows the title Dispatch holds, which is
    // what the form's sentence sends the reader to read.
    onError: (error) => {
      if (error instanceof SendStoppedError) refreshArtifact(queryClient, artifact);
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
  // The write is `inline`, so the shell lets go of a correction the reader stopped and this form
  // is the only place the reader learns what became of it: it may have been saved, which is no
  // failure to report.
  const problem = rename.isError
    ? rename.error instanceof SendStoppedError
      ? stoppedCorrection(rename.variables ?? "")
      : apiErrorMessage(rename.error, "Could not correct the title.")
    : undefined;

  return (
    <TitleForm
      busyLabel="Saving…"
      formLabel="Correct title"
      idle={
        <RefusableButton
          look="secondary-compact"
          onPress={() => {
            setTitle(artifact.name);
            setCorrecting(true);
          }}
          refusal={refusal}
        >
          Correct title
        </RefusableButton>
      }
      onCancel={() => {
        setCorrecting(false);
        setTitle(artifact.name);
      }}
      onSubmit={submit}
      onTitleChange={setTitle}
      open={correcting}
      pending={rename.isPending}
      problem={problem}
      refusal={refusal}
      submitLabel="Save"
      title={title}
    />
  );
}

/**
 * Whether a removal of `artifact` is offered at all. Dispatch removes a document of an issue that
 * plays no role and is not the issue's spec, and refuses every other artifact whatever state it
 * is in: a file or an image by its kind, the spec because it is the spec, and a plan or a
 * Verification document because it plays a role (`artifactIsUntouched`). A press that is always
 * refused is offered nowhere, so both hosts of the control read this.
 */
export function removalIsOffered(artifact: Artifact): boolean {
  return (
    artifact.kind === "doc" &&
    artifact.issue_key !== null &&
    !artifact.primary &&
    (artifact.role ?? "") === ""
  );
}

/**
 * Whether a correction of `artifact`'s title is offered at all. Dispatch corrects a document's
 * title, and refuses two kinds whatever name the correction carries: an artifact that is no
 * document, because a file or an image keeps the name it was uploaded with, and the issue's
 * spec, because the spec's name is how the spec is written whole (`renameArtifact`). A role
 * document and a project document are both corrected, so neither is read here. A press that is
 * always refused is offered nowhere, so the header reads this before it renders `CorrectTitle`.
 */
export function correctionIsOffered(artifact: Artifact): boolean {
  return artifact.kind === "doc" && !(artifact.primary && artifact.issue_key !== null);
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
}: {
  readonly artifact: Artifact;
  readonly onRemoved?: () => void;
  readonly refusal?: string | null;
}): ReactNode {
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const remove = useKeyedWrite({
    inline: true,
    // The document's name is what the press is of, as the indicator names it; the route takes
    // its id.
    label: (name: string) => `Remove ${name}`,
    // A removal is a `DELETE`, which sends no key, so Dispatch keeps no answer for one: made again
    // after a removal that landed - one the reader stopped, or one whose answer was lost - it is a
    // new `DELETE`, which finds no document and is answered `404 ARTIFACT_NOT_FOUND`. That answer
    // is the removal done, since the document is gone, which is what the person asked: the write
    // lands on it as on a `204` (`onSuccess`), and no refusal is shown.
    method: "DELETE",
    write: async (_name: string, bounds) => {
      try {
        await api.deleteArtifact(artifact.id, bounds);
      } catch (error) {
        if (!isArtifactNotFound(error)) throw error;
      }
    },
    onSettled: () => submitGuard.release(),
    onSuccess: () => {
      refreshArtifact(queryClient, artifact);
      onRemoved?.();
    },
  });
  // A removal the reader stopped may have been made, and Remove is its Retry (`onPress`).
  const problem = remove.isError
    ? remove.error instanceof SendStoppedError
      ? stoppedWriteNote(`the removal of “${artifact.name}”`, "made", {
          again: "Remove",
          method: "DELETE",
          stopped: remove.error,
        })
      : apiErrorMessage(remove.error, "Could not remove the document.")
    : undefined;

  return (
    <div className="flex min-w-0 flex-wrap items-center gap-2">
      <RefusableButton
        busy={remove.isPending ? "Removing…" : undefined}
        look="secondary-compact"
        // A press of Remove while its refusal is on screen is the Retry of the press that refusal
        // is about (`retry`). Where the reader stopped that removal and it had landed, the Retry
        // finds no document, which `write` takes for the removal done.
        onPress={() =>
          submitGuard.guard(() => (remove.isError ? remove.retry() : remove.mutate(artifact.name)))
        }
        refusal={refusal}
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
