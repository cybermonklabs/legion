// Oh My Pi's cost-gate.ts uses these fixed words for a refusal. The amounts vary.
const refusal =
  /^Cost cap reached: this session spent \$\S+ of its \$\S+ cap \(--max-cost \/ session\.maxCost\), so the next model request was not sent\.$/;

/** The host's cost-cap message from a dropped send, or undefined for another drop. */
export function costCapDrop(error: unknown): string | undefined {
  if (!(error instanceof Error) || error.name !== "PromptDroppedError") return undefined;
  return refusal.test(error.message) ? error.message : undefined;
}
