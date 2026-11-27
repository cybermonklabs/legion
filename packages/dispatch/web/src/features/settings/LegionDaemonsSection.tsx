// The Settings page's Legion section (dispatch://INFRA-1077): which project's state Dispatch
// reads from which daemon. Naming a project's daemon here is what makes Dispatch record each state
// change that daemon publishes (dispatch://INFRA-3931), reading its `GET /legion/v1/state` whole
// only where the changes cannot carry the state, and removing one stops the recording while
// everything already recorded stays. No page draws what Dispatch records: nothing in the dashboard
// reads the board it feeds (`GET /api/v1/projects/{key}/legion/board`).
//
// No route lists every project's daemon, so the section reads each project's
// `GET /api/v1/projects/{key}/legion/daemon` (`useLegionDaemons`), and a write changes the one
// entry of the project it names. The two writes are `PUT` and
// `DELETE /api/v1/projects/{key}/legion/daemon`, classed `scopeDeployment` beside a project's
// default repository and the Delivery settings: pointing Dispatch at a host is the deployment's
// business, not one project's. A credential naming one project is refused by the server, which is
// the only role Dispatch has, so the controls are offered to any signed-in person (`useIsHuman`)
// and the server refuses the rest; its refusal is shown under the form as it wrote it.
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useRef, useState } from "react";

