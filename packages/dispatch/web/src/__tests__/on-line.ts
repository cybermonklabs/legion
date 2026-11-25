/** Runs `run` while the browser says it is online or not (`navigator.onLine`), and once `run`
 *  has settled, answered or thrown, gives the browser back its own answer, so no later row finds
 *  the browser offline. */
export function withOnLine<T>(onLine: boolean, run: () => Promise<T>): Promise<T> {
  const descriptor = Object.getOwnPropertyDescriptor(Navigator.prototype, "onLine");
  Object.defineProperty(navigator, "onLine", { configurable: true, value: onLine });
  return run().finally(() => {
    Reflect.deleteProperty(navigator, "onLine");
    if (descriptor !== undefined) {
      Object.defineProperty(Navigator.prototype, "onLine", descriptor);
    }
  });
}
