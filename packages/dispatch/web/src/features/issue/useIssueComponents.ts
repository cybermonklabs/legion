import { useQueryClient } from "@tanstack/react-query";

import { issuePatchRequest } from "../../api/client";
import { architectureTreeQuery, issueQuery } from "../../api/queries";
import type {
  ArchitectureTree,
  ArchitectureTreeIssueRef,
  IssueComponents,
  IssueComponentsInput,
  IssueDetails,
} from "../../api/types";
import { predictTree } from "../architecture/architecture-model";
import { useKeyedWrite } from "../writes/useKeyedWrite";

export interface IssueComponentsWrite {
  /** The failed save's message, until the next attempt starts. */
  error: string | null;
  pending: boolean;
  /** Saves the attachment; resolves `true` once the server accepted it. */
  submit: (input: IssueComponentsInput) => Promise<boolean>;
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
 * the issue header's editor: the new attachment lands in the issue detail (when that cache
 * exists) and in the project's architecture tree (the issue moves between lists and rows, the
 * counts with it — `predictTree`), the store shows it there again over each answer that lands
 * while the write is live (R7) and puts back what Dispatch last gave both caches if the write
 * leaves without landing (R10), and both refetch once the server has answered so descendants
 * that inherit the change appear too.
 */
export function useIssueComponents(
  issue: ArchitectureTreeIssueRef & { project: string }
): IssueComponentsWrite {
  const queryClient = useQueryClient();
  const detailKey = issueQuery(issue.key).queryKey;
  const treeKey = architectureTreeQuery(issue.project).queryKey;
  const mutation = useKeyedWrite({
    // The components line says why a save was refused, beside the picker it came from.
    inline: true,
    label: () => `Components of ${issue.key}`,
    // One write to the issue, in press order with every other (`issue:<KEY>`).
    target: () => `issue:${issue.key}`,
    request: (input: IssueComponentsInput) => issuePatchRequest(issue.key, { components: input }),
    // A read already out was asked before this press.
    onMutate: () =>
      Promise.all([
        queryClient.cancelQueries({ queryKey: detailKey }),
        queryClient.cancelQueries({ queryKey: treeKey }),
      ]),
    /** The attachment this press shows: the issue's own `components`, where the server's answer
     *  can be predicted, and the tree with the issue moved to where it now belongs. Both are
     *  read off the cache as it stands, so an answer this is shown over again carries the move. */
    shows: (input) => (shown) => {
      const components = predictComponents(input);
      if (components !== undefined) {
        shown.on<IssueDetails>(detailKey, (current) => ({ ...current, components }));
      }
      shown.on<ArchitectureTree>(treeKey, (current) =>
        predictTree(current, issue, input, new Date().toISOString())
      );
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: treeKey });
      // Every open issue, this one included: descendants inherit the change with no event of
      // their own.
      void queryClient.invalidateQueries({ queryKey: ["issue"] });
    },
  });
  return {
    error: mutation.error === null ? null : mutation.error.message,
    pending: mutation.isPending,
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
