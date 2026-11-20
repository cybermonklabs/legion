// The Settings page's Legion section (dispatch://INFRA-1077): which project's state Dispatch
// reads from which daemon. Naming a project's daemon here is what makes Dispatch's sampler read
// that daemon's `GET /legion/v1/state` once a minute and record what changed, and removing one
// stops the reading while everything already read stays. No page draws what the sampler records:
// nothing in the dashboard reads the board it feeds (`GET /api/v1/projects/{key}/legion/board`).
//
// No route lists every project's daemon, so the section reads each project's
// `GET /api/v1/projects/{key}/legion/daemon` (`useLegionDaemons`), and a write changes the one
// entry of the project it names. The two writes are `PUT` and
// `DELETE /api/v1/projects/{key}/legion/daemon`, classed `scopeDeployment` beside the repository
// mappings and the Delivery settings: pointing Dispatch at a host is the deployment's business,
// not one project's. A credential naming one project is refused by the server, which is the only
// role Dispatch has, so the controls are offered to any signed-in person (`useIsHuman`) and the
// server refuses the rest; its refusal is shown under the form as it wrote it.
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { legionDaemonQuery } from "../../api/queries";
import type { LegionSetting } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useIsHuman } from "../../hooks/useIsHuman";
import {
  borderDefault,
  dangerHoverText,
  dangerText,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import {
  settingsFieldLabel,
  settingsMonoInput,
  settingsSubmitButton,
  settingsTableHead,
  settingsTableRow,
  settingsTableWrapper,
} from "./classes";
import { ProjectSelect } from "./ProjectSelect";
import { useLegionDaemons } from "./useLegionDaemons";

/** A daemon a person named: the project it serves and the base URL its state is read from. */
interface DaemonInput {
  readonly project: string;
  readonly state_url: string;
}

/** A write the server refused or never answered, or a URL the field itself refused, with what
 *  the page says about it: the server's own reason where it gave one, what a write the reader
 *  stopped may have done and what Retry does with it (`retry`), else a generic line. An `input`
 *  refusal is the field's, so no Retry is offered for it - the URL has to change first. */
type FailedAction = { readonly kind: "save" | "remove" | "input"; readonly message: string };

/** What a project's entry held before a write changed it: its daemon, `null` for none, or
 *  `undefined` where the entry was never read. */
type PreviousDaemon = { previous: LegionSetting | null | undefined };

/** The daemon base URL a setting may hold, trimmed, or the refusal that names what is wrong with
 *  it - the server's rule and the server's own words, so the field refuses what it would refuse
 *  and says the same thing (`legionStateURL`,
 *  packages/envoy/internal/dispatch/api/settings_legion.go). A URL the server parses differently
 *  is still its call: its 400 shows where this one would. */
export function legionStateUrl(raw: string): { readonly url: string; readonly refusal: string } {
  const trimmed = raw.trim().replace(/\/$/, "");
  if (trimmed === "") {
    return { refusal: "state_url is required", url: "" };
  }
  let parsed: URL;
  try {
    parsed = new URL(trimmed);
  } catch {
    return {
      refusal: "state_url must be an absolute http(s) URL with a host and no path",
      url: "",
    };
  }
  if (
    (parsed.protocol !== "http:" && parsed.protocol !== "https:") ||
    parsed.host === "" ||
    // A URL with no path parses here with `/` for one, since the trailing slash is trimmed above.
    (parsed.pathname !== "" && parsed.pathname !== "/")
  ) {
    return {
      refusal: "state_url must be an absolute http(s) URL with a host and no path",
      url: "",
    };
  }
  if (
    parsed.username !== "" ||
    parsed.password !== "" ||
    trimmed.includes("?") ||
    parsed.hash !== ""
  ) {
    return { refusal: "state_url must carry no user, password, query or fragment", url: "" };
  }
  return { refusal: "", url: trimmed };
}

/** Each project's Legion daemon, one row per project that names one, and - for a signed-in
 *  person - the field that names one and the Remove that stops reading it. */
export function LegionDaemonsSection(): ReactNode {
  const queryClient = useQueryClient();
  const human = useIsHuman();
  const [project, setProject] = useState("");
  const [stateUrl, setStateUrl] = useState("");
  const [failedAction, setFailedAction] = useState<FailedAction>();
  const legion = useLegionDaemons();
  const daemonKey = (named: string) => legionDaemonQuery(named).queryKey;
  // Both writes show their refusal under the form, with the row they changed put back.
  const saveDaemon = useKeyedWrite<LegionSetting, DaemonInput, PreviousDaemon>({
    method: "PUT",
    write: ({ project: named, state_url: url }, bounds) => api.putLegionDaemon(named, url, bounds),
    inline: true,
    label: ({ project: named }) => `${named}'s Legion daemon`,
    onError: (error, variables, context) => {
      queryClient.setQueryData(daemonKey(variables.project), context?.previous);
      setFailedAction({
        kind: "save",
        message:
          error instanceof SendStoppedError
            ? // Built inside `saveDaemon`'s own `onError`, where its `KeyedWriteResult` is not in
              // hand to read the method from; it is this write's own `PUT` (`useKeyedWrite` method).
              stoppedWriteNote(`the Legion daemon of ${variables.project}`, "saved", {
                method: "PUT",
                stopped: error,
              })
            : apiErrorMessage(error, `Couldn't save ${variables.project}'s Legion daemon.`),
      });
    },
    onMutate: async (variables) => {
      const key = daemonKey(variables.project);
      await queryClient.cancelQueries({ queryKey: key });
      const previous = queryClient.getQueryData(key);
      const optimistic: LegionSetting = {
        // As the server keeps it: whoever first named this project's daemon, else this person.
        created_by: previous?.created_by ?? { id: "you", kind: "user" },
        project: variables.project,
        state_url: variables.state_url,
        updated_at: new Date().toISOString(),
      };
      queryClient.setQueryData(key, optimistic);
      return { previous };
    },
    // A landing with no answer keeps the optimistic row; `onSettled` reads the project again.
    onSuccess: (setting) => {
      if (setting !== null) {
        queryClient.setQueryData(daemonKey(setting.project), setting);
      }
      setFailedAction(undefined);
      setProject("");
      setStateUrl("");
    },
    // The answer is the row as the server saved it when it answered; reading the project again
    // once the write settles leaves the page on the server's state, as the repository mappings do.
    onSettled: (_setting, _error, variables) =>
      queryClient.invalidateQueries({ queryKey: daemonKey(variables.project) }),
  });
  const removeDaemon = useKeyedWrite<void, string, PreviousDaemon>({
    method: "DELETE",
    write: (named: string, bounds) => api.deleteLegionDaemon(named, bounds),
    inline: true,
    label: (named) => `${named}'s Legion daemon removed`,
    onError: (error, named, context) => {
      queryClient.setQueryData(daemonKey(named), context?.previous);
      setFailedAction({
        kind: "remove",
        message:
          error instanceof SendStoppedError
            ? // Built inside `removeDaemon`'s own `onError`, where its `KeyedWriteResult` is not in
              // hand to read the method from; it is this write's own `DELETE` (`useKeyedWrite`).
              stoppedWriteNote(`the removal of ${named}'s Legion daemon`, "made", {
                method: "DELETE",
                stopped: error,
              })
            : apiErrorMessage(error, `Couldn't remove ${named}'s Legion daemon.`),
      });
    },
    onMutate: async (named) => {
      const key = daemonKey(named);
      await queryClient.cancelQueries({ queryKey: key });
      const previous = queryClient.getQueryData(key);
      queryClient.setQueryData(key, null);
      return { previous };
    },
    onSuccess: () => setFailedAction(undefined),
    onSettled: (_answer, _error, named) =>
      queryClient.invalidateQueries({ queryKey: daemonKey(named) }),
  });

  // The refusal on screen is the newest press of its write: a later press of either write that
  // lands lets it go, and Retry waits while either is out. So Retry is that write's own (`retry`),
  // which makes a press the reader stopped again as that press. Both are a `PUT` or a `DELETE`,
  // which sends no key, so that is the same change sent again. The field's own refusal of a URL
  // made no press, and offers no Retry.
  const retry = () => {
    if (failedAction?.kind === "save") {
      saveDaemon.retry();
      return;
    }
    if (failedAction?.kind === "remove") {
      removeDaemon.retry();
    }
  };
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const { refusal, url } = legionStateUrl(stateUrl);
    if (refusal !== "") {
      setFailedAction({ kind: "input", message: refusal });
      return;
    }
    saveDaemon.mutate({ project, state_url: url });
  };
  const writing = saveDaemon.isPending || removeDaemon.isPending;

  return (
    <section aria-labelledby="legion-daemons-heading" data-legion-daemons>
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id="legion-daemons-heading">
        Legion
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        Naming a project&apos;s Legion daemon here makes Dispatch read that daemon&apos;s state once
        a minute and record what changed. No page shows that record yet. The address is the
        daemon&apos;s base URL, with no path: Dispatch reads <code>/legion/v1/state</code> under it.
        Removing one stops the reading; what was read stays.
        {human ? "" : " Sign in to name one."}
      </p>

      {legion.isPending ? (
        <p className={`mt-6 ${textMutedOnCanvas}`}>Loading Legion daemons…</p>
      ) : null}
      {legion.isError ? (
        <div className="mt-6">
          <QueryError
            message="Couldn't load the Legion daemons."
            onRetry={legion.retry}
            retrying={legion.isFetching}
          />
        </div>
      ) : null}
      {legion.daemons !== undefined && legion.projects !== undefined ? (
        <>
          <div className={settingsTableWrapper}>
            <table className="w-full text-left text-sm">
              <thead className={settingsTableHead}>
                <tr>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Project
                  </th>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Daemon state URL
                  </th>
                  <th className="px-4 py-3 font-semibold" scope="col">
                    Changed
                  </th>
                  {human ? (
                    <th className="px-4 py-3" scope="col">
                      <span className="sr-only">Actions</span>
                    </th>
                  ) : null}
                </tr>
              </thead>
              <tbody>
                {legion.daemons.length === 0 ? (
                  <tr>
                    <td
                      className={`px-4 py-5 ${textMutedOnSurface}`}
                      colSpan={human ? 4 : 3}
                      data-legion-daemons-empty
                    >
                      No project names a Legion daemon yet.
                    </td>
                  </tr>
                ) : (
                  legion.daemons.map((setting) => (
                    <tr
                      className={settingsTableRow}
                      data-legion-daemon={setting.project}
                      key={setting.project}
                    >
                      <td className={`px-4 py-3 font-mono ${textPrimaryOnSurface}`}>
                        {setting.project}
                      </td>
                      <td
                        className={`px-4 py-3 font-mono ${textSecondaryOnSurface}`}
                        data-legion-daemon-url
                      >
                        {setting.state_url}
                      </td>
                      <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                        <Timestamp at={setting.updated_at} className={textMutedOnSurface} />
                      </td>
                      {human ? (
                        <td className="px-4 py-3 text-right">
                          <button
                            aria-label={`Remove the Legion daemon of ${setting.project}`}
                            className={`font-medium disabled:cursor-not-allowed disabled:opacity-50 ${dangerText} ${dangerHoverText}`}
                            disabled={writing}
                            onClick={() => removeDaemon.mutate(setting.project)}
                            type="button"
                          >
                            Remove
                          </button>
                        </td>
                      ) : null}
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>

          {human ? (
            <form
              className={`mt-6 grid gap-4 rounded-xl border p-4 sm:grid-cols-[1fr_1fr_auto] sm:items-end ${borderDefault}`}
              onSubmit={submit}
            >
              <label className={settingsFieldLabel} htmlFor="legion-project">
                Legion project
                <ProjectSelect
                  disabled={writing}
                  id="legion-project"
                  onChange={setProject}
                  projects={legion.projects}
                  value={project}
                />
              </label>
              <label className={settingsFieldLabel} htmlFor="legion-state-url">
                Legion daemon state URL
                <input
                  className={settingsMonoInput}
                  disabled={writing}
                  id="legion-state-url"
                  onChange={(event) => setStateUrl(event.target.value)}
                  placeholder="https://daemon.example"
                  required
                  value={stateUrl}
                />
              </label>
              <button className={settingsSubmitButton} disabled={writing} type="submit">
                Save Legion daemon
              </button>
            </form>
          ) : null}
          {failedAction !== undefined ? (
            <div className="mt-4">
              <QueryError
                message={failedAction.message}
                onRetry={failedAction.kind === "input" ? undefined : retry}
                retrying={writing}
              />
            </div>
          ) : null}
        </>
      ) : null}
    </section>
  );
}
