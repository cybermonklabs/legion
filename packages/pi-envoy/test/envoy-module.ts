// The extension's module as a test loads it. Each case imports `./envoy.ts?<case>`, so it gets a
// module of its own, and a query string names no file: neither tsc nor Biome can type that import.
// A case annotates it with `EnvoyModule`, so the factory's call carries its real return type and
// Biome's `noFloatingPromises` refuses one left without `await`: the factory is async, and a case
// that does not wait for it reads handlers its first `await` has not registered yet.
import type { PiApi } from "@legion/pi-shared/pi-types";
import type envoyExtension from "../extensions/envoy";

/** The extension's module, its factory written out: Biome does not follow a `typeof` of another
 *  module's export, and `FactoryMatches` below holds the two together. */
export type EnvoyModule = { readonly default: (hostApi: PiApi) => Promise<void> };

type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
type Holds<T extends true> = T;
/** Fails tsc once the factory's type and `EnvoyModule`'s part ways. */
export type FactoryMatches = Holds<Same<typeof envoyExtension, EnvoyModule["default"]>>;
