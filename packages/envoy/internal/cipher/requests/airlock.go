// packages/envoy/internal/cipher/requests/airlock.go
// airlock.go is the placeholder side of the airlock: the strings a release hands a session in
// place of a secret the rules file names, the table those strings live in
// (airlock_placeholders, migration 0029), the shape each type of secret is released in once its
// values are taken out of it, and the two reads the broker's proxy port makes — one per
// connection (AirlockConnection) and one per grant per pod (AirlockValues). The proxy itself,
// which decides and rewrites each request, is internal/cipher/proxyrules.
package requests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// ErrAirlockNoNames is AirlockConnection's refusal of a placeholder whose grant is live and holds
// no airlock name the session may still have: nobody decided the grant, and every one of its names
// is withheld from the session. The proxy answers it 407, as it answers an unknown placeholder,
// since a connection that may carry nothing is no connection.
var ErrAirlockNoNames = errors.New("this grant holds no airlock name the session may still have")

// ErrAirlockValueShape is airlockSplitOf's refusal of a value that does not hold what its type
// releases: no JSON object, a missing or non-string token, two installations of one owner, an
// azure-login identity that names no tenant or no application. The release of that name is
// refused rather than made, since the one other answer would be to hand over the real value the
// rules hold back, and no retry changes a value's shape, so the proxy answers it 403 in these
// words.
var ErrAirlockValueShape = errors.New("a name of this grant does not hold what its type releases")

// ErrMintedOnly is readRelease's refusal of a release under a rules entry of kind dpop,
// before anything of the name is read. A name of that kind is a capability the broker mints for a
// grant of its own and holds (MintedKind, mintedValues), and never a stored secret: a secret
// stored under such a name is never released, to the session or to the proxy, as a value or as a
// placeholder.
var ErrMintedOnly = errors.New("a name of kind dpop is a capability the broker mints and holds, never a stored secret's release")

// The slots of a secret. A slot names which value of a secret a placeholder stands for, because
// one secret can release several values at once and each needs a placeholder of its own:
//
//   - SlotValue: a stored secret's own value, and a google-login's access token;
//   - the three aws- slots: a stored AWS key pair, which the secret's airlock entry marks with
//     kind "aws"; the session token's slot exists only when the stored value holds one;
//   - SlotGitHubPrefix + owner: one installation of a github-app secret, the owner lower-cased;
//   - SlotAzureFederated: an azure-login secret's federated token.
//
// The set is open, not closed: the proxy adds one slot per exchange it makes of an azure-login
// secret's federated token, opening with SlotAzureAccessPrefix and carrying the token endpoint,
// the scope and the client_id of that exchange (internal/cipher/proxy's azure.go, which
// writes and reads that text and is the only code that does). Nothing may assume a slot it does
// not know is a mistake. SlotAWSSecretAccessKey is written in two halves because the public leak
// check reads the whole name as an AWS secret key; its value is the wire name.
const (
	SlotValue              = "value"
	SlotAWSAccessKeyID     = "aws-access-key-id"
	SlotAWSSecretAccessKey = "aw" + "s-secret-access-key"
	SlotAWSSessionToken    = "aws-session-token"
	SlotGitHubPrefix       = "github:"
	SlotAzureFederated     = "azure-federated"
	SlotAzureAccessPrefix  = "azure-access:"
)

// placeholderHash is a placeholder's SHA-256 in hex: the key airlock_placeholders holds it under,
// and the only form of it any query carries, so the plaintext a command holds never reaches a
// statement log, pg_stat_statements or a database error.
func placeholderHash(placeholder string) string {
	sum := sha256.Sum256([]byte(placeholder))
	return hex.EncodeToString(sum[:])
}

