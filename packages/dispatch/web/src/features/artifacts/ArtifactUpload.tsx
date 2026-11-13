import { isPictureType, pictureCaption } from "@legion/contracts";
import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import {
  type ChangeEvent,
  type ClipboardEvent,
  type DragEvent,
  type ReactNode,
  type RefObject,
  useId,
  useRef,
  useState,
} from "react";

import {
  ApiError,
  type ArtifactOwner,
  api,
  isArtifactNotFound,
  isDocumentSchemaError,
  type WriteBounds,
} from "../../api/client";
import type { Artifact, Version } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  calloutInfoBg,
  calloutInfoBorder,
  dangerText,
  inputClasses,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
} from "../../theme/classes";
import { buildDispatchReference, type DispatchReferenceRoute } from "../refs/routes";
import { retriesUnderItsKey } from "../writes/keyed-write";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { useKeyedWrite } from "../writes/useKeyedWrite";

/** Why a closed issue's Artifacts tab, or its document's repair, takes no upload: Dispatch refuses
 *  one until the issue is reopened. */
export const CLOSED_ISSUE_UPLOAD_REFUSAL = "This issue is closed. Reopen it to upload.";

export interface UploadResult {
  artifact: Artifact;
  version: Version;
}

/** The media type of `file` as Dispatch reads it: the server parses what the browser declared
 *  with `mime.ParseMediaType` (`artifactKind` in the server's `api/artifacts.go`), which drops
 *  the parameters and lowercases the type, so a browser's `text/markdown; charset=utf-8` is
 *  `text/markdown` there. A page comparing the raw string would read no base for a file the
 *  server stores as a document, and upload over that document naming nothing. */
function mediaTypeOf(file: File): string {
  return (file.type.split(";")[0] ?? "").trim().toLowerCase();
}

/** The name an upload is stored under: the one its surface fixes - a document's own name, so a
 *  repair replaces that document whatever the picked file is called - else the file's own. */
function uploadName(file: File, name: string | undefined): string {
  return name ?? (file.name || "artifact");
}

/**
 * The base one press of Upload sends: the token of the document it replaces, `undefined` where
 * there is none to name, or the failure the read of it ended with.
 *
 * It is read once, when the person chooses the file, and every attempt of that press sends this
 * one value. Dispatch's key covers the whole body, the `precondition` field included
 * (`bodyDigest` in the server's `api/request_keys.go`), so an attempt made again under the key
 * with a base read afresh would be answered `422 KEY_REUSED` rather than with what the first
 * attempt did.
 */
export type UploadBase = { readonly token: string | undefined } | { readonly unreadable: unknown };

/**
 * Reads the base for `file`'s upload to `owner`: the token of the live document the owner
 * already holds under the name this upload is stored as, through the route that resolves a slug
 * or a filename.
 *
 * Two answers mean there is no base to name, and no others: `404 ARTIFACT_NOT_FOUND`, where the
 * owner holds no document under that name and this upload is its first; and `409 DOC_SCHEMA`,
 * where the document's tree is outside the Proof schema, so it renders no markdown and has no
 * token, and an upload is the only way to repair it, which the server takes naming no base (the
 * repair exception in `docs.Service.ReplaceText`). Every other failure - the request never
 * reached Dispatch, the session lapsed, Dispatch answered a 5xx, or the name is one that more
 * than one document answers to (`400 ARTIFACT_REFERENCE_AMBIGUOUS`) - comes back as `unreadable`
 * and ends the upload (`UploadBaseUnreadableError`): the page does not know what it would be
 * replacing, and Dispatch refuses an upload over an existing document that names no base.
 *
 * Only a document has a base, and Dispatch decides that by the media type alone (`artifactKind`
 * in the server's `api/artifacts.go`: a `text/markdown` upload is a document and everything else
 * is a file or an image), so the name counts for nothing here and a base sent for anything else
 * is refused `400 INVALID_PRECONDITION`. A conversation holds files and images and no document
 * at all.
 */
