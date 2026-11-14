// The Legion view's settings panel: the daemon's own `legion.yaml`, as it reports it, with where
// each value came from — its configuration file, or a change someone made here, with who made it
// and when. Dispatch models none of the values: they ride from the daemon as JSON and go back as
// JSON (`GET /api/v1/projects/{key}/legion/board`'s `settings` and `setting_sources`).
//
// A change goes to `PUT /api/v1/legion/{project}/settings`, which relays it to the daemon with the
// pod's projected ServiceAccount token and the caller's login, and answers the daemon's own
// verdict for each key: applied at once, by rebuilding the client or listener that held it, or at
// the next worker launch — refused, with the daemon's reason, or failed, the daemon running on the
// new value with the holder of it keeping the old one. Dispatch re-reads none of that, so neither
// does this panel: it shows the verdict it was given, and shows a refusal and a failure as
// failures (`describeVerdict`).
//
// Who may change one: `scopeDeployment` refuses a caller whose credential names one project, which
// is the only role Dispatch has, so the control is offered to any signed-in person (`useIsHuman`)
// and the server refuses the rest — the way `GrantsSection` and the Delivery settings gate. A
// refusal lands on the row it was made from.
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useMemo, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { legionBoardQuery, whoAmIQuery } from "../../api/queries";
import type {
  LegionBoardResponse,
  LegionSettingSource,
  LegionSettingSources,
  LegionSettingsChangeResult,
  LegionSettingVerdict,
} from "../../api/types";
import { PanelTrigger } from "../../components/PanelTrigger";
import {
  TableBody,
  TableCell,
  TableHead,
  TableHeadCell,
  TableRow,
  TableShell,
} from "../../components/TableShell";
import { useIsHuman } from "../../hooks/useIsHuman";
import {
  borderDefault,
  card,
  dangerText,
  successText,
  surfaceMutedHoverBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { settingsMonoInput, settingsSubmitButton } from "../settings/classes";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { describeSettingSource, describeVerdict, formatSettingValue } from "./lib/format";

/** What one key of a change ended as, on the row it was made from: the daemon's verdict, that
 *  verdict where the daemon applied the change and could not record it, a refusal Dispatch
 *  itself made (no daemon, the daemon unreachable, a credential naming one project), or an
 *  answer Dispatch could not keep. */
type RowResult =
  | { readonly kind: "verdict"; readonly verdict: LegionSettingVerdict }
  | { readonly kind: "unstored"; readonly verdict: LegionSettingVerdict }
  | { readonly kind: "failed"; readonly message: string }
  | { readonly kind: "unanswered" };

/** One key's change: the dotted `legion.yaml` path and the value the daemon is asked to take. */
interface SettingChange {
  readonly key: string;
  readonly value: unknown;
}

/** `entries` with `key` taking `value`, or without it where `value` is undefined: a key the
 *  daemon reports no value for is one the panel must not invent one for. */
function withEntry<V>(
  entries: Readonly<Record<string, V>>,
  key: string,
  value: V | undefined
): Readonly<Record<string, V>> {
  const { [key]: _dropped, ...rest } = entries;
  return value === undefined ? rest : { ...rest, [key]: value };
}

/** The board as it stands with `key` taking `value` from this person, now: what the panel shows
 *  until the next read of the daemon confirms it. The daemon has already said it applied the
 *  change, and Dispatch learns the daemon's own `settingSources` only at its next pass. */
function withChangedSetting(
  board: LegionBoardResponse,
  key: string,
  value: unknown,
  by: string | null
): LegionBoardResponse {
  const source: LegionSettingSource = { source: "route", by, at: new Date().toISOString() };
  return {
    ...board,
    settings: { ...board.settings, [key]: value },
    setting_sources: { ...board.setting_sources, [key]: source },
  };
}

/** The board with `key` back as `previous` showed it, and every other field as it stands now: a
 *  change that was refused, or that failed, puts back the one key it touched and not a board a
 *  live delta has moved on since. */
function withSettingAsItWas(
  board: LegionBoardResponse,
  key: string,
  previous: LegionBoardResponse | undefined
): LegionBoardResponse {
  return {
    ...board,
    settings: withEntry(board.settings ?? {}, key, previous?.settings?.[key]),
    setting_sources: withEntry(board.setting_sources ?? {}, key, previous?.setting_sources?.[key]),
  };
}

/** The daemon's settings, each with its value and where that value came from, and — for a signed
 *  in person — a control that changes it. `board` is the project's own board answer: its
 *  `project` is whose daemon these are, and its `settings` and `setting_sources` are the two
 *  blocks an applied change writes back into through the board's cache entry. */
export function LegionSettingsPanel({ board }: { readonly board: LegionBoardResponse }): ReactNode {
  const { project } = board;
  // The board's two blocks as the daemon sends them. The server keeps the bytes unparsed
  // (`map[string]json.RawMessage`), so the document says the maps hold `unknown` and may be
  // null; `LegionSettingSources` says what a source is (`@legion/contracts`), narrowed here at
  // the one boundary that reads them.
  const settings = board.settings ?? {};
  const sources = (board.setting_sources ?? {}) as LegionSettingSources;
  const headingId = useId();
  const queryClient = useQueryClient();
  const human = useIsHuman();
  const viewer = useQuery(whoAmIQuery()).data?.login ?? null;
  const boardKey = legionBoardQuery({ kind: "legion", project }).queryKey;
  // The value each row's input holds while it is being edited; a row nobody has typed in shows
  // the daemon's own value.
  const [drafts, setDrafts] = useState<Readonly<Record<string, string>>>({});
  const [results, setResults] = useState<Readonly<Record<string, RowResult>>>({});
  const [hintOpen, setHintOpen] = useState(false);
  /** Puts `key` back as `previous` showed it: a change refused or failed puts back the one key it
   *  touched, leaving every other field as the live subscription has it now. */
  const rollBack = (key: string, previous: LegionBoardResponse | undefined) => {
    queryClient.setQueryData<LegionBoardResponse>(boardKey, (board) =>
      board === undefined ? board : withSettingAsItWas(board, key, previous)
    );
  };

  const change = useKeyedWrite<
    LegionSettingsChangeResult,
    SettingChange,
    { readonly previous: LegionBoardResponse | undefined }
  >({
    write: ({ key, value }, bounds) =>
      api.putLegionProjectSettings(project, { [key]: value }, bounds),
    inline: true,
    label: ({ key }) => `${key} changed on ${project}'s Legion daemon`,
    onError: (error, variables, context) => {
      rollBack(variables.key, context?.previous);
      setResults((current) => ({
        ...current,
        [variables.key]: {
          kind: "failed",
          message: apiErrorMessage(error, `Couldn't change ${variables.key}.`),
        },
      }));
    },
    onMutate: async (variables) => {
      await queryClient.cancelQueries({ queryKey: boardKey });
      const previous = queryClient.getQueryData<LegionBoardResponse>(boardKey);
      if (previous !== undefined) {
        queryClient.setQueryData(
          boardKey,
          withChangedSetting(previous, variables.key, variables.value, viewer)
        );
      }
      setResults((current) => withEntry(current, variables.key, undefined));
      return { previous };
    },
    // A landing Dispatch could not keep the answer of leaves the optimistic row: the daemon may
    // well have applied it, and the next read of the daemon is what settles it.
    onSuccess: (answer, variables, context) => {
      if (answer === null) {
        setResults((current) => ({ ...current, [variables.key]: { kind: "unanswered" } }));
        return;
      }
      // The daemon keys its results by path under `settings`, and Dispatch relays that body
      // unchanged (`LegionSettingsChangeResponse` in @legion/contracts/legion-api): a daemon
      // that answers nothing for the key leaves an empty verdict, which reads as unanswered.
      const verdict = answer.settings[variables.key] ?? {};
      // A change the daemon applied and could not record is a landing, not a failure: the
      // daemon is running on the new value, so the row stands as it was changed and says what
      // the restart will undo (`putLegionProjectSettings`, `recorded`).
      if (!answer.recorded) {
        setResults((current) => ({ ...current, [variables.key]: { kind: "unstored", verdict } }));
        setDrafts((current) => withEntry(current, variables.key, undefined));
        return;
      }
      // A key the daemon refused never changed, so the row goes back to the value it showed. A
      // key it failed on did change — the daemon runs on the new value and only the holder of it
      // kept the old one — so that row stands as it was changed, with the failure beside it.
      if (verdict.refused !== undefined) {
        rollBack(variables.key, context?.previous);
      }
      setResults((current) => ({ ...current, [variables.key]: { kind: "verdict", verdict } }));
      setDrafts((current) => withEntry(current, variables.key, undefined));
    },
    // No read follows: the board is live (`api/live-socket.ts`), so the daemon's next pass
    // replaces `settings` and `setting_sources` whole on this same subscription. Invalidating a
    // live query here would drop the subscription's own answer for one nothing re-reads.
  });

  // Every settable path, the ones the daemon withholds a value for included: a credential's
  // provenance is reported with no value anywhere, which is how a rotation is visible. Built
  // once per board rather than on every keystroke in a row's input.
  const keys = useMemo(
    () => [...new Set([...Object.keys(settings), ...Object.keys(sources)])].sort(),
    [settings, sources]
  );

  const submit = (event: FormEvent<HTMLFormElement>, key: string) => {
    event.preventDefault();
    const typed = drafts[key] ?? formatSettingValue(settings[key]);
    let value: unknown;
    try {
      value = JSON.parse(typed);
    } catch {
      setResults((current) => ({
        ...current,
        [key]: {
          kind: "failed",
          message: 'That is not JSON. A setting\'s value is written as JSON: 4, "legion", ["a"].',
        },
      }));
      return;
    }
    change.mutate({ key, value });
  };

  return (
    <section aria-labelledby={headingId} className="mt-6" data-legion-settings>
      {/* The help text sits behind an (i) hint beside the heading, as the Issues tab's does. */}
      <div className="flex items-center gap-2">
        <h3 className={`text-base font-semibold ${textPrimaryOnCanvas}`} id={headingId}>
          Daemon settings
        </h3>
        <PanelTrigger
          label="i"
          onOpenChange={setHintOpen}
          open={hintOpen}
          panelClassName={`z-50 flex w-72 max-w-[calc(100vw-16px)] flex-col gap-1.5 rounded-lg border p-3 text-xs font-normal shadow-lg ${card} ${textSecondaryOnCanvas}`}
          title="About the daemon settings"
          triggerClassName={`inline-flex size-9 shrink-0 items-center justify-center rounded-full border text-xs font-semibold italic md:size-7 ${borderDefault} ${surfaceMutedHoverBg} ${textSecondaryOnCanvas}`}
        >
          {() => (
            <>
              <p>
                The <code>legion.yaml</code> of {project}, as its daemon sends it
              </p>
              <p>Where each value came from: the file, or a change made here</p>
              <p>A change goes to the daemon at once, and the daemon tells how it used it</p>
              {human ? null : <p>Sign in to change a setting</p>}
            </>
          )}
        </PanelTrigger>
      </div>
      {keys.length === 0 ? (
        <p className={`mt-4 text-sm ${textMutedOnCanvas}`} data-legion-settings-empty>
          No read of this daemon has recorded its settings yet.
        </p>
      ) : (
        <TableShell className="mt-6" label="Daemon settings">
          <TableHead>
            <TableHeadCell>Setting</TableHeadCell>
            <TableHeadCell>Value</TableHeadCell>
            <TableHeadCell>Where it came from</TableHeadCell>
            {human ? <TableHeadCell>Change it</TableHeadCell> : null}
          </TableHead>
          <TableBody>
            {keys.map((key) => {
              const value = formatSettingValue(settings[key]);
              const source = sources[key];
              const result = results[key];
              return (
                <TableRow data-legion-setting={key} key={key}>
                  <TableHeadCell scope="row">
                    <span
                      className={`inline-block min-w-32 break-all font-mono font-normal ${textPrimaryOnSurface}`}
                    >
                      {key}
                    </span>
                  </TableHeadCell>
                  <TableCell data-legion-setting-value={key} mono tone="primary">
                    {/* A value is JSON of any length: it wraps, so the columns after it stay in
                        the box. */}
                    <span className="inline-block min-w-28 max-w-80 break-all md:min-w-48">
                      {value}
                    </span>
                  </TableCell>
                  <TableCell
                    data-legion-setting-source={source?.source ?? "unknown"}
                    tone="secondary"
                  >
                    {describeSettingSource(source)}
                  </TableCell>
                  {human ? (
                    <TableCell>
                      <form
                        className="flex items-center gap-2 whitespace-nowrap"
                        onSubmit={(event) => submit(event, key)}
                      >
                        <input
                          aria-label={`New value for ${key}`}
                          className={`${settingsMonoInput} w-40 text-sm`}
                          onChange={(event) =>
                            setDrafts((current) => ({ ...current, [key]: event.target.value }))
                          }
                          value={drafts[key] ?? value}
                        />
                        <button
                          className={`${settingsSubmitButton} text-sm`}
                          disabled={change.isPending}
                          type="submit"
                        >
                          {/* The row names the setting already; its name stays in the button's
                              accessible name, so the column stays narrow. */}
                          Change<span className="sr-only"> {key}</span>
                        </button>
                        {result === undefined ? null : (
                          <span data-legion-setting-result={key}>
                            <RowAnswer result={result} />
                          </span>
                        )}
                      </form>
                    </TableCell>
                  ) : null}
                </TableRow>
              );
            })}
          </TableBody>
        </TableShell>
      )}
    </section>
  );
}

/** What the daemon, or Dispatch, said about this row's change. */
function RowAnswer({ result }: { readonly result: RowResult }): ReactNode {
  if (result.kind === "failed") {
    return (
      <span className={`text-xs ${dangerText}`} role="alert">
        {result.message}
      </span>
    );
  }
  if (result.kind === "unanswered") {
    return (
      <span className={`text-xs ${textSecondaryOnSurface}`} role="status">
        Dispatch did not keep the daemon&apos;s answer; the next read of the daemon shows what it
        did.
      </span>
    );
  }
  // A verdict the daemon could not record is still the daemon's verdict: a refusal or a failure
  // in it is shown as one, with the note that a restart puts the value back.
  if (result.kind === "unstored") {
    const { failure, text } = describeVerdict(result.verdict);
    return (
      <span
        className={`text-xs ${failure ? dangerText : textSecondaryOnSurface}`}
        role={failure ? "alert" : "status"}
      >
        {text} The daemon is running on the new value but could not record it, so a restart puts
        back the value it had before.
      </span>
    );
  }
  const { failure, text } = describeVerdict(result.verdict);
  return (
    <span
      className={`text-xs ${failure ? dangerText : successText}`}
      role={failure ? "alert" : "status"}
    >
      {text}
    </span>
  );
}