// placeholdersOf reads every placeholder grant grantID holds, keyed by name and then by slot. It
// is one indexed read: grant_id leads the unique constraint's index.
func placeholdersOf(ctx context.Context, q querier, grantID string) (map[string]map[string]string, error) {
	rows, err := q.Query(ctx, `select name, slot, placeholder from airlock_placeholders where grant_id=$1`, grantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	held := map[string]map[string]string{}
	for rows.Next() {
		var name, slot, placeholder string
		if err := rows.Scan(&name, &slot, &placeholder); err != nil {
			return nil, err
		}
		if held[name] == nil {
			held[name] = map[string]string{}
		}
		held[name][slot] = placeholder
	}
	return held, rows.Err()
}

// mintPlaceholders answers the placeholder of every (name, slot) pair want names for grantID: the
// one the grant already holds, or a freshly minted one written with the grant's own expiry. A
// second release of one grant therefore hands back the placeholders the first one did and writes
// no row, which is what the unique (grant_id, name, slot) constraint makes true even for two
// releases racing on different pods — the loser's insert does nothing and the rows are read again
// rather than assumed. A pair the grant holds a row for that want no longer names (a key pair
// that lost its session token) keeps its row: it names no value the proxy can serve, and deleting
// it would cost a write on every release to save a row the grant's end removes anyway.
func (m *Machine) mintPlaceholders(ctx context.Context, grantID string, expires *time.Time, want map[string][]string) (map[string]map[string]string, error) {
	held, err := placeholdersOf(ctx, m.Store.Pool, grantID)
	if err != nil {
		return nil, err
	}
	var names, slots, texts, hashes []string
	for _, name := range slices.Sorted(maps.Keys(want)) {
		for _, slot := range want[name] {
			if _, minted := held[name][slot]; minted {
				continue
			}
			text, err := placeholder.Mint(placeholder.Prefix)
			if err != nil {
				return nil, err
			}
			names, slots = append(names, name), append(slots, slot)
			texts, hashes = append(texts, text), append(hashes, placeholderHash(text))
		}
	}
	if len(names) == 0 {
		return held, nil
	}
	if _, err := m.Store.Pool.Exec(ctx, `insert into airlock_placeholders (placeholder_sha256, placeholder, grant_id, name, slot, expires_at)
		select hash, placeholder, $1, name, slot, $6 from unnest($2::text[], $3::text[], $4::text[], $5::text[]) as minted(name, slot, placeholder, hash)
		on conflict (grant_id, name, slot) do nothing`, grantID, names, slots, texts, hashes, expires); err != nil {
		return nil, err
	}
	return placeholdersOf(ctx, m.Store.Pool, grantID)
}

// AirlockAccessPlaceholder answers the placeholder that stands for one access token of grantID's
// name, minting its row the first time and answering the same string after. slot is the
// access-token slot the proxy encoded the exchange into (SlotAzureAccessPrefix and the exchange
// it made), so a second exchange of one scope - by this pod or by another one - is answered the
// placeholder the first was, and the command's token keeps working wherever its next connection
// lands.
//
// The row is one of the grant's own, written with the grant's expiry like every other: it opens
// a connection of that grant while it lives, it goes when the grant's rows go
// (deleteSpentPlaceholders), and AirlockConnection answers it among the name's slots.
func (m *Machine) AirlockAccessPlaceholder(ctx context.Context, grantID, name, slot string) (string, error) {
	if _, _, minted := m.mintedKindOfGrant(grantID); minted {
		// A minted grant holds its kind's one slot, and no exchange is ever made for it.
		return "", ErrGrantNotLive
	}
	if !strings.HasPrefix(slot, SlotAzureAccessPrefix) {
		return "", fmt.Errorf("airlock: %q is no access-token slot", slot)
	}
	var expires *time.Time
	if err := m.Store.Pool.QueryRow(ctx, `select expires_at from grants where id=$1`, grantID).Scan(&expires); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrGrantNotLive
		}
		return "", err
	}
	minted, err := m.mintPlaceholders(ctx, grantID, expires, map[string][]string{name: {slot}})
	if err != nil {
		return "", err
	}
	placeholder := minted[name][slot]
	if placeholder == "" {
		return "", fmt.Errorf("airlock: no placeholder was minted for %s's access token", name)
	}
	return placeholder, nil
}

// deleteSpentPlaceholders deletes the placeholders of every grant that is revoked or has expired,
// answering how many rows went. The sweeper calls it each tick; see its comment for why a grant's
// rows may go the moment the grant ends.
func (m *Machine) deleteSpentPlaceholders(ctx context.Context) (int64, error) {
	tag, err := m.Store.Pool.Exec(ctx, `delete from airlock_placeholders p using grants g
		where g.id = p.grant_id and (g.revoked_at is not null or (g.expires_at is not null and g.expires_at <= now()))`)
	return tag.RowsAffected(), err
}

// AirlockRelease is how a release tells a session to reach the values it did not hand over: the
// proxy that holds them, the certificate authority to trust it by, the password every connection
// to it opens with, and each name the airlock holds back.
type AirlockRelease struct {
	ProxyURL      string
	CACertificate string // PEM
	ProxyPassword string
	Names         map[string]AirlockName
}

