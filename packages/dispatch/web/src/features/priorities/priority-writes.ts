import { useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
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
 * shows as the server holds it rather than as the page guessed.
 *
 * Whether the reader may write at all is `useIsHuman`: every priority write is classed as the
 * deployment's (`internal/dispatch/api/priorities.go`), which a credential scoped to a project
 * and a token naming no person are both refused, so a page that offered the controls to them
 * would offer a refusal.
 */
export function usePriorityWrites() {
  const queryClient = useQueryClient();
  const canWrite = useIsHuman();
  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ["priorities"] });
  };
  const add = useKeyedWrite({
    label: (input: CreatePriorityInput) => `New priority “${input.title}”`,
    write: (input, bounds) => api.createPriority(input, bounds),
    onSettled: refresh,
  });
  const patch = useKeyedWrite({
    label: (patch_: PriorityPatch) => patch_.label,
    write: (patch_, bounds) => api.patchPriority(patch_.id, patch_.input, bounds),
    onSettled: refresh,
  });
  return { add, canWrite, patch };
}
