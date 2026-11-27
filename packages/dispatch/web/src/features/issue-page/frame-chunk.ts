import { keptUntilRejected } from "../shell/chunk-loads";
import type { FrameOf } from "./frame-shell";

/** The frame's code once its chunk is in. */
let loadedFrame: FrameOf | undefined;

/**
 * The new issue page's frame (`frame-shell.tsx`), a chunk of its own so that no other reader's
 * entry chunk carries it. It is named once, here, so that the shell (`app.tsx`, `ChosenShell`),
 * the entry's start of the same chunk (`app.tsx`, `preloadRouteChunk`) and Settings' start of it
 * (`IssuePagePreference.tsx`) share one download. A download that fails is not kept
 * (`keptUntilRejected`): the next load asks for the chunk again.
 */
export const loadFrame = keptUntilRejected(() =>
  import("./frame-shell").then((module) => {
    loadedFrame = module.frameOf;
    return module.frameOf;
  })
);

/** The frame's code when its chunk is in, else `undefined`: what the shell draws from in its
 *  first render, so a chunk that is already in draws the frame in that render. */
export function frameIfLoaded(): FrameOf | undefined {
  return loadedFrame;
}