// AirlockName is one name the airlock holds back: what the command's environment variable of that
// name holds, the placeholder of each slot that value is built from, and the hosts the entry
// allows, which the command keeps off its NO_PROXY so they reach the proxy.
type AirlockName struct {
	Value string            // what the command's NAME holds
	Slots map[string]string // slot -> placeholder
	Hosts []string          // the entry's hosts, which the command keeps off its NO_PROXY
}

// airlockSplit is one airlock name's value taken apart, which one walk of that value answers
// both halves of: the real value behind each slot, which the proxy swaps into a request, and
// how to put each slot's placeholder where its own value was, which the release hands the
// session. One walk rather than two, because two readings of the same JSON by the same rules
// are two things that can drift: a slot the release mints a placeholder for and the proxy
// holds no value behind is a request the proxy cannot carry.
type airlockSplit struct {
	// values is the real value behind each slot. The proxy answers AirlockValues from it and
	// nothing else holds it: a release discards it with the rest of the real value.
	values map[string]string
	// fill answers what the session holds for this name, taking each slot's placeholder from
	// bySlot.
	fill func(bySlot map[string]string) (string, error)
	// azure is the application and tenant an azure-login document names, zero for every other
	// kind (azureLoginSplit). The proxy answers it beside values, as AirlockSecret.Azure, and
	// the session's release reads nothing of it: the session's document carries both ids as
	// they were minted.
	azure secrets.AzureLoginValue
}

// slots are the name's slots, sorted, so they are in one order whatever the map iteration gave,
// which is the order the proxy password is chosen in.
func (s airlockSplit) slots() []string { return slices.Sorted(maps.Keys(s.values)) }

// airlockSplitOf reads value, the real value a release of an airlock name read or minted, and
// answers which slots that value fills, what each of them holds, and the shape the session gets
// in its place. The shape is today's released shape with
// placeholders where the secret was, so a consumer that works today works unchanged:
//
//   - a stored secret and a google-login access token are one string, so they are the value
//     slot's placeholder alone;
//   - a GitHub profile whose github.value is token (gitHubValue, githubname.ValueToken), of a
//     github-app or a github-installations source (policy.ServesGitHubTokens), is released as the
//     one account's bare token, which is one string too, so it is the value slot's placeholder
//     alone;
//   - a stored AWS key pair, which the entry marks kind "aws", is the stored object re-emitted
//     with each of its three credential fields replaced;
//   - the installations object of a GitHub profile of either source, and an azure-login's minted
//     identity, are re-emitted with the token fields replaced and every other field as the
//     release wrote it.
//
// The policy type and the profile's value form decide first and the entry's kind only for a
// stored secret, because they are what says how the broker built the value, where the entry
// says where that value may go. A value that does not hold what its type releases — no object, a
// missing or non-string token — is an ErrAirlockValueShape rather than a release, since the one
// other answer would be to release the real value the rules hold back.
func airlockSplitOf(entryKind, secretType, gitHubValue, value string) (airlockSplit, error) {
	var split airlockSplit
	var err error
	switch {
	case policy.ServesGitHubTokens(secretType) && gitHubValue == githubname.ValueToken:
		return valueSplit(value), nil
	case policy.ServesGitHubTokens(secretType):
		split, err = githubAppSplit(value)
	case secretType == policy.TypeAzureLogin:
		split, err = azureLoginSplit(value)
	case secretType == "" && entryKind == proxyrules.KindAWS:
		split, err = objectSplit(value, map[string]string{"access_key_id": SlotAWSAccessKeyID, "secret_access_key": SlotAWSSecretAccessKey},
			map[string]string{"session_token": SlotAWSSessionToken})
	default:
		return valueSplit(value), nil
	}
	if err != nil {
		// One wrap for every shape the helpers below refuse, so each keeps its own words and
		// none of them can be the one that forgets the sentinel.
		return airlockSplit{}, fmt.Errorf("%w: %w", ErrAirlockValueShape, err)
	}
	return split, nil
}

// valueSplit is the split of a value that is one string: the value slot, holding the whole of it.
func valueSplit(value string) airlockSplit {
	return airlockSplit{
		values: map[string]string{SlotValue: value},
		fill:   func(bySlot map[string]string) (string, error) { return bySlot[SlotValue], nil },
	}
}

// objectSplit is the split of a JSON object whose fields hold the secrets: required names every
// field that must be a string, optional every field that is one when it is there, each against
// the slot that stands for it. Each such field's own string is what its slot holds, and every
// other field of the object is released as it was.
func objectSplit(value string, required, optional map[string]string) (airlockSplit, error) {
	doc, err := jsonObject(value)
	if err != nil {
		return airlockSplit{}, err
	}
	return fieldsSplit(doc, required, optional)
}

