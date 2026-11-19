package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// The census of the broker's credential columns (migration 0033): every column that decides
// whether a broker credential is live, for which person and machine it acts, and which key may
// present it, the transitions the broker's own writers make, and the guard that refuses every
// other. Each refusal row names the guard, and its control turns that guard off in the same
// setup and sees the statement pass. Each allowed row is one writer's own statement, with
// literals where the writer passes parameters, and names the writer. Every row runs in a
// transaction of its own that is rolled back, so no row's write reaches the next, and a control's
// disabled trigger reaches no other row.

// The credentials the rows are built from.
const (
	// columnsDeviceLogin is a live machine login a device approval minted: it names no subject.
	columnsDeviceLogin = `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at)
		values ('00000000-0000-0000-0000-0000000000c1', 'ada@example.com', 'ada-devbox', 'thumbprint-device', '{"kty":"EC"}', now() + interval '30 days')`
	// columnsServiceLogin is a live token login of a service account, which names its subject.
	columnsServiceLogin = `insert into launcher_credentials (id, service, host, key_thumbprint, public_jwk, subject, expires_at)
		values ('00000000-0000-0000-0000-0000000000c2', 'legion-daemon', 'example-ns/example-daemon', 'thumbprint-service', '{"kty":"EC"}',
			'system:serviceaccount:example-ns:example-daemon', now() + interval '1 hour')`
	columnsRevokeServiceLogin = `update launcher_credentials set revoked_at = now() where id = '00000000-0000-0000-0000-0000000000c2'`
	// columnsAccessToken is the device login's access token, bound to ada's box key.
	columnsAccessToken = `insert into oauth_access_tokens (signature, request_id, credential_id, request, expires_at)
		values ('access-1', 'request-1', '00000000-0000-0000-0000-0000000000c1',
			'{"client_id":"agent-secrets","session":{"jwk_thumbprint":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","extra":{"person":"ada@example.com"}}}',
			now() + interval '15 minutes')`
	// columnsRefreshToken is the device login's refresh token, of the same request.
	columnsRefreshToken = `insert into oauth_refresh_tokens (signature, request_id, access_signature, credential_id, request, expires_at)
		values ('refresh-1', 'request-1', 'access-1', '00000000-0000-0000-0000-0000000000c1',
			'{"client_id":"agent-secrets","session":{"extra":{"person":"ada@example.com"}}}', now() + interval '30 days')`
	columnsRotate = `update oauth_refresh_tokens set grace_until = now() + interval '5 minutes' where signature = 'refresh-1'`
	// columnsSignIn is ada's sign-in on her box.
	columnsSignIn = `insert into person_signins (id, person, thumbprint, machine_account)
		values ('00000000-0000-0000-0000-0000000000a1', 'ada@example.com', 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', 'example-fleet@example-project.iam.gserviceaccount.com')`
	columnsRevokeSignIn = `update person_signins set revoked_at = now()`
	// columnsNewDeviceCode is a device code no person has decided yet.
	columnsNewDeviceCode = `insert into oauth_device_codes (signature, user_code_signature, request, status, expires_at)
		values ('device-1', 'user-1', '{"client_id":"agent-secrets","session":{}}', 0, now() + interval '10 minutes')`
	// columnsDecidedDeviceCode is a device code ada approved for the device login.
	columnsDecidedDeviceCode = `insert into oauth_device_codes (signature, user_code_signature, request, status, expires_at)
		values ('device-1', 'user-1', '{"client_id":"agent-secrets","session":{"subject":"ada@example.com","extra":{"credential_id":"00000000-0000-0000-0000-0000000000c1"}},"last_checked":"2026-11-18T00:00:00Z"}',
			1, now() + interval '10 minutes')`
)

// The guards migration 0033 adds, each as the statement a control runs to turn it off.
const (
	accessTokenGuard    = `alter table oauth_access_tokens disable trigger oauth_access_tokens_credential_columns`
	refreshTokenGuard   = `alter table oauth_refresh_tokens disable trigger oauth_refresh_tokens_credential_columns`
	signInGuard         = `alter table person_signins disable trigger person_signins_credential_columns`
	signInDeleteGuard   = `alter table person_signins disable trigger person_signins_revoked_kept`
	signInTruncateGuard = `alter table person_signins disable trigger person_signins_not_truncated`
	machineLoginGuard   = `alter table launcher_credentials disable trigger launcher_credentials_credential_columns`
	deviceDecisionGuard = `alter table oauth_device_codes disable trigger oauth_device_codes_decision_kept`
)