import { apiErrorMessage, legionDaemonRemovalRequest, legionDaemonRequest } from "../../api/client";
import { legionDaemonQuery } from "../../api/queries";
import { invalidateWaitingForPaused, replaceQueryData } from "../../api/query-client";
import { useReadBound } from "../../api/read-bound";
import type { LegionSetting } from "../../api/types";
import { ARM_ON_PRESS, useFocusPlace } from "../../components/focus-place";
import { heldField, heldPress } from "../../components/held-press";
import { QueryError } from "../../components/QueryError";
import {
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeadCell,
  TableRow,
  TableShell,
} from "../../components/TableShell";
import { useIsHuman } from "../../hooks/useIsHuman";
import {
  borderDefault,
  dangerHoverText,
  dangerText,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { useKeyedWrite, usePendingKeyedPress } from "../writes/useKeyedWrite";
import { settingsFieldLabel, settingsMonoInput, settingsSubmitButton } from "./classes";
import { NoProjectPicked, ProjectSelect } from "./ProjectSelect";
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

/** What the section says when its daemons failed to load, and before why a project's read after
 *  a landed write has not settled (`useReadBound`). */
const LEGION_DAEMONS_NOT_LOADED = "Couldn't load the Legion daemons.";

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
  // The form was submitted with no project picked, and says so until one is.
  const [projectMissing, setProjectMissing] = useState(false);
  const [stateUrl, setStateUrl] = useState("");
  const [failedAction, setFailedAction] = useState<FailedAction>();
  const legion = useLegionDaemons();
  const list = useRef<HTMLElement>(null);
  const noteFocus = useFocusPlace(
    () => list.current,
    () => null
  );
  // Each project's daemon is a read of its own, and the section's one line says why the latest
  // write's read of a daemon has not settled: the hold is over the reads of every project's
  // daemon, under the key each of them begins with.
  const [unsettled, holdThrough] = useReadBound(["legion-daemon"]);
  const daemonKey = (named: string) => legionDaemonQuery(named).queryKey;
  // The line both writes of a daemon go on (`target`), in press order.
  const daemonTarget = (named: string) => `legion-daemon:${named}`;
  // Both writes show their refusal under the form, with the row they changed put back. Neither
  // holds back the project's reads while it is out (`holds`): another browser names a project's
  // daemon as this one does, and no live event names it, so a hold would keep that change back
  // until this write's answer, however late. A read already out at the press is given up in the
  // step the press shows its row, so no second press reads the row from before it; one Dispatch
  // answers before it takes the write can still show the old row until the write settles and the
  // project is read again. Both go on one line per daemon (`target`), in press order: a Save or a
  // Remove pressed while an earlier one of the same daemon is late waits its turn behind it, so
  // the daemon ends as the later press left it, however each one's tries wait out the network.
  // Each goes under a name of its own, and the section reads their presses from TanStack's
  // mutation cache, which tells its listeners at once (`usePendingKeyedPress`). The observer
  // `useMutation` reads is told a timer later, a task after the bound that ends a write's hold has
  // drawn why the daemons are not read, so a form held by that observer would refuse a press made
  // once the section says why.
  const saveKey = ["legion-daemon-save"];
  const removeKey = ["legion-daemon-remove"];
  const saveOut = usePendingKeyedPress(saveKey);
  const removeOut = usePendingKeyedPress(removeKey);
  const saveDaemon = useKeyedWrite<LegionSetting, DaemonInput, PreviousDaemon>({
    mutationKey: saveKey,
    target: ({ project: named }) => daemonTarget(named),
    request: ({ project: named, state_url: url }) => legionDaemonRequest(named, url),
    inline: true,
    readsLate: true,
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
      // The row is put back above; the project is read again to settle on the server's state, and
      // the form is not held through that read: after a stop made offline it is paused until the
      // network returns, and a stopped write is no longer late (`KeyedWriteResult.late`).
      void queryClient.invalidateQueries({ queryKey: daemonKey(variables.project) });
    },
    onMutate: (variables) => {
      const key = daemonKey(variables.project);
      void queryClient.cancelQueries({ exact: true, queryKey: key });
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
    // A landing with no answer keeps the optimistic row; reading the project again leaves the page
    // on the server's state, bounded by `useReadBound` so a paused offline read does not hold the
    // form forever. An answer goes over any read of the project still out, asked before it
    // (`replaceQueryData`).
    onSuccess: (setting, variables) => {
      if (setting !== null) {
        replaceQueryData(queryClient, daemonKey(setting.project), setting);
      }
      setFailedAction(undefined);
      setProject("");
      setStateUrl("");
      return holdThrough(invalidateWaitingForPaused(queryClient, daemonKey(variables.project)));
    },
  });
  const removeDaemon = useKeyedWrite<void, string, PreviousDaemon>({
    mutationKey: removeKey,
    target: daemonTarget,
    request: (named) => legionDaemonRemovalRequest(named),
    inline: true,
    readsLate: true,
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
      // As for a save: read again, with no hold on the form (see `saveDaemon`'s `onError`).
      void queryClient.invalidateQueries({ queryKey: daemonKey(named) });
    },
    onMutate: (named) => {
      const key = daemonKey(named);
      void queryClient.cancelQueries({ exact: true, queryKey: key });
      const previous = queryClient.getQueryData(key);
      queryClient.setQueryData(key, null);
      return { previous };
    },
    onSuccess: (_answer, named) => {
      setFailedAction(undefined);
      return holdThrough(invalidateWaitingForPaused(queryClient, daemonKey(named)));
    },
  });

  // The refusal on screen is the newest press of its write: a later press of either write that
  // lands lets it go, and Retry waits while either holds the form (`writing`). So Retry is that
  // write's own (`retry`), which makes a press the reader stopped again as that press. Both are a
  // `PUT` or a `DELETE`, which sends no key, so that is the same change sent again. The field's
  // own refusal of a URL made no press, and offers no Retry.
  const retry = () => {
    if (failedAction?.kind === "save") {
      saveDaemon.retry();
      return;
    }
    if (failedAction?.kind === "remove") {
      removeDaemon.retry();
    }
  };
  // While either write is out within its deadline, Save Legion daemon and every row's Remove are
  // held by `aria-disabled` (`heldPress`), and the address box by `aria-disabled` and read-only,
  // never disabled: the control the reader pressed or submitted from keeps the focus, and a press
  // or a submit then sends nothing. A write past its deadline (`late`) holds them no longer: it is
  // still tried under its key, and the shell's indicator offers to stop it, but one that never
  // answers - the browser offline - would otherwise hold the form for good. Nor does a press made
  // then hold them, which waits its turn behind that write (`late` as well) until it is decided.
  const writing = (saveOut.isPending && !saveOut.late) || (removeOut.isPending && !removeOut.late);
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (writing) return;
    // The project list is a button, which no `required` holds: an empty one files nothing here.
    if (project === "") {
      setProjectMissing(true);
      return;
    }
    const { refusal, url } = legionStateUrl(stateUrl);
    if (refusal !== "") {
      setFailedAction({ kind: "input", message: refusal });
      return;
    }
    saveDaemon.mutate({ project, state_url: url });
  };

  return (
    <section
      {...ARM_ON_PRESS}
      aria-labelledby="legion-daemons-heading"
      data-legion-daemons
      onFocus={noteFocus}
      ref={list}
    >
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id="legion-daemons-heading">
        Legion
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        Naming a project&apos;s Legion daemon here makes Dispatch record each state change that
        daemon publishes. Dispatch reads the daemon&apos;s whole state only to start that record or
        to check what it recorded; it does not poll the daemon on a timer. No page shows that record
        yet. The address is the daemon&apos;s base URL, with no path: Dispatch reads{" "}
        <code>/legion/v1/state</code> under it. Removing one stops the recording. What was recorded
        stays.
        {human ? "" : " Sign in to name one."}
      </p>

      {legion.isPending ? (
        <p className={`mt-6 ${textMutedOnCanvas}`}>Loading Legion daemons…</p>
      ) : null}
      {legion.isError ? (
        <div className="mt-6">
          <QueryError
            message={LEGION_DAEMONS_NOT_LOADED}
            onRetry={legion.retry}
            retrying={legion.isFetching}
          />
        </div>
      ) : null}
      {legion.daemons !== undefined && legion.projects !== undefined ? (
        <>
          <TableShell className="mt-6" label="Legion daemons">
            <TableHead>
              <TableHeadCell>Project</TableHeadCell>
              <TableHeadCell>Daemon state URL</TableHeadCell>
              <TableHeadCell>Changed</TableHeadCell>
              {human ? (
                <TableHeadCell>
                  <span className="sr-only">Actions</span>
                </TableHeadCell>
              ) : null}
            </TableHead>
            <TableBody>
              {legion.daemons.length === 0 ? (
                <TableEmpty colSpan={human ? 4 : 3}>
                  <span data-legion-daemons-empty>No project names a Legion daemon yet.</span>
                </TableEmpty>
              ) : (
                legion.daemons.map((setting) => (
                  <TableRow data-legion-daemon={setting.project} key={setting.project}>
                    <TableCell mono tone="primary">
                      {setting.project}
                    </TableCell>
                    <TableCell data-legion-daemon-url mono tone="secondary">
                      {setting.state_url}
                    </TableCell>
                    <TableCell tone="secondary">
                      <Timestamp at={setting.updated_at} className={textMutedOnSurface} />
                    </TableCell>
                    {human ? (
                      <TableCell align="right">
                        <button
                          aria-label={`Remove the Legion daemon of ${setting.project}`}
                          className={`font-medium refused:cursor-not-allowed refused:opacity-50 ${dangerText} ${dangerHoverText}`}
                          {...heldPress(writing, () => removeDaemon.mutate(setting.project))}
                          type="button"
                        >
                          Remove
                        </button>
                      </TableCell>
                    ) : null}
                  </TableRow>
                ))
              )}
            </TableBody>
          </TableShell>
          {unsettled === undefined ? null : (
            <div className="mt-4">
              <QueryError message={`${LEGION_DAEMONS_NOT_LOADED} ${unsettled}.`} />
            </div>
          )}

          {human ? (
            <form
              className={`mt-6 grid gap-4 rounded-xl border p-4 sm:grid-cols-[1fr_1fr_auto] sm:items-end ${borderDefault}`}
              onSubmit={submit}
            >
              <ProjectSelect
                held={writing}
                label="Legion project"
                onChange={(picked) => {
                  setProject(picked);
                  setProjectMissing(false);
                }}
                projects={legion.projects}
                value={project}
              />
              <label className={settingsFieldLabel} htmlFor="legion-state-url">
                Legion daemon state URL
                <input
                  {...heldField(writing)}
                  className={`${settingsMonoInput} aria-disabled:opacity-50`}
                  id="legion-state-url"
                  onChange={(event) => setStateUrl(event.target.value)}
                  placeholder="https://daemon.example"
                  required
                  value={stateUrl}
                />
              </label>
              <button
                aria-disabled={writing || undefined}
                className={settingsSubmitButton}
                type="submit"
              >
                Save Legion daemon
              </button>
            </form>
          ) : null}
          {projectMissing ? <NoProjectPicked /> : null}
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
