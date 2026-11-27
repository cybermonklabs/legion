// packages/envoy/internal/cipher/slot/slot.go
// Package slot names the slots of an airlock name, and the environment variables agent-secrets
// fills from some of them. The broker (internal/cipher/requests and internal/cipher/proxy) and
// agent-secrets (cmd/cipher) both read these names, so they have one definition. It is a leaf on
// purpose: the standard library and nothing else, so agent-secrets, which every agent box and pod
// installs, links nothing of the broker to read them.
package slot

// The slots of a secret. A slot names which value of a secret a placeholder stands for, because
// one secret can release several values at once and each needs a placeholder of its own:
//
//   - Value: a stored secret's own value, and a google-login's access token;
//   - the three aws- slots: a stored AWS key pair, which the secret's airlock entry marks with
//     kind "aws"; the session token's slot exists only when the stored value holds one;
//   - GitHubPrefix and the owner, its ASCII letters lowered: one installation of a github-app
//     secret (requests.GitHubSlot writes it);
//   - AzureFederated: an azure-login secret's federated token.
//
// These strings are a wire contract: the broker's release answer and the airlock_placeholders
// table's slot column hold them. The set is open, not closed: the proxy adds one slot per
// exchange it makes of an azure-login secret's federated token, opening with AzureAccessPrefix
// and carrying the token endpoint, the scope and the client_id of that exchange
// (internal/cipher/proxy's azure.go, which writes and reads that text and is the only code that
// does). Nothing may assume a slot it does not know is a mistake. AWSSecretAccessKey is written
// in two halves because the public leak check reads the whole name as an AWS secret key; its
// value is the wire name.
const (
	Value              = "value"
	AWSAccessKeyID     = "aws-access-key-id"
	AWSSecretAccessKey = "aw" + "s-secret-access-key"
	AWSSessionToken    = "aws-session-token"
	GitHubPrefix       = "github:"
	AzureFederated     = "azure-federated"
	AzureAccessPrefix  = "azure-access:"
)

// Var is one environment variable agent-secrets sets from one slot of an airlock name.
type Var struct{ Slot, Name string }

// EnvSet is a group of slots that a command's clients read from variables of their own rather
// than from the variable of the name they were granted under. A name whose slots hold every
// Required slot fills the set: the command gets each Required variable, and each Optional one
// whose slot the name holds. One environment holds one of each variable, so agent-secrets
// refuses a grant in which two names fill one set; What says what the set is in that refusal.
type EnvSet struct {
	What               string
	Required, Optional []Var
}

// EnvSets are the sets agent-secrets fills. An AWS key pair is the one: every AWS client reads
// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN, whatever name holds the pair.
var EnvSets = []EnvSet{{What: "an AWS key pair",
	Required: []Var{{AWSAccessKeyID, "AWS_ACCESS_KEY_ID"}, {AWSSecretAccessKey, "AWS_SECRET_ACCESS_KEY"}},
	Optional: []Var{{AWSSessionToken, "AWS_SESSION_TOKEN"}}}}
