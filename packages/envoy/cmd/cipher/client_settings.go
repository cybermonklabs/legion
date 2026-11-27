// packages/envoy/cmd/cipher/client_settings.go
//
// The two broker routes the secret forms call: GET /v1/settings, which takes no credential, and
// POST /v1/secrets/{name}/reread, which answers a session's proof in full and a call with none
// with the same 202 for every name.
package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// Settings is GET /v1/settings's exact response shape: the store and namespace every agent secret
// lives under, and the store's own fields (Fields: on AWS the key and the account and region that
// hold the secrets, on GCP the key and the project, on the memory store its file); on any store,
// the Google exchange audience a machine's own identity token must carry, from which the use
// command finds that identity with no setting (machineSource); and whether the broker reads a
// secret's type from the type tag (SecretTypeTag), without which the secret forms write no typed
// secret (requireTypeTag). A broker from before the store field answers none, and holds them in
// AWS Secrets Manager (secretstore.DefaultName); one from before the audience, or one that accepts
// no machine identity, names none; and one from before the type tag answers no secret_type_tag.
type Settings struct {
	Store                  string `json:"store,omitempty"`
	SecretsPrefix          string `json:"secrets_prefix"`
	GoogleExchangeAudience string `json:"google_exchange_audience,omitempty"`
	SecretTypeTag          bool   `json:"secret_type_tag,omitempty"`
	// JudgeKeys is each key whose judge tag gives the model judge seconds of it, with those
	// seconds and its guidance tag; absent where no key carries one. `secret show` reads it to
	// say who decides a request for a key and what the judge will look for.
	JudgeKeys map[string]JudgeKey `json:"judge_keys,omitempty"`
	// ApproverFields is the broker's own field, true where it takes a machine login's approver
	// account in a decision (api.settingsResponse.ApproverFields). No form reads it; it is held
	// here, among the fixed fields, so the settings this command reads and writes back are the
	// broker's answer member for member, and so no store may take its name.
	ApproverFields bool `json:"approver_fields,omitempty"`
	// Fields is every other string member of the body, by its JSON name: the store's own fields,
	// which its registry entry reads (store).
	Fields map[string]string `json:"-"`
	// answeredBy is the address whose answer these settings are (client.Settings), so a message
	// prints a field of them only as that address lets the text of its answer print (words).
	answeredBy cipheraddr.Address
}

// settingsFixed is Settings' fixed fields as JSON reads them, apart from Fields.
type settingsFixed struct {
	Store                  string              `json:"store,omitempty"`
	SecretsPrefix          string              `json:"secrets_prefix"`
	GoogleExchangeAudience string              `json:"google_exchange_audience,omitempty"`
	SecretTypeTag          bool                `json:"secret_type_tag,omitempty"`
	JudgeKeys              map[string]JudgeKey `json:"judge_keys,omitempty"`
	ApproverFields         bool                `json:"approver_fields,omitempty"`
}

// settingsFixedNames are settingsFixed's JSON names, which Fields never holds.
var settingsFixedNames = map[string]bool{"store": true, "secrets_prefix": true, "google_exchange_audience": true, "secret_type_tag": true, "judge_keys": true, "approver_fields": true}

// UnmarshalJSON reads the fixed fields, and puts every other member whose value is a JSON string
// into Fields; a member of any other kind is no store field, and is left out.
func (s *Settings) UnmarshalJSON(data []byte) error {
	var fixed settingsFixed
	if err := json.Unmarshal(data, &fixed); err != nil {
		return err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}
	fields := map[string]string{}
	for name, raw := range members {
		var value string
		if settingsFixedNames[name] || json.Unmarshal(raw, &value) != nil {
			continue
		}
		fields[name] = value
	}
	*s = Settings{
		Store: fixed.Store, SecretsPrefix: fixed.SecretsPrefix, GoogleExchangeAudience: fixed.GoogleExchangeAudience,
		SecretTypeTag: fixed.SecretTypeTag, JudgeKeys: fixed.JudgeKeys, ApproverFields: fixed.ApproverFields, Fields: fields,
	}
	return nil
}

