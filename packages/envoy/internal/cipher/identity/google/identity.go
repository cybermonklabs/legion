package google

// ComputeEngine is the instance a Google identity token was minted on, the `google` →
// `compute_engine` claims of a token asked for with format=full. Every field is empty for a token
// minted anywhere else: one a workload identity pool minted, and the one a pod's metadata server
// on Google Kubernetes Engine answers, neither of which runs on an instance of the caller's.
type ComputeEngine struct {
	// Project is the Google Cloud project the instance runs in (project_id).
	Project string
	// Zone is the zone it runs in.
	Zone string
	// Instance is the instance's name (instance_name), the one an entry's pattern matches.
	Instance string
	// InstanceID is the instance's numeric id, unique within a project and a zone.
	InstanceID string
}

// Whole reports whether the token carried a complete Compute Engine identity. A token carries all
// four of these claims or none, so anything between the two is a token this package will not read
// an identity out of.
func (c ComputeEngine) Whole() bool {
	return c.Project != "" && c.Zone != "" && c.Instance != "" && c.InstanceID != ""
}

// Empty reports whether the token carried no Compute Engine claim at all, which is what a token
// minted through a workload identity pool and a pod's own metadata token carry.
func (c ComputeEngine) Empty() bool {
	return c == ComputeEngine{}
}

// Identity is what a Google identity token says about the machine presenting it: the account it
// was minted for, whether the provider holds that address verified, and the Compute Engine
// instance it was minted on, when it was minted on one.
type Identity struct {
	// Account is the `email` claim, the service account the token was minted for.
	Account string
	// EmailVerified is the `email_verified` claim. Google writes it as a JSON boolean and some of
	// its endpoints as the string "true", so both are read.
	EmailVerified bool
	// Compute is the instance the token was minted on, every field empty for a token minted off
	// one.
	Compute ComputeEngine
}

// ReadIdentity reads a Google identity token's own claims out of the claim map of a token
// something else has already verified. It is the one reading of those claim shapes: the machine
// decision, the person decision's actor and the caller verifier all read one declaration rather
// than three spellings of Google's claim names. It refuses nothing: a claim that is absent or of
// another JSON type reads as empty, and which of those is a refusal is the reader's own rule.
func ReadIdentity(token map[string]any) Identity {
	identity := Identity{EmailVerified: isVerified(token["email_verified"])}
	identity.Account, _ = token["email"].(string)
	google, _ := token["google"].(map[string]any)
	compute, _ := google["compute_engine"].(map[string]any)
	identity.Compute.Project, _ = compute["project_id"].(string)
	identity.Compute.Zone, _ = compute["zone"].(string)
	identity.Compute.Instance, _ = compute["instance_name"].(string)
	identity.Compute.InstanceID, _ = compute["instance_id"].(string)
	return identity
}

// isVerified reads an `email_verified` claim, which is a JSON boolean on a token from Google's
// metadata server and the string "true" from some of its other endpoints.
func isVerified(claim any) bool {
	switch value := claim.(type) {
	case bool:
		return value
	case string:
		return value == "true"
	}
	return false
}
