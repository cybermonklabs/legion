/** The answers to a mocked request that a row holds: `next`, given as the request's
 *  implementation, makes a promise of `T` at each call and puts what resolves it on `answers`, in
 *  the order the requests were made. The row gives each answer when it chooses, inside `act`, so
 *  what the answer sets off runs there (`docs/testing.md`, "The act rule of a Dispatch unit
 *  test"). */
export function heldAnswers<T>(): {
  readonly answers: readonly ((value: T) => void)[];
  readonly next: () => Promise<T>;
} {
  const answers: ((value: T) => void)[] = [];
  return {
    answers,
    next: () => {
      const { promise, resolve } = Promise.withResolvers<T>();
      answers.push(resolve);
      return promise;
    },
  };
}
