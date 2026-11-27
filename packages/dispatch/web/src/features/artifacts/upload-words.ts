/**
 * The words an upload says where Dispatch gave none of its own, in one module that imports
 * nothing, so every surface that says them (`ArtifactUpload.tsx`) and a list of words that must
 * load where no component can, as the issue page's list does for a browser spec's Node loader,
 * read the one text.
 */

/** What an upload's refusal says where Dispatch gave no answer of its own (`uploadErrorMessage`). */
export const UPLOAD_FAILED_TEXT = "Could not upload the artifact.";

/** What a draft's upload the reader stopped is, and what it may have been, as its refusal says
 *  (`stoppedNote`). */
export const STOPPED_UPLOAD_WORDS = { made: "stored", what: "the upload" } as const;
