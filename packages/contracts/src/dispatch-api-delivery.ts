import type {
  LegionBoardColumn,
  LegionFiledIssue,
  LegionMetricsResponse,
  LegionRoleBar,
} from "./dispatch-api.generated";

/**
 * The delivery timeline's facets and the Legion section's names. Every body `GET
 * /api/v1/delivery/*` and `GET /api/v1/legion/*` answers is generated from the Go route table, so
 * what stays here is the two closed sets a page selects and colours by, which the table carries as
 * query parameter names rather than as a schema, so no generated type spells them; what one map's
 * values hold where the server keeps them unparsed, so the document can only say they are bytes
 * (`LegionSettingSource`); and the names a Legion page reads off a generated shape.
 *
 * A name read off a generated shape is none of those: `LegionMetricsScope` and its kind are
 * `LegionMetricsResponse["scope"]` and so on, so the set a field carries is stated once, by the
 * Go field's `enum` tag, and nothing here can drift from it.
 */

/** The timeline's facets, as the query parameters that select them and the `facet_counts` keys
 *  that count them. */
export type DeliveryFacet =
  | "repo"
  | "parent_agent"
  | "session"
  | "issue"
  | "priority"
  | "component"
  | "author"
  | "rework"
  | "deployed";

/** The facets the page colours merges by, as the `color_counts` keys that count them. */
export type DeliveryColorFacet = "repo" | "author" | "priority" | "component" | "parent_agent";

/** Where one of the daemon's settings got its value: its `legion.yaml` file or the settings route,
 *  who made the change — null for a change the file carried, which nobody signed — and when. The
 *  key it is held under is the setting's dotted `legion.yaml` path.
 *
 *  Hand-written because the server holds these bytes unparsed: the board keeps `setting_sources`
 *  as a `map[string]json.RawMessage`, the daemon owning the shape, so the document can only say
 *  the map's values are bytes — the case `InboxDismissSeen` is in. This is what each one is. */
export interface LegionSettingSource {
  readonly source: "file" | "route";
  readonly by: string | null;
  readonly at: string;
}

/** `LegionBoardResponse.setting_sources` read as the shapes the daemon sends rather than as the
 *  bytes the document declares, which is what the settings panel narrows the map to once. */
export type LegionSettingSources = Readonly<Record<string, LegionSettingSource>>;

/** One column of the Legion board (`GET /api/v1/legion/board`), by its id: the daemon's waiting
 *  line and the issues a blocker link holds, the roots holding a slot whose phase is still
 *  `admitted`, the six phases a role works, `merging` (`retro`, `merging` and `awaiting_merge`),
 *  `awaiting_person` and `held`. Every answer carries all ten, in this order. Read off the
 *  generated column, whose `id` the Go field's `enum` narrows (`legionBoardColumn`). */
export type LegionBoardColumnID = LegionBoardColumn["id"];

/** Which of a project's Legion issues an answer counts: the Legion section's own toggle. */
export type LegionMetricsScope = LegionMetricsResponse["scope"];

/** The bar of metric 2 an hour falls on: one of Legion's six worker types, or the seventh bar for
 *  the phases no role works (`metrics.bars`). */
export type LegionBarRole = LegionRoleBar["role"];

/** What a filing of metric 4 is: filed by a Legion session of the filer's own tree at design or
 *  after it, filed out of that tree, or filed by the project's controller, which works under no
 *  issue (`metrics.classes`, in the order the card lists them). */
export type LegionFilingClass = LegionFiledIssue["class"];

/** Who filed an issue of metric 4: one of Legion's six worker types, or the project's controller,
 *  which works no issue. It is the role the filing session's title names
 *  (`legion.SessionTitle.Role`, `metrics.FiledIssue.Filer`). */
export type LegionFiler = LegionFiledIssue["filer"];

/** The word a table of the Legion section names a phase by (`metrics.tablePhase`), which is the
 *  bar the phase falls on. The seven bar words are `design`, `planning`, `implementing`,
 *  `testing`, `reviewing`, `accepting` and `merging`, the last of which holds the daemon's
 *  `retro`, `merging` and `awaiting_merge` (`legion.MergerPhases`). Three more words a timeline
 *  holds are on no role's bar and pass through as they are: `queued`, the waiting line, and
 *  `awaiting_person` and `held`, the "no role" bar's. `done` is what an issue the daemon has
 *  finished and Dispatch has not closed yet holds, and `""` is a moment the issue's timeline does
 *  not cover, which is what every filing by the controller carries. */
export type LegionTablePhase = LegionFiledIssue["filer_phase"];
