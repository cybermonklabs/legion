import { type ClipboardEvent, type DragEvent, useEffect, useRef, useState } from "react";

import type { ArtifactOwner } from "../../api/client";
import { type DraftUpload, useDraftUpload } from "../artifacts/ArtifactUpload";
import type { ComposerOwner } from "./composer-model";
import { artifactOwnerOf, ownerWords } from "./send-request";

/** What the composer's field does with files: where a paste or a drop uploads to and the draft
 *  its reference joins, and what the composer says of files whose draft lives at another owner
 *  than the one it addresses now. */
export interface DraftHomes {
  /** Takes the drafts living at `from`, the owner the composer addressed before the reader moved
   *  it with its own controls (`ownMoves`), to the owner it addresses now. */
  readonly carry: (from: ArtifactOwner | undefined) => void;
  readonly drop: ((event: DragEvent<HTMLTextAreaElement>) => void) | undefined;
  /** What the composer says of the files that landed into a draft living at another owner, one
   *  line per owner (`landedFor`). */
  readonly landedElsewhere: readonly { readonly key: string; readonly text: string }[];
  readonly paste: ((event: ClipboardEvent<HTMLTextAreaElement>) => void) | undefined;
  /** What a refused upload says while the composer addresses another owner than the one its
   *  file's draft lives at (`pastedElsewhere`), in place of the upload status's own refusal and
   *  its Retry. */
  readonly pastedElsewhere: string | undefined;
  readonly upload: DraftUpload<number>;
  /** Where a pasted or dropped file goes: the issue or the project document the draft is for, or,
   *  for a direct message, the agent's conversation. An edit takes no files. */
  readonly uploadOwner: ArtifactOwner | undefined;
}

/** The one key the draft homes compare owners by: two owners are the same where their keys are,
 *  and no owner has the key of none. */
function ownerKey(owner: ArtifactOwner): string;
function ownerKey(owner: ArtifactOwner | undefined): string | undefined;
function ownerKey(owner: ArtifactOwner | undefined): string | undefined {
  return owner === undefined ? undefined : JSON.stringify(owner);
}

/** What a refused upload says while the composer addresses another owner than the one the draft
 *  its file was pasted into lives at: a Retry from here would add its reference to a message for
 *  somewhere else, so the Retry waits for the composer to be back there. */
function pastedElsewhere(owner: ArtifactOwner): string {
  return `This file was pasted ${ownerWords(owner, "for")}. Go back to it to try the upload again.`;
}

/** What the composer says of files that landed into a draft living at another owner than the one
 *  it addresses now: their references wait for that draft, and join it once the composer is back
 *  there. */
function landedFor(owner: ArtifactOwner, files: number): string {
  return files === 1
    ? `The file pasted ${ownerWords(owner, "for")} is uploaded. ` +
        "Its reference joins the message when you go back to it."
    : `The ${files} files pasted ${ownerWords(owner, "for")} are uploaded. ` +
        "Their references join the message when you go back to it.";
}

/**
 * Where each file pasted or dropped into the composer's field lives, for the composer addressing
 * `owner` (`editing`, an edit, takes no files). A file's reference belongs to the draft it was
 * pasted or dropped into, so each press carries that draft (`useDraftUpload`), as an ask card's
 * carries its ask: a number of this composer's (`draftAt`), mapped to the owner the draft lives at
 * (`draftHomes`). That is the owner the composer addressed at the paste, and wherever the reader
 * then takes the composer with its own controls, since the draft goes with such a move (`carry`).
 * A host can move the composer to another owner too - another row, a Reply on a message - and the
 * field is then that owner's message, where a reference to a file pasted into another draft does
 * not belong. So what lands is kept here by the draft it was pasted into, and joins the field
 * (`appendToField`) while the composer addresses the owner that draft lives at: at once, or once
 * the composer is back there, and the composer says so until then.
 */