export async function captureUploadBase(
  owner: ArtifactOwner,
  file: File,
  name?: string
): Promise<UploadBase> {
  if ("session" in owner || mediaTypeOf(file) !== "text/markdown") {
    return { token: undefined };
  }
  try {
    return { token: (await api.ownerArtifactText(owner, uploadName(file, name))).token };
  } catch (error) {
    if (isArtifactNotFound(error) || isDocumentSchemaError(error)) {
      return { token: undefined };
    }
    return { unreadable: error };
  }
}

/** What ends an upload whose base could not be read (`captureUploadBase`): nothing was sent, and
 *  the row says so. It is none of the failures a write is made again for (`retryAfter` in
 *  `features/writes/keyed-write.ts`), so the write ends on it at once rather than trying under
 *  its key. */
export class UploadBaseUnreadableError extends Error {
  constructor(cause: unknown) {
    super("the document this upload would replace could not be read", { cause });
    this.name = "UploadBaseUnreadableError";
  }
}

/** One attempt of `file`'s upload to `owner`, under the write's key (`bounds`), naming the base
 *  read for this press (`options.base`, from `captureUploadBase`). The base is read before the
 *  write rather than here, so every attempt under one key sends the same bytes. */
export async function uploadFile(
  owner: ArtifactOwner,
  file: File,
  options: { base: UploadBase; name?: string; summary?: string },
  bounds: WriteBounds
): Promise<UploadResult> {
  if ("unreadable" in options.base) {
    throw new UploadBaseUnreadableError(options.base.unreadable);
  }
  const base = options.base.token;
  return api.uploadArtifact(
    owner,
    {
      file,
      name: uploadName(file, options.name),
      summary: options.summary,
      ...(base === undefined ? {} : { precondition: { document: base } }),
    },
    bounds
  );
}

/** What the row says when the document moved under an upload that named the version it was
 *  replacing. The server writes its own refusal for a caller holding the `dispatch` command,
 *  naming the command that reads the token and the argument that sends it; a person uploading
 *  here runs no command and pastes no token. Pressing Upload again reads the token afresh, so it
 *  names the version the document is at now - but reading what changed first is the point of the
 *  guard, which is why the row says to open the document rather than only to try again. */
export const UPLOAD_BASE_REFUSAL =
  "This document changed while you were uploading it. Open it to see what it says now, then upload again.";

/** What the row says when the upload named no version and Dispatch requires every replacement to
 *  name one (`400 PRECONDITION_REQUIRED`). It does not mean the document changed: the page names
 *  the version for every document whose token it could read, and ends an upload whose read
 *  failed, so what reaches this is an upload whose read found no document under the name while
 *  the server found one - a document created in the moment between that read and the post. */
export const UPLOAD_BASE_REQUIRED_REFUSAL =
  "The page could not say which text this upload replaces, and Dispatch requires it. Open the document to see what it says now, then upload again.";

/** What the row says when the version's token could not be read at all
 *  (`UploadBaseUnreadableError`), followed by what Dispatch said of that read where it answered
 *  at all. */
export const UPLOAD_BASE_UNREADABLE_REFUSAL =
  "Could not read the document this upload would replace, so nothing was uploaded.";

// A refused upload is described by the server, which names the bound it hit: a markdown document
// is at most 1 MiB and any other file at most 25 MiB, so a 413 is not one message. The refusals
// about the version an upload replaces are the exception: the server writes those for the
// `dispatch` command and the dashboard says what they mean to a person. They are three
// sentences, not one - the document moved under the upload, the upload named no version where
// Dispatch requires one, and the token could not be read at all - and a person told the wrong
// one of those is told something untrue.
export function uploadErrorMessage(error: unknown): string {
  if (error instanceof UploadBaseUnreadableError) {
    const said = error.cause instanceof ApiError ? ` ${error.cause.message}` : "";
    return `${UPLOAD_BASE_UNREADABLE_REFUSAL}${said}`;
  }
  if (error instanceof ApiError) {
    if (error.code === "PRECONDITION_FAILED") {
      return UPLOAD_BASE_REFUSAL;
    }
    if (error.code === "PRECONDITION_REQUIRED") {
      return UPLOAD_BASE_REQUIRED_REFUSAL;
    }
    return error.message;
  }
  return "Could not upload the artifact.";
}