// fieldsSplit is objectSplit of a document already read, for a kind that reads more of the same
// document than its secret fields (azureLoginSplit) and so must not read it twice.
func fieldsSplit(doc map[string]json.RawMessage, required, optional map[string]string) (airlockSplit, error) {
	holes := map[string]string{}  // field -> slot
	values := map[string]string{} // slot -> the field's own string
	for field, slot := range required {
		held, ok := jsonString(doc[field])
		if !ok {
			return airlockSplit{}, fmt.Errorf("its value holds no %s string, which the airlock stands a placeholder in for", field)
		}
		holes[field], values[slot] = slot, held
	}
	for field, slot := range optional {
		if held, ok := jsonString(doc[field]); ok {
			holes[field], values[slot] = slot, held
		}
	}
	return airlockSplit{values: values, fill: func(bySlot map[string]string) (string, error) {
		filled := maps.Clone(doc)
		for field, slot := range holes {
			filled[field] = jsonStringOf(bySlot[slot])
		}
		return marshalObject(filled)
	}}, nil
}

// azureLoginSplit is the split of an azure-login's minted identity: the federated token's slot,
// taken as objectSplit takes it, and beside it the tenant and the application the document
// names, read in the same walk so the ids the proxy holds are the ones of the document whose
// token it holds. A document that names either one as no string, or as an empty one, releases
// nothing: the proxy holds every exchange of the token to the two (AirlockSecret.Azure), and
// could hold an exchange to nothing. The minter refuses such a document before it ever mints
// (secrets.AzureLogin), so this refuses what no release made by it can hold.
func azureLoginSplit(value string) (airlockSplit, error) {
	doc, err := jsonObject(value)
	if err != nil {
		return airlockSplit{}, err
	}
	split, err := fieldsSplit(doc, map[string]string{"federated_token": SlotAzureFederated}, nil)
	if err != nil {
		return airlockSplit{}, err
	}
	if split.azure.TenantID, err = azureIDOf(doc, "tenant_id"); err != nil {
		return airlockSplit{}, err
	}
	if split.azure.ClientID, err = azureIDOf(doc, "client_id"); err != nil {
		return airlockSplit{}, err
	}
	return split, nil
}

// azureIDOf is one id field of an azure-login document. The refusal names the field and never
// what the document holds there.
func azureIDOf(doc map[string]json.RawMessage, field string) (string, error) {
	id, ok := jsonString(doc[field])
	if !ok || id == "" {
		return "", fmt.Errorf("its minted identity holds no %s string, which the airlock holds every exchange of its federated token to", field)
	}
	return id, nil
}

// githubAppSplit is the split of a github-app secret's minted installations: one slot per
// installation, holding that installation's own token, and a shape with each token replaced by
// the placeholder of its owner's slot, every other field of the document and of
// each installation as the minter wrote it. The slot lower-cases the owner, where the document
// keeps the key GitHub answered with.
func githubAppSplit(value string) (airlockSplit, error) {
	doc, err := jsonObject(value)
	if err != nil {
		return airlockSplit{}, err
	}
	var installations map[string]map[string]json.RawMessage
	if json.Unmarshal(doc["installations"], &installations) != nil {
		return airlockSplit{}, errors.New("its minted value holds no installations object, which the airlock stands a placeholder in for")
	}
	slotOf := map[string]string{} // slot -> installation key
	values := map[string]string{} // slot -> that installation's own token
	for owner, installation := range installations {
		token, ok := jsonString(installation["token"])
		if !ok {
			return airlockSplit{}, fmt.Errorf("its minted installation %q holds no token string, which the airlock stands a placeholder in for", owner)
		}
		slot := SlotGitHubPrefix + strings.ToLower(owner)
		if other, clash := slotOf[slot]; clash {
			return airlockSplit{}, fmt.Errorf("its minted installations %q and %q are the same owner, so one placeholder would stand for both", other, owner)
		}
		slotOf[slot], values[slot] = owner, token
	}
	return airlockSplit{values: values, fill: func(bySlot map[string]string) (string, error) {
		filled := maps.Clone(installations)
		for slot, owner := range slotOf {
			installation := maps.Clone(filled[owner])
			installation["token"] = jsonStringOf(bySlot[slot])
			filled[owner] = installation
		}
		held, err := json.Marshal(filled)
		if err != nil {
			return "", err
		}
		out := maps.Clone(doc)
		out["installations"] = held
		return marshalObject(out)
	}}, nil
}

