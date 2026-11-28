package gcp

import (
	"context"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// RereadBudget is the whole fleet's budget of POST /v1/secrets/{name}/reread on Google Secret
// Manager, which unlike the default store does bound that route by a provider's quota: Secret
// Manager serves 600 read requests a minute per project (cloud.google.com/secret-manager/quotas,
// "Read request"), the whole project shares them, and one reread spends two of them (the secret
// and its version). The budget is a fifth of that quota - 120 metadata reads a minute, one reread a
// second - which is what one pod alone was allowed; the other four fifths are what every pod's
// five-minute namespace reload, its miss-path rereads and its releases spend. Those are multiplied
// by running more pods, not by the fan-out: each pod spends its own, and none of them notifies -
// only the reread route does, while a miss-path reread, a reload and a release send nothing - so
// they too grow with the replica count, and the route's share is not the place to find more room.
//
// internal/cipher/api divides it among the pods (RereadOverallFor) by the replica count squared,
// for the reason its default bucket gives - each accepted reread costs a store read on every pod,
// so the fleet spends replicas² × one pod's rate - which for two pods is 2 pods × 0.25 reread/s ×
// 2 store reads × 2 metadata reads = 2 metadata reads a second, the same 120 a minute. Three pods
// divide the budget by nine rather than by four, one reread every 9 s each, and four by sixteen,
// one every 16 s.
//
// Its burst is the default budget's ten, undivided by the replica count for the reason given
// there: the depth a burst admits is one person's write burst, which the replica count does not
// change, and what the depth guards is a pod's own writer lock rather than the project's quota.
// The whole fleet's transient at that depth is 2 pods × 10 rereads × 2 store reads × 2 metadata
// reads = 80 metadata reads, eight seconds of the shared 600 a minute, after which the rate above
// holds the traffic down again.
var RereadBudget = ratelimit.Limit{Every: time.Second, Burst: 10}

// Backend is the GCP store's registry entry.
type Backend struct{}

func (Backend) Name() string                  { return Name }
func (Backend) ID() string                    { return "gcp" }
func (Backend) Development() bool             { return false }
func (Backend) RereadBudget() ratelimit.Limit { return RereadBudget }
func (Backend) Variables() []string           { return []string{keyVariable} }

// OpenStore opens Secret Manager under Application Default Credentials (NewClient) and reads the
// agent secrets on the key cfg names.
func (Backend) OpenStore(ctx context.Context, cfg secretstore.Config) (secretstore.Opened, error) {
	client, err := NewClient(ctx)
	if err != nil {
		return secretstore.Opened{}, err
	}
	return secretstore.Opened{Store: New(client, cfg.(config).keyName), Credentials: "application-default", Close: client.Close}, nil
}

// Kind is the store's kind as the CLI's lines name it.
func (Backend) Kind() string { return "GCP Secret Manager" }

// Place is the project the broker's settings put the agent secrets in, as a line names it after
// Kind: "project <number>", "" where they name none; as the broker's address lets its answer
// print (Settings.Words).
func (Backend) Place(s secretstore.Settings) string {
	settings := settingsOf(s)
	if settings.GCPProject == "" {
		return ""
	}
	return "project " + settings.words(settings.GCPProject)
}

// OpenWriter refuses settings whose prefix, project or key name do not agree
// (checkSettingsKMSKeyName), then signs in with the first Google credential source present
// (newWriter), running every gcloud through opts.Run.
func (Backend) OpenWriter(ctx context.Context, s secretstore.Settings, opts secretstore.WriterOptions) (secretstore.Writer, error) {
	settings := settingsOf(s)
	if err := checkSettingsKMSKeyName(settings); err != nil {
		return nil, err
	}
	w, err := newWriter(ctx, settings, opts.Run)
	if err != nil {
		return nil, err
	}
	return w, nil
}

// HelpRows are the Google variables of the CLI's environment table.
func (Backend) HelpRows() string {
	return `  CLOUDSDK_AUTH_ACCESS_TOKEN a Google access token the secret forms sign in with against a broker
                             on Google Cloud; it outranks every other Google source
  GOOGLE_APPLICATION_CREDENTIALS
                             a Google credentials file the secret forms sign in with where no
                             token is lent; it outranks the gcloud variables below
  CLOUDSDK_ACTIVE_CONFIG_NAME
                             the gcloud configuration whose login the secret forms sign in with,
                             before the well-known credentials file, the active gcloud account
                             and the metadata server
  CLOUDSDK_CORE_ACCOUNT      the gcloud login the secret forms sign in with, at the same place in
                             that order; the store's IAM decides what each may do
`
}

// ProfileHint is the Google Cloud clause of the CLI's --profile refusal.
func (Backend) ProfileHint() (secretstore.ProfileHint, bool) {
	return secretstore.ProfileHint{Rank: 10, Clause: "on Google Cloud set CLOUDSDK_ACTIVE_CONFIG_NAME (a gcloud configuration) or CLOUDSDK_CORE_ACCOUNT (a gcloud account), which CLOUDSDK_AUTH_ACCESS_TOKEN and GOOGLE_APPLICATION_CREDENTIALS outrank when set"}, true
}
