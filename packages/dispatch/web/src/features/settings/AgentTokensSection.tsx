import { DISPATCH_SCOPES, DISPATCH_TOKEN_SCOPES, MAX_TOKEN_PROJECTS } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api } from "../../api/client";
import { projectsQuery } from "../../api/queries";
import { replaceQueryData } from "../../api/query-client";
import type {
  AgentToken,
  CreatedAgentToken,
  DispatchScope,
  DispatchTokenScope,
  Project,
} from "../../api/types";
import { MultiSelect } from "../../components/MultiSelect";
import { QueryError } from "../../components/QueryError";
import { useCopyFeedback } from "../../hooks/useCopyFeedback";
import { useRestrictedMember } from "../../hooks/useRestrictedMember";
import { compareTimestamps } from "../../lib/timestamps";
import {
  borderDefault,
  card,
  checkboxAccent,
  dangerHoverText,
  dangerText,
  inputClasses,
  linkHoverText,
  linkText,
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
import {
  settingsSubmitButton,
  settingsTableHead,
  settingsTableRow,
  settingsTableWrapper,
} from "./classes";

const agentTokensQueryKey = ["agent-tokens"] as const;

const whoAmIScopesQueryKey = ["whoami-scopes"] as const;

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
    "Reviews documents, answers approvals and keeps decisions for you, approves the credentials agents ask you for, and changes who may see your projects and the Slack channels they post to.",
  id: "approve",
  label: "Approve and control credentials",
};

const READ_ONLY: TokenScope<"read"> = {
  allows: "Only reads: Dispatch refuses it every write, whatever else it holds.",
  id: "read",
  label: "Read only",
};

const SET_P0: TokenScope<"p0"> = {
  allows: "Set an issue's priority to P0.",
  id: "p0",
  label: "Set P0",
};

/** The scopes a token that writes can hold, in the order a row shows them. The ids are the
 *  contract's own (`DISPATCH_SCOPES`), so a row cannot offer a scope the server does not mint, and
 *  a row offers no Read only: no token that writes becomes read only (the server refuses it, 400
 *  TOKEN_SCOPES_INPUT), since a person makes a new token for that. */
const WRITING_SCOPES: readonly TokenScope<DispatchScope>[] = [ACT_AS_YOU, APPROVE, SET_P0];

/** Every word the mint form offers, in the order it shows them (`DISPATCH_TOKEN_SCOPES`). */
const MINT_SCOPES: readonly TokenScope<DispatchTokenScope>[] = [
  ACT_AS_YOU,
  APPROVE,
  SET_P0,
  READ_ONLY,
];

/** `offered` without Set P0: what a row and the mint form offer until Dispatch says the person at
 *  the browser holds `p0` (`offersP0` in `AgentTokensSection`). A build before p0 knows no such
 *  word, and a credential gives no scope it does not hold itself, so a box drawn before then would
 *  be a refusal waiting to happen. */
function withoutSetP0<Id extends DispatchTokenScope>(
  offered: readonly TokenScope<Id>[]
): readonly TokenScope<Id>[] {
  return offered.filter(({ id }) => id !== "p0");
}

const WRITING_SCOPES_WITHOUT_P0 = withoutSetP0(WRITING_SCOPES);

const MINT_SCOPES_WITHOUT_P0 = withoutSetP0(MINT_SCOPES);

/** What a new token holds unless the reader changes it: every scope but `p0`, as a mint that
 *  names none gives. The reader ticks Set P0 for a token that is to hold it. */
const NEW_TOKEN_SCOPES: readonly DispatchTokenScope[] = DISPATCH_SCOPES.filter(
  (scope) => scope !== "p0"
);

/** What a restricted member's token may do, which its row and the mint form say in place of the
 *  boxes: Dispatch gives such a token no scope, and makes them no read-only token (403
 *  RESTRICTED_MEMBER for a mint or an edit naming any word, `p0` and `read` among them), since it
 *  is their agent's, held to the one project it names. */
const RESTRICTED_TOKEN_NOTE =
  "Works as you in the one project it names, and approves nothing: no login, no token, no credential request.";

/** What a restricted member's read-only token says in place of its scopes and its projects. Such a
 *  token was made while they were a full member. It lists no project, and Dispatch takes no edit
 *  of a read-only token (400 TOKEN_SCOPES_INPUT), so it acts for nobody (403 RESTRICTED_MEMBER)
 *  until they revoke it and make a token that names their one project. The token list answers it
 *  holding `read` alone, which is how the row knows it is read-only. */
