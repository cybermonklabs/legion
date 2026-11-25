// packages/envoy/internal/cipher/identity/google/servicetoken.go
//
// A service of the bus proving itself to the NATS auth service with this machine's own Google
// identity token: the client half of what the provider's NATS verifier reads
// (BROKER_NATS_GOOGLE_AUDIENCE). The bus, natsprobe and `envoy-dispatch settings` reach it
// through identity/providers, so none of them names this package.

package google

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/identity"
)

// ServiceAudienceVar is the audience of the Google identity token a service presents to the NATS
// auth service, as a process of the bus names it.
const ServiceAudienceVar = "NATS_SERVICE_GOOGLE_AUDIENCE"

// serviceReadTimeout bounds one read of the metadata server.
const serviceReadTimeout = 10 * time.Second

// ServiceToken is this VM's Google identity token for audience, minted by its metadata server at
// every Read (MetadataIdentity).
func ServiceToken(audience string) identity.TokenReader {
	return serviceToken{identity: NewMetadataIdentity(audience)}
}

type serviceToken struct {
	identity *MetadataIdentity
}

func (g serviceToken) Read() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), serviceReadTimeout)
	defer cancel()
	token, _, err := g.identity.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("natscred: read %s: %w", g.identity.Describe(), err)
	}
	return token, nil
}

// ServiceTokenSource is this provider's entry of the service-token registry: the variable a
// process of the bus names it by, natsprobe's flag, the row `envoy-dispatch settings` prints, and
// the words bus.CredentialName names it by, each the text they held before the registry.
func ServiceTokenSource() identity.ServiceTokenSource {
	return identity.ServiceTokenSource{
		Provider:    Kind,
		Variable:    ServiceAudienceVar,
		Flag:        "service-google-audience",
		Usage:       "connect as a service presenting this VM's Google identity token for this audience",
		Description: "Audience of the Google identity token this VM's metadata server mints, which Dispatch presents to a NATS hub whose connections the auth service admits, as `NATS_SERVICE_TOKEN_FILE` presents a file's. Set beside another credential variable, it refuses startup naming both.",
		Named:       "the Google identity token for the audience",
		Reader:      func(value string) identity.TokenReader { return ServiceToken(value) },
	}
}