export function useDraftHomes(
  owner: ComposerOwner,
  editing: boolean,
  appendToField: (texts: readonly string[]) => void
): DraftHomes {
  const [draftHomes, setDraftHomes] = useState<ReadonlyMap<number, ArtifactOwner>>(() => new Map());
  const nextDraft = useRef(0);
  const [uploadedFiles, setUploadedFiles] = useState<
    readonly { readonly draft: number; readonly text: string }[]
  >([]);
  const upload = useDraftUpload((text: string, draft: number) =>
    setUploadedFiles((current) => [...current, { draft, text }])
  );
  const uploadOwner = editing ? undefined : artifactOwnerOf(owner);
  const uploadKey = ownerKey(uploadOwner);
  /** The draft a file pasted or dropped for `pastedFor` goes into: the one living there, or a new
   *  one. */
  const draftAt = (pastedFor: ArtifactOwner): number => {
    const key = ownerKey(pastedFor);
    for (const [draft, home] of draftHomes) {
      if (ownerKey(home) === key) return draft;
    }
    const draft = nextDraft.current;
    nextDraft.current += 1;
    setDraftHomes((current) => new Map(current).set(draft, pastedFor));
    return draft;
  };
  useEffect(() => {
    const here = uploadedFiles.filter((file) => ownerKey(draftHomes.get(file.draft)) === uploadKey);
    if (here.length === 0) return;
    setUploadedFiles((current) => current.filter((file) => !here.includes(file)));
    appendToField(here.map((file) => file.text));
  }, [appendToField, draftHomes, uploadedFiles, uploadKey]);
  /** What landed into a draft living at another owner than the composer addresses, by that
   *  owner. */
  const landed = new Map<string, { owner: ArtifactOwner; files: number }>();
  for (const file of uploadedFiles) {
    const home = draftHomes.get(file.draft);
    if (home === undefined) continue;
    const key = ownerKey(home);
    if (key === uploadKey) continue;
    landed.set(key, { files: (landed.get(key)?.files ?? 0) + 1, owner: home });
  }
  // A file goes to the owner its paste or drop named, and a refused upload's Retry makes that
  // press again (`DraftUpload.retry`): to that owner, and for the draft it was pasted into, which
  // the reader's own moves have taken along. A host can move the composer to another owner in
  // between, so the refusal is read with the draft its press went into (`refusedDraft`), and
  // while the composer addresses another owner than the one that draft lives at the refusal
  // offers no Retry here: it says where the file was pasted, and its Retry is back once the
  // composer is.
  const refusedHome =
    upload.isError && upload.refusedDraft !== undefined
      ? draftHomes.get(upload.refusedDraft)
      : undefined;
  return {
    carry: (from) => {
      const left = ownerKey(from);
      const to = uploadOwner;
      if (left === undefined || to === undefined || left === uploadKey) return;
      setDraftHomes((current) =>
        [...current.values()].some((home) => ownerKey(home) === left)
          ? new Map(
              [...current].map(
                ([draft, home]) => [draft, ownerKey(home) === left ? to : home] as const
              )
            )
          : current
      );
    },
    drop:
      uploadOwner === undefined
        ? undefined
        : (event) => {
            if (event.dataTransfer.files.length === 0) return;
            upload.drop(event, uploadOwner, draftAt(uploadOwner));
          },
    landedElsewhere: [...landed].map(([key, { files, owner: pastedFor }]) => ({
      key,
      text: landedFor(pastedFor, files),
    })),
    paste:
      uploadOwner === undefined
        ? undefined
        : (event) => {
            if (event.clipboardData.files.length === 0) return;
            upload.paste(event, uploadOwner, draftAt(uploadOwner));
          },
    pastedElsewhere:
      refusedHome !== undefined && ownerKey(refusedHome) !== uploadKey
        ? pastedElsewhere(refusedHome)
        : undefined,
    upload,
    uploadOwner,
  };
}
