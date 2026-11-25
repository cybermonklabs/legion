/**
 * Whether the tab regained focus before the deployment watch was listening.
 *
 * The watch offers a reload once another build is live, from a check the tab's `focus` starts
 * (`features/shell/DeploymentResilience.tsx`). The entry asks for it with `import()` beside the
 * app (`TheDeploymentWatch` in `main.tsx`, dispatch://INFRA-341, R11), so between the page's
 * `load` and the watch's first commit there is a window in which the watch's own `focus`
 * listener is not bound. A focus in that window reaches none of the app's listeners, so without
 * this record no check would start and no new deployment would be offered until the tab is
 * focused again.
 *
 * So the entry listens from its own task and holds whether it heard a focus until the watch binds
 * its own listener, which then runs the check that focus asked for. The focus is answered late
 * rather than lost.
 *
 * What it holds is one fact, not the events: one check answers any number of focuses, as the
 * watch's own throttle already decides, and a check that runs late reads which builds are live
 * now, which is what the reader needs. A page that heard no focus asks for no check when the
 * watch binds. And it holds nothing once the watch has bound, so a live focus is the watch's
 * alone.
 */

let focused = false;
let listening: (() => void) | undefined;

/** Start holding whether the tab regains focus. Called from the entry chunk, before the watch is
 *  asked for. */
export function rememberEarlyFocus(): void {
  if (listening !== undefined) {
    return;
  }
  const onFocus = () => {
    focused = true;
  };
  listening = onFocus;
  window.addEventListener("focus", onFocus);
}

/**
 * Say whether the tab regained focus while the entry held it, and stop holding. Called by
 * `DeploymentResilience` the moment its own listener is bound, so a focus that came before it
 * starts the same check a live one does.
 */
export function takeEarlyFocus(): boolean {
  if (listening !== undefined) {
    window.removeEventListener("focus", listening);
    listening = undefined;
  }
  const heard = focused;
  focused = false;
  return heard;
}
