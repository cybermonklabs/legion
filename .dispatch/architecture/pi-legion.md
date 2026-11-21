---
title: Oh My Pi Legion plugin
parent: legion
depends_on: [envoy-client, contracts, pi-shared]
paths: [packages/pi-legion]
---
The Oh My Pi plugin a Legion pane loads beside the Envoy plugin (`@sjawhar/pi-legion`): the Legion lifecycle tool, the daemon handshake and credential grants for root architects, phase workers and the controller, the task agents Legion's prompts dispatch, and the six Legion skills. It needs the Envoy plugin's interface from `pi-shared` at the version it speaks, and refuses to run a Legion session without it. The skill-scenario rig's (`scripts/skill-scenarios/rig.sh`) Dispatch database is the pgvector-on-PostgreSQL-18 image Envoy's `internal/testpg` pins, by the same digest.

Its Dispatch client, its listener publisher and the production-Dispatch reader of `scripts/skill-scenarios/` present the `credential` that `ActiveDispatchConfig` and `EnabledEnvoySettings` carry rather than a bearer string, so they work under a deployment whose Dispatch token is the secrets broker's DPoP-bound access token.

Every Dispatch request it makes, the handoff record's read among them, names it in its `User-Agent` as `pi-legion/<version>`, from its own `package.json` (`PI_LEGION_USER_AGENT`, `extensions/legion.ts`). It still calls `/api/v1`, which Dispatch deprecates from the first `legion-envoy` release that carries pull request #4274, the release that first serves `/api/v2`.
