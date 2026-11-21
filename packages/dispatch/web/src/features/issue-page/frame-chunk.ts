import { chunk } from "../shell/chunk-component";
import type { FrameShellProps } from "./frame-shell";

/**
 * The shell of a reader who turned the new issue page on (`frame-shell.tsx`), a chunk of its own
 * so that no other reader's entry chunk carries it. It is named once, here, so that the shell's
 * switch (`app.tsx`, `ChosenShell`) and the entry's start of the same chunk (`app.tsx`,
 * `preloadRouteChunk`) share one download, and the switch draws it in its own commit when the
 * entry's download is already in (`features/shell/chunk-component.tsx` says why that matters).
 */
export const frameShellChunk = chunk<FrameShellProps>(() =>
  import("./frame-shell").then((module) => module.FrameShell)
);
