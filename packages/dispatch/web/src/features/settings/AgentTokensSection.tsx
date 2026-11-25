import { DISPATCH_SCOPES, DISPATCH_TOKEN_SCOPES } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api } from "../../api/client";
import { replaceQueryData } from "../../api/query-client";
import type {
  AgentToken,
  CreatedAgentToken,
  DispatchScope,
  DispatchTokenScope,
} from "../../api/types";
import { heldPress } from "../../components/held-press";
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
import { useCopyFeedback } from "../../hooks/useCopyFeedback";
import { compareTimestamps } from "../../lib/timestamps";
import {
  borderDefault,
  card,
  checkboxAccent,
  dangerHoverText,
  dangerText,
  inputClasses,
  primaryButtonBg,
  primaryButtonHoverBg,
  successText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import { useOneCreateOut } from "../writes/useOneCreateOut";
import { settingsSubmitButton } from "./classes";

const agentTokensQueryKey = ["agent-tokens"] as const;

/** A mint Dispatch made and lost the answer to: the label it was pressed with, the tokens already
 *  listed under any label when it was pressed (`before`), and whether the token list has been read
 *  again since. */
interface LostMint {
  readonly label: string;
  readonly before: readonly string[];
  readonly listed: boolean;
}

/** The live tokens labelled `label` that were not listed at the press, newest first: the tokens
 *  the lost mint can be. Read by id rather than by time, since the browser's clock and the
 *  database's need not agree. */
function lostTokens(
  tokens: readonly AgentToken[] | undefined,
  { before, label }: LostMint
): AgentToken[] {
  return (tokens ?? [])
    .filter(
      (token) => token.name === label && token.revoked_at === null && !before.includes(token.id)
    )
    .sort((a, b) => compareTimestamps(b.created_at, a.created_at));
}

/** A word a personal token's scopes can hold, as its checkbox reads it: what the reader ticks, and
 *  the line under it saying what a token holding it may do. */
interface TokenScope<Id extends DispatchTokenScope> {
  readonly id: Id;
  readonly label: string;
  readonly allows: string;
}

const ACT_AS_YOU: TokenScope<"person"> = {
  allows: "Reads and writes as you: issues, comments, asks, documents and messages.",
  id: "person",
  label: "Act as you",
};

const APPROVE: TokenScope<"approve"> = {
  allows:
    "Reviews documents, answers approvals and keeps decisions for you, and approves the credentials agents ask you for.",
  id: "approve",
  label: "Approve and control credentials",
};

const READ_ONLY: TokenScope<"read"> = {
  allows: "Only reads: Dispatch refuses it every write, whatever else it holds.",
  id: "read",
  label: "Read only",
};

/** The two scopes a token that writes can hold, in the order a row shows them. The ids are the
 *  contract's own (`DISPATCH_SCOPES`), so a row cannot offer a scope the server does not mint, and
 *  a row offers no Read only: no token that writes becomes read only (the server refuses it, 400
 *  TOKEN_SCOPES_INPUT), since a person makes a new token for that. */
const WRITING_SCOPES: readonly TokenScope<DispatchScope>[] = [ACT_AS_YOU, APPROVE];

/** Every word the mint form offers, in the order it shows them (`DISPATCH_TOKEN_SCOPES`). */
const MINT_SCOPES: readonly TokenScope<DispatchTokenScope>[] = [ACT_AS_YOU, APPROVE, READ_ONLY];

/** Both scopes: what a new token holds unless the reader clears one. */
const BOTH_SCOPES: readonly DispatchTokenScope[] = DISPATCH_SCOPES;

/** The words a token holds, `read` among them. The route table carries them as plain strings;
 *  `DISPATCH_TOKEN_SCOPES` is the closed list each is one of, so a word this build does not know
 *  is left out rather than drawn as a box nobody can clear, and a token whose list came back
 *  absent holds none. A lost mint is replaced by a token holding this set, so a read-only token's
 *  replacement is read only too. */
function heldScopes(scopes: readonly string[] | null): DispatchTokenScope[] {
  return DISPATCH_TOKEN_SCOPES.filter((known) => scopes?.includes(known) ?? false);
}

/** The whole set after one box is ticked or cleared, in the order `offered` shows them. No token
 *  holds `approve` without `person`, so ticking Approve ticks Act as you and clearing Act as you
 *  clears Approve; and no token holds `read` beside `approve`, since approving is a write, so
 *  ticking either clears the other. The server refuses every such pair (400 TOKEN_SCOPES_INPUT),
 *  and a form that offered one would be a refusal waiting to happen. No box is ever disabled for
 *  these rules: each press is taken, and the other boxes follow it. */
function scopesAfterChoice<Id extends DispatchTokenScope>(
  offered: readonly TokenScope<Id>[],
  held: readonly Id[],
  scope: Id,
  ticked: boolean
): Id[] {
  const next = new Set<DispatchTokenScope>(held);
  if (ticked) {
    next.add(scope);
    if (scope === "approve") {
      next.add("person");
      next.delete("read");
    }
    if (scope === "read") next.delete("approve");
  } else {
    next.delete(scope);
    if (scope === "person") next.delete("approve");
  }
  return offered.filter((each) => next.has(each.id)).map((each) => each.id);
}

/** The scope boxes `offered`, for a token's row and for the mint form. `subject` is the token
 *  they belong to, so the boxes of one row are told from another row's by name. `disabled` boxes
 *  are a revoked token's; `held` boxes keep the focus and their Tab stop while a write of theirs
 *  is out, and a press on one then changes nothing (`heldPress`). */
function TokenScopeChoices<Id extends DispatchTokenScope>({
  chosen,
  disabled = false,
  held,
  offered,
  onChoose,
  subject,
}: {
  chosen: readonly Id[];
  disabled?: boolean;
  held: boolean;
  offered: readonly TokenScope<Id>[];
  onChoose: (scopes: Id[]) => void;
  subject: string;
}): ReactNode {
  return (
    <div className="space-y-2">
      {offered.map((scope) => (
        <div key={scope.id}>
          <label
            className={`flex items-center gap-2 text-sm font-medium ${textSecondaryOnSurface}`}
          >
            <input
              aria-disabled={held || undefined}
              aria-label={`${scope.label} (${subject})`}
              checked={chosen.includes(scope.id)}
              className={`size-4 shrink-0 rounded aria-disabled:opacity-50 ${checkboxAccent}`}
              disabled={disabled}
              onChange={(event) => {
                // Held, a change writes nothing, and the box, controlled, shows the set as it was.
                if (held) return;
                onChoose(scopesAfterChoice(offered, chosen, scope.id, event.target.checked));
              }}
              type="checkbox"
            />
            {scope.label}
          </label>
          <p className={`mt-0.5 max-w-xs text-xs ${textMutedOnSurface}`}>{scope.allows}</p>
        </div>
      ))}
    </div>
  );
}

/** A read-only token's scopes, shown as words with no box: no token changes to or from read
 *  only, and a read-only token's scopes never change, so a person makes a new token instead. */
function ReadOnlyScopes({
  held,
  subject,
}: {
  held: readonly DispatchTokenScope[];
  subject: string;
}): ReactNode {
  return (
    <ul aria-label={`What ${subject} may do`} className="space-y-1">
      {MINT_SCOPES.filter((scope) => held.includes(scope.id)).map((scope) => (
        <li className={`text-sm font-medium ${textSecondaryOnSurface}`} key={scope.id}>
          {scope.label}
        </li>
      ))}
    </ul>
  );
}

export function AgentTokensSection(): ReactNode {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  /** What the mint form's boxes hold: both scopes until the reader changes them. */
  const [newScopes, setNewScopes] = useState<readonly DispatchTokenScope[]>(BOTH_SCOPES);
  const [shownToken, setShownToken] = useState<CreatedAgentToken>();
  /** When the token list was last read again after a mint whose answer was lost. */
  const [rereadAt, setRereadAt] = useState<number | undefined>(undefined);
  const { copy, reset: resetCopyStatus, status: copyStatus } = useCopyFeedback();
  const tokens = useQuery({ queryKey: agentTokensQueryKey, queryFn: () => api.listAgentTokens() });
  // The form shows a refused mint beside the name it was refused over. Every write here is read
  // late (`readsLate`): each holds its controls only while it is out within its deadline.
  const createToken = useKeyedWrite({
    inline: true,
    label: (input: {
      name: string;
      before: readonly string[];
      scopes: readonly DispatchTokenScope[];
    }) => `New agent token ${input.name}`,
    method: "POST",
    readsLate: true,
    // The whole set the boxes hold, `read` among them for a read-only token.
    write: ({ name: label, scopes }, bounds) =>
      api.createAgentToken({ name: label, scopes: [...scopes] }, bounds),
    onSuccess: (created) => {
      setName("");
      setNewScopes(BOTH_SCOPES);
      if (created === null) {
        // Minted, and its value - shown once only - is gone with the answer
        // (`createToken.unanswered`): the token is listed once the list is read again, and the
        // reader can replace it.
        setShownToken(undefined);
        void queryClient
          .invalidateQueries({ queryKey: agentTokensQueryKey })
          .then(() => setRereadAt(Date.now()));
        return;
      }
      const { token, ...safeToken } = created;
      // Each write's answer goes over any read of the list still out, which was asked before the
      // write and would put back the list without it (`replaceQueryData`).
      replaceQueryData<AgentToken[]>(queryClient, agentTokensQueryKey, (current = []) => [
        safeToken,
        ...current,
      ]);
      setShownToken(created);
      resetCopyStatus();
    },
  });
  const revokeToken = useKeyedWrite({
    label: (id: string) => `Agent token ${id} revoked`,
    method: "DELETE",
    readsLate: true,
    write: (id: string, bounds) => api.revokeAgentToken(id, bounds),
    onSuccess: (_result, id) => {
      replaceQueryData<AgentToken[]>(queryClient, agentTokensQueryKey, (current = []) =>
        current.map((token) =>
          token.id === id ? { ...token, revoked_at: new Date().toISOString() } : token
        )
      );
    },
  });
  /** One writing token's two boxes, written as the whole set the token is to hold, which never
   *  holds `read`. The row reads the set back from the list, so the boxes follow the write rather
   *  than the press. */
  const setTokenScopes = useKeyedWrite({
    label: (input: { id: string; name: string; scopes: DispatchScope[] }) =>
      `What agent token ${input.name} may do`,
    method: "PATCH",
    readsLate: true,
    write: (input, bounds) => api.setAgentTokenScopes(input.id, input.scopes, bounds),
    onSuccess: (updated, input) => {
      replaceQueryData<AgentToken[]>(queryClient, agentTokensQueryKey, (current = []) =>
        current.map((token) =>
          token.id === input.id ? (updated ?? { ...token, scopes: input.scopes }) : token
        )
      );
    },
  });
  // Each write holds its controls while it is out within its deadline, by `aria-disabled`, so the
  // control the reader pressed keeps the focus and a press of it then sends nothing. A write past
  // its deadline (`KeyedWriteResult.late`) holds them no longer. A mint let go that way is still
  // out, so New token then sends no second token, and the form says so (`oneMint`).
  const minting = createToken.isPending && !createToken.late;
  const revoking = revokeToken.isPending && !revokeToken.late;
  const scoping = setTokenScopes.isPending && !setTokenScopes.late;
  const oneMint = useOneCreateOut(createToken);
  /** Every token the page lists now, read at a press. */
  const listedIds = () => (tokens.data ?? []).map((token) => token.id);
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    // An emptied form - a mint that landed - sends nothing, whatever submits it, and so does one
    // whose mint is out: a held New token is only `aria-disabled`, so its press still submits, and
    // one let go past the mint's deadline says why it sent nothing (`oneMint`).
    if (name.trim() === "" || minting || oneMint.refuse()) return;
    createToken.mutate({ before: listedIds(), name, scopes: newScopes });
  };
  /** A mint whose value was lost with its answer, until the next mint or the reader writes
   *  another label: the press's label and listed tokens, and whether the list has been read
   *  again since it landed. */
  const unanswered = createToken.unanswered;
  const lost: LostMint | null =
    unanswered === undefined
      ? null
      : {
          before: unanswered.variables.before,
          label: unanswered.variables.name,
          listed: rereadAt !== undefined && rereadAt >= unanswered.at,
        };
  const candidates = lost === null ? [] : lostTokens(tokens.data, lost);
  /** Revokes `token` - once the reader confirms it, by its label and when it was made - and then
   *  mints another under its label, holding the scopes the lost one held. */
  const replaceLost = (token: AgentToken) => {
    const made = new Date(token.created_at).toLocaleString();
    if (
      !window.confirm(
        `Revoke “${token.name}”, created ${made}? Agents using it stop working at once, and a new token “${token.name}” is minted in its place.`
      )
    ) {
      return;
    }
    revokeToken.mutate(token.id, {
      onSuccess: () =>
        createToken.mutate({
          before: listedIds(),
          name: token.name,
          scopes: heldScopes(token.scopes),
        }),
    });
  };
  const replacing = revoking || minting;
  const copyToken = () => {
    if (shownToken === undefined) {
      return;
    }
    copy(shownToken.token);
  };
  const revoke = (token: AgentToken) => {
    if (
      window.confirm(`Revoke ${token.name}? Agents using this token will stop working immediately.`)
    ) {
      revokeToken.mutate(token.id);
    }
  };

  return (
    <section aria-labelledby="agent-tokens-heading">
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id="agent-tokens-heading">
        Agent tokens
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        Create a token for an agent you run. Its writes are attributed to you.
      </p>

      {tokens.isPending ? (
        <p className={`mt-6 ${textMutedOnCanvas}`}>Loading agent tokens…</p>
      ) : null}
      {tokens.isError ? (
        <div className="mt-6">
          <QueryError
            message="Couldn't load agent tokens."
            onRetry={() => void tokens.refetch()}
            retrying={tokens.isFetching}
          />
        </div>
      ) : null}
      {tokens.isSuccess ? (
        <TableShell className="mt-6" label="Agent tokens">
          <TableHead>
            <TableHeadCell>Name</TableHeadCell>
            <TableHeadCell>Token</TableHeadCell>
            <TableHeadCell>Created</TableHeadCell>
            <TableHeadCell>Last used</TableHeadCell>
            <TableHeadCell>What it may do</TableHeadCell>
            <TableHeadCell>
              <span className="sr-only">Actions</span>
            </TableHeadCell>
          </TableHead>
          <TableBody>
            {tokens.data.length === 0 ? (
              <TableEmpty colSpan={6}>No agent tokens yet.</TableEmpty>
            ) : (
              tokens.data.map((token) => {
                const tokenScopes = heldScopes(token.scopes);
                return (
                  <TableRow key={token.id}>
                    <TableCell tone="primary" weight="medium">
                      {token.name}
                    </TableCell>
                    <TableCell mono tone="secondary">
                      {`dsp_${token.prefix}…`}
                    </TableCell>
                    <TableCell tone="secondary">
                      <Timestamp at={token.created_at} />
                    </TableCell>
                    <TableCell tone="secondary">
                      {token.last_used_at === null ? (
                        "Never used"
                      ) : (
                        <Timestamp at={token.last_used_at} />
                      )}
                    </TableCell>
                    <TableCell>
                      {tokenScopes.includes("read") ? (
                        <ReadOnlyScopes held={tokenScopes} subject={token.name} />
                      ) : (
                        <TokenScopeChoices
                          chosen={DISPATCH_SCOPES.filter((scope) => tokenScopes.includes(scope))}
                          disabled={token.revoked_at !== null}
                          held={scoping}
                          offered={WRITING_SCOPES}
                          onChoose={(scopes) =>
                            setTokenScopes.mutate({ id: token.id, name: token.name, scopes })
                          }
                          subject={token.name}
                        />
                      )}
                    </TableCell>
                    <TableCell align="right">
                      {token.revoked_at === null ? (
                        <button
                          aria-label={`Revoke ${token.name}`}
                          className={`min-h-11 px-2 font-medium refused:cursor-not-allowed refused:opacity-50 ${dangerText} ${dangerHoverText}`}
                          {...heldPress(revoking, () => revoke(token))}
                          type="button"
                        >
                          Revoke
                        </button>
                      ) : (
                        <span className={textMutedOnSurface}>Revoked</span>
                      )}
                    </TableCell>
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </TableShell>
      ) : null}

      <form
        className={`mt-6 flex flex-col gap-4 rounded-xl border p-4 ${borderDefault}`}
        onSubmit={submit}
      >
        <div className="flex flex-col gap-3 sm:flex-row sm:items-end">
          <label
            className={`grid flex-1 gap-1 text-sm font-medium ${textSecondaryOnCanvas}`}
            htmlFor="agent-token-name"
          >
            Token label
            <input
              className={`min-h-11 rounded-md px-3 py-2 ${inputClasses(false)} ${textPrimaryOnSurface}`}
              id="agent-token-name"
              onChange={(event) => {
                createToken.forgetUnanswered();
                setName(event.target.value);
              }}
              placeholder="Architect"
              required
              value={name}
            />
          </label>
          <button
            aria-disabled={minting || undefined}
            className={settingsSubmitButton}
            type="submit"
          >
            New token
          </button>
        </div>
        {oneMint.refused ? (
          <p className={`text-sm ${textSecondaryOnCanvas}`} role="status">
            New token sent nothing: the token “{createToken.variables?.name}” is still being sent.
            It shows here once Dispatch makes it; if it fails, this form says why and you can press
            New token again.
          </p>
        ) : null}
        <TokenScopeChoices
          chosen={newScopes}
          held={minting}
          offered={MINT_SCOPES}
          onChoose={setNewScopes}
          subject="new token"
        />
      </form>
      {lost === null ? null : (
        <div className="mt-4 space-y-2">
          <LandedWithoutAnswer what={`Token “${lost.label}”`}>
            Its value is shown only once and was lost with the answer, so revoke it and mint
            another.
          </LandedWithoutAnswer>
          {!lost.listed ? (
            <p className={`text-sm ${textMutedOnCanvas}`} role="status">
              Looking for it…
            </p>
          ) : candidates.length === 0 ? (
            <p className={`text-sm ${textSecondaryOnCanvas}`} role="status">
              No live token “{lost.label}” made since you pressed New token is listed, so none was
              revoked.
            </p>
          ) : candidates.length === 1 ? (
            <button
              className={settingsSubmitButton}
              {...heldPress(replacing, () => {
                if (candidates[0] !== undefined) replaceLost(candidates[0]);
              })}
              type="button"
            >
              Revoke it and mint another
            </button>
          ) : (
            <>
              <p className={`text-sm ${textSecondaryOnCanvas}`}>
                {candidates.length} live tokens “{lost.label}” were made since you pressed New
                token. Pick the one to revoke:
              </p>
              <ul className="space-y-2">
                {candidates.map((token) => (
                  <li key={token.id}>
                    <button
                      className={settingsSubmitButton}
                      {...heldPress(replacing, () => replaceLost(token))}
                      type="button"
                    >
                      Revoke the one created {new Date(token.created_at).toLocaleString()}
                    </button>
                  </li>
                ))}
              </ul>
            </>
          )}
        </div>
      )}
      {createToken.isError ? (
        <div className="mt-4">
          <QueryError
            message={
              stoppedNote(createToken, "the new agent token", "made") ??
              "Couldn't create the agent token."
            }
            onRetry={createToken.retry}
            retrying={createToken.isPending}
          />
        </div>
      ) : null}
      {shownToken === undefined ? null : (
        <div className={`mt-4 rounded-xl border p-4 ${card}`} role="status">
          <p className={`font-medium ${textPrimaryOnSurface}`}>
            Copy this token now. It will not be shown again.
          </p>
          <div className="mt-3 flex flex-col gap-3 sm:flex-row sm:items-center">
            <code
              className={`break-all rounded-md px-3 py-2 select-text ${surfaceMutedBg} ${textPrimaryOnSurface}`}
            >
              {shownToken.token}
            </code>
            <button
              className={`min-h-11 rounded-md px-4 py-2 font-medium ${primaryButtonBg} ${primaryButtonHoverBg}`}
              onClick={copyToken}
              type="button"
            >
              Copy token
            </button>
          </div>
          {copyStatus === "idle" ? null : (
            <p
              aria-live="polite"
              className={`mt-2 text-sm ${copyStatus === "copied" ? successText : dangerText}`}
            >
              {copyStatus === "copied" ? "Copied" : "Copy failed - select the text"}
            </p>
          )}
          <p className={`mt-4 text-sm ${textSecondaryOnSurface}`}>
            Put this in <code>~/.config/legion/envoy.json</code>:
          </p>
          <pre
            className={`mt-2 overflow-x-auto rounded-md p-3 text-sm ${surfaceMutedBg} ${textPrimaryOnSurface}`}
          >
            <code>
              {JSON.stringify(
                {
                  dispatch: {
                    enabled: true,
                    serverUrl: window.location.origin,
                    token: shownToken.token,
                  },
                },
                null,
                2
              )}
            </code>
          </pre>
          <p className={`mt-2 text-sm ${textSecondaryOnSurface}`}>
            If the file exists, replace its <code>dispatch</code> section with this one and keep the
            rest. A session under a named omp profile, or with <code>CLAUDE_CONFIG_DIR</code> set,
            reads the <code>envoy.json</code> in that directory instead; any other session reads the
            file <code>ENVOY_CONFIG_FILE</code> names when it is set.
          </p>
          <p className={`mt-4 text-sm ${textSecondaryOnSurface}`}>
            Or set these environment variables:
          </p>
          <pre
            className={`mt-2 overflow-x-auto rounded-md p-3 text-sm ${surfaceMutedBg} ${textPrimaryOnSurface}`}
          >
            <code>{`DISPATCH_URL=${window.location.origin}\nDISPATCH_TOKEN=${shownToken.token}`}</code>
          </pre>
        </div>
      )}
    </section>
  );
}