// jsonObject reads a value the airlock must take apart as a JSON object, keeping each field's own
// bytes so every field it does not replace is released exactly as it was read or minted.
func jsonObject(value string) (map[string]json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if json.Unmarshal([]byte(value), &doc) != nil || doc == nil {
		return nil, errors.New("its value is not a JSON object, which the airlock must take apart to stand placeholders in for its fields")
	}
	return doc, nil
}

// jsonString reads a field that must be a JSON string, answering false for a field that is
// missing or is anything else.
func jsonString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// jsonStringOf is a placeholder as one JSON field. A placeholder is 58 characters of the JSON
// string alphabet, so this never fails and the error is dropped.
func jsonStringOf(placeholder string) json.RawMessage {
	raw, _ := json.Marshal(placeholder)
	return raw
}

// marshalObject re-emits a document whose fields are raw JSON. The fields come out sorted by
// name, since Go marshals a map that way; a JSON object's field order carries no meaning.
func marshalObject(doc map[string]json.RawMessage) (string, error) {
	out, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// airlockRelease mints each airlock name's placeholders, writes proxy as those names' delivery,
// and answers what the session is told: where the proxy is, the certificate to trust it by, the
// password its connections open with, and each name's released value, slots and hosts.
func (m *Machine) airlockRelease(ctx context.Context, grantID string, expires *time.Time, splits map[string]airlockSplit) (*AirlockRelease, error) {
	want := map[string][]string{}
	for name, split := range splits {
		want[name] = split.slots()
	}
	minted, err := m.mintPlaceholders(ctx, grantID, expires, want)
	if err != nil {
		return nil, err
	}
	released := &AirlockRelease{ProxyURL: m.AirlockProxyURL, CACertificate: m.AirlockCACertificate, Names: map[string]AirlockName{}}
	for name, split := range splits {
		slots := map[string]string{}
		for _, slot := range split.slots() {
			slots[slot] = minted[name][slot]
		}
		value, err := split.fill(slots)
		if err != nil {
			return nil, fmt.Errorf("release %s behind the airlock: %w", name, err)
		}
		released.Names[name] = AirlockName{Value: value, Slots: slots, Hosts: m.Airlock.Lookup(name).Hosts}
	}
	released.ProxyPassword = proxyPassword(released.Names)
	// Which names went out as a placeholder rather than as their value, recorded where the
	// request's own decision for each name is. Nothing reads it; it is the audit trail's answer
	// to "what did this grant actually hand over".
	if _, err := m.Store.Pool.Exec(ctx, `update request_secrets set delivery='proxy'
		where request_id = (select request_id from grants where id=$1) and name = any($2)`,
		grantID, slices.Sorted(maps.Keys(splits))); err != nil {
		return nil, err
	}
	return released, nil
}

// proxyPassword is the placeholder of the first (name, slot) pair in sorted order, by name and
// then by slot. Every placeholder of a live grant opens a connection anyway — they sit in one
// command's environment and are of equal power — so one of them has to be the password, and
// taking the first in sorted order is what makes a grant of several names unambiguous for the
// command and for the proxy.
func proxyPassword(names map[string]AirlockName) string {
	for _, name := range slices.Sorted(maps.Keys(names)) {
		slots := names[name].Slots
		for _, slot := range slices.Sorted(maps.Keys(slots)) {
			return slots[slot]
		}
	}
	return ""
}

// AirlockConnection is what one proxy connection's password proves: the live grant behind it and
// every airlock name it may carry. It runs the checks that must hold on EVERY connection - the
// grant's liveness, its enrollment's lease, whether Dispatch calls the enrollment's person a
// restricted member, and the names its operator withheld from a grant nobody decided - and no
// others; the full release checks run once per grant per pod, in AirlockValues.
type AirlockConnection struct {
	GrantID, EnrollmentID, RequestID string
	// SessionID and RuntimeID are the enrollment's: the session the grant belongs to, empty for
	// an enrollment that names none, and the runtime it runs as, the enrollment's runtime id (a
	// pod's uid for a pod enrollment). They are read for the lines the proxy writes about where
	// it dialled (proxy's tunnel and failed-dial lines), which must name the caller, and
	// they cost nothing: the read below already joins the enrollment row.
	SessionID, RuntimeID string
	Issue                string // the first Dispatch issue key in the request's own signed reason, "" when it names none
	// Minted marks a grant of a capability the broker mints and holds (MintedKind): it belongs to
	// no enrollment, and the proxy holds it to the one host its grant binds.
	Minted  bool
	Expires *time.Time
	Names   map[string]AirlockGrantName
}

// AirlockGrantName is one name a connection may carry: the placeholder of each of its slots, and
// the type that says what those slots are.
type AirlockGrantName struct {
	Slots map[string]string // slot -> placeholder
	Type  string            // the policy type: "", "github-app", "google-login", "azure-login"
}

// AirlockConnection answers what the placeholder a proxy connection authenticated with proves. It
// refuses with ErrGrantNotLive when no row holds that placeholder, when its grant is revoked or
// expired, or when its enrollment is revoked or its lease lapsed; it drops, from a grant whose
// request nobody decided, every name the enrollment's operator withheld, read as
// policy.Set.Withholds reads them, so every other name served from a withheld name's source is
// dropped too, and refuses with ErrAirlockNoNames when that leaves none, which the proxy answers
// 407.
//
// A grant someone decided keeps its withheld names, as Values releases them: stillAllowed judges a
// withheld name by the login that decided the request, so only an automatic grant, which no one
// decided, never passes. A withhold ends every grant that got a name it covers without asking, in
// the transaction that withholds it (withhold), so a live grant someone decided that holds a
// withheld name had that name approved: by a person after the withhold, whom the withheld name let
// approve it (currentPolicyAdmits), or as a name its request waited on a decision for. Either
// approval stands. A name the request got without asking, beside one it waited on a person for,
// is approved with the whole request.
//
// It asks, on every connection, whether Dispatch calls the person the enrollment was made for a
// restricted member (restrictedOperator), because the proxy serves a grant's values from what it
// read once for as long as its pod holds them (AirlockValues), and this is the one read every
// call makes. A restricted member's grant ends for good here (errRestrictedMember), answered
// ErrGrantNotLive, which the proxy answers 407 and which cuts the calls it carries for the grant;
// an answer that cannot be had is the *projectaccess.MemberKindUncheckable restrictedOperator
// returns, which the proxy answers 503, so nothing is carried for the person while it cannot be
// told. The answer is projectaccess's, cached CacheTTL, so most connections ask Dispatch nothing.
//
// It applies the project rule as well (projectFacts, policy.Set.Evaluate's project block): when
// a held name is tagged with a private project the session no longer names, or with one Dispatch
// does not hold, the grant ends for good (endGrant, errProjectNotNamed) and the connection is
// refused ErrGrantNotLive. That is read only for a grant holding a tagged name, from the K3
// client's cache (at most one call to Dispatch per project per projectaccess.CacheTTL), so an
// untagged grant's connection asks Dispatch nothing.
//
// Besides those two questions it is indexed reads alone: the row by its primary key with its
// grant, its enrollment and its request, which says who decided it, joined in, the grant's other
// placeholders by grant_id, and the enrollment's withheld names by enrollment_id. Each name's type
// comes from the policy snapshot already in memory, which costs no query. The checks a release
// makes besides — the approval chain, the policy re-evaluation, the max-grant and the profile
// fingerprint — are AirlockValues's, once per grant per pod.
func (m *Machine) AirlockConnection(ctx context.Context, presented string) (AirlockConnection, error) {
	if kind, minted := m.mintedKindOfPlaceholder(presented); minted {
		return m.mintedConnection(ctx, kind, presented)
	}
	var conn AirlockConnection
	var reason, decidedBy string
	var live bool
	var enr enrollmentRow
	err := m.Store.Pool.QueryRow(ctx, `select p.grant_id, g.enrollment_id, g.request_id, g.expires_at, r.reason, coalesce(r.decided_by, ''),
		e.runtime_id, coalesce(e.session_id, ''),
		g.revoked_at is null and `+grantUnexpired("g")+` and e.revoked_at is null and enrollment_lease_live(e.lease_expires_at),
		e.kind, e.operator, e.subject, e.projects, c.service
		from airlock_placeholders p
		join grants g on g.id = p.grant_id
		join enrollments e on e.id = g.enrollment_id
		join requests r on r.id = g.request_id
		left join launcher_credentials c on c.id = e.launcher_credential_id
		where p.placeholder_sha256 = $1`, placeholderHash(presented)).
		Scan(&conn.GrantID, &conn.EnrollmentID, &conn.RequestID, &conn.Expires, &reason, &decidedBy,
			&conn.RuntimeID, &conn.SessionID, &live, &enr.Kind, &enr.Operator, &enr.Subject, &enr.Projects, &enr.Service)
	if errors.Is(err, pgx.ErrNoRows) {
		return AirlockConnection{}, ErrGrantNotLive
	}
	if err != nil {
		return AirlockConnection{}, err
	}
	if !live {
		return AirlockConnection{}, ErrGrantNotLive
	}
	enr.ID = conn.EnrollmentID
	restricted, err := m.restrictedOperator(ctx, enr)
	if err != nil {
		return AirlockConnection{}, err
	}
	if restricted {
		return AirlockConnection{}, m.endGrant(ctx, conn.GrantID, restrictedMember())
	}
	conn.Issue = record.IssueKeyInReason(reason)
	held, err := placeholdersOf(ctx, m.Store.Pool, conn.GrantID)
	if err != nil {
		return AirlockConnection{}, err
	}
	rows, err := m.Store.Pool.Query(ctx, `select name from withheld_secrets where enrollment_id=$1`, conn.EnrollmentID)
	if err != nil {
		return AirlockConnection{}, err
	}
	withheld, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return AirlockConnection{}, err
	}
	set := m.Policy.Get()
	requester := policy.Requester{Withheld: withheld}
	conn.Names = map[string]AirlockGrantName{}
	for name, slots := range held {
		if decidedBy == "" && set.Withholds(requester, name) {
			continue
		}
		conn.Names[name] = AirlockGrantName{Slots: slots, Type: set.Secrets[name].Type()}
	}
	if err := m.airlockProjectRule(ctx, conn.GrantID, enr, slices.Collect(maps.Keys(conn.Names)), set); err != nil {
		return AirlockConnection{}, err
	}
	if len(conn.Names) == 0 {
		return AirlockConnection{}, ErrAirlockNoNames
	}
	return conn, nil
}

