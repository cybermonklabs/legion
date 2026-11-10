/** `Promise.allSettled` over `items`, at most `limit` of them running at a time, answering in
 *  the order the items came in. A bulk action is as large as a project, so the work it starts
 *  is a bounded stream rather than every request at once. */
export async function settledWithLimit<T, R>(
  items: readonly T[],
  limit: number,
  run: (item: T) => Promise<R>
): Promise<PromiseSettledResult<R>[]> {
  const results: PromiseSettledResult<R>[] = Array.from({ length: items.length });
  let next = 0;
  await Promise.all(
    Array.from({ length: Math.min(limit, items.length) }, async () => {
      for (let index = next++; index < items.length; index = next++) {
        const item = items[index] as T;
        try {
          results[index] = { status: "fulfilled", value: await run(item) };
        } catch (reason) {
          results[index] = { status: "rejected", reason };
        }
      }
    })
  );
  return results;
}
