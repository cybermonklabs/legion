// packages/envoy/cmd/agent-secrets/client_settings.go
//
// The two broker routes the secret forms call: GET /v1/settings, which takes no credential, and
// POST /v1/secrets/{name}/reread, which answers a session's proof in full and a call with none
// with the same 202 for every name.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Settings is GET /v1/settings's exact response shape: the store and namespace every agent secret
// lives under and, per store, the key they are encrypted with and where they are; on either store,
// the Google exchange audience a machine's own identity token must carry, from which the use
// command finds that identity with no setting (machineSource); and whether the broker reads a
// secret's type from the type tag (SecretTypeTag), without which the secret forms write no typed
// secret (requireTypeTag). A broker from before the store field answers none, and holds them in
// AWS Secrets Manager; one from before the audience, or one that accepts no machine identity,
// names none; and one from before the type tag answers no secret_type_tag.
type Settings struct {
	Store                  string `json:"store,omitempty"`
	SecretsPrefix          string `json:"secrets_prefix"`
	KMSKeyARN              string `json:"kms_key_arn,omitempty"`
	AWSAccountID           string `json:"aws_account_id,omitempty"`
	AWSRegion              string `json:"aws_region,omitempty"`
	KMSKeyName             string `json:"kms_key_name,omitempty"`
	GCPProject             string `json:"gcp_project,omitempty"`
	GoogleExchangeAudience string `json:"google_exchange_audience,omitempty"`
	SecretTypeTag          bool   `json:"secret_type_tag,omitempty"`
	// JudgeKeys is each key the broker's airlock rules give the model judge, with its judge
	// seconds and its guidance; absent where the rules give the judge nothing. `secret show`
	// reads it to say who decides a request for a key and what the judge will look for.
	JudgeKeys map[string]JudgeKey `json:"judge_keys,omitempty"`
}

// JudgeKey is one key in Settings.JudgeKeys.
type JudgeKey struct {
	// Seconds is the longest grant the judge makes of the key, and its default lifetime.
	Seconds uint32 `json:"seconds"`
	// Guidance is the key owner's instruction to the judge, "" for an entry that gives none.
	Guidance string `json:"guidance,omitempty"`
}

func (c *client) Settings(ctx context.Context) (Settings, error) {
	raw, err := c.do(ctx, http.MethodGet, c.route("/v1/settings"), nil, nil)
	if err != nil {
		return Settings{}, err
	}
	var settings Settings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return Settings{}, fmt.Errorf("decode settings: %w", err)
	}
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
	if err := json.Unmarshal(raw, &reread); err != nil {
		return Reread{}, fmt.Errorf("decode reread: %w", err)
	}
	reread.Told = true
	return reread, nil
}
