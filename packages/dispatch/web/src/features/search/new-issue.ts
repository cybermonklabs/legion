import { api } from "../../api/client";
import type { Issue, NewIssueSettings } from "../../api/types";
import type { Attempt, Writes } from "../writes/write-store";
import { press, refusalText } from "./offer";

/**
 * The whole content of the Verification document a new issue from the bar gets: the words the
 * person typed, as one `check` block carrying no attributes. The words are a declaration of what
 * they want to be able to do, so they are the issue's first verification criterion word for
 * word; the block carries no attributes so Dispatch names no surface, no command and no control
 * of its own, and writes no word of its own into the issue. The spec and the plan are written
 * under it afterwards, by whoever takes the issue.
 */
export function firstCriterion(words: string): string {
  return `:::check{}\n${words}\n:::\n`;
}

/** What one acceptance of the offer writes. */
export interface NewIssueWrite {
  /** The project of the page the bar was opened on. */
  readonly project: string;
  /** The label the new issue carries and the session it goes to, as the deployment set them. */
  readonly settings: NewIssueSettings;
  /** The words the person typed, as they typed them. */
  readonly words: string;
}

/** A write onto an issue the create has already made, which Dispatch refused: which write it
 *  was, in the words the bar reports it in, and the reason Dispatch gave. */
export interface RefusedWrite {
  readonly what: string;
  readonly why: string;
}

/** What one acceptance made. */
export interface FiledIssue {
  readonly issue: Issue;
  /** The session the issue was routed to and its first message went to; `""` where the
   *  deployment names none, which is the issue made with no conversation started on it. */
  readonly session: string;
  /** The words it was made from, so a bar whose box has moved on drops the result. */
  readonly words: string;
  /** The first write onto the issue Dispatch refused, where one was: the filing ended there,
   *  and the issue stands without it. Undefined where every write landed. */
  readonly refused?: RefusedWrite;
}

/**
 * One write onto an issue the create has already made. The issue exists whatever becomes of
 * this, so a refusal is answered rather than thrown: a throw would leave the bar saying it
 * could not make an issue it has made, and offering to make it a second time, which is the
 * husk R14 (dispatch://INFRA-1042/spec#b-r14) exists to stop.
 */
async function writeOnto<T>(
  writes: Writes,
  what: string,
  label: string,
  write: Attempt<T>
): Promise<RefusedWrite | undefined> {
  try {
    await press(writes, label, write);
    return undefined;
  } catch (error) {
    return { what, why: refusalText(error, "the write did not reach Dispatch.") };
  }
}

/**
 * Files the person's typed words as an issue, through the routes every other client writes
 * through: the create, whose own related-item check answers whether anything already in the
 * project is a close match, then the Verification document holding the one criterion, and then,
 * where the deployment names a session, the issue's route and its first message to that session.
 *
 * The create alone decides whether there is an issue. Once it lands the issue is the answer
 * whatever becomes of the writes onto it: the first of those Dispatch refuses ends the filing
 * and is answered as the issue's `refused`, so the bar reports it against the issue it has
 * rather than offering to make another one. A refused create throws, because then there is no
 * issue and the row is still the way to make one.
 *
 * Answers `undefined` for a create Dispatch made and had no answer to give for: the issue is
 * made, and nothing here knows which one, so the rest is the person's to write on it.
 */
export async function fileNewIssue(
  writes: Writes,
  { project, settings, words }: NewIssueWrite
): Promise<FiledIssue | undefined> {
  const issue = await press(writes, `New issue “${words}”`, (bounds) =>
    api.createIssue(
      { project, title: words, ...(settings.label === "" ? {} : { labels: [settings.label] }) },
      bounds
    )
  );
  if (issue === null) {
    return undefined;
  }
  const filed = (refused?: RefusedWrite): FiledIssue => ({
    issue,
    refused,
    session: settings.session,
    words,
  });
  const criterion = await writeOnto(
    writes,
    "its first criterion",
    `The first criterion of ${issue.key}`,
    (bounds) =>
      api.uploadArtifact(
        { issue: issue.key },
        { content: firstCriterion(words), name: "verification.md" },
        bounds
      )
  );
  if (criterion !== undefined || settings.session === "") {
    return filed(criterion);
  }
  const target = `session:${settings.session}`;
  const route = await writeOnto(
    writes,
    `its route to the session ${settings.session}`,
    `${issue.key} to ${settings.session}`,
    (bounds) => api.patchIssue(issue.key, { route: target }, bounds)
  );
  if (route !== undefined) {
    return filed(route);
  }
  return filed(
    await writeOnto(writes, "its first message", `The first message on ${issue.key}`, (bounds) =>
      api.createMessage(issue.key, { body: words, delivery: "aside", target }, bounds)
    )
  );
}