/** The sentence a tab with no document of its own shows about the uploads that follow the one
 *  making it: such an upload replaces the whole document, and Dispatch takes it only where it
 *  names the version it replaces. `ReplacingUploadNote` is the only reader: the words are held
 *  to by that component's own case, and each tab's case asks only that the note is there. */
const REPLACING_UPLOAD_NOTE =
  "A later upload under that name replaces the document, so it names the token of the version it replaces";

/** `REPLACING_UPLOAD_NOTE` as an empty state shows it, with where that token is read: the Plan
 *  tab and the Verification tab render this one paragraph under the command that makes their
 *  document, so a person reads the same words on either. Each tab's case finds this paragraph
 *  by its test id, which is how it asks for the note without asking what it says. */
export function ReplacingUploadNote(): ReactNode {
  return (
    <p className="mt-3" data-testid="replacing-upload-note">
      {REPLACING_UPLOAD_NOTE}: add <code>--precondition-json</code> with the document token{" "}
      <code>dispatch doc-read</code> prints.
    </p>
  );
}

/** Refreshes the lists a write to `owner` adds a row to: an upload's, and a new document's. */
export function refreshOwnerArtifacts(queryClient: QueryClient, owner: ArtifactOwner): void {
  if ("issue" in owner) {
    void queryClient.invalidateQueries({ queryKey: ["artifacts", owner.issue] });
    void queryClient.invalidateQueries({ queryKey: ["issue", owner.issue] });
  } else if ("project" in owner) {
    void queryClient.invalidateQueries({ queryKey: ["project", owner.project, "artifacts"] });
  } else {
    void queryClient.invalidateQueries({ queryKey: ["agent-artifacts", owner.session] });
  }
}

/** `text` added to the end of a draft, a space apart from what is already there. */
export function appendToDraft(draft: string, text: string): string {
  return `${draft}${draft.length === 0 || /\s$/.test(draft) ? "" : " "}${text}`;
}

/**
 * What an uploaded file adds to the draft it was pasted or dropped into. A picture (a PNG, JPEG,
 * GIF or WebP by the browser's reading of its type, the four kinds the server stores as a picture
 * and a model is shown) is Markdown's image syntax captioned with the file's name and addressed by
 * its own reference at the version the upload created -
 * `![shot.png](dispatch://CORE-1/artifact/shot-png@v1)` - so the picture is part of what is sent
 * and shows wherever the text renders. Any other file, an SVG included, is its plain reference.
 */
export function uploadedFileText(
  owner: ArtifactOwner,
  file: File,
  { artifact, version }: UploadResult
): string {
  const pinned = isPictureType(file.type) ? version.number : undefined;
  const route: DispatchReferenceRoute =
    "issue" in owner
      ? { key: owner.issue, kind: "artifact", slug: artifact.slug }
      : "project" in owner
        ? { kind: "document", project: owner.project, slug: artifact.slug }
        : { kind: "agent-artifact", session: owner.session, slug: artifact.slug };
  if (pinned === undefined) {
    return buildDispatchReference(route);
  }
  const caption = pictureCaption(file.name || artifact.name);
  return `![${caption}](${buildDispatchReference({ ...route, version: pinned })})`;
}

