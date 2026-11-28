import { goBlankPattern } from "@legion/contracts/go-blank";

/**
 * A role's name as the listener reads a claim's: with the characters Go's `strings.TrimSpace`
 * takes trimmed from both ends, which is Go's set and not JavaScript's `trim` (`goBlankPattern`
 * says where the two differ).
 */
export function listenerRoleName(role: string): string {
  const characters = [...role];
  let start = 0;
  let end = characters.length;
  while (start < end && goBlankPattern.test(characters[start] ?? "")) start += 1;
  while (end > start && goBlankPattern.test(characters[end - 1] ?? "")) end -= 1;
  return characters.slice(start, end).join("");
}
