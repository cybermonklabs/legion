import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import {
  type ArtifactOwner,
  api,
  apiErrorMessage,
  isArtifactNotFound,
  removedEvenIfGone,
} from "../../api/client";
import { useReadBound } from "../../api/read-bound";
import type { Artifact } from "../../api/types";
import { RefusableButton } from "../../components/RefusableButton";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { dangerText } from "../../theme/classes";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { endedStopped, useKeyedWrite } from "../writes/useKeyedWrite";
import { refreshOwnerArtifacts } from "./ArtifactUpload";
import { TitleForm } from "./TitleForm";

/** Why a closed issue takes no correction. Its document is read-only there, as the page says,
 *  and its title is part of what is read. */
export const CLOSED_ISSUE_RENAME_REFUSAL = "This issue is closed. Reopen it to correct the title.";

/** Why a closed issue takes no removal. One sentence covers both ways to the control: the row in
 *  the issue's document list, and the document's own page. */
export const CLOSED_ISSUE_REMOVE_REFUSAL =
  "This issue is closed. Reopen it to remove this document.";

/** What the Artifacts tab says when its list read failed, and what a removal's row says before
 *  why the list read after it has not settled. */
export const ARTIFACTS_NOT_LOADED = "Could not load artifacts.";

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
 * the same keys name the document after it. Answers when the owner's document list has been read
 * again, or has failed to be (`refreshOwnerArtifacts`).
 *
 * The live stream refreshes the same reads from the event the write appends
 * (`api/event-refresh.ts`), which is how one reader's correction reaches another's page; this is
 * what makes the writer's own page move whether or not that stream is up.
 */
function refreshArtifact(queryClient: QueryClient, artifact: Artifact): Promise<void> {
  const list = refreshOwnerArtifacts(queryClient, artifactOwner(artifact));
  void queryClient.invalidateQueries({ queryKey: ["artifact", artifact.id] });
  void queryClient.invalidateQueries({
    queryKey: ["artifact-ref", `${artifact.project}/${artifact.slug}`],
  });
  return list;
}

/** What the correction form says of a correction the reader stopped: Dispatch may have saved it,
 *  and the heading is read again from Dispatch (`refreshArtifact`), so it shows which title holds.
 *  Save is a press of its own, under a key of its own (`useKeyedWrite`), and sends nothing where
 *  the field holds the title the page shows (`CorrectTitle`). */
function stoppedCorrection(title: string): string {
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
 *
 * It holds the correction it made - the open form, the title typed into it and what became of
 * its last Save - so it is drawn for one document alone, and its host keys it by the document
 * (`ArtifactHeader`), which an issue's page and a project document's page each draw for one
 * document after another.
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
    // The press carries the document it corrects: the indicator names it, the route takes its
    // id, and the reads made again are its own, from the press rather than from the render the
    // write runs under.
    label: ({ artifact: corrected, name }: { artifact: Artifact; name: string }) =>
      `Rename ${corrected.name} to ${name}`,
    method: "PATCH",
    write: ({ artifact: corrected, name }, bounds) =>
      api.renameArtifact(corrected.id, { name }, bounds),
    onSettled: () => submitGuard.release(),
    onSuccess: (_result, { artifact: corrected }) => {
      setCorrecting(false);
      void refreshArtifact(queryClient, corrected);
    },
    // A correction the reader stopped may have landed, and the heading still shows the title the
    // page held before it. Reading the document again is all this does - the write is settled and
    // no attempt of it is left to make - so the heading shows the title Dispatch holds, which is
    // what the form's sentence sends the reader to read.
    onError: (error, { artifact: corrected }) => {
      if (error instanceof SendStoppedError) void refreshArtifact(queryClient, corrected);
    },
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const name = title.trim();
    if (name === "" || name === artifact.name || refusal !== null) {
      setCorrecting(false);
      return;
    }
    submitGuard.guard(() => rename.mutate({ artifact, name }));
  };
  // The write is `inline`, so the shell lets go of a correction the reader stopped and this form
  // is the only place the reader learns what became of it: it may have been saved, which is no
  // failure to report.
  const problem = rename.isError
    ? rename.error instanceof SendStoppedError
      ? stoppedCorrection(rename.variables?.name ?? "")
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
 * It holds the removal it made - `Removing…`, a refusal and its Retry, and why the list read after
 * it has not settled - so it is drawn for one document alone, and each host keys it by the
 * document: the issue's document list keys the row it sits in (`ArtifactRow`), and the page's
 * header, which the issue's page draws for one document after another, keys the control itself
 * (`ArtifactHeader`).
 *
 * `onRemoved` is the host's own way back from the page of a document that has gone, handed the
 * document the removal was of. It is called when the removal lands, by when the reader may have
 * left that document's page.
 */
export function RemoveDocument({
  artifact,
  onRemoved,
  refusal = null,
}: {
  readonly artifact: Artifact;
  readonly onRemoved?: (removed: Artifact) => void;
  readonly refusal?: string | null;
}): ReactNode {
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const [unsettled, holdThrough] = useReadBound();
  const remove = useKeyedWrite({
    inline: true,
    // The press carries the document it removes: the indicator names it, and the route takes its
    // id from the press rather than from the render the write runs under.
    label: ({ name }: Artifact) => `Remove ${name}`,
    // A removal made again after one that landed finds no document (`404 ARTIFACT_NOT_FOUND`),
    // which is the removal done (`removedEvenIfGone`): it lands as a `204` does (`onSuccess`), and
    // no refusal is shown.
    method: "DELETE",
    write: ({ id }, bounds) =>
      removedEvenIfGone(api.deleteArtifact(id, bounds), isArtifactNotFound),
    onSettled: () => submitGuard.release(),
    // Pending, and the button held, through the read of the document list that takes the row
    // away, so the press is still the cause of the row's going (`causedByReader`). That read
    // settles either way, and a failed one lets the button go. A read that has not settled by the
    // request deadline (`REQUEST_DEADLINE_MS`) - one paused while the browser is offline - lets
    // it go then, and the row says why the list is not read yet (`useReadBound`).
    onSuccess: (_result, removed) => {
      const list = refreshArtifact(queryClient, removed);
      onRemoved?.(removed);
      return holdThrough(list);
    },
  });
  // A removal the reader stopped may have been made, and Remove is its Retry (`onPress`). A list
  // read that has not settled is said in the words the Artifacts tab says for one that failed.
  const problem = remove.isError
    ? endedStopped(remove)
      ? stoppedWriteNote(`the removal of “${(remove.variables ?? artifact).name}”`, "made", {
          again: "Remove",
          method: remove.method,
          stopped: remove.error,
        })
      : apiErrorMessage(remove.error, "Could not remove the document.")
    : unsettled === undefined
      ? undefined
      : `${ARTIFACTS_NOT_LOADED} ${unsettled}.`;

  return (
    <div className="flex min-w-0 flex-wrap items-center gap-2">
      <RefusableButton
        busy={remove.isPending ? "Removing…" : undefined}
        look="secondary-compact"
        // A press of Remove while its refusal is on screen is the Retry of the press that refusal
        // is about (`retry`). Where the reader stopped that removal and it had landed, the Retry
        // finds no document, which `write` takes for the removal done.
        onPress={() =>
          submitGuard.guard(() => (remove.isError ? remove.retry() : remove.mutate(artifact)))
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
