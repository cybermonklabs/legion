import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useId, useMemo, useState } from "react";

import { issueCollaboratorsQuery } from "../../../api/queries";
import type { Message, MessageDeliveryMode } from "../../../api/types";
import {
  focusVisibleRing,
  pageField,
  pageFocusedRow,
  pageInkText,
  pageMutedText,
  pageRowHoverBg,
} from "../../../theme/classes";
import { plainDelivery } from "../../conversation/delivery";
import { useAgents } from "../../conversation/useAgents";
import { useAgentText } from "../../refs/AgentName";
import { type MentionOption, mentionOptions, stoppedRoleOptions } from "../../refs/party-options";
import { authorText, pageChrome } from "../page-structure";
import { THREADS_WORDS } from "../page-words";
import { MessageBox } from "./MessageBox";

/** One party the chooser offers, and the mode a message to it goes in (R10, plan choice 2). */
interface Party {
  readonly option: MentionOption;
  readonly delivery: MessageDeliveryMode;
}

/**
 * New thread (dispatch://INFRA-5704/spec#b-r-new-thread, R10), in the main column: the chooser of
 * the agent, then the box, which holds the caret once New thread opens. It writes a message to an
 * agent, and nothing else.
 *
 * The chooser lists the parties the `@` picker offers on `main` (`useRefPicker.ts`): the live
 * sessions and the roles they hold (`mentionOptions`), then the issue's Legion roles that no live
 * session holds (`stoppedRoleOptions`), which a list that is not current leaves out, as the picker
 * does. Each is drawn by its name and its detail, both values of the registry, and a field above
 * them filters them. It reads what the picker reads for its parties and nothing else (R15): the
 * agent registry (`useAgents`, kept fresh by the shell's poller while New thread is open) and the
 * issue's collaborators. Both go out as New thread opens, not before, and the chooser draws its
 * parties once both have answered, so it never lists the live ones alone.
 *
 * The box is this part's own `MessageBox` (`threads-new-thread` in `DISPATCH_WRITING_BOXES`), so
 * `@` offers nothing and stores no mention (R11). It offers Send once an agent is chosen and it
 * holds words. Send writes a root message of the issue with the chosen party's target, in the mode
 * `plainDelivery` gives its live session's capabilities, or `aside` for a role no live session
 * holds, as `features/search/new-issue.ts` sends one (plan choice 2). `onSent` takes Dispatch's
 * answer, which the view opens the new thread from.
 */
export function NewThread({
  issueKey,
  onSent,
}: {
  readonly issueKey: string;
  readonly onSent: (message: Message) => void;
}): ReactNode {
  const text = useAgentText();
  const live = useAgents(true, true);
  const collaborators = useQuery(issueCollaboratorsQuery(issueKey));
  const [chosen, setChosen] = useState<string>();
  const [filter, setFilter] = useState("");
  const listId = useId();
  const settled = !live.isPending && !collaborators.isPending;
  const parties = useMemo((): readonly Party[] => {
    if (!settled) return [];
    const stopped = live.current
      ? stoppedRoleOptions(live.agents, collaborators.data?.agents ?? [], text)
      : [];
    return [
      ...mentionOptions(live.agents, text).map((option) => ({
        delivery: plainDelivery([option.capabilities]),
        option,
      })),
      ...stopped.map((option) => ({ delivery: "aside" as const, option })),
    ];
  }, [collaborators.data, live.agents, live.current, settled, text]);
  const party = parties.find((one) => one.option.target === chosen);
  // The filter keeps a party whose title or detail holds its words, in any case (R10).
  const needle = filter.trim().toLocaleLowerCase();

  return (
    <div className="flex min-w-0 flex-col gap-3" data-new-thread="">
      <div className="flex min-w-0 flex-col gap-2">
        <span
          className={`text-sm font-medium ${pageInkText}`}
          id={`${listId}-name`}
          {...pageChrome}
        >
          {THREADS_WORDS.chooser}
        </span>
        <input
          aria-controls={listId}
          aria-label={THREADS_WORDS.chooser_filter}
          className={`w-full min-w-0 rounded-[7px] border px-2 py-1 text-sm outline-none ${pageField}`}
          onChange={(event) => setFilter(event.target.value)}
          placeholder={THREADS_WORDS.chooser_filter}
          type="text"
          value={filter}
        />
        <div
          aria-labelledby={`${listId}-name`}
          className="flex max-h-72 min-w-0 flex-col overflow-y-auto"
          id={listId}
          role="listbox"
        >
          {parties
            .filter(
              ({ option }) =>
                needle === "" ||
                option.title.toLocaleLowerCase().includes(needle) ||
                option.detail.toLocaleLowerCase().includes(needle)
            )
            .map(({ option }) => (
              <button
                aria-selected={option.target === chosen}
                className={`flex min-w-0 flex-col items-start rounded-[7px] px-2 py-1 text-left ${focusVisibleRing} ${option.target === chosen ? pageFocusedRow : pageRowHoverBg}`}
                data-thread-party={option.target}
                key={option.target}
                onClick={() => setChosen(option.target)}
                role="option"
                type="button"
              >
                <span
                  className={`min-w-0 text-sm font-medium [overflow-wrap:anywhere] ${pageInkText}`}
                  {...authorText}
                >
                  {option.title}
                </span>
                <span
                  className={`min-w-0 text-xs [overflow-wrap:anywhere] ${pageMutedText}`}
                  {...authorText}
                >
                  {option.detail}
                </span>
              </button>
            ))}
        </div>
      </div>
      <div data-thread-box="new">
        <MessageBox
          address={
            party === undefined
              ? undefined
              : { thread: { delivery: party.delivery, target: party.option.target } }
          }
          autoFocus
          issueKey={issueKey}
          label={THREADS_WORDS.message_box}
          onSent={onSent}
          sendKey={["threads-new-thread", issueKey]}
        />
      </div>
    </div>
  );
}