// credentialRefusal is one transition no writer of the broker makes: after setup, statement is
// refused with SQLSTATE 23514; with guard run after setup instead, it passes.
type credentialRefusal struct {
	column, transition string
	setup              []string
	statement          string
	guard              string
}

var (
	accessTokenSetup  = []string{columnsDeviceLogin, columnsServiceLogin, columnsAccessToken}
	refreshTokenSetup = []string{columnsDeviceLogin, columnsServiceLogin, columnsAccessToken, columnsRefreshToken}
	revokedSignIn     = []string{columnsSignIn, columnsRevokeSignIn}
	revokedService    = []string{columnsServiceLogin, columnsRevokeServiceLogin}
)

var credentialRefusals = slices.Concat([]credentialRefusal{
	{"oauth_access_tokens.active", "false -> true: a revoked token acts again",
		append(slices.Clone(accessTokenSetup), `update oauth_access_tokens set active = false`),
		`update oauth_access_tokens set active = true`, accessTokenGuard},
	{"oauth_access_tokens.request", "its person rewritten",
		accessTokenSetup,
		`update oauth_access_tokens set request = jsonb_set(request, '{session,extra,person}', '"mallory@example.com"')`, accessTokenGuard},
	{"oauth_access_tokens.request", "its key rewritten",
		accessTokenSetup,
		`update oauth_access_tokens set request = jsonb_set(request, '{session,jwk_thumbprint}', '"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"')`, accessTokenGuard},
	{"oauth_access_tokens.request", "any other part rewritten",
		accessTokenSetup,
		`update oauth_access_tokens set request = jsonb_set(request, '{client_id}', '"another-client"')`, accessTokenGuard},
	{"oauth_access_tokens.signature", "changed",
		accessTokenSetup,
		`update oauth_access_tokens set signature = 'access-2'`, accessTokenGuard},
	{"oauth_access_tokens.request_id", "changed",
		accessTokenSetup,
		`update oauth_access_tokens set request_id = 'request-2'`, accessTokenGuard},
	{"oauth_access_tokens.credential_id", "moved to another credential",
		accessTokenSetup,
		`update oauth_access_tokens set credential_id = '00000000-0000-0000-0000-0000000000c2'`, accessTokenGuard},
	{"oauth_access_tokens.credential_id", "taken off",
		accessTokenSetup,
		`update oauth_access_tokens set credential_id = null`, accessTokenGuard},

	{"oauth_refresh_tokens.active", "false -> true: a revoked token is redeemable again",
		append(slices.Clone(refreshTokenSetup), `update oauth_refresh_tokens set active = false`),
		`update oauth_refresh_tokens set active = true`, refreshTokenGuard},
	{"oauth_refresh_tokens.grace_until", "cleared: a rotated token is redeemable for good",
		append(slices.Clone(refreshTokenSetup), columnsRotate),
		`update oauth_refresh_tokens set grace_until = null`, refreshTokenGuard},
	{"oauth_refresh_tokens.grace_until", "moved later",
		append(slices.Clone(refreshTokenSetup), columnsRotate),
		`update oauth_refresh_tokens set grace_until = grace_until + interval '1 day'`, refreshTokenGuard},
	{"oauth_refresh_tokens.grace_until", "moved earlier",
		append(slices.Clone(refreshTokenSetup), columnsRotate),
		`update oauth_refresh_tokens set grace_until = now() - interval '1 second'`, refreshTokenGuard},
	{"oauth_refresh_tokens.request", "its person rewritten",
		refreshTokenSetup,
		`update oauth_refresh_tokens set request = jsonb_set(request, '{session,extra,person}', '"mallory@example.com"')`, refreshTokenGuard},
	{"oauth_refresh_tokens.signature", "changed",
		refreshTokenSetup,
		`update oauth_refresh_tokens set signature = 'refresh-2'`, refreshTokenGuard},
	{"oauth_refresh_tokens.request_id", "changed",
		refreshTokenSetup,
		`update oauth_refresh_tokens set request_id = 'request-2'`, refreshTokenGuard},
	{"oauth_refresh_tokens.credential_id", "moved to another credential",
		refreshTokenSetup,
		`update oauth_refresh_tokens set credential_id = '00000000-0000-0000-0000-0000000000c2'`, refreshTokenGuard},

	{"person_signins.person", "changed",
		[]string{columnsSignIn},
		`update person_signins set person = 'mallory@example.com'`, signInGuard},
	{"person_signins.thumbprint", "moved to another box key",
		[]string{columnsSignIn},
		`update person_signins set thumbprint = 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'`, signInGuard},
	{"person_signins.machine_account", "moved to another machine",
		[]string{columnsSignIn},
		`update person_signins set machine_account = 'example-pool@example-project.iam.gserviceaccount.com'`, signInGuard},
	{"person_signins.created_at", "moved",
		[]string{columnsSignIn},
		`update person_signins set created_at = created_at - interval '1 day'`, signInGuard},
	{"person_signins.revoked_at", "cleared: a revoked box key trades again",
		revokedSignIn,
		`update person_signins set revoked_at = null`, signInGuard},
	{"person_signins.revoked_at", "moved",
		revokedSignIn,
		`update person_signins set revoked_at = revoked_at + interval '1 day'`, signInGuard},
	{"person_signins row", "a revoked sign-in deleted",
		revokedSignIn,
		`delete from person_signins`, signInDeleteGuard},
	{"person_signins row", "truncated",
		revokedSignIn,
		`truncate person_signins`, signInTruncateGuard},

	{"launcher_credentials.subject", "moved to another account",
		[]string{columnsServiceLogin},
		`update launcher_credentials set subject = 'system:serviceaccount:example-ns:another'`, machineLoginGuard},
	{"launcher_credentials.subject", "taken off",
		[]string{columnsServiceLogin},
		`update launcher_credentials set subject = null`, machineLoginGuard},
	{"launcher_credentials.revoked_at", "cleared: a revoked machine trades again",
		revokedService,
		`update launcher_credentials set revoked_at = null`, machineLoginGuard},
	{"launcher_credentials.revoked_at", "moved",
		revokedService,
		`update launcher_credentials set revoked_at = revoked_at + interval '1 day'`, machineLoginGuard},
	{"launcher_credentials.expires_at", "moved on a revoked login",
		revokedService,
		`update launcher_credentials set expires_at = expires_at + interval '30 days'`, machineLoginGuard},
}, machineLoginMoveRefusals(), []credentialRefusal{
	{"oauth_device_codes.request", "a decided code's person rewritten",
		[]string{columnsDecidedDeviceCode},
		`update oauth_device_codes set request = jsonb_set(request, '{session,subject}', '"mallory@example.com"')`, deviceDecisionGuard},
	{"oauth_device_codes.request", "a decided code's credential rewritten",
		[]string{columnsDecidedDeviceCode},
		`update oauth_device_codes set request = jsonb_set(request, '{session,extra,credential_id}', '"00000000-0000-0000-0000-0000000000c2"')`, deviceDecisionGuard},
	{"oauth_device_codes.request", "a decided code's request replaced by a value that is not an object",
		[]string{columnsDecidedDeviceCode},
		`update oauth_device_codes set request = '"approved"'`, deviceDecisionGuard},
	{"oauth_device_codes.status", "approved -> new: a decided code set back to new",
		[]string{columnsDecidedDeviceCode},
		`update oauth_device_codes set status = 0`, deviceDecisionGuard},
	{"oauth_device_codes.status", "invalidated -> new",
		[]string{columnsDecidedDeviceCode, `update oauth_device_codes set status = -1`},
		`update oauth_device_codes set status = 0`, deviceDecisionGuard},
	{"oauth_device_codes.status", "invalidated -> approved: an exchanged code is exchanged again",
		[]string{columnsDecidedDeviceCode, `update oauth_device_codes set status = -1`},
		`update oauth_device_codes set status = 1`, deviceDecisionGuard},
	{"oauth_device_codes.status", "approved -> denied: a decision moved to the other",
		[]string{columnsDecidedDeviceCode},
		`update oauth_device_codes set status = 2`, deviceDecisionGuard},
})