// airlockProjectRule is AirlockConnection's project rule over names, the airlock names a
// connection may carry: a name tagged with a private project the credential no longer names, or
// with a project Dispatch does not hold, ends the grant for good (projectNotNamed). A grant
// holding no tagged name asks Dispatch nothing.
func (m *Machine) airlockProjectRule(ctx context.Context, grantID string, enr enrollmentRow, names []string, set *policy.Set) error {
	facts, err := m.projectFacts(ctx, enr, names, set)
	if err != nil || facts.states == nil {
		return err
	}
	// policy.ProjectRule is the rule Evaluate applies at the release, so the proxy's check and the
	// release's cannot drift. projectFacts resolves a state for every tagged project of names, so
	// ErrNoProjectState here is a broken caller, and fails the connection rather than keeping it.
	requester := facts.requester(policy.Requester{})
	slices.Sort(names)
	for _, name := range names {
		project := set.Secrets[name].Project
		reason, err := policy.ProjectRule(project, requester)
		if err != nil {
			return fmt.Errorf("%w: %s is tagged %s", err, name, project)
		}
		if reason != "" {
			return m.endGrant(ctx, grantID, projectNotNamed(name))
		}
	}
	return nil
}

// AirlockSecret is one airlock name as the proxy holds it: the real value behind each of its
// slots, when the broker must read or mint it again, the policy type that says what the slots
// are, and for an azure-login name the application and tenant its document names.
type AirlockSecret struct {
	Slots   map[string]string // slot -> the real value that slot's placeholder stands for
	Expires time.Time         // zero for a stored value, which no clock ends
	Type    string
	// Limited says a private project's limit applies to this secret: the grant's credential names
	// a private project and the secret carries the project-channels tag. The proxy then checks
	// every request that spends it (proxy's slackPostAllowed). A flag, not a non-nil list:
	// an empty Channels under Limited is a credential that may post nowhere.
	Limited bool
	// Channels, under Limited, are the Slack channel ids every private project the credential
	// names allows: the intersection of their owners' lists, sorted, never nil.
	Channels []string
	// Azure is the tenant and the application an azure-login name's own document names, which
	// the proxy holds every exchange of its federated token to, and zero for every other type.
	// The two travel as fields of their own rather than as slots or as the document whole. Not
	// slots, because a slot is a value a placeholder stands for: the release would mint
	// placeholders for them and put those in the session's document, where az needs the ids
	// themselves. Not the document, because the proxy would then hold every field a minter ever
	// writes there; these are the two it bounds an exchange by, read in the one walk that takes
	// the federated token out (azureLoginSplit). Neither is a secret: the session is handed both
	// in the clear.
	Azure secrets.AzureLoginValue
}

