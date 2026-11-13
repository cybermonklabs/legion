import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";
import { useNavigate } from "react-router-dom";

import { api, apiErrorMessage } from "../../api/client";
import type { Artifact } from "../../api/types";
import { RefusableButton } from "../../components/RefusableButton";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { refreshOwnerArtifacts } from "./ArtifactUpload";
import { TitleForm } from "./TitleForm";

/** Why a closed issue takes no new document. The issue's upload control says it too, so one
 *  sentence covers both ways of adding a document there; the repair upload of a document already
 *  being read keeps its own (`CLOSED_ISSUE_UPLOAD_REFUSAL`). */
export const CLOSED_ISSUE_DOCUMENT_REFUSAL = "This issue is closed. Reopen it to add a document.";

/** Who holds a new document: an issue, or a project. An agent's conversation holds files only. */
export type DocumentOwner = { issue: string } | { project: string };

/** Where the made document is read: on its issue page, in the artifacts panel, or on its project's
 *  own document page. */
function documentPath(owner: DocumentOwner, slug: string): string {
  return "issue" in owner
    ? buildIssuePath({ key: owner.issue, kind: "artifact", slug })
    : buildProjectPath({ kind: "document", project: owner.project, slug });
}

/**
 * The document of `owner` that already holds `title`, or undefined while none does. The write
 * compares the title with each of the owner's artifacts by `$2 in (name, slug)`, byte for byte,
 * and prefers the one whose slug it is (`artifactByNameOrSlugSQL`), so this reads the same two
 * fields the same way rather than holding a second copy of the server's `artifactSlug` here.
 */
function documentHolding(documents: readonly Artifact[], title: string): Artifact | undefined {
  return (
    documents.find((document) => document.slug === title) ??
    documents.find((document) => document.name === title)
  );
}

/** One control's making of a document: whether its form is open, the press that opens it, and
 *  the strip the form is drawn in. `CreateDocument` holds one behind its own button; the issue
 *  page's node menu holds one behind an item of the menu, which closes as it is pressed, so the
 *  form is drawn beside the menu rather than in it (`features/issue/NodeMenu.tsx`). */
export interface DocumentCreation {
  /** Whether the form is open. */
  readonly open: boolean;
  /** Opens the form, forgetting what the last press answered. */
  readonly start: () => void;
  /** The strip: `idle` while the form is closed, the form while it is open, and in both what
   *  the last write said where it landed with no answer. */
  readonly strip: (idle: ReactNode) => ReactNode;
}

/**
 * The one way to make a document, on an issue's Documents tab and on a project's document
 * list: a `New document` button, then a title and a submit. It writes under an idempotency key, so
 * two presses make one document, and it sends the title alone - no content - so the document holds
 * nothing and the person writes the first word in it. When it is made the person is taken to it,
 * which on an issue is the artifacts panel of the page they are already on.
 *
 * A title the owner already holds is refused here, before the write, in the words of the list the
 * control was given; every other refusal is the server's own sentence, shown as it came. With a
 * `refusal` - the issue is closed - nothing can be made, and the submit says why: on the line the
 * host already shows it on (`refusalShownBy`, the one line a closed issue's strip shows for every
 * control in it), or on a hidden line of the button's own.
 */
export function useCreateDocument({
  documents,
  owner,
  refusal = null,
  refusalShownBy,
}: {
  readonly documents: readonly Artifact[];
  readonly owner: DocumentOwner;
  readonly refusal?: string | null;
  readonly refusalShownBy?: string;
}): DocumentCreation {
  const [creating, setCreating] = useState(false);
  const [title, setTitle] = useState("");
  const [takenBy, setTakenBy] = useState<Artifact | undefined>();
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const navigate = useNavigate();
  const create = useKeyedWrite({
    inline: true,
    label: (name: string) => `New document ${name}`,
    write: (name: string, bounds) => api.uploadArtifact(owner, { name }, bounds),
    onSettled: () => submitGuard.release(),
    onSuccess: (made) => {
      setCreating(false);
      setTitle("");
      refreshOwnerArtifacts(queryClient, owner);
      // Made, with no answer to open: the form closes, so a second press makes nothing, and the
      // control says what was made (`create.unanswered`).
      if (made === null) return;
      navigate(documentPath(owner, made.artifact.slug));
    },
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const name = title.trim();
    if (name === "" || refusal !== null) {
      return;
    }
    const held = documentHolding(documents, name);
    if (held !== undefined) {
      setTakenBy(held);
      return;
    }
    setTakenBy(undefined);
    submitGuard.guard(() => create.mutate(name));
  };
  // The artifact that holds the title is named for what it is: the list holds the owner's images
  // and files beside its documents, and the write refuses their names too.
  const problem =
    takenBy !== undefined
      ? `${takenBy.kind === "doc" ? "A document" : "An artifact"} on ${"issue" in owner ? "this issue" : "this project"} already holds that title: ${takenBy.name}.`
      : create.isError
        ? apiErrorMessage(create.error, "Could not create the document.")
        : undefined;

  return {
    open: creating,
    start: () => {
      setCreating(true);
      setTakenBy(undefined);
      create.forgetUnanswered();
    },
    strip: (idle) => (
      <TitleForm
        busyLabel="Creating…"
        formLabel="New document"
        idle={idle}
        onCancel={() => {
          setCreating(false);
          setTitle("");
          setTakenBy(undefined);
        }}
        onSubmit={submit}
        onTitleChange={(typed) => {
          setTitle(typed);
          setTakenBy(undefined);
        }}
        open={creating}
        pending={create.isPending}
        problem={problem}
        refusal={refusal}
        refusalShownBy={refusalShownBy}
        submitLabel="Create"
        title={title}
      >
        {create.unanswered === undefined ? null : (
          <div className="w-full">
            <LandedWithoutAnswer what={`Document ${create.unanswered.variables}`} />
          </div>
        )}
      </TitleForm>
    ),
  };
}

/** `useCreateDocument` behind a `New document` button of its own, which says why it cannot be
 *  pressed while there is a `refusal`. */
export function CreateDocument({
  documents,
  owner,
  refusal = null,
  refusalShownBy,
}: {
  readonly documents: readonly Artifact[];
  readonly owner: DocumentOwner;
  readonly refusal?: string | null;
  readonly refusalShownBy?: string;
}): ReactNode {
  const creation = useCreateDocument({ documents, owner, refusal, refusalShownBy });
  return creation.strip(
    <RefusableButton
      look="secondary-compact"
      onPress={creation.start}
      refusal={refusal}
      refusalShownBy={refusalShownBy}
    >
      New document
    </RefusableButton>
  );
}
