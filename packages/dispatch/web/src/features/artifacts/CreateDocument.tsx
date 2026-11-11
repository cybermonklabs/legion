import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";
import { useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import { RefusableButton } from "../../components/RefusableButton";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { dangerText, inputClasses, linkHoverText, linkText } from "../../theme/classes";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { refreshOwnerArtifacts } from "./ArtifactUpload";

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

/**
 * The one control that makes a document, on an issue's Documents tab and on a project's document
 * list: a `New document` button, then a title and a submit. It writes under an idempotency key, so
 * two presses make one document, and it sends the title alone - no content - so the document holds
 * nothing and the person writes the first word in it. When it is made the person is taken to it,
 * which on an issue is the artifacts panel of the page they are already on.
 *
 * A title the owner already holds is refused here, before the write, in the words of the list the
 * control was given; every other refusal is the server's own sentence, shown as it came. With a
 * `refusal` - the issue is closed - nothing can be made, and the button says why: on the line the
 * host already shows it on (`refusalShownBy`, the one line a closed issue's strip shows for every
 * control in it), or on a hidden line of the button's own.
 */
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
  const [creating, setCreating] = useState(false);
  const [title, setTitle] = useState("");
  const [takenBy, setTakenBy] = useState<string | undefined>();
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
      setTakenBy(held.name);
      return;
    }
    setTakenBy(undefined);
    submitGuard.guard(() => create.mutate(name));
  };
  const problem =
    takenBy !== undefined
      ? `A document on ${"issue" in owner ? "this issue" : "this project"} already holds that title: ${takenBy}.`
      : create.isError
        ? create.error instanceof Error
          ? create.error.message
          : "Could not create the document."
        : undefined;

  return (
    // The host owns the strip this sits in: idle it is one button beside what the host holds
    // there, and open it takes the line.
    <div
      className={`flex min-w-0 flex-wrap items-center gap-2 ${creating ? "w-full" : "shrink-0"}`}
    >
      {creating ? (
        <form
          aria-label="New document"
          className="flex w-full min-w-0 flex-wrap items-center gap-2"
          onSubmit={submit}
        >
          <label className="block min-w-48 flex-1">
            <span className="sr-only">Title</span>
            <input
              aria-label="Title"
              className={`block min-h-11 w-full rounded-lg border px-3 py-2 text-sm ${inputClasses(true)}`}
              onChange={(event) => {
                setTitle(event.target.value);
                setTakenBy(undefined);
              }}
              placeholder="Title"
              required
              value={title}
            />
          </label>
          <RefusableButton
            busy={create.isPending ? "Creating…" : undefined}
            className="min-h-11 shrink-0"
            refusal={refusal}
            refusalShownBy={refusalShownBy}
            type="submit"
          >
            Create
          </RefusableButton>
          <button
            className={`min-h-11 shrink-0 text-sm font-medium ${linkText} ${linkHoverText}`}
            disabled={create.isPending}
            onClick={() => {
              setCreating(false);
              setTitle("");
              setTakenBy(undefined);
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
            setCreating(true);
            setTakenBy(undefined);
            create.forgetUnanswered();
          }}
          refusal={refusal}
          refusalShownBy={refusalShownBy}
        >
          New document
        </RefusableButton>
      )}
      {create.unanswered === undefined ? null : (
        <div className="w-full">
          <LandedWithoutAnswer what={`Document ${create.unanswered.variables}`} />
        </div>
      )}
    </div>
  );
}
