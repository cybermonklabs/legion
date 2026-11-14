import { useCache } from "../../api/cache/cache";
import type { WriteChange } from "../../api/cache/write-store";
import { api, priorityPatchRequest } from "../../api/client";
import { prioritiesQuery } from "../../api/queries";
import type {
  CreatePriorityInput,
  PatchPriorityInput,
  Priorities,
  PriorityRow,
} from "../../api/types";
import { useIsHuman } from "../../hooks/useIsHuman";
import { useKeyedWrite } from "../writes/useKeyedWrite";

/** One write of an existing priority: its id, what it changes, and the words the shell's
 *  pending-writes indicator names the write by while it is out. */
export interface PriorityPatch {
  readonly id: string;
  readonly input: PatchPriorityInput;
  readonly label: string;
}

/** The fields of a priority's row a patch writes as they are read - its title, description and
 *  owner - or none, for a patch that writes only its place in the order. */
type ShownFields = Partial<Pick<PriorityRow, "description" | "owner" | "title">>;

function shownFields({ description, owner, title }: PatchPriorityInput): ShownFields | undefined {
  const fields: ShownFields = {
    ...(description == null ? {} : { description }),
    ...(owner == null ? {} : { owner }),
    ...(title == null ? {} : { title }),
  };
  return Object.keys(fields).length === 0 ? undefined : fields;
}

/** What a patch shows: its fields on that priority's row in every read of the priorities body
 *  (`prioritiesQuery`, whichever projects it narrows to), which is where the priority page, the
 *  open row of the priorities table and an issue's priority line read a priority's title,
 *  description and owner. A move names no cache: its `rank` is a placement Dispatch turns into
 *  an order, not a value a row carries. */
function patchShows({ id, input }: PriorityPatch): WriteChange {
  const fields = shownFields(input);
  if (fields === undefined) return () => undefined;
  return (shown) =>
    shown.under<Priorities>(prioritiesQuery().queryKey, (body) =>
      body.priorities === null
        ? body
        : {
            ...body,
            priorities: body.priorities.map((row) => (row.id === id ? { ...row, ...fields } : row)),
          }
    );
}

/**
 * The three writes these pages make: a new priority, a change to one, and the move that is a
 * drag of the order. Each goes out under its own key and is retried under it (`useKeyedWrite`),
 * and each refreshes the whole `["priorities"]` family when it settles — the body the pages
 * read and the summary the rail reads sit under that one prefix. A change to a priority's title,
 * description or owner shows on its row from the press (`patchShows`): the store composes it
 * over every read of the body while the write is live and, once it lands, until that cache
 * answers a whole read begun after the landing, and a refused or discarded change simply stops
 * being shown, so no reader keeps a copy of what it sent. A change to one priority is on that
 * priority's line (`priority:<id>`), so two changes of one priority land in the order they were
 * pressed; a new priority names no row until Dispatch makes it.
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
    shows: patchShows,
    target: (patch_: PriorityPatch) => `priority:${patch_.id}`,
    onSettled: refresh,
  });
  return { add, canWrite, patch };
}
