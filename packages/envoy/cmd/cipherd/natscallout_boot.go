package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/cipher/natshold"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/oidc"
)

// natsCalloutBoot is the verifiers of the platform tokens the NATS auth service's services present,
// for a deployment that runs it (cfg.NATS): one verifier per NATS audience set, on the key set the
// broker already reads for that issuer. First, RequireAudiences refuses the start when
// cfg.NATS.Services (which config.Load parsed, refusing there, naming the entry, every issuer
// name config.AccountIssuers refuses) names a service, an edge listener among them, whose kind of
// token has no audience. A Kubernetes token is checked against the issuer of
// BROKER_K8S_OIDC_ISSUER its iss names, through the clusters' issuer set the pod routes read in
// the background (loggedIssuers.audience), so the auth service reads no issuer of its own and an
// issuer that cannot be read stops no boot. A deployment that runs no auth service needs no NATS
// audience and no key set, and reads none.
func natsCalloutBoot(ctx context.Context, cfg config.Config, discovered verifiers, keysFor keysFunc) (natscallout.PlatformTokens, error) {
	if !cfg.NATS.Enabled() {
		return natscallout.PlatformTokens{}, nil
	}
	if err := natscallout.RequireAudiences(cfg.NATS.Services, cfg.NATS.K8sAudience, cfg.NATS.GoogleAudience); err != nil {
		return natscallout.PlatformTokens{}, err
	}
	tokens := natscallout.PlatformTokens{K8sIssuers: cfg.K8sOIDCIssuers, GoogleIssuer: cfg.GoogleOIDCIssuer}
	if cfg.NATS.K8sAudience != "" {
		tokens.K8s = enroll.K8sPodVerifier{Issuers: discovered.Clusters.audience(cfg.NATS.K8sAudience)}
	}
	if cfg.NATS.GoogleAudience != "" {
		keys := discovered.GoogleKeys
		if keys == nil {
			// A broker that takes no machine's Google token at its exchange read no Google key
			// set at discovery; the auth service's audience needs one.
			var err error
			if keys, err = keysFor(ctx, cfg.GoogleOIDCIssuer); err != nil {
				return natscallout.PlatformTokens{}, fmt.Errorf("read the Google issuer's keys for BROKER_NATS_GOOGLE_AUDIENCE: %w", err)
			}
		}
		tokens.Google = api.GoogleTokens{Verifier: oidc.NewWithKeys(cfg.GoogleOIDCIssuer, cfg.NATS.GoogleAudience, keys)}
	}
	return tokens, nil
}

// operatorEntry is the operator entry of services, the one identity that writes the NATS writer
// hold, and whether services lists one (config.ParseNATSServices admits one at most).
func operatorEntry(services []config.NATSService) (config.NATSService, bool) {
	for _, entry := range services {
		if entry.Kind == config.NATSOperator {
			return entry, true
		}
	}
	return config.NATSService{}, false
}

// refuseHoldReadWithoutOperator refuses BROKER_NATS_HOLD_READ=true without an operator entry in
// BROKER_NATS_SERVICES or without BROKER_NATS_HOLD_SIGNING_KEYS, naming the switch and what is
// missing. With the switch on, the auth service refuses every client but the operator until it
// reads a hold row signed by one of those keys, and only the operator's token writes one through
// the hold's routes: without either, no such row could ever be written, and the hub would admit
// nobody but an operator it may not even list. main runs it beside the other refusals of the
// configuration, before it migrates the database or reads an issuer's keys, so the refusal costs
// a deployment nothing but the start. It does not ask for a row: the broker starts with none, so
// /v1/nats-hold/restore can write the first one and the secrets broker serves meanwhile.
func refuseHoldReadWithoutOperator(cfg config.Config) error {
	if !cfg.NATS.HoldRead {
		return nil
	}
	var missing []string
	if _, listed := operatorEntry(cfg.NATS.Services); !listed {
		missing = append(missing, "operator entry in BROKER_NATS_SERVICES")
	}
	if len(cfg.NATS.HoldSigningKeys) == 0 {
		missing = append(missing, "key in BROKER_NATS_HOLD_SIGNING_KEYS")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("BROKER_NATS_HOLD_READ is true, and there is no %s: the auth service would refuse every client but the operator until it read a hold row the operator signed, which no one could write; list the operator and its keys, or set BROKER_NATS_HOLD_READ=false",
		strings.Join(missing, " and no "))
}

// migratedSchema is the schema st's migrations created their tables in, which the hold's routes
// and its read name the hold's tables by: the first schema of the pool's search path that exists,
// public in a deployment and a test's own where its database URL names one.
func migratedSchema(ctx context.Context, st *store.Store) (string, error) {
	var schema string
	if err := st.Pool.QueryRow(ctx, `select current_schema()`).Scan(&schema); err != nil {
		return "", fmt.Errorf("read the schema the broker's tables are in: %w", err)
	}
	if schema == "" {
		return "", fmt.Errorf("the database's search path names no schema the broker's tables are in")
	}
	return schema, nil
}

// holdRoutes is the wiring of the NATS writer hold's routes: the broker's own pool and the schema
// its migrations ran in, the operator's keys, the process's floor of the hold row, which the auth
// service's hold view shares, and the operator entry's account and issuers with the auth service's
// own verifier of its token. A broker that lists no operator entry, or runs no auth service and so
// verifies no NATS audience, gets no verifier, and its routes refuse every caller 403 not_operator.
func holdRoutes(cfg config.Config, st *store.Store, schema string, services []config.NATSService, tokens natscallout.PlatformTokens, floor *signedrecord.Floor) api.NATSHold {
	hold := api.NATSHold{Pool: st.Pool, Schema: schema, Keys: cfg.NATS.HoldSigningKeys, RowFloor: floor}
	if operator, listed := operatorEntry(services); listed && (tokens.K8s != nil || tokens.Google != nil) {
		hold.OperatorToken, hold.OperatorAccount, hold.OperatorGoogle, hold.OperatorIssuers = tokens.Verify, operator.Account, operator.Google, operator.Issuers
	}
	return hold
}

// openHoldPool opens the hold read's own pool of size connections, from the shared pool's own
// configuration so it signs in as the broker does, an RDS IAM token minter included (as
// store.Store.Listener opens its pool). It is a pool of its own so a hold read, which every
// decision of the auth service makes while BROKER_NATS_HOLD_READ is true, never waits on a
// connection a secret request holds, nor holds one a secret request waits on. It dials nothing
// until the first read, so a broker whose Postgres is down still starts.
func openHoldPool(st *store.Store, size int) (*pgxpool.Pool, error) {
	cfg := st.Pool.Config().Copy()
	cfg.MaxConns = int32(size)
	cfg.MinConns, cfg.MinIdleConns = 0, 0
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("open the hold read's connection pool: %w", err)
	}
	return pool, nil
}

// holdReader is the read a natscallout.HoldView takes, over pool: natshold.Read of schema's hold
// row, under its own deadline.
func holdReader(pool *pgxpool.Pool, schema string) func(ctx context.Context) (natshold.Reading, bool, error) {
	return func(ctx context.Context) (natshold.Reading, bool, error) {
		return natshold.Read(ctx, pool, schema)
	}
}
