import { useQueries, useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";
import { Link } from "react-router-dom";

import {
  type EvalKind,
  evalsViewQuery,
  legionDaemonQuery,
  projectDeliverySettingsQuery,
} from "../../api/queries";
import type { EvalListResponse } from "../../api/types";
import { linkText, textMutedOnCanvas } from "../../theme/classes";
import { CiHealth } from "./CiHealth";
import { DeploysSection } from "./DeploysSection";
import { EvalPanelHost } from "./EvalPanel";
import { EvalsHeader } from "./EvalsHeader";
import { EvalsSection } from "./EvalsSection";
import { keepLogged, LoggedEvals, UnnamedEvals } from "./EveryEval";
import { type EvalsUrlState, useEvalsUrlState } from "./evals-url";
import { LegionSection } from "./LegionSection";
import { TargetCards } from "./TargetCards";
import { TimelineSection } from "./TimelineSection";
import { narrowingQuery, useSettledSearch, useTimelineUrlState } from "./timeline-url";
import { brushDays, useTabScopes } from "./useEvalsScope";
import type { EvalsScope, EvalsViewSelect, EvalsViewState } from "./useEvalsViews";

/** One section of the tab: what it is, its heading, the kinds of eval it reads, what it keeps
 *  of each page of their views (all of it where absent), and whether no eval it draws is narrowed
 *  by the facets (plan D3). */
interface Section {
  readonly id: "cards" | "legion" | "ci" | "deploys" | "timeline" | "logged";
  readonly title: string;
  readonly kinds: readonly EvalKind[];
  readonly select?: EvalsViewSelect;
  readonly windowOnly?: boolean;
}

/** The tab's sections, in the order dispatch://INFRA-2918/spec R2 sets: the target cards, the
 *  Legion measures, CI health, deploys and lead time, the merge timeline, and then every eval the
 *  project's agents and jobs log. Each reads the views of its kinds: the target cards the delivery
 *  and the Legion evals (plan D3), the Deploys and lead time section the same delivery view as the
 *  cards, the merge timeline none, since it reads a view of its own once it is opened (spec R13),
 *  and the Logged section every view the tab reads, since a logged eval may be of any kind (plan
 *  D7). No facet narrows a Legion or a CI eval (plan D7a). */
const SECTIONS: readonly Section[] = [
  { id: "cards", title: "Target cards", kinds: ["delivery", "legion"] },
  { id: "legion", title: "Legion measures", kinds: ["legion"], windowOnly: true },
  { id: "ci", title: "CI health", kinds: ["ci"], windowOnly: true },
  { id: "deploys", title: "Deploys and lead time", kinds: ["delivery"] },
  { id: "timeline", title: "Merge timeline", kinds: [] },
  {
    id: "logged",
    title: "Logged",
    kinds: ["delivery", "legion", "ci", "experiment"],
    select: keepLogged,
  },
];

/** The sections a project's sources show (spec R5, R19): the Legion evals only where it has a
 *  Legion daemon, and the delivery and CI evals only where its delivery is on, each section with
 *  the views of the kinds it shows and none with no kind left, but the merge timeline, which reads
 *  no evals view. */
function sectionsOf(legion: boolean, delivery: boolean): Section[] {
  const shown = (kind: EvalKind) =>
    kind === "experiment" || (kind === "legion" ? legion : delivery);
  return SECTIONS.flatMap((section) => {
    const kinds = section.kinds.filter(shown);
    return section.id === "timeline" || kinds.length > 0 ? [{ ...section, kinds }] : [];
  });
}

/** The line the header shows for a project with no delivery settings (spec R5, plan D3):
 *  "Delivery: not set up", linking to the project's settings, where its delivery is set up
 *  (dispatch://INFRA-978); or, where every source is missing (`everySource`: no Legion daemon, and
 *  no settings row or delivery off), one line naming both settings, each linked, the daemon's on
 *  the Settings page. */
function MissingSources({
  everySource,
  project,
}: {
  everySource: boolean;
  project: string;
}): ReactNode {
  const settings = `/projects/${project}/settings`;
  return (
    <p className={`text-sm ${textMutedOnCanvas}`} data-missing-sources>
      {everySource ? (
        <>
          Nothing to show yet: set this project's{" "}
          <Link className={linkText} to="/settings">
            Legion daemon
          </Link>{" "}
          or its{" "}
          <Link className={linkText} to={settings}>
            delivery settings
          </Link>
          .
        </>
      ) : (
        <Link className={linkText} to={settings}>
          Delivery: not set up
        </Link>
      )}
    </p>
  );
}

/** The window and compare range of an answer, which are the same in every view of one scope, and
 *  the project's delivery freshness, which only the delivery view answers (null in every other
 *  view, and for a project with no deploy source). */
function headOf(
  answer: EvalListResponse
): Pick<EvalListResponse, "window" | "compare" | "delivery_freshness"> {
  return {
    compare: answer.compare,
    delivery_freshness: answer.delivery_freshness,
    window: answer.window,
  };
}

/** The header, naming the days of the first answer any of the tab's views gives, and the
 *  freshness line of the delivery view's `delivery_freshness`, read at that view's last answer
 *  (plan D2). It reads only each view's window, compare range and delivery freshness, and the
 *  delivery view's answer time, so a delta of another view that moves none of them renders no
 *  header. */
function LiveHeader({
  kinds,
  onChange,
  scope,
  state,
}: {
  kinds: readonly EvalKind[];
  onChange: (next: EvalsUrlState) => void;
  scope: EvalsScope;
  state: EvalsUrlState;
}): ReactNode {
  const heads = useQueries({
    queries: kinds.map((kind) => ({
      ...evalsViewQuery({ ...scope, kind, offset: 0 }),
      select: headOf,
    })),
  });
  const first = heads.find((view) => view.data !== undefined)?.data;
  const delivery = heads[kinds.indexOf("delivery")];
  const freshness = delivery?.data?.delivery_freshness;
  return (
    <EvalsHeader
      compare={first?.compare}
      freshness={
        delivery === undefined || freshness === undefined || freshness === null
          ? undefined
          : { freshness, readAtMs: delivery.dataUpdatedAt }
      }
      onChange={onChange}
      state={state}
      window={first?.window}
    />
  );
}

/** What a section draws of its views: its own charts and lists, then, for a kind's section, the
 *  computed evals of that kind it does not draw by name; the Logged section, every logged eval.
 *  The target cards name the brushed days they read, while the merge timeline's brush is set
 *  (`days`). */
function drawSection(
  id: Exclude<Section["id"], "timeline">,
  project: string,
  scope: EvalsScope,
  delivery: boolean,
  days: { from: string; to: string } | null
): (views: readonly EvalsViewState[]) => ReactNode {
  switch (id) {
    case "cards":
      return (views) => (
        <TargetCards days={days} delivery={delivery} project={project} views={views} />
      );
    case "legion":
      return (views) => (
        <>
          <LegionSection project={project} repos={scope.repos} views={views} />
          <UnnamedEvals kind="legion" scope={scope} views={views} />
        </>
      );
    case "ci":
      return (views) => (
        <>
          <CiHealth scope={scope} views={views} />
          <UnnamedEvals kind="ci" scope={scope} views={views} />
        </>
      );
    case "deploys":
      return (views) => (
        <>
          <DeploysSection project={project} views={views} />
          <UnnamedEvals kind="delivery" scope={scope} views={views} />
        </>
      );
    case "logged":
      return (views) => <LoggedEvals scope={scope} views={views} />;
  }
}

/**
 * A project's Evals tab (`/projects/<KEY>/evals`, dispatch://INFRA-2918/spec R1, R2, R3, R5): its
 * one page of numbers, every project's alike, under one header. It is a subscriber: it opens one
 * evals view per kind it shows, all at once, and each section paints from its own as it lands,
 * never waiting for another's (plan D3). In parallel it reads the project's Legion daemon, which
 * decides whether the Legion section and cards show, and its delivery settings, which decide
 * whether the delivery and CI sections, cards and targets show: with no settings row or delivery
 * off they do not, though Dispatch's eval reads still hold those evals, and with no row the header
 * says delivery is not set up; with no daemon either, every source is missing, and the tab shows
 * one line naming both settings and no section.
 * Until each read answers its sections show, so their views are opened with the others rather
 * than after them. The merge timeline opens no view until the reader opens it (spec R13); its
 * brush sets the target cards' days and its Repository facet the repository every number reads
 * (decision 3). Its other facets and its search, which the URL holds whether it is open or not,
 * narrow the delivery view the cards and the Deploys and lead time section read (D7a); the first
 * paint is never narrowed, so a tab opened with one set paints from the unnarrowed delivery view
 * and draws the narrowed one once it lands (`useTabScopes`). A card's or a chart's title opens the
 * number's panel (plan D12).
 *
 * The compare range a preset's default names moves at each UTC midnight the tab stays open past,
 * with the window the server moves (`useEvalsScope`).
 */
export function EvalsTab({ project }: { project: string }): ReactNode {
  const [state, setState] = useEvalsUrlState();
  const [timeline] = useTimelineUrlState();
  const search = useSettledSearch(timeline.filters);
  const daemon = useQuery(legionDaemonQuery(project));
  const settings = useQuery(projectDeliverySettingsQuery(project));
  const legion = daemon.data !== null;
  // Off for no settings row (null) and for delivery off, which is also how a row with no deploy
  // source reads, since Dispatch saves delivery on only with one; on until the read answers.
  const delivery = settings.data === undefined || settings.data?.delivery === true;
  const everySourceMissing = daemon.data === null && !delivery;
  const { cards: cardsScope, scope } = useTabScopes(
    project,
    state,
    narrowingQuery(timeline.filters, search)
  );
  const days = useMemo(() => brushDays(state.brush), [state.brush]);
  const sections = everySourceMissing ? [] : sectionsOf(legion, delivery);
  const kinds = [...new Set(sections.flatMap((section) => section.kinds))];
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-col gap-2">
        <LiveHeader kinds={kinds} onChange={setState} scope={scope} state={state} />
        {everySourceMissing || settings.data === null ? (
          <MissingSources everySource={everySourceMissing} project={project} />
        ) : null}
      </div>
      <EvalPanelHost kinds={kinds} scope={scope}>
        {sections.map((section) =>
          section.id === "timeline" ? (
            <TimelineSection
              brush={state.brush}
              key={section.id}
              onBrush={(brush) => setState({ ...state, brush })}
              project={project}
              scope={scope}
            />
          ) : (
            <EvalsSection
              id={section.id}
              key={section.id}
              kinds={section.kinds}
              scope={section.id === "cards" ? cardsScope : scope}
              select={section.select}
              title={section.title}
              windowOnly={section.windowOnly}
            >
              {drawSection(section.id, project, scope, delivery, days)}
            </EvalsSection>
          )
        )}
      </EvalPanelHost>
    </div>
  );
}
