import {
  type ChangeRequestEnding,
  outcomeLine,
  parseOutcomeLines,
  type SpecOutcomeRecord,
} from "@legion/contracts";
import {
  type CommandContext,
  type DispatchToolResult,
  optionalBoolean,
  optionalString,
  stringArg,
} from "./context";
import { refusalWithCode } from "./failures";

/** Who recorded a list: a session by its id, a person by their login. */
function recorder(record: SpecOutcomeRecord): string {
  return record.actor.kind === "session" ? `session ${record.actor.id}` : record.actor.id;
}

/** One record as `dispatch outcomes` prints it: a heading line, then each outcome in the line
 *  form, or a line saying the list is empty. */
function recordLines(record: SpecOutcomeRecord): string[] {
  const outcomes = record.outcomes ?? [];
  return [
    `${record.list} list of ${record.pull_request} at ${record.head}, recorded by ${recorder(record)} at ${record.recorded_at}:`,
    ...(outcomes.length === 0
      ? ["- (no outcome)"]
      : outcomes.map((outcome) => `- ${outcomeLine(outcome)}`)),
  ];
}

/** `dispatch outcomes`: with a body, record one list of outcomes of a pull request's head; with
 *  none, print the lists recorded for the issue. */
export async function outcomesCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor } = context;
  const issue = stringArg(args, "issue");
  const pullRequest = optionalString(args, "pull_request");
  const head = optionalString(args, "head");
  const list = optionalString(args, "list");
  const body = optionalString(args, "body");
  try {
    if (body !== undefined) {
      const parsed = parseOutcomeLines(body);
      if (parsed.problem !== undefined) throw new Error(`dispatch outcomes: ${parsed.problem}`);
      const record = await client.recordOutcomes(
        issue,
        {
          pull_request: stringArg(args, "pull_request"),
          head: stringArg(args, "head"),
          list: stringArg(args, "list"),
          outcomes: parsed.outcomes,
        },
        actor
      );
      const count = (record.outcomes ?? []).length;
      return {
        text: [
          `Recorded the ${record.list} list of ${record.pull_request} at ${record.head} for ${record.issue}: ${count} ${count === 1 ? "outcome" : "outcomes"} (record ${record.id}).`,
          ...recordLines(record).slice(1),
        ].join("\n"),
        details: { record },
      };
    }
    const records = await client.listOutcomes(issue, {
      ...(pullRequest === undefined ? {} : { pull_request: pullRequest }),
      ...(head === undefined ? {} : { head }),
      ...(list === undefined ? {} : { list }),
    });
    return {
      text:
        records.length === 0
          ? `No list of outcomes is recorded for ${issue}.`
          : records.flatMap(recordLines).join("\n"),
      details: { issue, records },
    };
  } catch (error) {
    throw refusalWithCode(error);
  }
}

/** The lines `dispatch spec-ending` prints: ready or waiting, each reason it waits, then each
 *  reason of the conclusion's judgment. */
function endingLines(ending: ChangeRequestEnding): string[] {
  return [
    ending.state,
    ...(ending.reasons ?? []).map((reason) => `${reason.code}: ${reason.detail}`),
    ...(ending.judgment_reasons ?? []).map((reason) => `${reason.code}: ${reason.detail}`),
  ];
}

/** `dispatch spec-ending`: whether the change request of an issue may land at a head. Either
 *  answer is a success; only an error fails the command. */
export async function specEndingCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client } = context;
  let ending: ChangeRequestEnding;
  try {
    ending = await client.changeRequestEnding(stringArg(args, "issue"), stringArg(args, "head"));
  } catch (error) {
    throw refusalWithCode(error);
  }
  return {
    text:
      optionalBoolean(args, "json") === true
        ? JSON.stringify(ending, null, 2)
        : endingLines(ending).join("\n"),
    details: { ending },
  };
}
