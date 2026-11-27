// The hold a row puts on its editor's build between two of its batches (`holdTheBuild`), and
// what the page says through it. docs/testing.md says what the hook this file sets is, which rows
// set it, and that nothing else may. It stands beside `large-documents-page.ts` rather than in it,
// because it would have taken that file past 1,000 lines. The two selectors its init script finds
// the editor being built and the finished editor by are `large-documents-rows.ts`'s.
//
// The other files of this family, and what each holds, are listed once, at the head of
// `large-documents-rows.ts`.
import type { Page } from "@playwright/test";

import { theMomentThePageReached } from "./large-documents-page";
import { liveEditorRoot, namedEditor } from "./large-documents-rows";

/** Where `holdTheBuild` held a build: how many blocks the live editor held then, at any depth
 *  (`[data-block-id]`). */
export interface TheBuildHeldAt {
  readonly blocks: number;
}

declare global {
  interface Window {
    /** What the page says of the hold `holdTheBuild` puts on its build: where the build was
     *  held, answered once the row lets the build go on, or `null` once the finished editor is
     *  named with no hold made. */
    __theBuildHeld?: (at: TheBuildHeldAt | null) => Promise<void>;
  }
}

/** What `holdTheBuild` answers: a wait for the hold, and its release. */
export interface TheBuildHeld {
  /** Resolves with where the build was held. Fails by name where the finished editor was named
   *  with no hold made, or where no hold came inside `timeoutMs`. Only a hold given `at` holds
   *  anything, so a row that only paces its build never waits on this. */
  readonly held: (timeoutMs: number) => Promise<TheBuildHeldAt>;
  /** Lets the build go on from the boundary it is held at. Called before the hold is reached,
   *  it lets the build pass that boundary too, answered at once; called again, it does nothing. */
  readonly release: () => void;
}

/**
 * Holds `page`'s editor build at a boundary between two of its batches until the row releases
 * it, and has the page say through a binding where the build is held (`TheBuildHeldAt`). The
 * hold is `window.__dispatchHoldBuild` (docs/testing.md), which the page reads as its editor is
 * constructed, so install before the navigation.
 *
 * `at` says which boundary: the first where the live editor's text holds every one of
 * `texts`. The build asks at a boundary in the task its batch landed in, before a frame of the
 * page can act on that batch, and asks only while blocks are still to be placed, so a hold
 * always leaves part of the document unbuilt. The build asks nothing after its last batch, so
 * the end of a build that was never held is something no boundary can say: a frame loop says it
 * instead, the first time it finds the finished editor named. Without `at` nothing is held.
 *
 * `eachBoundary` says what every other boundary waits for. "a timer", where it is not given, is
 * the build's own hand-back (`endOfTask` in `packages/proof-editor/src/progressive-build.ts`).
 * "a frame" is an animation frame and then a timer, so a frame of the page, which is where a
 * row that acts in a frame acts, falls between every two batches. That is the same
 * frame-then-timer shape `whatTheBuildServes` in `large-documents-build-probe.ts` stamps the
 * end of a build's swap with.
 */
export async function holdTheBuild(
  page: Page,
  options: {
    /** Where the build is held: the first boundary with the live editor's text holding every
     *  one of `texts`. Without it the build is only paced. */
    readonly at?: { readonly texts: readonly string[] };
    /** What every other boundary waits for: "a timer" where it is not given. */
    readonly eachBoundary?: "a frame" | "a timer";
  }
): Promise<TheBuildHeld> {
  const said = Promise.withResolvers<TheBuildHeldAt | null>();
  const released = Promise.withResolvers<void>();
  await page.exposeFunction("__theBuildHeld", async (at: TheBuildHeldAt | null) => {
    said.resolve(at);
    await released.promise;
  });
  const texts = options.at?.texts ?? [];
  await page.addInitScript(
    (arg: {
      eachBoundary: "a frame" | "a timer";
      holding: boolean;
      live: string;
      named: string;
      texts: readonly string[];
    }) => {
      // Whether the hold has been made. The boundary that makes it sets it and the frame loop
      // that says a build ended unheld reads it, so a build is held once at most and is never
      // said to have ended unheld once it was.
      let held = false;
      const pace = (): Promise<void> => {
        const { promise, resolve } = Promise.withResolvers<void>();
        if (arg.eachBoundary === "a frame") {
          requestAnimationFrame(() => setTimeout(resolve, 0));
        } else {
          setTimeout(resolve, 0);
        }
        return promise;
      };
      window.__dispatchHoldBuild = () => {
        const root = document.querySelector(arg.live);
        const say = window.__theBuildHeld;
        if (!arg.holding || held || root === null || say === undefined) {
          return pace();
        }
        const text = root.querySelector('[contenteditable="true"]')?.textContent ?? "";
        if (!arg.texts.every((quote) => text.includes(quote))) {
          return pace();
        }
        held = true;
        return say({ blocks: root.querySelectorAll("[data-block-id]").length });
      };
      if (!arg.holding) return;
      const look = () => {
        if (held) return;
        if (document.querySelector(arg.named) !== null) {
          void window.__theBuildHeld?.(null);
          return;
        }
        requestAnimationFrame(look);
      };
      requestAnimationFrame(look);
    },
    {
      eachBoundary: options.eachBoundary ?? "a timer",
      holding: options.at !== undefined,
      live: liveEditorRoot,
      named: namedEditor,
      texts,
    }
  );
  const where = `the editor holding every one of ${JSON.stringify(texts)}`;
  return {
    held: async (timeoutMs) => {
      const at = await theMomentThePageReached(
        said.promise,
        timeoutMs,
        `the build was never held: no boundary between two of its batches came with ${where}`
      );
      if (at === null) {
        throw new Error(
          "the build ended without being held: the editor was named before any boundary " +
            `between two of its batches came with ${where}`
        );
      }
      return at;
    },
    release: () => released.resolve(),
  };
}
