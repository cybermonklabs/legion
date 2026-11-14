import { useCache } from "../../api/cache/cache";
import { api, priorityPatchRequest } from "../../api/client";
import type { CreatePriorityInput, PatchPriorityInput } from "../../api/types";
import { useIsHuman } from "../../hooks/useIsHuman";
import { useKeyedWrite } from "../writes/useKeyedWrite";

/** One write of an existing priority: its id, what it changes, and the words the shell's
 *  pending-writes indicator names the write by while it is out. */
export interface PriorityPatch {
  readonly id: string;
  readonly input: PatchPriorityInput;
  readonly label: string;
}

/**
 * The three writes these pages make: a new priority, a change to one, and the move that is a
 * drag of the order. Each goes out under its own key and is retried under it (`useKeyedWrite`),
 * and each refreshes the whole `["priorities"]` family when it settles — the body the pages
 * read and the summary the rail reads sit under that one prefix — so a write a reader made
 * shows as the server holds it rather than as the page guessed. A change to one priority is on
 * that priority's line (`priority:<id>`), so two changes of one priority land in the order they
 * were pressed; a new priority names no row until Dispatch makes it.
 *
 * Whether the reader may write at all is `useIsHuman`: every priority write is classed as the
 * deployment's (`internal/dispatch/api/priorities.go`), which a credential scoped to a project
 * and a token naming no person are both refused, so a page that offered the controls to them
 * would offer a refusal.
 */
export function usePriorityWrites() {
  const cache = useCache();
  const canWrite = useIsHuman();
  const refresh = () => {
    void cache.invalidate({ queryKey: ["priorities"] });
  };
  const add = useKeyedWrite({
    label: (input: CreatePriorityInput) => `New priority “${input.title}”`,
    unlined: "a create names no row until Dispatch makes the priority",
    write: (input, bounds) => api.createPriority(input, bounds),
    onSettled: refresh,
  });
  const patch = useKeyedWrite({
    label: (patch_: PriorityPatch) => patch_.label,
    request: (patch_: PriorityPatch) => priorityPatchRequest(patch_.id, patch_.input),
    target: (patch_: PriorityPatch) => `priority:${patch_.id}`,
    onSettled: refresh,
  });
  return { add, canWrite, patch };
}
