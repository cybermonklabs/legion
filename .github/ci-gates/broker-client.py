# The broker_client filter's gate: what job broker-client reads.
GATE = gate("broker_client", "broker-client", reads=reads(
    ("packages/cipher-client/**", f"{at('broker-client', 'Typecheck the broker client')} and test in "
                                  "packages/cipher-client; its modules import one another, node:, bun:, the "
                                  "third-party packages its manifest names and @legion/contracts"),
    (CONTRACTS_SRC, "packages/cipher-client/src/token-client.ts imports @legion/contracts/trailing-slash"),
    (TEST_FIXTURES_SRC, "packages/cipher-client/src/__tests__ import @legion/test-fixtures"),
    (f"{ENVOY}/internal/ciphertoken/testdata/connection-proof-go.json", "packages/cipher-client/src/__tests__/dpop.test.ts reads the Go signer's committed connection proof and verifies it with jose"),
    (f"{ENVOY}/internal/ciphertoken/testdata/connection-proof-ts.json", "packages/cipher-client/src/__tests__/dpop.test.ts reads this signer's committed connection proof and holds it to the signer's claim names"),
    (f"{ENVOY}/internal/cipher/oauth/testdata/refusal-descriptions.json", "packages/cipher-client/src/__tests__/refusal-corpus.test.ts reads the broker's refusal texts as its OAuth library writes them, which internal/cipher/oauth's refusal-corpus test writes"),
    (f"{ENVOY}/internal/cipher/oauth/testdata/refusal-codes.json", "packages/cipher-client/src/__tests__/refusal-corpus.test.ts reads the OAuth codes the broker's library writes, which internal/cipher/oauth's refusal-corpus test writes, and holds LIBRARY_OAUTH_ERROR_CODES to them"),
    (f"{ENVOY}/internal/cipherclient/testdata/wire.json", "packages/cipher-client/src/__tests__/wire.test.ts reads the Go package's own corpus by relative path (CORPUS_PATH) - that file itself, never a copy, so the two clients are held to one wire; refusal-corpus.test.ts reads it for the broker's own refusal codes"),
    (f"{ENVOY}/internal/cipherclient/testdata/renewal.json", "packages/cipher-client/src/__tests__/renewal.test.ts reads the Go package's renewal corpus by relative path (RENEWAL_CORPUS), which cipherclient's renewal test replays too, so both clients renew at the moment it names"),
) + dispatch_api_types(at("broker-client", "Typecheck the broker client")) + bun_job("broker-client"))
