import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";
import { useNavigate } from "react-router-dom";

import { api, apiErrorMessage } from "../../api/client";
import type { Artifact } from "../../api/types";
import type { BarActionProps, BarActionRefusal } from "../../components/BarAction";
import { textGlyph } from "../../components/bar-look";
import { RefusableButton } from "../../components/RefusableButton";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { refreshOwnerArtifacts } from "./ArtifactUpload";
import { TitleForm } from "./TitleForm";

/** Why a closed issue takes no new document. The issue's upload control says it too, so one
 *  sentence covers both ways of adding a document there; the repair upload of a document already
 *  being read keeps its own (`CLOSED_ISSUE_UPLOAD_REFUSAL`). */
export const CLOSED_ISSUE_DOCUMENT_REFUSAL = "This issue is closed. Reopen it to add a document.";

/** What the form says of a create the reader stopped (`stoppedWriteNote`): Dispatch may have made
 *  it, and a press of Create again is a new write, which Dispatch refuses where the first one made
 *  the document. */
function stoppedCreate(title: string): string {
  return stoppedWriteNote(`the document “${title}”`, "made", {
    anew: "Press Create again: if it was, Dispatch says that title is taken and the list shows the document.",
  });
}

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

/** What makes a document, for a host to draw: `start` opens the form, `close` shuts it as
 *  **Cancel** does, `open` says whether it stands open, and `form` draws the strip with `idle`, the
 *  control the strip holds while the form is closed, or none where that control stands outside
 *  the strip (`opener`, the Artifacts tab's control bar). */
interface DocumentCreate {
  readonly close: () => void;
  readonly form: (idle: ReactNode, opener?: () => HTMLElement | null) => ReactNode;
  readonly open: boolean;
  readonly start: () => void;
}

/**
 * The one way a document is made, on an issue's Artifacts tab and on a project's document list:
 * a `New document` control, then a title and a submit. It writes under an idempotency key, so
 * two presses make one document, and it sends the title alone - no content - so the document holds
 * nothing and the person writes the first word in it. When it is made the person is taken to it,
 * which on an issue is the artifacts panel of the page they are already on.
 *
 * A title the owner already holds is refused here, before the write, in the words of the list the
 * control was given; every other refusal is the server's own sentence, shown as it came. With a
 * `refusal` - the issue is closed - nothing can be made, and the submit says why: on the line the
 * host already shows it on (`refusalShownBy`, the one line a closed issue's tab shows for every
 * control of its bar), or on a hidden line of its own.
 */
function useDocumentCreate({
  documents,
  owner,
  refusal,
  refusalShownBy,
}: {
  readonly documents: readonly Artifact[];
  readonly owner: DocumentOwner;
  readonly refusal: string | null;
  readonly refusalShownBy: string | undefined;
}): DocumentCreate {
  const [creating, setCreating] = useState(false);
  const [title, setTitle] = useState("");
  const [takenBy, setTakenBy] = useState<Artifact | undefined>();
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const navigate = useNavigate();
  const create = useKeyedWrite({
    inline: true,
    label: (name: string) => `New document ${name}`,
    method: "POST",
    write: (name: string, bounds) => api.uploadArtifact(owner, { name }, bounds),
    onSettled: () => submitGuard.release(),
    onSuccess: (made) => {
      setCreating(false);
      setTitle("");
      void refreshOwnerArtifacts(queryClient, owner);
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
  // and files beside its documents, and the write refuses their names too. A create the reader
  // stopped is no verdict of Dispatch's, and this form offers no Retry of it: a press of Create
  // again is a press the person made anew, under a key of its own whatever title it carries
  // (`useKeyedWrite`). Where the stopped create had reached Dispatch, that press names a document
  // that exists now, and Dispatch refuses it - a write with no content makes no document - so the
  // person reads that refusal here and the list shows the document. The write is `inline`, so
  // the shell lets go of it once stopped and this form is the only place the reader learns what
  // became of it. The stopped write's own words promise a Retry under its key, which this form
  // does not make, so it says what pressing Create again does instead.
  const problem =
    takenBy !== undefined
      ? `${takenBy.kind === "doc" ? "A document" : "An artifact"} on ${"issue" in owner ? "this issue" : "this project"} already holds that title: ${takenBy.name}.`
      : create.isError
        ? create.error instanceof SendStoppedError
          ? stoppedCreate(create.variables ?? "")
          : apiErrorMessage(create.error, "Could not create the document.")
        : undefined;

  const close = () => {
    setCreating(false);
    setTitle("");
    setTakenBy(undefined);
  };

  return {
    close,
    form: (idle, opener) => (
      <TitleForm
        busyLabel="Creating…"
        formLabel="New document"
        idle={idle}
        onCancel={close}
        onSubmit={submit}
        onTitleChange={(typed) => {
          setTitle(typed);
          setTakenBy(undefined);
        }}
        open={creating}
        opener={opener}
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
    open: creating,
    start: () => {
      setCreating(true);
      setTakenBy(undefined);
      create.forgetUnanswered();
    },
  };
}

/** The glyph **New document** draws before its words where the control bar draws it. */
const NEW_DOCUMENT_GLYPH = textGlyph("+");

/**
 * **New document** in an issue's Artifacts tab's control bar: the action the bar draws (a quiet
 * button of its line, and an item of **More** below 640 px of the bar's width), refused with the
 * tab's one line on a closed issue (`refusal`), and the form it opens, which the tab draws under
 * the bar, outside it. A press while the form stands open shuts it, as **Cancel** does. The form
 * hands the focus back to the action as it closes, or to **More** where the action is its item
 * (`opener`).
 */
export function useNewDocument({
  documents,
  opener,
  owner,
  refusal,
}: {
  readonly documents: readonly Artifact[];
  readonly opener: () => HTMLElement | null;
  readonly owner: DocumentOwner;
  /** The closed issue's refusal and the id of the line under the bar that shows it, or
   *  undefined while a document can be made. */
  readonly refusal: BarActionRefusal | undefined;
}): { action: BarActionProps; form: ReactNode } {
  const create = useDocumentCreate({
    documents,
    owner,
    refusal: refusal?.words ?? null,
    refusalShownBy: refusal?.id,
  });
  return {
    action: {
      icon: NEW_DOCUMENT_GLYPH,
      label: "New document",
      onPress: create.open ? create.close : create.start,
      refusal,
    },
    form: create.form(null, opener),
  };
}

/**
 * The control that makes a document on a project's document list (`useDocumentCreate`): a
 * `New document` button, which the form takes the place of while it stands open.
 */
export function CreateDocument({
  documents,
  owner,
}: {
  readonly documents: readonly Artifact[];
  readonly owner: DocumentOwner;
}): ReactNode {
  const create = useDocumentCreate({ documents, owner, refusal: null, refusalShownBy: undefined });
  return create.form(
    <RefusableButton look="secondary-compact" onPress={create.start} refusal={null}>
      New document
    </RefusableButton>
  );
}