/** Files pasted or dropped into a draft's field, each uploaded and its text added to the draft. */
export interface DraftUpload {
  /** Uploads the files a paste holds to `owner`, in place of the paste; a paste of text alone is
   *  left to the field. */
  paste(event: ClipboardEvent<HTMLElement>, owner: ArtifactOwner): void;
  /** Uploads the files a drop holds to `owner`, in place of the drop. */
  drop(event: DragEvent<HTMLElement>, owner: ArtifactOwner): void;
  /** Uploads still out. */
  pending: number;
  error: unknown;
  isError: boolean;
  isPending: boolean;
  /** The name of the last file Dispatch stored with no answer to give for it, so its reference
   *  could not be added to the draft; null otherwise. */
  madeUnanswered: string | null;
  /** Uploads the last refused file again, to the owner it was sent to. Where that refusal is one
   *  the reader stopped, the press behind it may have reached Dispatch, so this makes that press
   *  again, base and all: it goes under its key, and Dispatch answers it with what the first one
   *  did rather than storing the file a second time. A paste of several files is several presses
   *  at once, and the reader can stop more than one of them, so the press made again is the one
   *  of the file whose refusal is on screen (`stopped`), never whichever press stopped last.
   *  Where Dispatch itself decided the refusal, that file has no stopped press: it is sent
   *  afresh, its base read again and the press under a key of its own, which is what a refusal
   *  Dispatch decided asks for. */
  retry(): void;
}

/**
 * The one upload behind a draft's field - a composer's, an ask's answer, a reply under an ask:
 * every file pasted or dropped into it uploads to the owner the paste or drop named, and once the
 * server has it, `insert` receives the text to add (`uploadedFileText`). The owner rides the
 * upload as its own mutation variable, as a send carries its request: TanStack gives a pending
 * mutation each new render's options before `mutationFn` runs, so an owner read from the closure
 * would follow a pick made in the paste's own task. The text, the refreshed lists and a Retry all
 * read that same owner. The base rides with it (`captureUploadBase`), read once for the press
 * before the write is made, so every attempt of that press sends the same bytes.
 */
export function useDraftUpload(insert: (text: string) => void): DraftUpload {
  const queryClient = useQueryClient();
  const [pending, setPending] = useState(0);
  const retryGuard = useSubmitGuard();
  // Each file is one write under a key of its own, its refusal shown here beside the draft, and
  // its file kept across a reload with it.
  const upload = useKeyedWrite<
    { file: File; owner: ArtifactOwner; result: UploadResult },
    { base: UploadBase; file: File; owner: ArtifactOwner }
  >({
    write: async ({ base, file, owner }, bounds) => ({
      file,
      owner,
      result: await uploadFile(owner, file, { base }, bounds),
    }),
    file: ({ file }) => file,
    inline: true,
    label: ({ file }) => `Upload of ${file.name || "a file"}`,
    // Stored either way; with no answer to give, its reference cannot be added to the draft, and
    // the field says so (`upload.unanswered`).
    onSuccess: (landed, { file, owner }) => {
      refreshOwnerArtifacts(queryClient, owner);
      if (landed !== null) insert(uploadedFileText(owner, file, landed.result));
    },
    onSettled: () => {
      setPending((count) => count - 1);
      retryGuard.release();
    },
  });
  /** Uploads `files` to `owner`, each as a press of its own, in the order they were taken in:
   *  each base is read before its press is made, which holds that order through the reads.
   *  `pending` counts from the press rather than from `onMutate`, which the base read comes
   *  before: a draft that said nothing was uploading would take a Reply over an upload to come. */
  const send = async (files: readonly File[], owner: ArtifactOwner): Promise<void> => {
    for (const file of files) {
      setPending((count) => count + 1);
      upload.mutate({ base: await captureUploadBase(owner, file), file, owner });
    }
  };
  const receive = (files: FileList, owner: ArtifactOwner, event: { preventDefault(): void }) => {
    if (files.length === 0) {
      return;
    }
    event.preventDefault();
    upload.forgetUnanswered();
    // The files are taken here: a clipboard event's own list is good for this task alone.
    void send([...files], owner);
  };
  return {
    drop: (event, owner) => receive(event.dataTransfer.files, owner, event),
    error: upload.error,
    isError: upload.isError,
    isPending: upload.isPending,
    madeUnanswered:
      upload.unanswered === undefined ? null : upload.unanswered.variables.file.name || "the file",
    paste: (event, owner) => receive(event.clipboardData.files, owner, event),
    pending,
    retry: () => {
      const last = upload.variables;
      if (last === undefined) {
        return;
      }
      retryGuard.guard(() => {
        // The refusal on screen is the newest press's, which is not always the press the reader
        // stopped last, so the press made again is the one that sent this very file; a refusal
        // Dispatch decided has no stopped press of its own to make again.
        const again = retriesUnderItsKey(upload.error ?? undefined)
          ? upload.stopped((press) => press.file === last.file)
          : undefined;
        if (again === undefined) {
          void send([last.file], last.owner);
          return;
        }
        // The very same press, base and all: nothing is read for it, so the write counts as
        // pending from here, as `send` counts one of its own.
        setPending((count) => count + 1);
        upload.mutate(again);
      });
    },
  };
}

