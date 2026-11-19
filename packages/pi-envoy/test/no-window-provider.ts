// A stand-in model with a context window of zero, for the real-binary row of
// extensions/session-surface-omp.test.ts that switches a session to one (dispatch://INFRA-4325).
// The test loads this file beside envoy.ts, names the stand-in gateway in
// NO_WINDOW_PROVIDER_BASE_URL and enables the provider in the profile (`writeStandinProfile`'s
// `registered`). It lives under `test/`, which the package never publishes (`package.json`
// `files`).
//
// An extension is the one way the pinned Oh My Pi (`.omp-pin`) gives a session such a model.
// models.yml cannot carry one: the pin refuses a `contextWindow` of zero or below there and fails
// the whole file (`config/models-config.ts:116-119`), and a model that leaves the field out gets
// 128,000 (`config/custom-models.ts:137`). A provider an extension registers is checked in
// "runtime-register" mode, which skips that check, and its explicit window outlasts the model's
// finalization (`custom-models.ts:158`), so the session's `getContextUsage()` answers a window of
// zero (`session/session-stats.ts:287-288`).

/** The one method of the host's `ExtensionAPI` this file calls, as the pin declares it
 *  (`extensibility/extensions/types.ts`, `registerProvider` and `ProviderModelConfig`). */
interface ProviderHost {
  registerProvider(
    name: string,
    config: {
      readonly baseUrl: string;
      readonly api: "anthropic-messages";
      readonly apiKey: string;
      readonly models: readonly {
        readonly id: string;
        readonly name: string;
        readonly reasoning: boolean;
        readonly input: readonly "text"[];
        readonly cost: {
          readonly input: number;
          readonly output: number;
          readonly cacheRead: number;
          readonly cacheWrite: number;
        };
        readonly contextWindow: number;
        readonly maxTokens: number;
      }[];
    }
  ): void;
}

/** The model this file registers, as `set_model` names it. */
export const NO_WINDOW_MODEL = { provider: "standin-nowindow", modelId: "nowindow" } as const;

export default function noWindowProvider(pi: ProviderHost): void {
  const baseUrl = process.env.NO_WINDOW_PROVIDER_BASE_URL;
  if (baseUrl === undefined) throw new Error("NO_WINDOW_PROVIDER_BASE_URL is unset");
  pi.registerProvider(NO_WINDOW_MODEL.provider, {
    baseUrl,
    api: "anthropic-messages",
    // The stand-in reads no key. The value names itself a placeholder, which the pre-push leak
    // check takes as no credential.
    apiKey: "standin-not-a-secret",
    models: [
      {
        id: NO_WINDOW_MODEL.modelId,
        name: "Stand-in with no context window",
        reasoning: false,
        input: ["text"],
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
        contextWindow: 0,
        maxTokens: 8000,
      },
    ],
  });
}