const RESTRICTED_READ_ONLY_NOTE =
  "Read only, made before your access changed: it works nowhere now. Revoke it, then make a token that names your one project.";

/** The words a token holds, `p0` and `read` among them. The route table carries them as plain
 *  strings; `DISPATCH_TOKEN_SCOPES` is the closed list each is one of, so a word this build does
 *  not know is left out rather than drawn as a box nobody can clear, and a token whose list came
 *  back absent holds none. A lost mint is replaced by a token holding this set, so a read-only
 *  token's replacement is read only too. */
function heldScopes(scopes: readonly string[] | null): DispatchTokenScope[] {
  return DISPATCH_TOKEN_SCOPES.filter((known) => scopes?.includes(known) ?? false);
}

/** The whole set after one box is ticked or cleared: the words `offered` draws, in its order, and
 *  after them each word of `held` it does not draw, kept as it is. A row draws Set P0 only once
 *  Dispatch says the person at the browser holds `p0`, so a change made on a row before then
 *  still sends the `p0` its token holds, and no change drops it in silence. No token holds
 *  `approve` without `person`, so ticking Approve ticks Act as you and clearing Act as you clears
 *  Approve, and leaves Set P0, which needs no other scope; and no token holds `read` beside
 *  `approve` or `p0`, since approving and setting P0 are writes, so ticking Read only clears both
 *  and ticking either clears Read only. The server refuses every such pair (400
 *  TOKEN_SCOPES_INPUT), and a form that offered one would be a refusal waiting to happen. No box
 *  is ever disabled for these rules: each press is taken, and the other boxes follow it. */
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
    if (scope === "p0") next.delete("read");
    if (scope === "read") {
      next.delete("approve");
      next.delete("p0");
    }
  } else {
    next.delete(scope);
    if (scope === "person") next.delete("approve");
  }
  const drawn = offered.filter((each) => next.has(each.id)).map((each) => each.id);
  const kept = held.filter((each) => next.has(each) && !offered.some(({ id }) => id === each));
  return [...drawn, ...kept];
}

/** The scope boxes `offered`, for a token's row and for the mint form. `subject` is the token
 *  they belong to, so the boxes of one row are told from another row's by name. */
