import { type ApiError, apiErrorCodeIn } from "../../api/client";

/**
 * The refusals a Retry cannot cure, because only the reader can settle them: Dispatch refused the
 * write `409` over what it holds now, and refuses the same body the same way every time it is
 * sent. A priority's title or description another person wrote since the reader's edit started
 * (`PRIORITY_CHANGED`), an issue's Advances another writer moved since its picker opened
 * (`ADVANCES_REVISION`), and a new priority whose id a priority already carries
 * (`PRIORITY_EXISTS`). Each is named by the words the shell's pending-writes line says it in.
 */
const CONFLICT_WORDS: Readonly<Record<string, string>> = {
  ADVANCES_REVISION: "Not saved because another person changed it",
  PRIORITY_CHANGED: "Not saved because another person changed it",
  PRIORITY_EXISTS: "Not saved because a priority with that id already exists",
};

const CONFLICT_CODES: ReadonlySet<string> = new Set(Object.keys(CONFLICT_WORDS));

/** Whether `refusal` is one only the reader can settle (`CONFLICT_WORDS`): no Retry sends its
 *  body again, and the line offers none. */
export function isConflict(refusal: unknown): refusal is ApiError {
  return apiErrorCodeIn(refusal, CONFLICT_CODES) && refusal.status === 409;
}

/** What the line says of `refusal` where it is a conflict, and `undefined` for any other. */
export function conflictWords(refusal: unknown): string | undefined {
  return isConflict(refusal) ? CONFLICT_WORDS[refusal.code ?? ""] : undefined;
}
