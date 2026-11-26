import { useQueryClient } from "@tanstack/react-query";

import { issuePatchRequest } from "../../api/client";
import type {
  ArchitectureTree,
  ArchitectureTreeIssueRef,
  IssueComponents,
  IssueComponentsInput,
  IssueDetails,
} from "../../api/types";
import { componentsWritten } from "../../api/write-events";
import { predictTree } from "../architecture/logic/architecture-model";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { useKeyedWrite } from "../writes/useKeyedWrite";

export interface IssueComponentsWrite {
  /** The failed save's message, or what a save the reader stopped may have done and what a new
   *  save does, until the next attempt starts. */
  error: string | null;
  pending: boolean;
  /** Saves the attachment; resolves `true` once the server accepted it. */
  submit: (input: IssueComponentsInput) => Promise<boolean>;
}

interface Snapshot {
  details: IssueDetails | undefined;
  tree: ArchitectureTree | undefined;
}

/** The attachment `input` resolves to when the server accepts it, for the issue detail cache;
 *  `inherit` depends on the ancestors, so the detail keeps its current value until the refetch. */
function predictComponents(input: IssueComponentsInput): IssueComponents | undefined {
  if (input.mode === "explicit") {
    return {
      ids: input.ids ?? [],
      inherited_from: null,
      mode: "explicit",
      reason: null,
      unknown: [],
    };
  }
  if (input.mode === "none") {
    return { ids: [], inherited_from: null, mode: "none", reason: input.reason ?? "", unknown: [] };
  }
  return undefined;
}

/**
 * The one way the SPA writes an issue's component attachment (`PATCH /issues/{key}`
 * `components`), from the Architecture pane's Unassigned and Not architectural lists and from
 * the issue header's editor: the new attachment lands optimistically in the issue detail (when
 * that cache exists) and in the project's architecture tree (the issue moves between lists and
 * rows, the counts with it — `predictTree`), rolls back on failure, and both refetch once the
 * server has answered so descendants that inherit the change appear too.
 */
export function useIssueComponents(
  issue: ArchitectureTreeIssueRef & { project: string }
): IssueComponentsWrite {
  const queryClient = useQueryClient();
  const detailKey = ["issue", issue.key] as const;
  const treeKey = ["architecture", issue.project] as const;
  const mutation = useKeyedWrite({
    // A read of the issue or the tree asked while the save is out waits until Dispatch has taken
    // it - its answer, or the viewer's `issue.updated` that changed its components - for at most
    // `SEND_DEADLINE_MS`: Dispatch can answer it before, with the components from before the
    // press. Only those two queries, by their exact keys: the issue's other reads under its key
    // (coverage, results, steps) are none of this write's.
    holds: () => ({ endsOn: componentsWritten(issue.key), keys: [detailKey, treeKey] }),
    // The components line says why a save was refused, beside the picker it came from.
    inline: true,
    label: () => `Components of ${issue.key}`,
    readsLate: true,
    // One write to the issue, in press order with every other (`issue:<KEY>`).
    target: () => `issue:${issue.key}`,
    request: (input: IssueComponentsInput) => issuePatchRequest(issue.key, { components: input }),
    onMutate: async (input): Promise<Snapshot> => {
      // The two queries the save writes, by their exact keys, as the hold names them: a coverage,
      // results or steps read under the issue's key is none of the save's.
      await Promise.all([
        queryClient.cancelQueries({ exact: true, queryKey: detailKey }),
        queryClient.cancelQueries({ exact: true, queryKey: treeKey }),
      ]);
      const snapshot: Snapshot = {
        details: queryClient.getQueryData<IssueDetails>(detailKey),
        tree: queryClient.getQueryData<ArchitectureTree>(treeKey),
      };
      const components = predictComponents(input);
      if (components !== undefined) {
        queryClient.setQueryData<IssueDetails>(detailKey, (current) =>
          current === undefined ? undefined : { ...current, components }
        );
      }
      queryClient.setQueryData<ArchitectureTree>(treeKey, (current) =>
        current === undefined
          ? undefined
          : predictTree(current, issue, input, new Date().toISOString())
      );
      return snapshot;
    },
    onError: (_error, _input, snapshot) => {
      if (snapshot?.details !== undefined) {
        queryClient.setQueryData<IssueDetails>(detailKey, snapshot.details);
      }
      if (snapshot?.tree !== undefined) {
        queryClient.setQueryData<ArchitectureTree>(treeKey, snapshot.tree);
      }
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: treeKey });
      // Every open issue, this one included: descendants inherit the change with no event of
      // their own.
      void queryClient.invalidateQueries({ queryKey: ["issue"] });
    },
  });
  return {
    // A save the reader stopped may have been made, and nothing here makes it again: the next
    // save is a press of its own. The error's own words are a composer's, of a message.
    error:
      mutation.error === null
        ? null
        : mutation.error instanceof SendStoppedError
          ? stoppedWriteNote(`the components of ${issue.key}`, "saved", {
              anew: "They are read again from Dispatch, and saving them again sends a new write.",
            })
          : mutation.error.message,
    pending: mutation.isPending && !mutation.late,
    submit: async (input) => {
      try {
        await mutation.mutateAsync(input);
        return true;
      } catch {
        return false;
      }
    },
  };
}