/** `Uploading file…` while any of a draft's uploads is out, and a refused one's reason with its
 *  Retry. */
export function DraftUploadStatus({ upload }: { upload: DraftUpload }): ReactNode {
  return (
    <>
      {upload.pending > 0 ? (
        <p className={`text-sm ${textMutedOnSurfaceMuted}`} role="status">
          Uploading file…
        </p>
      ) : null}
      {upload.isError ? (
        <QueryError
          message={uploadErrorMessage(upload.error)}
          onRetry={upload.retry}
          retrying={upload.isPending}
        />
      ) : null}
      {upload.madeUnanswered === null ? null : (
        <LandedWithoutAnswer what={`The upload of ${upload.madeUnanswered}`}>
          Its reference was not added to your message.
        </LandedWithoutAnswer>
      )}
    </>
  );
}

interface ArtifactDropTarget {
  isDragging: boolean;
  onDragLeave(): void;
  onDragOver(event: DragEvent<HTMLElement>): void;
  onDrop(event: DragEvent<HTMLElement>): void;
}

export interface ArtifactUpload {
  /** Discards the staged file, its summary and the base read for it. */
  cancel(): void;
  /** Uploads the staged file, under the base read when it was chosen. A press that follows a
   *  press Dispatch decided reads the base again: the person has read what the document says
   *  now, so their next press names what it says now. A press of the file a press the reader
   *  stopped carried is that press made again, as it stands, so Dispatch answers it under the
   *  first's key rather than writing a second version of the document. */
  confirm(): void;
  dropTarget: ArtifactDropTarget;
  error: unknown;
  fileInputRef: RefObject<HTMLInputElement | null>;
  isError: boolean;
  isPending: boolean;
  /** The name of the last file Dispatch stored with no answer to give for it; null otherwise. */
  madeUnanswered: string | null;
  openPicker(): void;
  pendingFile: File | null;
  /** Why no file can be uploaded here - its issue is closed - or null while one can. */
  refusal: string | null;
  selectFile(event: ChangeEvent<HTMLInputElement>): void;
  setSummary(value: string): void;
  summary: string;
}

/**
 * The one artifact upload: a picked or dropped file either uploads at once (`immediate`, the
 * project Documents list) or is staged until `confirm` (the Artifacts tab's row, whose summary
 * field appears once there is something to summarize and whose "Cancel" discards the pick).
 * `name` uploads every file under that document name, replacing that document, rather than under
 * the picked file's own name. Success invalidates the owner's artifact list and, for an issue, the
 * issue itself. With a `refusal` - the issue is closed - nothing can be picked or dropped, so no
 * upload starts that Dispatch would refuse, and the row says why on its Upload control.
 *
 * The base a replacement names is read when the file is chosen (`captureUploadBase`), and one
 * press sends that one value however many attempts it takes. So the window this upload guards
 * runs from the choice to the post: a write landing in it is refused rather than written over,
 * and a change made before the file was chosen is replaced with no refusal.
 */
