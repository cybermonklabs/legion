import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { apiErrorMessage, goingSettingsRequest } from "../../api/client";
import { goingSettingsQuery } from "../../api/queries";
import type { GoingSettings } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { textMutedOnCanvas, textPrimaryOnCanvas, textSecondaryOnCanvas } from "../../theme/classes";
import { useKeyedWrite } from "../writes/useKeyedWrite";

type Mode = GoingSettings["mode"];

/** The two modes, in the order the section lists them, each with its label. */
const MODES: readonly { readonly mode: Mode; readonly label: string }[] = [
  { label: "Ask", mode: "ask" },
  { label: "Auto-review", mode: "auto_review" },
];

/**
 * The person's own command-bar setting: its mode, which says which changes of a run's plan wait
 * for them. It is held on Dispatch (`GET`/`PUT /api/v1/me/going/settings`), so it follows the
 * person to every browser they sign in on, and it is Ask until they choose Auto-review. In Ask
 * every change of a plan waits for the person's approval; in Auto-review a review of the plan
 * against their words lets through the plain changes those words ask for, and the rest wait.
 */
export function CommandBarSection(): ReactNode {
  const queryClient = useQueryClient();
  const settings = useQuery(goingSettingsQuery());
  // A save Dispatch refused: what it said, and the mode it refused, which Retry sends again.
  const [refusal, setRefusal] = useState<{ readonly message: string; readonly mode: Mode }>();
  // The mode the person pressed, drawn until Dispatch answers the save. It is the component's own
  // state, so the choice changes in the press itself; a read of the setting that was out when the
  // person pressed cannot draw the old mode over it, however that read ends.
  const [pressed, setPressed] = useState<Mode>();
  const key = goingSettingsQuery().queryKey;
  const save = useKeyedWrite<GoingSettings, Mode>({
    inline: true,
    label: (mode) =>
      mode === "auto_review"
        ? "Command bar: Auto-review lets through what your words ask for"
        : "Command bar: Ask before every change",
    onError: (error, mode) => {
      setPressed(undefined);
      setRefusal({ message: apiErrorMessage(error, "The setting did not reach Dispatch."), mode });
    },
    onSuccess: (saved) => {
      if (saved !== null) {
        queryClient.setQueryData(key, saved);
        setPressed(undefined);
        return;
      }
      void queryClient.invalidateQueries({ queryKey: key }).finally(() => setPressed(undefined));
    },
    request: (mode) => goingSettingsRequest({ mode }),
    target: () => "going-settings",
  });
  const choose = (mode: Mode) => {
    setPressed(mode);
    setRefusal(undefined);
    save.mutate(mode);
  };
  const chosen = pressed ?? settings.data?.mode;

  return (
    <section aria-labelledby="command-bar-heading">
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id="command-bar-heading">
        Command bar
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        The bar plans its changes before it makes any. In Ask, every change of a plan waits for your
        approval. In Auto-review, a review of the plan against your words lets through the plain
        changes they ask for, and the rest wait for you.
      </p>
      {settings.isPending ? (
        <p className={`mt-4 ${textMutedOnCanvas}`}>Loading the setting…</p>
      ) : null}
      {settings.isError ? (
        <div className="mt-4">
          <QueryError
            message="Couldn't load the command-bar setting."
            onRetry={() => void settings.refetch()}
            retrying={settings.isFetching}
          />
        </div>
      ) : null}
      {settings.data === undefined ? null : (
        <fieldset className="mt-4 space-y-2">
          <legend className={`text-sm font-medium ${textPrimaryOnCanvas}`}>Mode</legend>
          {MODES.map(({ label, mode }) => (
            <label className={`flex items-center gap-2 text-sm ${textPrimaryOnCanvas}`} key={mode}>
              <input
                checked={chosen === mode}
                className="size-4 cursor-pointer"
                name="going-mode"
                onChange={() => choose(mode)}
                type="radio"
                value={mode}
              />
              {label}
            </label>
          ))}
        </fieldset>
      )}
      {refusal === undefined ? null : (
        <div className="mt-4">
          <QueryError
            message={refusal.message}
            onRetry={() => choose(refusal.mode)}
            retrying={save.isPending}
          />
        </div>
      )}
    </section>
  );
}
