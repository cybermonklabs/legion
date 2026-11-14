/**
 * `T` as Dispatch answered it: the one kind of value a cache write takes (`Cache.setDispatched`),
 * so the cache holds Dispatch's answers alone (R10 of dispatch://INFRA-299) - nothing a screen
 * built can be written there, because nothing a screen builds is one. Dispatch's body is held in
 * a private field (`#body`) that only this module's mint fills, never carried as the answer's own
 * fields, and TypeScript compares a class with such a field by its declaration: an object
 * literal, a spread of an answer or of its body, and an `Object.assign` onto either are none, by
 * type, and the write checks the field again as it runs (`answeredBody`), throwing for any other
 * value. Dispatch's body is read through `body`; reading it writes nothing.
 *
 * It is minted at the two places Dispatch's data enters the page: the API client's answer to a
 * write (`api/client.ts`' `send`, whose request builders name their answer `Answered`) and a live
 * frame as it is decoded (`live-socket.ts`' snapshot and delta, `event-log.ts`' event). A test's
 * rig plays Dispatch's part through `test-client.ts`' `answeredByDispatch`. Lint refuses naming
 * this type, and importing this module, everywhere else (`biome.json`), so no other module can
 * mint one; the class is not exported, so none can make one with `new`.
 */
class Answer<T> {
  readonly #body: T;

  constructor(body: T) {
    this.#body = body;
  }

  /** Dispatch's body, as it sent it. */
  get body(): T {
    return this.#body;
  }

  /** Whether `value` is an answer this module minted. */
  static is(value: unknown): value is Answer<unknown> {
    return typeof value === "object" && value !== null && #body in value;
  }

  /** `answer`'s body: throws for a value this module did not mint, which only a cast past the
   *  type checker can hand it. */
  static bodyOf<T>(answer: Answer<T>): T {
    if (!Answer.is(answer)) {
      throw new TypeError("a cache write takes Dispatch's answer (Answered), and this is none");
    }
    return answer.#body;
  }
}

export type Answered<T> = Answer<T>;

/** `body`, which Dispatch sent, as its answer: the one mint, for the modules named above. */
export function answered<T>(body: T): Answered<T> {
  return new Answer(body);
}

/** `answer`'s body, checked to be one this module minted (`Answer.bodyOf`): for the cache write
 *  (`Cache.setDispatched`) and the client's methods that answer a plain body (`sendBody`). */
export function answeredBody<T>(answer: Answered<T>): T {
  return Answer.bodyOf(answer);
}

/** Whether `value` is Dispatch's answer as this module minted it, for a test's rig that hands
 *  code a fixture as one (`test-client.ts`) and may be handed one already minted. */
export function isAnswered(value: unknown): value is Answered<unknown> {
  return Answer.is(value);
}
