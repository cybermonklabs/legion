/**
 * Runs `run` once, after the browser has painted its next frame, and returns a function that
 * cancels a run that has not happened yet.
 *
 * A timeout set inside a frame callback runs after the paint of that frame, so the work it does
 * stays off the frame the reader is waiting for. A runtime with no frames - a test's
 * document-less one - runs `run` at once, since there is no paint there to wait for.
 *
 * The callers are the end of the shell's hold (`content-drawn.ts`, `contentDrawn`) and the start
 * of the Inbox peek's chunk (`features/inbox/InboxHost.tsx`, `usePeekChunkAfterFirstPaint`).
 */
export function afterNextPaint(run: () => void): () => void {
  if (typeof requestAnimationFrame !== "function") {
    run();
    return () => undefined;
  }
  let afterPaint: number | undefined;
  const frame = requestAnimationFrame(() => {
    afterPaint = window.setTimeout(run, 0);
  });
  return () => {
    cancelAnimationFrame(frame);
    window.clearTimeout(afterPaint);
  };
}