function TokenScopeChoices<Id extends DispatchTokenScope>({
  chosen,
  disabled,
  offered,
  onChoose,
  subject,
}: {
  chosen: readonly Id[];
  disabled: boolean;
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
              aria-label={`${scope.label} (${subject})`}
              checked={chosen.includes(scope.id)}
              className={`size-4 shrink-0 rounded ${checkboxAccent}`}
              disabled={disabled}
              onChange={(event) =>
                onChoose(scopesAfterChoice(offered, chosen, scope.id, event.target.checked))
              }
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

/** The projects a token may use, for a token's row and for the mint form. A token that names no
 *  project uses the public projects only. A private project needs a token that names it: a full
 *  member's token uses every public project and the private projects it names, and a restricted
 *  member's uses the projects it names alone. It offers the projects the reader sees
 *  (`projects`), and any the token names that this list does not hold - a list read before the
 *  token's, or not read yet - ticked, so
 *  they can be taken off. The choice is staged while the picker is open and handed to `onChoose`
 *  once it closes, and only when it changed.
 *  `subject` is the token it belongs to, as for the scope boxes. A restricted member's token
 *  names exactly one project (`single`): a pick replaces the one picked before, and a mint that
 *  picks none takes the one project they see, which Dispatch refuses where they see several.
 *
 *  Dispatch cuts a token's list to what the reader sees, and tells the person's own browser when
 *  the cut removed some (`hidden`): such a token is never shown as one of the public projects
 *  only, and the picker says a change drops them, since an edit names the whole list and Dispatch
 *  refuses a project the reader does not see. A token whose every project is hidden opens the
 *  picker with nothing ticked, so no change of it sends anything; the button beside the note does
 *  what the note says a change does, once the reader confirms it: it saves the list they see, the
 *  empty list where they see none, which makes the token one of the public projects only. */
function TokenProjectsChoice({
  chosen,
  disabled,
  hidden = false,
  onChoose,
  projects,
  single = false,
  subject,
}: {
  chosen: readonly string[];
  disabled: boolean;
  hidden?: boolean;
  onChoose: (projects: string[]) => void;
  projects: readonly Project[];
  single?: boolean;
  subject: string;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<string[]>([]);
  const [error, setError] = useState<string | null>(null);
  // A closed picker draws no option (MultiSelect), so its options are built only while it is open:
  // a page of tokens re-renders every row's picker on each change, and each would build them all.
  const names = new Map(open ? projects.map((listed) => [listed.key, listed.name]) : []);
  const options = open ? [...names.keys(), ...chosen.filter((key) => !names.has(key))] : [];
  // The name holds the text the trigger shows, after whose token it is, so a reader who says what
  // they see reaches it (WCAG 2.5.3, Label in Name).
  const shown =
    chosen.length === 0
      ? hidden
        ? "No project you can see"
        : single
          ? "Your one project"
          : "Public projects only"
      : hidden
        ? `${chosen.join(", ")}, and projects you cannot see`
        : chosen.join(", ");
  return (
    <>
      <MultiSelect
        disabled={disabled}
        emptyMessage="No project matches that."
        error={error}
        label="Projects"
        onChange={(picked) => {
          const next = single ? picked.filter((key) => !draft.includes(key)).slice(-1) : picked;
          if (next.length > MAX_TOKEN_PROJECTS) {
            setError(`A token names at most ${MAX_TOKEN_PROJECTS} projects.`);
            return;
          }
          setError(null);
          setDraft(next);
        }}
        onOpenChange={(next) => {
          if (next) {
            setDraft([...chosen]);
            setError(null);
            setOpen(true);
            return;
          }
          if (!open) return;
          setOpen(false);
          if (draft.length !== chosen.length || draft.some((key) => !chosen.includes(key))) {
            onChoose(draft);
          }
        }}
        open={open}
        optionDetail={(key) => names.get(key)}
        options={options}
        searchLabel="Search projects"
        selected={open ? draft : chosen}
        triggerAriaLabel={`Projects (${subject}): ${shown}`}
      >
        {shown}
      </MultiSelect>
      {hidden ? (
        <div className="mt-1 max-w-xs space-y-1">
          <p className={`text-xs ${textMutedOnSurface}`}>
            A change here drops the projects you cannot see.
          </p>
          <button
            aria-label={`Drop the projects you cannot see (${subject})`}
            className={`text-xs underline ${linkText} ${linkHoverText} disabled:cursor-not-allowed disabled:opacity-50`}
            disabled={disabled}
            onClick={() => {
              const after =
                chosen.length === 0
                  ? "uses the public projects only"
                  : `names ${chosen.join(", ")} alone`;
              if (
                window.confirm(
                  `Drop the projects you cannot see from ${subject}? It then ${after}, and a project you cannot see cannot be added back.`
                )
              ) {
                onChoose([...chosen]);
              }
            }}
            type="button"
          >
            Drop the projects you cannot see
          </button>
        </div>
      ) : null}
    </>
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
  /** A restricted member's token is their agent's: it holds no scope and names exactly one
   *  project they see, and Dispatch refuses any other (403 RESTRICTED_MEMBER). */
  const restricted = useRestrictedMember();
  const [name, setName] = useState("");
  /** What the mint form's boxes hold: Act as you and Approve until the reader changes them. */
  const [newScopes, setNewScopes] = useState<readonly DispatchTokenScope[]>(NEW_TOKEN_SCOPES);
  /** The projects the mint form names: none, the public projects only, until the reader picks. */
  const [newProjects, setNewProjects] = useState<readonly string[]>([]);
  /** Whether the mint form makes a read-only token, which lists no project: Dispatch refuses a
   *  mint naming projects beside read (400 TOKEN_PROJECTS_INPUT). While Read only is ticked, the
   *  projects picker is disabled and shows none, and the mint names none; the projects picked
   *  before it was ticked are named again once it is cleared. A restricted member's form draws no
   *  Read only. */
  const mintsReadOnly = !restricted && newScopes.includes("read");
  const [shownToken, setShownToken] = useState<CreatedAgentToken>();
  /** When the token list was last read again after a mint whose answer was lost. */
  const [rereadAt, setRereadAt] = useState<number | undefined>(undefined);
  const { copy, reset: resetCopyStatus, status: copyStatus } = useCopyFeedback();
  const tokens = useQuery({ queryKey: agentTokensQueryKey, queryFn: () => api.listAgentTokens() });
  /** The projects a token may name: those the reader sees. */
  const projects = useQuery(projectsQuery()).data ?? [];
  // The scopes Dispatch says the person at this browser holds (`GET /api/v1/whoami`). Set P0 is
  // drawn, on the mint form and on each row of a token that writes, only once they name `p0`:
  // while the read has not answered, where it failed, and where a build before p0 answered it, no
  // box is drawn and the form mints as it did before p0. A failed read says nothing on the page,
  // since the rest of the section does not need it.
  const callerScopes = useQuery({
    queryKey: whoAmIScopesQueryKey,
    queryFn: () => api.whoAmIScopes(),
  });
  const offersP0 = callerScopes.data?.includes("p0") === true;
  const mintScopes = offersP0 ? MINT_SCOPES : MINT_SCOPES_WITHOUT_P0;
  // The form shows a refused mint beside the name it was refused over. A mint that names no
  // project sends no `projects`, so Dispatch gives the token the caller's own list.
  const createToken = useKeyedWrite({
    inline: true,
    label: (input: {
      name: string;
      before: readonly string[];
      projects: readonly string[];
      scopes: readonly DispatchTokenScope[];
    }) => `New agent token ${input.name}`,
    method: "POST",
    // The whole set the boxes hold, `read` among them for a read-only token.
    write: ({ name: label, projects: named, scopes }, bounds) =>
      api.createAgentToken(
        named.length === 0
          ? { name: label, scopes: [...scopes] }
          : { name: label, projects: [...named], scopes: [...scopes] },
        bounds
      ),
    onSuccess: (created) => {
      setName("");
      setNewScopes(NEW_TOKEN_SCOPES);
      setNewProjects([]);
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
    write: (id: string, bounds) => api.revokeAgentToken(id, bounds),
    onSuccess: (_result, id) => {
      replaceQueryData<AgentToken[]>(queryClient, agentTokensQueryKey, (current = []) =>
        current.map((token) =>
          token.id === id ? { ...token, revoked_at: new Date().toISOString() } : token
        )
      );
    },
  });
  /** One writing token's boxes or its projects, written as the whole set the token is to hold,
   *  which never holds `read`: what its boxes hold, and each word it holds that its row does not
   *  draw (`scopesAfterChoice`), or none for a restricted member's token. The edit always names
   *  the scopes, and names the projects only when they are what changed, which leaves them as
   *  they are otherwise. The row reads both back from the list, so it follows the write rather
   *  than the press. */
  const updateToken = useKeyedWrite({
    label: (input: { id: string; name: string; projects?: string[]; scopes: DispatchScope[] }) =>
      input.projects === undefined
        ? `What agent token ${input.name} may do`
        : `Which projects agent token ${input.name} may use`,
    method: "PATCH",
    write: ({ id, projects: named, scopes }, bounds) =>
      api.updateAgentToken(
        id,
        named === undefined ? { scopes } : { projects: named, scopes },
        bounds
      ),
    // A landing whose answer was lost writes the row as the press did. An edit that names the
    // projects names only ones the reader sees, so the row lists none they cannot see, as the
    // server's own answer would say; a scope edit leaves the list, and what it hides, alone.
    onSuccess: (updated, input) => {
      replaceQueryData<AgentToken[]>(queryClient, agentTokensQueryKey, (current = []) =>
        current.map((token) =>
          token.id === input.id
            ? (updated ?? {
                ...token,
                scopes: input.scopes,
                ...(input.projects === undefined
                  ? {}
                  : { projects: input.projects, projects_hidden: false }),
              })
            : token
        )
      );
    },
  });
  /** Every token the page lists now, read at a press. */
  const listedIds = () => (tokens.data ?? []).map((token) => token.id);
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    // An emptied form - a mint that landed - sends nothing, whatever submits it.
    if (name.trim() === "") return;
    // The boxes the form draws now, and no word it does not: a Set P0 ticked before a later read
    // of whoami named no p0 is no longer drawn, so the reader no longer sees it, and it is not sent.
    const drawn = newScopes.filter((scope) => mintScopes.some(({ id }) => id === scope));
    createToken.mutate({
      before: listedIds(),
      name,
      projects: mintsReadOnly ? [] : newProjects,
      // A restricted member's form draws no box, and their mint names no scope.
      scopes: restricted ? [] : drawn,
    });
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
   *  mints another under its label, holding the scopes and the projects the lost one held. */
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
          projects: token.projects ?? [],
          scopes: restricted ? [] : heldScopes(token.scopes),
        }),
    });
  };
  const replacing = revokeToken.isPending || createToken.isPending;
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
        Create a token for an agent you run. Its writes are attributed to you.{" "}
        {restricted
          ? "Your token works in the one project it names, and holds no right of yours to approve anything."
          : "A token that names no project uses only the public projects you see. A private project needs a token that names it: your token uses every public project you see and the private projects it names, or, if you are a restricted member, the projects it names alone."}
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
        <div className={settingsTableWrapper}>
          <table className="w-full text-left text-sm">
            <thead className={settingsTableHead}>
              <tr>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Name
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Token
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Created
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Last used
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  What it may do
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Projects
                </th>
                <th className="px-4 py-3" scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {tokens.data.length === 0 ? (
                <tr>
                  <td className={`px-4 py-5 ${textMutedOnSurface}`} colSpan={7}>
                    No agent tokens yet.
                  </td>
                </tr>
              ) : (
                tokens.data.map((token) => {
                  const held = heldScopes(token.scopes);
                  // A read-only token's scopes never change and it lists no project (400
                  // TOKEN_SCOPES_INPUT for any edit of it), so its row draws both as words, with no
                  // box and no picker: such a token reads the public projects its person sees.
                  const readOnly = held.includes("read");
                  // What an edit of a writing token names as its scopes: the set it holds.
                  const writing = DISPATCH_SCOPES.filter((scope) => held.includes(scope));
                  return (
                    <tr className={settingsTableRow} key={token.id}>
                      <td className={`px-4 py-3 font-medium ${textPrimaryOnSurface}`}>
                        {token.name}
                      </td>
                      <td className={`px-4 py-3 font-mono ${textSecondaryOnSurface}`}>
                        {`dsp_${token.prefix}…`}
                      </td>
                      <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                        <Timestamp at={token.created_at} />
                      </td>
                      <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                        {token.last_used_at === null ? (
                          "Never used"
                        ) : (
                          <Timestamp at={token.last_used_at} />
                        )}
                      </td>
                      <td className="px-4 py-3">
                        {restricted ? (
                          <p className={`max-w-xs text-xs ${textMutedOnSurface}`}>
                            {readOnly ? RESTRICTED_READ_ONLY_NOTE : RESTRICTED_TOKEN_NOTE}
                          </p>
                        ) : readOnly ? (
                          <ReadOnlyScopes held={held} subject={token.name} />
                        ) : (
                          <TokenScopeChoices
                            chosen={writing}
                            disabled={token.revoked_at !== null || updateToken.isPending}
                            offered={offersP0 ? WRITING_SCOPES : WRITING_SCOPES_WITHOUT_P0}
                            onChoose={(scopes) =>
                              updateToken.mutate({ id: token.id, name: token.name, scopes })
                            }
                            subject={token.name}
                          />
                        )}
                      </td>
                      <td className="px-4 py-3">
                        {readOnly ? (
                          <p className={`text-sm font-medium ${textSecondaryOnSurface}`}>
                            {restricted ? "No project" : "Public projects only"}
                          </p>
                        ) : (
                          <TokenProjectsChoice
                            chosen={token.projects ?? []}
                            hidden={token.projects_hidden === true}
                            disabled={token.revoked_at !== null || updateToken.isPending}
                            onChoose={(named) =>
                              updateToken.mutate({
                                id: token.id,
                                name: token.name,
                                projects: named,
                                scopes: restricted ? [] : writing,
                              })
                            }
                            projects={projects}
                            single={restricted}
                            subject={token.name}
                          />
                        )}
                      </td>
                      <td className="px-4 py-3 text-right">
                        {token.revoked_at === null ? (
                          <button
                            aria-label={`Revoke ${token.name}`}
                            className={`min-h-11 px-2 font-medium disabled:cursor-not-allowed disabled:opacity-50 ${dangerText} ${dangerHoverText}`}
                            disabled={revokeToken.isPending}
                            onClick={() => revoke(token)}
                            type="button"
                          >
                            Revoke
                          </button>
                        ) : (
                          <span className={textMutedOnSurface}>Revoked</span>
                        )}
                      </td>
                    </tr>
                  );
                })
              )}
            </tbody>
          </table>
        </div>
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
          <button className={settingsSubmitButton} disabled={createToken.isPending} type="submit">
            New token
          </button>
        </div>
        {restricted ? (
          <p className={`max-w-xs text-xs ${textMutedOnCanvas}`}>{RESTRICTED_TOKEN_NOTE}</p>
        ) : (
          <TokenScopeChoices
            chosen={newScopes}
            disabled={createToken.isPending}
            offered={mintScopes}
            onChoose={setNewScopes}
            subject="new token"
          />
        )}
        <div
          className={`flex flex-wrap items-center gap-2 text-sm font-medium ${textSecondaryOnCanvas}`}
        >
          Projects
          <TokenProjectsChoice
            chosen={mintsReadOnly ? [] : newProjects}
            disabled={createToken.isPending || mintsReadOnly}
            onChoose={setNewProjects}
            projects={projects}
            single={restricted}
            subject="new token"
          />
        </div>
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
              disabled={replacing}
              onClick={() => candidates[0] !== undefined && replaceLost(candidates[0])}
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
                      disabled={replacing}
                      onClick={() => replaceLost(token)}
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
