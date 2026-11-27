/**
 * What a check's head line, as `dispatch results` prints it, answers for whether a pass stands: the
 * words between `A pass stands: ` and the `.` the CLI always puts after them (`yes`, `no`, `not
 * known`; `yes, from ci` for a check CI proves), or undefined where the line has no such answer.
 */
export function passStandsAnswer(line: string): string | undefined {
  return /A pass stands: ([^.]*)\./.exec(line)?.[1];
}

/** Whether `line` answers exactly `answer`: `A pass stands: not known.` answers `not known` alone,
 *  though it holds the words `A pass stands: no`. */
export function passStands(line: string, answer: "yes" | "no" | "not known"): boolean {
  return passStandsAnswer(line) === answer;
}