// AirlockValues runs the full release checks of Values - the record chain, the policy
// re-evaluation, the max-grant, the profile fingerprint - and answers the real value of each
// slot of each airlock name, which is what the proxy swaps into a request: a placeholder stands
// for one slot, not for a name's whole released value, so a github-app name answers one token
// per installation and an AWS pair one string per field. The split is the one the release makes
// (readRelease, release.go, the one loop both releases of a grant run), so no slot the session
// holds a placeholder for is a slot this answers nothing for. A GitHub profile is minted here
// exactly as the session's release mints it (release): its permissions, for the target its
// request recorded, else for the profile's own accounts and repositories (gitHubScope), so the
// token the proxy swaps in is the profile's for that target and its slots are the target's. The
// proxy calls it on a grant's first connection on its own pod.
//
// It answers beside the values the policy version those checks ran under. The proxy holds the
// two together and asks PolicyVersion whether that version still stands, which is what makes a
// policy change the broker took by its periodic reload - a retag the console or an
// infra-applier made, a raised tier, a disabled version - reach the values a pod already holds,
// rather than only the grants released through Values.
//
// It takes no enrollment: the connection's placeholder is what proves the grant, and
// AirlockConnection has already checked that the grant and its enrollment are live. A grant the
// checks refuse is ended exactly as Values ends it, since they are the same checks, and a grant
// whose chain fails is logged as Values logs it, under at=airlock (GrantChainInvalidMessage),
// since the proxy refuses it GRANT_CHAIN_INVALID as the session's release does.
func (m *Machine) AirlockValues(ctx context.Context, grantID string) (map[string]AirlockSecret, string, error) {
	if kind, id, minted := m.mintedKindOfGrant(grantID); minted {
		return m.mintedValues(ctx, kind, id)
	}
	checked, err := m.checkRelease(ctx, grantID, "", "airlock")
	if err != nil {
		return nil, "", err
	}
	read, err := m.readRelease(ctx, grantID, checked, releaseRead{airlockOnly: true})
	if err != nil {
		return nil, "", err
	}
	// A grant holding a project-tagged name is read again by the proxy within
	// airlockProjectRecheck: whether the project is private, and whether the session's person may
	// still see it, are Dispatch's and move no policy version, so nothing else would make a pod
	// holding these values run the release checks again once access is lost.
	var recheck time.Time
	if anyProjectTagged(checked.set, checked.granted) {
		recheck = time.Now().Add(airlockProjectRecheck)
	}
	// The channels the grant's credential may post to, for a project-channels secret released to
	// a credential that names a private project (channelLimit). Such a secret is bounded to
	// airlockProjectRecheck itself whenever its credential names any project, Limited or not:
	// whether a named project is private, and the channels its owner allows, are Dispatch's and
	// move no policy version, and the Slack bot token carries no dispatch-project tag, so the
	// bound above would never reach it. Without it, a project the credential names turning
	// private, or an owner's change to its list, would never reach a running proxy.
	channels, limited := m.channelLimit(ctx, checked)
	readAt := time.Now()
	values := map[string]AirlockSecret{}
	for _, released := range read {
		expires := released.expires
		if !recheck.IsZero() && (expires.IsZero() || expires.After(recheck)) {
			expires = recheck
		}
		value := AirlockSecret{Slots: released.split.values, Expires: expires, Type: released.secretType, Azure: released.split.azure}
		if checked.set.Secrets[released.name].ProjectChannels == policy.ProjectChannelsSlack {
			if limited {
				value.Limited, value.Channels = true, channels
			}
			if bound := readAt.Add(airlockProjectRecheck); len(checked.facts.named) > 0 && (value.Expires.IsZero() || value.Expires.After(bound)) {
				value.Expires = bound
			}
		}
		values[released.name] = value
	}
	// The same grant.used row and line a session's own release writes, marked as the proxy's: the
	// values did leave the broker, and an operator reading the trail must see that they did.
	if err := m.auditGrantUsed(ctx, grantID, checked, grantUse{names: releasedNames(read), airlock: true}); err != nil {
		m.giveBack(ctx, read)
		return nil, "", err
	}
	return values, checked.set.Version, nil
}

// PolicyVersion is the version of the policy snapshot this broker holds now: policy.Set.Version,
// which is the one statement of what the version covers, beside each served secret's airlock
// fingerprint. The proxy holds the version AirlockValues answered beside the values it answered,
// and reads this to tell whether that snapshot is still the live one.
//
// The type tag is in it so that a retag of the type - which changes what the broker hands over
// for the same stored value, not who may have it - reaches a pod holding values the proxy read
// before the retag, since the proxy rereads them on no event but a name's own reread or this
// version moving.
func (m *Machine) PolicyVersion() string { return m.Policy.Get().Version }