// The six columns that say who a machine login acts for and which key presents it, each with a
// statement that moves it, which a login that names no subject and a revoked login both refuse.
var machineLoginMoves = []struct{ column, statement string }{
	{"operator", `update launcher_credentials set operator = 'mallory@example.com'`},
	{"service", `update launcher_credentials set service = 'another-service'`},
	{"host", `update launcher_credentials set host = 'another-host'`},
	{"key_thumbprint", `update launcher_credentials set key_thumbprint = 'thumbprint-another'`},
	{"public_jwk", `update launcher_credentials set public_jwk = '{"kty":"EC","x":"another"}'`},
	{"pod_uid", `update launcher_credentials set pod_uid = 'pod-another'`},
}

// machineLoginMoveRefusals is a refusal row for each of machineLoginMoves, on a login a device
// approval made and on a revoked login.
func machineLoginMoveRefusals() []credentialRefusal {
	var rows []credentialRefusal
	for _, move := range machineLoginMoves {
		rows = append(rows,
			credentialRefusal{"launcher_credentials." + move.column, "moved on a login a device approval made (no subject)",
				[]string{columnsDeviceLogin}, move.statement, machineLoginGuard},
			credentialRefusal{"launcher_credentials." + move.column, "moved on a revoked login",
				revokedService, move.statement, machineLoginGuard})
	}
	return rows
}

