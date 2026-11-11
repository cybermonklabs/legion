export interface SavedView {
  readonly name: string;
  readonly query: string;
}

function isSavedView(view: unknown): view is SavedView {
  if (view === null || typeof view !== "object" || Array.isArray(view)) return false;
  const saved = view as { readonly name?: unknown; readonly query?: unknown };
  return (
    typeof saved.name === "string" &&
    saved.name.length > 0 &&
    saved.name.length <= 14 &&
    typeof saved.query === "string"
  );
}

export function readSavedViews(stored: string | null): Record<string, SavedView[]> {
  try {
    const parsed: unknown = JSON.parse(stored ?? "{}");
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) return {};
    return Object.fromEntries(
      Object.entries(parsed).flatMap(([project, saved]) =>
        Array.isArray(saved) ? [[project, saved.filter(isSavedView)]] : []
      )
    );
  } catch {
    return {};
  }
}

export function writeSavedViews(views: Record<string, SavedView[]>): string {
  return JSON.stringify(views);
}

export function saveView(list: readonly SavedView[], view: SavedView): SavedView[] {
  const existing = list.findIndex((candidate) => candidate.name === view.name);
  if (existing === -1) return [...list, view];
  return list.map((candidate, index) => (index === existing ? view : candidate));
}

export function deleteView(list: readonly SavedView[], name: string): SavedView[] {
  return list.filter((view) => view.name !== name);
}

function queryWithoutPage(query: URLSearchParams): string {
  const entries = [...query.entries()].filter(([key]) => key !== "page");
  entries.sort(([leftKey, leftValue], [rightKey, rightValue]) =>
    leftKey === rightKey ? leftValue.localeCompare(rightValue) : leftKey.localeCompare(rightKey)
  );
  return new URLSearchParams(entries).toString();
}

export function activeSavedView(
  list: readonly SavedView[],
  params: URLSearchParams
): SavedView | undefined {
  const query = queryWithoutPage(params);
  return list.find((view) => queryWithoutPage(new URLSearchParams(view.query)) === query);
}
