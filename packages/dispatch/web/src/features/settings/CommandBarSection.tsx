import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useId, useState } from "react";

import { apiErrorMessage, goingSettingsRequest } from "../../api/client";
import { goingSettingsQuery } from "../../api/queries";
import type { GoingSettings } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  card,
  focusVisibleRing,
  radioRing,
  successText,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useKeyedWrite } from "../writes/useKeyedWrite";

type Mode = GoingSettings["mode"];

/** The two approval modes, in the order the section lists them, each with its label, which the
 *  saved notice names it by, and the line that says what it does. */
const MODES: readonly { readonly mode: Mode; readonly label: string; readonly does: string }[] = [
  { does: "Every change of a plan waits for your approval.", label: "Ask", mode: "ask" },
  {
    does: "A review of the plan lets through the plain changes your words ask for.",
    label: "Auto-review",
    mode: "auto_review",
  },
];

/** A radio drawn by the page rather than the browser, so its ring reads on either scheme
 *  (`radioRing`), filled in the page's blue once chosen. */
const RADIO = `size-4 shrink-0 cursor-pointer appearance-none rounded-full border-2 outline-none checked:border-[5px] focus-visible:ring-2 focus-visible:ring-offset-2 ${radioRing} ${focusVisibleRing}`;

/** How long "Saved" stands beside the mode a choice saved: a moment, as the choice saves in the
 *  press itself and the radio already shows it (review: Settings). */
export const SAVED_SHOWS_MS = 3_000;

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
  // The mode Dispatch last answered a save of, for the notice that says it was saved, which
  // stands for a moment (`SAVED_SHOWS_MS`) and then goes.
  const [saved, setSaved] = useState<Mode>();
  useEffect(() => {
    if (saved === undefined) {
      return undefined;
    }
    const timer = window.setTimeout(() => setSaved(undefined), SAVED_SHOWS_MS);
    return () => window.clearTimeout(timer);
  }, [saved]);
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
    onSuccess: (answer, mode) => {
      setSaved(mode);
      if (answer !== null) {
        queryClient.setQueryData(key, answer);
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
    setSaved(undefined);
    save.mutate(mode);
  };
  const chosen = pressed ?? settings.data?.mode;
  const ids = useId();

  return (
    <section aria-labelledby="command-bar-heading">
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id="command-bar-heading">
        Command bar
      </h2>
      {/* One sentence: each radio's own line says what its mode does (review n8). */}
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        The bar plans its changes before it makes any.
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
        // The setting in a card, as every other section's controls are, under the name spec
        // v61 gives it, "Approval mode"; "Mode" alone is the name of Fast, Standard and Deep.
        <fieldset className={`mt-6 rounded-xl border p-4 ${card}`}>
          <legend className={`float-left mb-1 w-full text-sm font-medium ${textPrimaryOnSurface}`}>
            Approval mode
          </legend>
          {/* The legend floats so it sits inside the card rather than on its border, and what
              follows clears it: a row is a flex box, which stands beside a float rather than under
              it, so each row stood to the right of the full-width legend, past the screen's edge. */}
          <div className="clear-left space-y-3">
            {MODES.map(({ does, label, mode }) => (
              // The radio's name is its label alone; what the mode does describes it.
              <div className="flex items-start gap-2 text-sm" key={mode}>
                <input
                  aria-describedby={`${ids}-${mode}`}
                  checked={chosen === mode}
                  className={`mt-0.5 ${RADIO}`}
                  id={`${ids}-${mode}-radio`}
                  name="going-mode"
                  onChange={() => choose(mode)}
                  type="radio"
                  value={mode}
                />
                <div>
                  <label
                    className={`cursor-pointer font-medium ${textPrimaryOnSurface}`}
                    htmlFor={`${ids}-${mode}-radio`}
                  >
                    {label}
                  </label>
                  {saved === mode ? (
                    // The choice saves as it is made, with no Save button, so the mode it saved
                    // says so beside its name for a moment (review: Settings); the status line
                    // below is the same notice for a screen reader.
                    <span
                      aria-hidden="true"
                      className={`ml-2 text-xs font-medium ${successText}`}
                      data-testid="going-settings-saved-mark"
                    >
                      ✓ Saved
                    </span>
                  ) : null}
                  <p className={`text-xs ${textMutedOnSurface}`} id={`${ids}-${mode}`}>
                    {does}
                  </p>
                </div>
              </div>
            ))}
          </div>
          {/* Outside the spaced rows: this line takes no room, but a row's margin would still
              stand under the last help line and leave the card deeper at its foot than at its
              head (review a14). */}
          <p className="sr-only" data-testid="going-settings-saved" role="status">
            {saved === undefined
              ? null
              : `Saved: ${MODES.find((one) => one.mode === saved)?.label ?? saved}.`}
          </p>
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
