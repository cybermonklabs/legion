/** One rendering frame. happy-dom runs animation frames as immediates, in the order they were
 *  asked for, so this is one turn of the event loop, after every frame the page asked for before
 *  it. A row waits one out before it reads what a component did not do in a frame of its own, or
 *  before its next move a frame on: a margin's hold counts its relayout window in frames, and an
 *  opener reaches its row two frames on.
 *  This one stays on the real clock (INFRA-983): those rows read no state the frame brings, so the
 *  frame is the thing they wait for, and Bun's fake clock does not hold happy-dom's frame, which
 *  no move of it runs. A row that reads what a frame brings waits for that with `waitFor`. */
export async function nextFrame(): Promise<void> {
  const done = Promise.withResolvers<void>();
  requestAnimationFrame(() => done.resolve());
  await done.promise;
}
