package embed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/smithy-go/logging"

	"github.com/sjawhar/envoy/awsbody"
	"github.com/sjawhar/envoy/internal/dispatch/files"
)

const (
	// BedrockModel is the Bedrock model a BedrockClient calls unless WithModel says otherwise:
	// Cohere Embed v4 through the US cross-region inference profile - the exact model and region
	// LEGION-386's benchmark measured (searchbench-results.md, linear-parity-plan.md "Who we
	// call") - reached through the AWS SDK's default credential chain (Dispatch's task role in
	// production, the devbox's instance role locally) and bedrock:InvokeModel, never Cohere's own
	// API or a shared API key (LEGION-549 ask 649cdaf2: Sami ruled out a hand-seeded secret and
	// any one-off admin apply). It is also the identity every Bedrock vector already stored
	// carries, so a deployment that keeps Bedrock never re-embeds.
	BedrockModel = "us.cohere.embed-v4:0"
	// BedrockMaxBatchTexts is the most texts one Bedrock call sends: Cohere Embed v4's texts
	// input accepts up to 96, on Bedrock as through Cohere's own API.
	BedrockMaxBatchTexts = 96
	// bedrockBackgroundTokensPerMinute is two thirds of the account's Cohere Embed v4 quota of
	// 300,000 tokens a minute, left to background work; the other third is live search's.
	bedrockBackgroundTokensPerMinute = 200_000
	// bedrockMaxBatchBytes is Limits().MaxBatchBytes: Azure's byte budget for the same model
	// (azureMaxBatchBytes), because Bedrock's own rate is not measured. It keeps a group inside
	// embedqueue's callBound, the only bound a Bedrock call has, only while Bedrock embeds at least
	// bedrockMaxBatchBytes ÷ callBound tokens a second: at one token a byte, the most a group can
	// cost, 320,000 in 60 s, about 5,300, a third of Azure's slowest measured rate (16,152). If
	// Bedrock is slower, the largest group runs out at the bound, which is an outage (Bounded),
	// and is bisected until each half fits (embedqueue's bisectSplit): a budget set too large
	// costs wasted calls and their token reservations, not rows. What embedqueue's rows prove is the
	// grouping: a text that would cross the edge starts the next group whole, never cut to fit, and
	// no group past the budget reaches the call.
	bedrockMaxBatchBytes = azureMaxBatchBytes
)

// BedrockClient calls Cohere Embed v4 on AWS Bedrock (InvokeModel).
type BedrockClient struct {
	bedrock  *bedrockruntime.Client
	model    string
	endpoint string
}

// BedrockOption configures a BedrockClient beyond its AWS configuration.
type BedrockOption func(*BedrockClient)

// WithModel overrides the Bedrock model id; a test points it at a fake endpoint's model id. The
// client's Identity follows it, so a vector records the model that actually embedded it.
func WithModel(model string) BedrockOption {
	return func(c *BedrockClient) { c.model = model }
}

// WithBaseEndpoint overrides Bedrock's regional endpoint; a test points it at a local server
// that stands in for Bedrock's InvokeModel API (the AWS SDK still signs every request against
// whatever credentials the client's config carries, which a test's static, fake credentials
// satisfy - nothing before Bedrock's own side verifies the signature).
func WithBaseEndpoint(url string) BedrockOption {
	return func(c *BedrockClient) { c.endpoint = url }
}

// NewBedrock loads the AWS SDK's default configuration (the environment, the shared config files,
// and the instance/task credential chain - nothing this package reads directly) and returns a
// BedrockClient. It returns an error if no region is configured - the one failure
// LoadDefaultConfig would otherwise accept silently and only fail on the first call - or if
// loading the configuration itself fails. Either is meaning search unavailable (LEGION-549's
// degraded mode): a caller logs it and runs with a nil Embedder, not a reason to refuse to boot -
// a CI job or a devbox with no AWS_REGION set must still start Dispatch and answer search,
// keyword-only.
func NewBedrock(ctx context.Context, opts ...BedrockOption) (*BedrockClient, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithLogger(logging.LoggerFunc(files.LogSDK)))
	if err != nil {
		return nil, fmt.Errorf("embed: load AWS configuration: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("embed: needs an AWS region: set AWS_REGION, or a region in the shared AWS config")
	}
	return NewBedrockWithConfig(cfg, opts...), nil
}

// NewBedrockWithConfig builds a BedrockClient directly from an already-loaded AWS configuration,
// skipping NewBedrock's region check and credential-chain loading - a test supplies its own
// static, fake credentials and WithBaseEndpoint rather than touching the real AWS SDK default
// chain. Its HTTP client goes through awsbody.Client, so an answered InvokeModel over HTTP/1.1 is
// not failed by smithy-go closing the request body before net/http has finished with it (package
// awsbody). Bedrock's own endpoint answered over HTTP/2, which that failure does not reach; a
// stand-in over plain HTTP, as the queue's tests use, is HTTP/1.1.
func NewBedrockWithConfig(cfg aws.Config, opts ...BedrockOption) *BedrockClient {
	c := &BedrockClient{model: BedrockModel}
	for _, opt := range opts {
		opt(c)
	}
	c.bedrock = bedrockruntime.NewFromConfig(cfg, func(o *bedrockruntime.Options) {
		if c.endpoint != "" {
			o.BaseEndpoint = aws.String(c.endpoint)
		}
		o.HTTPClient = awsbody.Client(o.HTTPClient)
	})
	return c
}

// Identity is the Bedrock model id this client calls: BedrockModel unless WithModel overrode it.
func (c *BedrockClient) Identity() string { return c.model }