// MarshalJSON writes the fixed fields as UnmarshalJSON reads them, and each of Fields as a member
// of its own; a field that has a fixed field's name is not written.
func (s Settings) MarshalJSON() ([]byte, error) {
	raw, err := json.Marshal(settingsFixed{
		Store: s.Store, SecretsPrefix: s.SecretsPrefix, GoogleExchangeAudience: s.GoogleExchangeAudience,
		SecretTypeTag: s.SecretTypeTag, JudgeKeys: s.JudgeKeys, ApproverFields: s.ApproverFields,
	})
	if err != nil || len(s.Fields) == 0 {
		return raw, err
	}
	members := map[string]any{}
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	for name, value := range s.Fields {
		if !settingsFixedNames[name] {
			members[name] = value
		}
	}
	return json.Marshal(members)
}

// words is text of these settings - a field, or a value read out of one, or a check's words that
// quote one - as a message may print it (cipheraddr.Address.Words): whoever answered at an address
// that is not clean can repeat its credential in any field, so beside such an address no field
// prints. Settings no client read (the zero answeredBy) print none of their text either.
func (s Settings) words(text string) string { return s.answeredBy.Words(text) }

// store is s as a store's writer and registry entry read them (secretstore.Settings): the store,
// the prefix, the store's own fields, and how a field prints (words).
func (s Settings) store() secretstore.Settings {
	return secretstore.Settings{Store: s.Store, Prefix: s.SecretsPrefix, Fields: maps.Clone(s.Fields), Words: s.words}
}

// JudgeKey is one key in Settings.JudgeKeys.
type JudgeKey struct {
	// Seconds is the longest grant the judge makes of the key, and its default lifetime.
	Seconds uint32 `json:"seconds"`
	// Guidance is the key owner's instruction to the judge, "" for an entry that gives none.
	Guidance string `json:"guidance,omitempty"`
	// Source is the stored secret the name is a profile of, whose judge and guidance tags these
	// are, "" for a name served as itself; `secret show` of that source names its judged profiles.
	Source string `json:"source,omitempty"`
}

func (c *client) Settings(ctx context.Context) (Settings, error) {
	raw, err := c.do(ctx, http.MethodGet, c.route("/v1/settings"), nil, nil)
	if err != nil {
		return Settings{}, err
	}
	var settings Settings
	if err := c.decode(raw, &settings, "decode settings"); err != nil {
		return Settings{}, err
	}
	settings.answeredBy = c.baseURL
	return settings, nil
}

// Reread is POST /v1/secrets/{name}/reread's exact response shape to a call a session signed:
// whether the broker serves the secret now, as which type ("" for a stored secret, and from a
// broker older than the field), and, when it does not serve it, why. Told is whether the broker
// said so at all: a broker answers a call with no proof 202, the same for every name, and says
// nothing of what it serves, only that it took the reread.
type Reread struct {
	Name   string `json:"name"`
	Served bool   `json:"served"`
	Reason string `json:"reason"`
	Type   string `json:"type"`
	Told   bool   `json:"-"`
}

// RereadSecret asks the broker to read the secret name from its store now, as whoever just wrote
// it does, so the write is served (or gone) at once instead of at its next reload. name is a
// validated agent secret name, so it needs no escaping in the path. With signer the call is a
// session's, and the broker answers it once the reread is done with what it now serves (Told); a
// signer that can register its session again (a person's sign-in) does so once where the broker
// says the enrollment is not live (doProofStatus), and the retry spends a second token of both
// reread buckets. With a nil signer the call carries no proof, and a broker of this release
// answers 202 at once, before it reads the store, and says nothing more (Told false); a broker
// from before that answer says what it serves to either call.
func (c *client) RereadSecret(ctx context.Context, signer proofSigner, name string) (Reread, error) {
	path := "/v1/secrets/" + name + "/reread"
	var status int
	var raw []byte
	var err error
	if signer != nil {
		status, raw, err = c.doProofStatus(ctx, signer, http.MethodPost, path, nil)
	} else {
		status, raw, err = c.doStatus(ctx, http.MethodPost, c.route(path), nil, nil)
	}
	if err != nil {
		return Reread{}, err
	}
	if status == http.StatusAccepted {
		return Reread{Name: name}, nil
	}
	var reread Reread
	if err := c.decode(raw, &reread, "decode reread"); err != nil {
		return Reread{}, err
	}
	reread.Told = true
	return reread, nil
}
