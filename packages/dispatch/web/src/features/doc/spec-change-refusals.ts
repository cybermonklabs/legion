import type { ApiError } from "../../api/client";

/**
 * A refusal of the approved spec as a page shows it (R22): Dispatch's own words, then one line that
 * names each thing the refusal is for. A stale node names the version the request read and the one
 * the approved spec holds now; a node that breaks a rule names the rule. A refusal that names
 * nothing shows Dispatch's words alone. The same approval is refused the same way until the request
 * is written again, so the page offers no Retry with it.
 */
export function specChangeRefusalText(error: ApiError): string {
  const named = [
    ...(error.statements ?? []).map((node) => {
      const read = node.base === 0 ? "read no version" : `read version ${node.base}`;
      const now =
        node.removed === true
          ? "now removed"
          : node.current === 0
            ? "now no version"
            : `now version ${node.current}`;
      return `${node.id} (${read}, ${now})`;
    }),
    ...(error.problems ?? []).map((problem) => `${problem.node} (${problem.rule})`),
  ];
  return named.length === 0 ? error.message : `${error.message}\nIt names: ${named.join(", ")}.`;
}