// credentialTransition is one statement a writer of the broker sends, which every guard lets
// through, and the number of rows it writes.
type credentialTransition struct {
	writer, transition string
	setup              []string
	statement          string
	rows               int64
}

var credentialTransitions = []credentialTransition{
	{"oauthstore.Store.RevokeAccessToken", "a token's revoke",
		accessTokenSetup,
		`update oauth_access_tokens set active = false where request_id = 'request-1' and active`, 1},
	{"oauthstore.Store.RevokeRefreshToken", "a family's revoke",
		refreshTokenSetup,
		`update oauth_refresh_tokens set active = false where request_id = 'request-1' and active`, 1},
	{"oauthstore.EndCredential", "the credential's refresh tokens ended",
		refreshTokenSetup,
		`update oauth_refresh_tokens set active = false where credential_id = '00000000-0000-0000-0000-0000000000c1' and active`, 1},
	{"oauthstore.EndCredential", "the credential's access tokens ended",
		accessTokenSetup,
		`update oauth_access_tokens set active = false where credential_id = '00000000-0000-0000-0000-0000000000c1' and active`, 1},
	{"oauthstore.Store.RotateRefreshToken", "a rotation sets the grace",
		refreshTokenSetup,
		`update oauth_refresh_tokens set grace_until = coalesce(grace_until, now() + make_interval(secs => 300)) where signature = 'refresh-1'`, 1},
	{"oauthstore.Store.RotateRefreshToken", "a retry inside the grace keeps it",
		append(slices.Clone(refreshTokenSetup), columnsRotate),
		`update oauth_refresh_tokens set grace_until = coalesce(grace_until, now() + make_interval(secs => 300)) where signature = 'refresh-1'`, 1},
	{"oauthstore.Store.RotateRefreshToken", "a retry inside the grace ends the earlier redemption's branch",
		append(slices.Clone(refreshTokenSetup), columnsRotate,
			`insert into oauth_refresh_tokens (signature, request_id, credential_id, request, expires_at)
				values ('refresh-2', 'request-1', '00000000-0000-0000-0000-0000000000c1', '{"client_id":"agent-secrets"}', now() + interval '30 days')`),
		`update oauth_refresh_tokens set active = false where request_id = 'request-1' and signature <> 'refresh-1' and active`, 1},
	{"oauthstore.Store.Sweep", "an expired access token deleted",
		append(slices.Clone(accessTokenSetup), `update oauth_access_tokens set expires_at = now() - interval '1 second'`),
		`delete from oauth_access_tokens where signature in (select signature from oauth_access_tokens
			where expires_at <= now() order by expires_at limit 100)`, 1},
	{"oauthstore.Store.Sweep", "a revoked refresh family deleted",
		append(slices.Clone(refreshTokenSetup), `update oauth_refresh_tokens set active = false`),
		`delete from oauth_refresh_tokens t where signature = any(array['refresh-1'])
			and (not t.active or t.expires_at <= now() or t.grace_until < now())
			and not exists (select 1 from oauth_refresh_tokens r where r.request_id = t.request_id
				and r.active and r.expires_at > now() and (r.grace_until is null or r.grace_until >= now()))`, 1},
	{"oauthstore.Store.Sweep", "an expired decided device code deleted",
		[]string{columnsDecidedDeviceCode, `update oauth_device_codes set expires_at = now() - interval '2 hours'`},
		`delete from oauth_device_codes where signature in (select signature from oauth_device_codes
			where expires_at < now() - interval '1 hour' order by expires_at limit 100)`, 1},
	{"oauthstore.DeleteCredentialTokens", "a retire deletes the credential's refresh tokens",
		refreshTokenSetup,
		`delete from oauth_refresh_tokens where credential_id = '00000000-0000-0000-0000-0000000000c1'`, 1},
	{"oauthstore.DeleteCredentialTokens", "a retire deletes the credential's access tokens",
		accessTokenSetup,
		`delete from oauth_access_tokens where credential_id = '00000000-0000-0000-0000-0000000000c1'`, 1},
	{"oauthstore.Store.DeleteRefreshTokenSession", "the library's reuse path deletes the token presented",
		append(slices.Clone(refreshTokenSetup), columnsRotate),
		`delete from oauth_refresh_tokens where signature = 'refresh-1'`, 1},

	{"store.RecordPersonSignIn", "a trade of a live sign-in moves its last use",
		[]string{columnsSignIn},
		`insert into person_signins (person, thumbprint, machine_account)
			values ('ada@example.com', 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', lower('example-fleet@example-project.iam.gserviceaccount.com'))
			on conflict (person, thumbprint) do update
				set last_used_at = now()
				where person_signins.revoked_at is null
					and person_signins.machine_account = excluded.machine_account
			returning 1`, 1},
	{"store.RecordPersonSignIn", "a trade of a revoked box key changes nothing",
		revokedSignIn,
		`insert into person_signins (person, thumbprint, machine_account)
			values ('ada@example.com', 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', lower('example-fleet@example-project.iam.gserviceaccount.com'))
			on conflict (person, thumbprint) do update
				set last_used_at = now()
				where person_signins.revoked_at is null
					and person_signins.machine_account = excluded.machine_account
			returning 1`, 0},
	{"store.RevokePersonSignInTx", "a sign-in's revoke, with the access tokens bound to its box key",
		append([]string{columnsSignIn}, accessTokenSetup...),
		`with target as (
				select id from person_signins where id = '00000000-0000-0000-0000-0000000000a1' and person = 'ada@example.com' for no key update
			), revoked as (
				update person_signins set revoked_at = now()
				where id in (select id from target) and revoked_at is null
				returning person, thumbprint
			), tokens as (
				update oauth_access_tokens set active = false
				where active and (request #>> '{session,extra,person}', request #>> '{session,jwk_thumbprint}')
					in (select person, thumbprint from revoked)
				returning 1
			)
			select (select count(*) from target), (select count(*) from tokens),
				(select thumbprint from revoked)`, 1},
	// No writer here deletes a sign-in, so this row runs the delete itself: a live row may go,
	// which ends it, and cybermonklabs/legion#4199's RecordPersonSignIn deletes one that records no
	// account before it records the pair anew.
	{"the delete of a live sign-in", "on #4199, RecordPersonSignIn's replacement of one recording no account",
		[]string{columnsSignIn},
		`delete from person_signins where person = 'ada@example.com' and thumbprint = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' and revoked_at is null`, 1},

	{"enroll.endCredential", "a device login's revoke",
		[]string{columnsDeviceLogin},
		`update launcher_credentials set revoked_at=now() where id='00000000-0000-0000-0000-0000000000c1'`, 1},
	{"enroll.endCredential", "a service login's revoke",
		[]string{columnsServiceLogin},
		`update launcher_credentials set revoked_at=now() where id='00000000-0000-0000-0000-0000000000c2'`, 1},
	{"machine.Service.Refreshed", "a device login's refresh moves its expiry",
		[]string{columnsDeviceLogin},
		`update launcher_credentials set expires_at = now() + make_interval(secs => 2592000)
			where id = '00000000-0000-0000-0000-0000000000c1' and revoked_at is null and expires_at > clock_timestamp() returning expires_at`, 1},
	{"enroll.Service.RetireCredential", "a retire ends the expiry now",
		[]string{columnsDeviceLogin},
		`update launcher_credentials set expires_at = now() where id = '00000000-0000-0000-0000-0000000000c1'`, 1},
	// A person's sign-in credential names its box key in its subject (exchange.PersonSubject), as a
	// machine's own login does (exchange.MachineSubject), so its key never moves, and the upsert
	// still moves its operator, host and expiry.
	{"enroll.Service.IssueTokenLoginTx", "the upsert moves a live sign-in credential's operator, host and expiry",
		[]string{`insert into launcher_credentials (id, operator, service, subject, host, key_thumbprint, public_jwk, expires_at)
			values ('00000000-0000-0000-0000-0000000000c3', 'ada@example.com', 'example-fleet',
				'signin:ada@example.com|example-fleet@example-project.iam.gserviceaccount.com#AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA',
				'example-project/example-zone-a/example-vm-1', 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', '{"kty":"EC"}', now() + interval '1 hour')`},
		`insert into launcher_credentials (id, operator, service, host, key_thumbprint, public_jwk, record_id, subject, pod_uid, expires_at)
			values (gen_random_uuid(), 'grace@example.com', 'example-fleet', 'example-project/example-zone-a/example-vm-2',
				'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', '{"kty":"EC"}', null,
				'signin:ada@example.com|example-fleet@example-project.iam.gserviceaccount.com#AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA',
				nullif('',''), now() + interval '2 hours')
			on conflict (subject) where subject is not null and revoked_at is null
			do update set operator=excluded.operator, service=excluded.service, host=excluded.host,
				key_thumbprint=excluded.key_thumbprint, public_jwk=excluded.public_jwk,
				pod_uid=excluded.pod_uid, expires_at=excluded.expires_at
			returning id`, 1},
	{"enroll.Service.IssueTokenLoginTx", "the upsert moves a live service login to its new key",
		[]string{columnsServiceLogin},
		`insert into launcher_credentials (id, operator, service, host, key_thumbprint, public_jwk, record_id, subject, pod_uid, expires_at)
			values (gen_random_uuid(), null, 'legion-daemon', 'example-ns/example-daemon', 'thumbprint-service-2', '{"kty":"EC","x":"2"}', null,
				'system:serviceaccount:example-ns:example-daemon', nullif('',''), now() + interval '1 hour')
			on conflict (subject) where subject is not null and revoked_at is null
			do update set operator=excluded.operator, service=excluded.service, host=excluded.host,
				key_thumbprint=excluded.key_thumbprint, public_jwk=excluded.public_jwk,
				pod_uid=excluded.pod_uid, expires_at=excluded.expires_at
			returning id`, 1},
	{"enroll.Service.IssueTokenLoginTx", "the pod claim writes a live service login's pod",
		[]string{`drop index launcher_credentials_live_subject`, columnsServiceLogin},
		`update launcher_credentials set pod_uid='pod-1'
			where subject='system:serviceaccount:example-ns:example-daemon' and key_thumbprint='thumbprint-service' and pod_uid is null and revoked_at is null
			  and not exists (select 1 from launcher_credentials held
				where held.subject='system:serviceaccount:example-ns:example-daemon' and held.pod_uid='pod-1' and held.revoked_at is null)`, 1},
	{"enroll.Service.IssueTokenLoginTx", "the per-pod upsert moves a live pod login to its new key",
		[]string{`drop index launcher_credentials_live_subject`, columnsServiceLogin,
			`update launcher_credentials set pod_uid = 'pod-1' where id = '00000000-0000-0000-0000-0000000000c2'`},
		`insert into launcher_credentials (id, operator, service, host, key_thumbprint, public_jwk, record_id, subject, pod_uid, expires_at)
			values (gen_random_uuid(), null, 'legion-daemon', 'example-ns/example-daemon', 'thumbprint-service-2', '{"kty":"EC","x":"2"}', null,
				'system:serviceaccount:example-ns:example-daemon', nullif('pod-1',''), now() + interval '1 hour')
			on conflict (subject, pod_uid) where subject is not null and pod_uid is not null and revoked_at is null
			do update set operator=excluded.operator, service=excluded.service, host=excluded.host,
				key_thumbprint=excluded.key_thumbprint, public_jwk=excluded.public_jwk,
				pod_uid=excluded.pod_uid, expires_at=excluded.expires_at
			returning id`, 1},

	{"oauthstore.Store.UpdateDeviceCodeSession", "new -> approved: a new code's approval",
		[]string{columnsNewDeviceCode},
		`update oauth_device_codes set
			user_code_signature = case when status = 0 then 'user-1' else user_code_signature end,
			request = case when status = 0 then '{"client_id":"agent-secrets","session":{"subject":"ada@example.com","extra":{"credential_id":"00000000-0000-0000-0000-0000000000c1"}}}'::jsonb
				else request || jsonb_build_object('last_checked', '"2026-11-18T00:01:00Z"'::jsonb) end,
			expires_at = case when status = 0 then now() + interval '10 minutes' else expires_at end,
			status = case when status = 0 then 1 else status end
			where signature = 'device-1'`, 1},
	{"oauthstore.Store.UpdateDeviceCodeSession", "a poll of a decided code moves its last_checked",
		[]string{columnsDecidedDeviceCode},
		`update oauth_device_codes set
			user_code_signature = case when status = 0 then 'user-1' else user_code_signature end,
			request = case when status = 0 then '{"client_id":"agent-secrets","session":{}}'::jsonb
				else request || jsonb_build_object('last_checked', '"2026-11-18T00:01:00Z"'::jsonb) end,
			expires_at = case when status = 0 then now() + interval '10 minutes' else expires_at end,
			status = case when status = 0 then 0 else status end
			where signature = 'device-1'`, 1},
	{"oauthstore.Store.UpdateDeviceCodeSession", "new -> denied: a new code's denial",
		[]string{columnsNewDeviceCode},
		`update oauth_device_codes set
			user_code_signature = case when status = 0 then 'user-1' else user_code_signature end,
			request = case when status = 0 then '{"client_id":"agent-secrets","session":{}}'::jsonb
				else request || jsonb_build_object('last_checked', '"2026-11-18T00:01:00Z"'::jsonb) end,
			expires_at = case when status = 0 then now() + interval '10 minutes' else expires_at end,
			status = case when status = 0 then 2 else status end
			where signature = 'device-1'`, 1},
	{"oauthstore.Store.InvalidateDeviceCodeSession", "approved -> invalidated: an approved code exchanged",
		[]string{columnsDecidedDeviceCode},
		`update oauth_device_codes set status = -1 where signature = 'device-1' and status <> -1`, 1},
}

