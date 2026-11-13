import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { apiErrorMessage, goingSettingsRequest } from "../../api/client";
import { goingSettingsQuery } from "../../api/queries";
import type { GoingSettings } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { textMutedOnCanvas, textPrimaryOnCanvas, textSecondaryOnCanvas } from "../../theme/classes";
import { useKeyedWrite } from "../writes/useKeyedWrite";

/**
 * The person's own command-bar setting: whether every write the bar makes for them asks first.
 * It is held on Dispatch (`GET`/`PUT /api/v1/me/going/settings`), so it follows the person to
 * every browser they sign in on, and it is off until they turn it on. With it on, the bar shows
 * each write it would make and runs it only once they confirm (`approved: confirmed`); Dispatch
 * refuses a write the bar sends at once for them while it is on.
 */
export function CommandBarSection(): ReactNode {
  const queryClient = useQueryClient();
  const settings = useQuery(goingSettingsQuery());
  // A save Dispatch refused: what it said, and the value it refused, which Retry sends again.
  const [refusal, setRefusal] = useState<{ readonly message: string; readonly on: boolean }>();
  const key = goingSettingsQuery().queryKey;
  // One press: the value chosen, and the setting as the page drew it before, which a refusal
  // puts back.
  type Press = { readonly on: boolean; readonly previous: GoingSettings | undefined };
  const save = useKeyedWrite<GoingSettings, Press>({
    inline: true,
    label: ({ on }) =>
      on ? "Ask before every command-bar write" : "Command-bar writes run at once",
    onError: (error, { on, previous }) => {
      queryClient.setQueryData(key, previous);
      setRefusal({ message: apiErrorMessage(error, "The setting did not reach Dispatch."), on });
    },
    onSuccess: (saved) => {
      if (saved === null) void queryClient.invalidateQueries({ queryKey: key });
      else queryClient.setQueryData(key, saved);
    },
    request: ({ on }) => goingSettingsRequest({ confirm_writes: on }),
    target: () => "going-settings",
  });
  // The checkbox is drawn from the query, so the choice goes into it in the press itself, before
  // the write starts: a box that waited for Dispatch's answer would show no change when clicked.
  // A read still out is cancelled first, so its answer cannot draw the old value over the choice.
  const choose = (on: boolean) => {
    void queryClient.cancelQueries({ queryKey: key });
    const previous = queryClient.getQueryData<GoingSettings>(key);
    queryClient.setQueryData<GoingSettings>(key, { confirm_writes: on });
    setRefusal(undefined);
    save.mutate({ on, previous });
  };

  return (
    <section aria-labelledby="command-bar-heading">
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id="command-bar-heading">
        Command bar
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        Where the bar is sure what you asked for, it makes the change when you submit. Turn this on
        and it shows each change first and makes it only once you confirm.
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
        <label className={`mt-4 flex items-center gap-2 text-sm ${textPrimaryOnCanvas}`}>
          <input
            checked={settings.data.confirm_writes}
            className="size-4 cursor-pointer"
            onChange={(event) => choose(event.target.checked)}
            type="checkbox"
          />
          Ask before every write
        </label>
      )}
      {refusal === undefined ? null : (
        <div className="mt-4">
          <QueryError
            message={refusal.message}
            onRetry={() => choose(refusal.on)}
            retrying={save.isPending}
          />
        </div>
      )}
    </section>
  );
}
