import {
  Fragment,
  type KeyboardEvent,
  type ReactNode,
  type Ref,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useLocation } from "react-router-dom";

import { SearchBox } from "../../components/SearchBox";
import { inputMethodKey } from "../../lib/input-method-key";
import {
  borderDefault,
  calloutInfoBg,
  calloutInfoBorder,
  calloutInfoTitleText,
  dangerText,
  hoverToDangerText,
  linkHoverText,
  linkText,
  successText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { keyTokens } from "../shell/key-text";
import { CHORD_TIMEOUT_MS, comboOf, KeymapPersonContext } from "../shell/keymap";
import {
  type CatalogueEntry,
  type CatalogueScope,
  catalogueEntry,
  KEYMAP_ACTIONS,
  type KeymapActionId,
  keepsOwnKeys,
  type PersonKeys,
} from "../shell/keymap-catalogue";
import { resetAllKeys, setActionKeys } from "../shell/keymap-keys";
import { refusalOf } from "../shell/keymap-refusal";
import {
  KeyHints,
  kbdClass,
  SCOPE_LABELS,
  SCOPE_ORDER,
  SHORTCUTS_SECTION_ID,
} from "../shell/ShortcutHelp";

/** The scopes the section lists, in the help's order: each one but `dialog`, whose actions are
 *  those of the page a dialog shows (the Inbox drawer's are the Inbox's). */
const SCOPES = SCOPE_ORDER.filter((scope): scope is CatalogueScope => scope !== "dialog");

/** One action as the section lists it. */
interface ListedAction {
  readonly action: KeymapActionId;
  readonly entry: CatalogueEntry;
  /** The keys the person can change: theirs where they changed them, else the defaults. */
  readonly keys: readonly string[];
  readonly changed: boolean;
  /** For each global action these keys stop on a page, the refusal check's notice. */
  readonly notices: readonly string[];
}

/** The key the section is recording: for `action`, in place of its key at `replacing`, or as one
 *  more key where that is undefined; the first of two presses while a second may still come; and
 *  why the last key was refused. */
interface Recording {
  readonly action: KeymapActionId;
  readonly replacing: number | undefined;
  readonly first: string | undefined;
  readonly refusal: string | undefined;
}

/** A Reset the section refused: the action, why, and the person's keys it was refused under,
 *  since a later change of them may free the key it names. */
interface RefusedReset {
  readonly action: KeymapActionId;
  readonly message: string;
  readonly personKeys: PersonKeys;
}

/** A key as a control's name says it: each press as the help draws it, `g then r` for two. */
function spoken(key: string): string {
  return keyTokens(key)
    .map(({ token }) => token)
    .join(" then ");
}

/** A key as the help draws it, with `then` between the two presses of a key of two. */
function KeyText({ keys }: { keys: string }): ReactNode {
  return (
    <span className="inline-flex items-center gap-1">
      {keyTokens(keys).map(({ key, token }, step) => (
        <Fragment key={key}>
          {/* Spaced in its text as well as by the gap, so the words a control's name reads are
              the words it shows: `g then r`. */}
          {step === 0 ? null : <span className={`text-xs ${textMutedOnCanvas}`}>{" then "}</span>}
          <kbd className={kbdClass}>{token}</kbd>
        </Fragment>
      ))}
    </span>
  );
}

/** The class of the section's buttons that read as links: Add key and Reset. */
const textButtonClass = `min-h-11 text-sm font-medium xl:min-h-8 ${linkText} ${linkHoverText}`;

/**
 * A control that records a key once pressed: one of an action's keys, which it then changes, or
 * the action's Add key. While it records it says so, and reads every press (`onKeyDown`); leaving
 * it ends the recording (`onLeave`). The same element does both, so the focus stays on it
 * throughout.
 */
function RecorderButton({
  active,
  children,
  className,
  label,
  onKeyDown,
  onLeave,
  onStart,
  ref,
  refusalId,
}: {
  /** The recording this control is doing, or undefined while it does none. */
  active: Recording | undefined;
  /** What the control shows while it does not record. */
  children: ReactNode;
  className: string;
  /** Its name while it does not record, where its text alone does not say what it does. */
  label?: string;
  onKeyDown: (event: KeyboardEvent<HTMLButtonElement>) => void;
  onLeave: () => void;
  onStart: (from: HTMLButtonElement) => void;
  ref?: Ref<HTMLButtonElement>;
  refusalId: string;
}): ReactNode {
  return (
    <button
      aria-describedby={active?.refusal === undefined ? undefined : refusalId}
      aria-label={active === undefined ? label : undefined}
      className={
        active === undefined
          ? className
          : `min-h-11 rounded-md border-2 border-dashed px-2 text-xs xl:min-h-8 ${calloutInfoBorder} ${calloutInfoBg} ${calloutInfoTitleText}`
      }
      onBlur={active === undefined ? undefined : onLeave}
      onClick={(event) => {
        if (active === undefined) onStart(event.currentTarget);
      }}
      onKeyDown={active === undefined ? undefined : onKeyDown}
      ref={ref}
      type="button"
    >
      {active === undefined ? (
        children
      ) : (
        <>
          {active.first === undefined ? (
            "Press the new key…"
          ) : (
            <>
              <KeyText keys={active.first} /> then …
            </>
          )}{" "}
          Esc cancels
        </>
      )}
    </button>
  );
}

/**
 * Settings' Keyboard shortcuts (R14): every action of the keymap, by its scope in the help's
 * order, whether or not its page is open, with its keys, narrowed by `Filter actions`. The
 * signed-in person changes a key, adds one, removes one, and sets one action, or every action,
 * back to its defaults; the keys are kept in this browser (`keymap-keys.ts`) and every open tab
 * of it takes them at once. Each key is a recorder (`Change <key>`) beside its `Remove <key>`,
 * each action that takes a key has `Add key`, a changed one `Reset` and a `Changed from …` note
 * (`Added` for one that had no key), and the section has `Reset all`. A key that keeps its own
 * action (the digits, the arrows, Enter, Escape, Tab, Space) is drawn and never changed, and an
 * action whose every key keeps its own is marked `Fixed`. Each action says which global action
 * its keys stop on a page, its defaults included (the Agents page's `/` and `i`).
 *
 * A recording takes every press (`preventDefault` and `stopPropagation`, so no page key fires:
 * the window's keymap skips a handled press): the next press, with or without Ctrl, Cmd, Alt or
 * Shift, and a second press within `CHORD_TIMEOUT_MS`, which makes one key of two presses. A
 * modifier alone ends nothing. Escape ends it with nothing changed. Tab and leaving the control
 * end it too: a first press still waiting for its second is then the key, as the end of the
 * second makes it (R14: the new key is the next key press), and with no press waiting nothing
 * changes. A key the refusal check refuses is not kept: the section says why, naming the action
 * that holds it, and waits for another, until the reader leaves the control. A Reset whose
 * defaults the refusal check refuses, since another action holds one of them now, changes
 * nothing: the section says why, naming that action, so no action loses a key the person gave it
 * unless they take it away themselves.
 */
export function KeyboardShortcutsSection(): ReactNode {
  const { login, personKeys } = useContext(KeymapPersonContext);
  const location = useLocation();
  const [filter, setFilter] = useState("");
  const [recording, setRecording] = useState<Recording>();
  const [refusedReset, setRefusedReset] = useState<RefusedReset>();
  // The chord timer's callback outlives the render that started it, so it reads these.
  const recordingRef = useRef<Recording | undefined>(undefined);
  const personKeysRef = useRef(personKeys);
  personKeysRef.current = personKeys;
  const timer = useRef<number | undefined>(undefined);
  /** Each action's Add key, which takes the focus when the control the reader pressed goes. */
  const addButtons = useRef(new Map<KeymapActionId, HTMLButtonElement>());
  const sectionRef = useRef<HTMLElement>(null);

  const listed = useMemo(
    () =>
      KEYMAP_ACTIONS.map((action): ListedAction => {
        const entry = catalogueEntry(action);
        const keys = personKeys[action] ?? entry.keys;
        const verdict = keys.length === 0 ? undefined : refusalOf(action, keys, personKeys);
        return {
          action,
          changed: personKeys[action] !== undefined,
          entry,
          keys,
          notices: verdict?.kind === "accepted" ? verdict.notices : [],
        };
      }),
    [personKeys]
  );

  // The address names the section when the help's link, or a link of the reader's own, leads
  // here. A frame on, so the section takes the focus after what this move focuses first: the
  // page's `<main>` (`AppShell`), and whatever the help hands the focus back to as it closes.
  useEffect(() => {
    if (location.hash !== `#${SHORTCUTS_SECTION_ID}`) return;
    const frame = window.requestAnimationFrame(() => {
      sectionRef.current?.scrollIntoView?.({ block: "start" });
      sectionRef.current?.focus({ preventScroll: true });
    });
    return () => window.cancelAnimationFrame(frame);
  }, [location]);

  useEffect(() => () => window.clearTimeout(timer.current), []);

  const record = (next: Recording | undefined) => {
    recordingRef.current = next;
    setRecording(next);
  };
  const stopTimer = () => {
    window.clearTimeout(timer.current);
    timer.current = undefined;
  };
  const cancel = () => {
    stopTimer();
    record(undefined);
  };
  const start = (action: KeymapActionId, replacing: number | undefined, from: HTMLElement) => {
    stopTimer();
    setRefusedReset(undefined);
    record({ action, first: undefined, refusal: undefined, replacing });
    from.focus();
  };

  /** Keeps `key` for the action being recorded, or says why it is refused and waits again. */
  const finish = (key: string) => {
    const current = recordingRef.current;
    if (current === undefined || login === undefined) return;
    const keys = personKeysRef.current[current.action] ?? catalogueEntry(current.action).keys;
    const next =
      current.replacing === undefined
        ? [...keys, key]
        : keys.map((held, index) => (index === current.replacing ? key : held));
    const verdict = refusalOf(current.action, next, personKeysRef.current);
    if (verdict.kind === "refused") {
      record({ ...current, first: undefined, refusal: verdict.message });
      return;
    }
    record(undefined);
    setActionKeys(login, current.action, next);
  };

  /** Ends the recording as the focus leaves the recorder - a click elsewhere, Tab. A first press
   *  still waiting for its second is kept, as the timer would keep it; a key the refusal check
   *  refuses is not, and the recording ends all the same, as it does with no press waiting. */
  const leave = () => {
    const first = recordingRef.current?.first;
    stopTimer();
    if (first !== undefined) finish(first);
    if (recordingRef.current !== undefined) cancel();
  };

  const onRecorderKeyDown = (event: KeyboardEvent<HTMLButtonElement>) => {
    const current = recordingRef.current;
    // Tab keeps its own action: it moves the focus on, which ends the recording (`leave`).
    if (current === undefined || event.key === "Tab") return;
    event.preventDefault();
    event.stopPropagation();
    // A held key repeats its press, which is no second press of a key of two. An input method's
    // key (`inputMethodKey`) records nothing and cancels nothing: one pressed in a composition,
    // or the one that ends it, which Safari sends with `isComposing` false and keyCode 229.
    if (inputMethodKey(event.nativeEvent) || event.repeat) return;
    if (event.key === "Escape") {
      cancel();
      return;
    }
    const press = comboOf(event.nativeEvent);
    if (press === null) return;
    if (current.first === undefined) {
      record({ ...current, first: press, refusal: undefined });
      timer.current = window.setTimeout(() => {
        timer.current = undefined;
        finish(press);
      }, CHORD_TIMEOUT_MS);
      return;
    }
    stopTimer();
    finish(`${current.first} ${press}`);
  };

  /** Stores `keys` for `action` (undefined: its defaults) and hands the focus to its Add key,
   *  since the control pressed (a key's Remove, the action's Reset) may go with the change. */
  const change = (action: KeymapActionId, keys: readonly string[] | undefined) => {
    if (login === undefined) return;
    cancel();
    setActionKeys(login, action, keys);
    addButtons.current.get(action)?.focus();
  };

  /** Gives `action` its defaults again, where the refusal check accepts them beside every other
   *  action's keys; else changes nothing and says why, naming the action that holds the key. */
  const reset = (action: KeymapActionId) => {
    if (login === undefined) return;
    const current = personKeysRef.current;
    const verdict = refusalOf(action, catalogueEntry(action).keys, current);
    if (verdict.kind === "accepted") {
      change(action, undefined);
      return;
    }
    cancel();
    setRefusedReset({
      action,
      message: `Reset is refused: ${verdict.message}${verdict.other === undefined ? "" : " Change that action's keys first."}`,
      personKeys: current,
    });
  };

  const needle = filter.trim().toLowerCase();
  const shown =
    needle === ""
      ? listed
      : listed.filter(({ entry, keys }) =>
          [entry.label, ...[...keys, ...(entry.fixedKeys ?? [])].map(spoken)]
            .join(" ")
            .toLowerCase()
            .includes(needle)
        );

  return (
    <section
      aria-labelledby={`${SHORTCUTS_SECTION_ID}-heading`}
      className="scroll-mt-4 outline-none"
      id={SHORTCUTS_SECTION_ID}
      ref={sectionRef}
      tabIndex={-1}
    >
      <h2
        className={`text-xl font-semibold ${textPrimaryOnCanvas}`}
        id={`${SHORTCUTS_SECTION_ID}-heading`}
      >
        Keyboard shortcuts
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        Change the key of each action, give it a second key, or take its keys away. Your keys are
        kept in this browser, and every open tab uses them at once.
        {login === undefined ? " Sign in to change them." : null}
      </p>
      <div className="mt-3 flex items-center gap-2">
        <SearchBox
          className="min-w-0 flex-1"
          label="Filter actions"
          onChange={setFilter}
          placeholder="Filter actions"
          value={filter}
        />
        {login === undefined ? null : (
          <button
            className={`min-h-11 shrink-0 rounded-md border px-3 text-sm font-medium xl:min-h-9 ${borderDefault} ${textSecondaryOnCanvas}`}
            onClick={() => {
              cancel();
              resetAllKeys(login);
            }}
            type="button"
          >
            Reset all
          </button>
        )}
      </div>
      {shown.length === 0 ? (
        <p className={`mt-3 text-sm ${textMutedOnCanvas}`}>No actions match</p>
      ) : null}
      {SCOPES.filter((scope) => shown.some(({ entry }) => entry.scope === scope)).map((scope) => (
        <section aria-labelledby={`${SHORTCUTS_SECTION_ID}-${scope}`} className="mt-4" key={scope}>
          <h3
            className={`text-xs font-semibold tracking-wide uppercase ${textMutedOnCanvas}`}
            id={`${SHORTCUTS_SECTION_ID}-${scope}`}
          >
            {SCOPE_LABELS[scope]}
          </h3>
          <ul className="mt-1">
            {shown
              .filter(({ entry }) => entry.scope === scope)
              .map(({ action, changed, entry, keys, notices }) => {
                const fixed = entry.fixedKeys ?? [];
                // An action whose every key keeps its own action reads the key pressed, and
                // takes no other (`refusalOf`).
                const changeable = login !== undefined && !keepsOwnKeys(entry);
                const ours = recording?.action === action ? recording : undefined;
                // A refused Reset stands until the person's keys change, which may free its key.
                const resetRefusal =
                  refusedReset?.action === action && refusedReset.personKeys === personKeys
                    ? refusedReset.message
                    : undefined;
                const refusalId = `${SHORTCUTS_SECTION_ID}-${action}-refusal`;
                // One refusal for the row, drawn in one paragraph, so its id names one element.
                // The two are never set at once: starting a recording clears a refused Reset, and
                // Reset ends the recording before it is refused. Were both set, the recording's
                // would win: its recorder has the focus and waits for another key, and the Reset
                // was refused before the recording began.
                const refusal =
                  ours?.refusal === undefined ? resetRefusal : `${ours.refusal} Press another key.`;
                const resetRefused = ours?.refusal === undefined && resetRefusal !== undefined;
                return (
                  <li
                    aria-labelledby={`${SHORTCUTS_SECTION_ID}-${action}`}
                    className={`flex flex-wrap items-center gap-x-4 gap-y-1 border-t py-1.5 text-sm ${borderDefault}`}
                    data-changed={changed ? "true" : undefined}
                    data-keymap-action={action}
                    key={action}
                  >
                    <div className="min-w-0 flex-1 basis-56 break-words">
                      <span
                        className={textPrimaryOnCanvas}
                        id={`${SHORTCUTS_SECTION_ID}-${action}`}
                      >
                        {entry.label}
                      </span>
                      {changed ? (
                        <span className={`ml-2 text-xs font-semibold ${successText}`}>
                          {entry.keys.length === 0
                            ? "Added"
                            : `Changed from ${entry.keys.map(spoken).join(" or ")}`}
                        </span>
                      ) : null}
                      {notices.map((notice) => (
                        <p className={`text-xs ${textSecondaryOnCanvas}`} key={notice}>
                          {notice}
                        </p>
                      ))}
                      {refusal === undefined ? null : (
                        <p className={`text-xs ${dangerText}`} id={refusalId} role="alert">
                          {refusal}
                        </p>
                      )}
                    </div>
                    <div className="flex min-w-0 flex-wrap items-center gap-1.5">
                      {keys.map((key, index) => {
                        if (!changeable) return <KeyText key={key} keys={key} />;
                        const changing = ours?.replacing === index ? ours : undefined;
                        return (
                          // The slot keeps its element while its key changes, so the recorder
                          // keeps the focus the press gave it.
                          // biome-ignore lint/suspicious/noArrayIndexKey: a key is changed in its place
                          <span className="inline-flex items-center" key={index}>
                            <RecorderButton
                              active={changing}
                              className="inline-flex min-h-11 items-center rounded xl:min-h-8"
                              label={`Change ${spoken(key)}`}
                              onKeyDown={onRecorderKeyDown}
                              onLeave={leave}
                              onStart={(from) => start(action, index, from)}
                              refusalId={refusalId}
                            >
                              <KeyText keys={key} />
                            </RecorderButton>
                            {changing === undefined ? (
                              <button
                                aria-label={`Remove ${spoken(key)}`}
                                className={`inline-flex min-h-11 min-w-11 items-center justify-center text-xs xl:min-h-8 xl:min-w-5 ${textMutedOnCanvas} ${hoverToDangerText}`}
                                onClick={() =>
                                  change(
                                    action,
                                    keys.filter((_, other) => other !== index)
                                  )
                                }
                                type="button"
                              >
                                <span aria-hidden="true">×</span>
                              </button>
                            ) : null}
                          </span>
                        );
                      })}
                      {fixed.length === 0 ? null : <KeyHints keys={fixed} />}
                      {keys.length === 0 && fixed.length === 0 ? (
                        <span className={`italic ${textMutedOnCanvas}`}>No key</span>
                      ) : null}
                    </div>
                    <div className="ml-auto flex items-center gap-3">
                      {changeable ? (
                        <RecorderButton
                          active={ours?.replacing === undefined ? ours : undefined}
                          className={textButtonClass}
                          onKeyDown={onRecorderKeyDown}
                          onLeave={leave}
                          onStart={(from) => start(action, undefined, from)}
                          ref={(element) => {
                            if (element === null) addButtons.current.delete(action);
                            else addButtons.current.set(action, element);
                          }}
                          refusalId={refusalId}
                        >
                          Add key
                        </RecorderButton>
                      ) : null}
                      {login !== undefined && !changeable ? (
                        <span className={`text-xs italic ${textMutedOnCanvas}`}>Fixed</span>
                      ) : null}
                      {changeable && changed ? (
                        <button
                          aria-describedby={resetRefused ? refusalId : undefined}
                          className={textButtonClass}
                          onClick={() => reset(action)}
                          type="button"
                        >
                          Reset
                        </button>
                      ) : null}
                    </div>
                  </li>
                );
              })}
          </ul>
        </section>
      ))}
    </section>
  );
}