// Limits reports Bedrock's batch limit, the background share of the account's quota and the byte
// budget each background call keeps to (bedrockMaxBatchBytes).
func (c *BedrockClient) Limits() Limits {
	return Limits{
		MaxBatchTexts:             BedrockMaxBatchTexts,
		MaxBatchBytes:             bedrockMaxBatchBytes,
		BackgroundTokensPerMinute: bedrockBackgroundTokensPerMinute,
	}
}

type bedrockEmbedRequest struct {
	InputType       InputType `json:"input_type"`
	Texts           []string  `json:"texts"`
	EmbeddingTypes  []string  `json:"embedding_types"`
	OutputDimension int       `json:"output_dimension"`
}

// bedrockEmbedResponse is Bedrock's actual response shape, confirmed by a live call (2026-10-05):
// the vectors nest under embeddings.float regardless of how many embedding_types were requested -
// the AWS documentation's claim of a flat array for a single requested type does not match what
// Bedrock answers.
type bedrockEmbedResponse struct {
	Embeddings struct {
		Float [][]float32 `json:"float"`
	} `json:"embeddings"`
}

// Embed calls Bedrock once for up to BedrockMaxBatchTexts texts; a caller with more batches it
// itself (embedqueue does, one batch at a time). A throttle comes back marked for IsThrottled, and
// an outage code (bedrockOutageErrorCodes) or a call that ran out of time while ctx was live
// (timedOut: the AWS SDK's HTTP client's own timeout) for IsOutage. It sets no time bound of its
// own: its caller bounds the call through ctx (embedqueue's callBound, search's
// searchEmbedTimeout).
func (c *BedrockClient) Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error) {
	bounded, err := boundBatch(texts, BedrockMaxBatchTexts)
	if err != nil || bounded == nil {
		return nil, err
	}
	body, err := json.Marshal(bedrockEmbedRequest{
		InputType:       inputType,
		Texts:           bounded,
		EmbeddingTypes:  []string{"float"},
		OutputDimension: Dimension,
	})
	if err != nil {
		return nil, fmt.Errorf("embed: encode request: %w", err)
	}
	contentType := aws.String("application/json")
	out, err := c.bedrock.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(c.model),
		Body:        body,
		ContentType: contentType,
		Accept:      contentType,
	})
	if err != nil {
		err = fmt.Errorf("embed: bedrock invoke model: %w", err)
		switch {
		case bedrockThrottled(err):
			return nil, &throttleError{err: err}
		case bedrockOutage(err), timedOut(ctx, err):
			return nil, &outageError{err: err}
		}
		return nil, err
	}
	var decoded bedrockEmbedResponse
	if err := json.Unmarshal(out.Body, &decoded); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if len(decoded.Embeddings.Float) != len(texts) {
		return nil, fmt.Errorf("embed: bedrock returned %d embeddings for %d texts", len(decoded.Embeddings.Float), len(texts))
	}
	return decoded.Embeddings.Float, nil
}

// bedrockThrottleErrorCodes extends the AWS SDK's own retry.DefaultThrottleErrorCodes with two
// Bedrock-specific codes the SDK's generic set does not classify as throttles (confirmed against
// the pinned aws-sdk-go-v2's retry/standard.go: neither is in DefaultThrottleErrorCodes):
// ServiceUnavailableException (Bedrock's own capacity temporarily unavailable, nothing to do
// with the text being embedded) and ModelNotReadyException (an on-demand model still scaling up
// - the AWS SDK's own retry-behavior guide recommends backing off on it) are both transient
// conditions about Bedrock's capacity, never evidence about a specific row's content, exactly
// like ThrottlingException already is.
var bedrockThrottleErrorCodes = func() map[string]struct{} {
	codes := make(map[string]struct{}, len(retry.DefaultThrottleErrorCodes)+2)
	for code := range retry.DefaultThrottleErrorCodes {
		codes[code] = struct{}{}
	}
	codes["ServiceUnavailableException"] = struct{}{}
	codes["ModelNotReadyException"] = struct{}{}
	return codes
}()

var bedrockThrottles = retry.IsErrorThrottles{retry.ThrottleErrorCode{Codes: bedrockThrottleErrorCodes}}

// bedrockThrottled reports whether err is Bedrock signaling "too many requests, back off" or one
// of the two closely related capacity conditions bedrockThrottleErrorCodes adds. It uses the AWS
// SDK's own throttle-classification machinery (retry.IsErrorThrottles/ThrottleErrorCode - the
// same checker retry.DefaultThrottles uses, over an extended code set) instead of matching one
// named exception type by hand, so a related condition this package has not written a case for
// is still recognized correctly. Embed marks what it finds for IsThrottled.
func bedrockThrottled(err error) bool {
	return bedrockThrottles.IsErrorThrottle(err) == aws.TrueTernary
}

// bedrockOutageErrorCodes are Bedrock's codes for an answer it could not give now that is no
// throttle, as Azure's 408 and 5xx are (azureStatusError): ModelTimeoutException (408, the model
// did not answer in time) and InternalServerException (500, the service failed). Like those, one
// may be a text Bedrock fails on every time, so embedqueue bisects the batch and confirms the row
// with its canary, and only a failing canary backs the pace off (IsOutage).
var bedrockOutageErrorCodes = map[string]struct{}{"ModelTimeoutException": {}, "InternalServerException": {}}

// bedrockOutage reports whether err carries one of bedrockOutageErrorCodes as its error code (the
// smithy API error the SDK answers a coded failure with). Embed marks what it finds for IsOutage.
// The code is read here rather than through the SDK's throttle classifier bedrockThrottled uses:
// an outage is no throttle.
func bedrockOutage(err error) bool {
	var coded interface{ ErrorCode() string }
	if !errors.As(err, &coded) {
		return false
	}
	_, ok := bedrockOutageErrorCodes[coded.ErrorCode()]
	return ok
}
