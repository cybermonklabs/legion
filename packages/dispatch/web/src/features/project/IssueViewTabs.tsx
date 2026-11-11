import type { ReactNode } from "react";
import { useMemo } from "react";
import { useSearchParams } from "react-router-dom";
import { Tabs } from "../../components/Tabs";
import { readUrl, viewIsChanged } from "./issues-table-model";
import { activeSavedView, type SavedView } from "./issues-table-saved-views";
import { BUILT_IN_VIEWS, type BuiltInView, type BuiltInViewId, viewOf } from "./issues-table-views";

export interface IssueViewTabsProps {
  readonly project: string;
  readonly hasLegion: boolean;
  readonly counts: Partial<Record<BuiltInViewId, number | string>>;
  readonly saved: readonly SavedView[];
}

function applicableView(params: URLSearchParams, hasLegion: boolean): BuiltInView {
  const view = viewOf(params);
  return view.legionOnly && !hasLegion ? viewOf(new URLSearchParams()) : view;
}

export function IssueViewTabs({ counts, hasLegion, saved }: IssueViewTabsProps): ReactNode {
  const [params, setParams] = useSearchParams();
  const builtIn = useMemo(
    () => BUILT_IN_VIEWS.filter((view) => hasLegion || !view.legionOnly),
    [hasLegion]
  );
  const view = applicableView(params, hasLegion);
  const activeSaved = activeSavedView(saved, params);
  const savedIndex = activeSaved === undefined ? -1 : saved.indexOf(activeSaved);
  const activeTab = savedIndex === -1 ? view.id : `saved-${savedIndex}`;
  const marked =
    activeSaved === undefined && viewIsChanged(params, readUrl(params, hasLegion), hasLegion);
  const tabs = [
    ...builtIn.map((builtInView) => ({
      count: counts[builtInView.id],
      id: builtInView.id,
      label: builtInView.name,
      marked: marked && builtInView.id === view.id,
    })),
    ...saved.map((savedView, index) => ({
      id: `saved-${index}`,
      label: savedView.name,
    })),
  ];

  return (
    <Tabs
      activeTab={activeTab}
      ariaLabel="Issue views"
      compact
      idPrefix="view"
      onSelect={(id) => {
        const savedView = saved[Number(id.slice("saved-".length))];
        if (id.startsWith("saved-") && savedView !== undefined) {
          setParams(new URLSearchParams(savedView.query));
          return;
        }
        const next = builtIn.find((candidate) => candidate.id === id);
        if (next === undefined) return;
        setParams(next.params(hasLegion));
      }}
      tabs={tabs}
    />
  );
}