export function useArtifactUpload(
  owner: ArtifactOwner,
  {
    immediate = false,
    name,
    refusal = null,
  }: { immediate?: boolean; name?: string; refusal?: string | null } = {}
): ArtifactUpload {
  const queryClient = useQueryClient();
  const fileInputRef = useRef<HTMLInputElement>(null);
  const [pendingFile, setPendingFile] = useState<File | null>(null);
  const [summary, setSummary] = useState("");
  const [isDragging, setIsDragging] = useState(false);
  /** The base read when the staged file was chosen, for the press that sends it to take. */
  const chosenBase = useRef<Promise<UploadBase> | undefined>(undefined);
  // One press per answer: `confirm` reads the base before it mutates, so a second click within
  // that read would be a second upload, which `isPending` is too late to hold.
  const pressGuard = useSubmitGuard();
  const upload = useKeyedWrite<UploadResult, { base: UploadBase; file: File }>({
    write: ({ base, file }, bounds) =>
      uploadFile(
        owner,
        file,
        { base, name, summary: summary.trim() === "" ? undefined : summary.trim() },
        bounds
      ),
    file: ({ file }) => file,
    inline: true,
    label: ({ file }) => `Upload of ${file.name || "a file"}`,
    onSettled: () => pressGuard.release(),
    onSuccess: (result) => {
      // Stored either way: the staged file and its summary go, so a second press sends nothing;
      // with no answer to give, the row says so (`upload.unanswered`).
      setPendingFile(null);
      setSummary("");
      refreshOwnerArtifacts(queryClient, owner);
      if (result !== null) {
        void queryClient.invalidateQueries({ queryKey: ["artifact", result.artifact.id] });
      }
    },
  });
  const send = async (file: File, base: UploadBase | Promise<UploadBase>): Promise<void> => {
    upload.mutate({ base: await base, file });
  };
  const receive = (file: File | undefined) => {
    if (file === undefined || refusal !== null) {
      return;
    }
    upload.forgetUnanswered();
    const base = captureUploadBase(owner, file, name);
    if (immediate) {
      void send(file, base);
    } else {
      chosenBase.current = base;
      setPendingFile(file);
    }
  };

  return {
    cancel: () => {
      chosenBase.current = undefined;
      setPendingFile(null);
      setSummary("");
    },
    confirm: () => {
      const file = pendingFile;
      if (file === null) {
        return;
      }
      pressGuard.guard(() => {
        // The press the reader stopped may have reached Dispatch, which answers its key with
        // what it did there - but only while the two send the same bytes, since the key covers
        // the whole body, the `precondition` field included (`bodyDigest` in the server's
        // `api/request_keys.go`). So a press of the staged file asks the hook for the press that
        // sent that very file and sends that press back as it stands: the hook reaches a stopped
        // press through the object it was made with, and variables built here afresh, base and
        // all, would be a press of its own. A press of another file finds no such press and is
        // one of its own: it takes the base read when that file was chosen, or reads the token
        // the document is at now.
        const again = upload.stopped((press) => press.file === file);
        const chosen = chosenBase.current;
        chosenBase.current = undefined;
        if (again !== undefined) {
          upload.mutate(again);
          return;
        }
        void send(file, chosen ?? captureUploadBase(owner, file, name));
      });
    },
    dropTarget: {
      isDragging,
      onDragLeave: () => setIsDragging(false),
      onDragOver: (event) => {
        event.preventDefault();
        setIsDragging(true);
      },
      onDrop: (event) => {
        event.preventDefault();
        setIsDragging(false);
        receive(event.dataTransfer.files[0]);
      },
    },
    error: upload.error,
    fileInputRef,
    isError: upload.isError,
    isPending: upload.isPending,
    madeUnanswered:
      upload.unanswered === undefined
        ? null
        : (name ?? (upload.unanswered.variables.file.name || "the file")),
    openPicker: () => {
      if (refusal === null) fileInputRef.current?.click();
    },
    pendingFile,
    refusal,
    selectFile: (event) => {
      receive(event.target.files?.[0]);
      event.target.value = "";
    },
    setSummary,
    summary,
  };
}

/** The upload control of a strip: its button, and, while a file is staged, that file's name, its
 *  summary and the two presses that store or discard it. `refusalShownBy` is the id of a line the
 *  host already shows the refusal on - the one line a closed issue's strip shows for every control
 *  in it; without it the row shows its own. */
