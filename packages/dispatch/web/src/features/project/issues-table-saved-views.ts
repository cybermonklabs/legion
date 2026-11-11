export interface SavedView {
  readonly name: string;
  readonly query: string;
}

export function readSavedViews(stored: string | null): Record<string, SavedView[]> {
  try {
    const parsed: unknown = JSON.parse(stored ?? "{}");
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) return {};
    const views = Object.entries(parsed).map(([project, saved]) => {
      if (
        !Array.isArray(saved) ||
        !saved.every(
          (view): view is SavedView =>
            view !== null &&
            typeof view === "object" &&
            typeof view.name === "string" &&
            view.name.length > 0 &&
            view.name.length <= 14 &&
            typeof view.query === "string"
        )
      )
        throw new Error("Invalid saved views");
      return [project, saved] as const;
    });
    return Object.fromEntries(views);
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
