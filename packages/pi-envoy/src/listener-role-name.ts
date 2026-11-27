import { goTrimSpace } from "@legion/contracts/go-blank";

/**
 * A role's name as the listener reads a claim's: with the characters Go's `strings.TrimSpace`
 * takes trimmed from both ends (`goTrimSpace`), which is Go's set and not JavaScript's `trim`
 * (`GO_TRIM_SPLIT` says where the two differ).
 */
export function listenerRoleName(role: string): string {
  return goTrimSpace(role);
}
