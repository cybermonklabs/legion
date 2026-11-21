/** How the bar names the actions of a run. */

/** An action as the bar names it: the catalogue's name without its prefix. */
export function actionName(action: string): string {
  return action.replace(/^dispatch_/, "").replaceAll("_", " ");
}

/** An action and the item it acts on, as a plan row and an Undo line name them: `issue update
 *  CORE-3`, or the action alone where it names no item. */
export function actionLabel(action: string, item: string): string {
  return item === "" ? actionName(action) : `${actionName(action)} ${item}`;
}
