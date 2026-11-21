/**
 * Runs a row's teardown: each of `steps`, the last one pushed first, emptying the list, as a
 * file's `afterEach` does. Every step runs even where one before it throws, so a stop that fails
 * leaves no stand-in after it alive past the row; once the last step has run, what the steps
 * threw is thrown together, one `AggregateError` whose message names each failure.
 */
export async function runTeardown(steps: (() => unknown)[]): Promise<void> {
  const failures: unknown[] = []
  for (const step of steps.splice(0).reverse()) {
    try {
      await step()
    } catch (error) {
      failures.push(error)
    }
  }
  if (failures.length === 0) return
  const named = failures.map((failure) =>
    failure instanceof Error ? failure.message : String(failure),
  )
  throw new AggregateError(
    failures,
    `${failures.length} of the row's teardown steps failed, after every step ran: ${named.join("; ")}`,
  )
}