// TestEveryBrokerCredentialColumnMovesOnlyAsItsWritersMoveIt holds the census above: each refused
// transition is refused with SQLSTATE 23514, by the guard it names, and passes with that guard
// off; and each writer's transition passes with every guard on, writing the rows it names.
func TestEveryBrokerCredentialColumnMovesOnlyAsItsWritersMoveIt(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	// run runs setup and then statement in one transaction it rolls back, and answers the
	// statement's command tag and error (a setup error fails the test).
	run := func(t *testing.T, setup []string, statement string) (pgconn.CommandTag, error) {
		t.Helper()
		tx, err := st.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		for _, step := range setup {
			if _, err := tx.Exec(ctx, step); err != nil {
				t.Fatalf("setup %q: %v", step, err)
			}
		}
		return tx.Exec(ctx, statement)
	}
	for _, row := range credentialRefusals {
		t.Run("refused/"+row.column+"/"+row.transition, func(t *testing.T) {
			var pgError *pgconn.PgError
			if _, err := run(t, row.setup, row.statement); !errors.As(err, &pgError) || pgError.Code != "23514" {
				t.Errorf("%s: %s answered %v, want it refused with SQLSTATE 23514", row.column, row.transition, err)
			}
			if _, err := run(t, append(slices.Clone(row.setup), row.guard), row.statement); err != nil {
				t.Errorf("control: with %q, %s answered %v, want it to pass, so the guard named is what refuses it", row.guard, row.transition, err)
			}
		})
	}
	for _, row := range credentialTransitions {
		t.Run("allowed/"+row.writer+"/"+row.transition, func(t *testing.T) {
			tag, err := run(t, row.setup, row.statement)
			if err != nil {
				t.Fatalf("%s: %s, the writer's own statement, answered %v, want it to pass", row.writer, row.transition, err)
			}
			if tag.RowsAffected() != row.rows {
				t.Errorf("%s: %s wrote %d rows, want %d, so the row tests the transition it names", row.writer, row.transition, tag.RowsAffected(), row.rows)
			}
		})
	}
}