export function ArtifactUploadRow({
  refusalShownBy,
  upload,
}: {
  readonly refusalShownBy?: string;
  readonly upload: ArtifactUpload;
}): ReactNode {
  const ownRefusalId = useId();
  return (
    // The host owns the strip this sits in, so the row carries no divider of its own: idle it
    // is one button beside the filter, staging a file it takes the line.
    <div
      className={`flex min-w-0 flex-wrap items-center gap-2 ${upload.pendingFile === null ? "shrink-0" : "w-full"}`}
    >
      {upload.pendingFile === null ? (
        <>
          <RefusableButton
            className="min-h-11 shrink-0"
            look="secondary-compact"
            onPress={upload.openPicker}
            refusal={upload.refusal}
            refusalShownBy={upload.refusal === null ? undefined : (refusalShownBy ?? ownRefusalId)}
          >
            Upload
          </RefusableButton>
          {upload.refusal === null || refusalShownBy !== undefined ? null : (
            <span className={`text-xs ${textMutedOnSurface}`} id={ownRefusalId}>
              {upload.refusal}
            </span>
          )}
        </>
      ) : (
        <>
          <span className={`min-w-0 truncate text-sm ${textMutedOnSurface}`}>
            {upload.pendingFile.name}
          </span>
          <input
            aria-label="Summary (optional)"
            className={`min-h-11 min-w-40 flex-1 rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
            onChange={(event) => upload.setSummary(event.target.value)}
            placeholder="Summary (optional)"
            value={upload.summary}
          />
          <button
            className={`min-h-11 shrink-0 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
            disabled={upload.isPending}
            onClick={upload.confirm}
            type="button"
          >
            Upload
          </button>
          <button
            className={`min-h-11 shrink-0 text-sm font-medium ${linkText} ${linkHoverText}`}
            disabled={upload.isPending}
            onClick={upload.cancel}
            type="button"
          >
            Cancel
          </button>
        </>
      )}
      <ArtifactUploadStatus upload={upload} />
    </div>
  );
}

/** "Uploading artifact…" while in flight, else the upload error, else nothing. */
export function ArtifactUploadStatus({ upload }: { upload: ArtifactUpload }): ReactNode {
  if (upload.isPending) {
    return (
      <p className={`w-full text-sm ${textMutedOnSurface}`} role="status">
        Uploading artifact…
      </p>
    );
  }
  if (upload.isError) {
    return (
      <p className={`w-full text-sm ${dangerText}`} role="alert">
        {uploadErrorMessage(upload.error)}
      </p>
    );
  }
  if (upload.madeUnanswered !== null) {
    return (
      <div className="w-full">
        <LandedWithoutAnswer what={`The upload of ${upload.madeUnanswered}`} />
      </div>
    );
  }
  return null;
}

/** The two ways to hand `upload` a file: a drop anywhere over `children`, and the one hidden
 * `Upload artifact` file input that the host's picker button opens through `openPicker`. While the
 * upload is refused the input is disabled, so no key opens a chooser whose file would be ignored. */
export function ArtifactDropZone({
  children,
  upload,
}: {
  children: ReactNode;
  upload: ArtifactUpload;
}): ReactNode {
  const { dropTarget } = upload;
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: a passive drag-and-drop surface — the host's own picker button remains the keyboard-reachable way to add a file.
    <div
      className={`rounded-lg ${
        dropTarget.isDragging ? `border border-dashed ${calloutInfoBorder} ${calloutInfoBg}` : ""
      }`}
      onDragLeave={dropTarget.onDragLeave}
      onDragOver={dropTarget.onDragOver}
      onDrop={dropTarget.onDrop}
    >
      {children}
      <input
        aria-label="Upload artifact"
        className="sr-only left-0"
        disabled={upload.refusal !== null}
        onChange={upload.selectFile}
        ref={upload.fileInputRef}
        type="file"
      />
    </div>
  );
}
