/** One rendering frame. happy-dom runs animation frames as immediates, so this is one turn of the
 *  event loop. A row waits one out before it reads what a component does in a frame of its own: a
 *  margin's hold counts its relayout window in frames. */
export async function nextFrame(): Promise<void> {
  const done = Promise.withResolvers<void>();
  requestAnimationFrame(() => done.resolve());
  await done.promise;
}
