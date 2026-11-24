// Package secrets holds the broker's minters, which receive each value from the secret store
// (internal/cipher/secretstore) and mint what a grant releases, beside AWS and GCP, the stores over
// AWS Secrets Manager and Google Cloud Secret Manager, and Local, the in-memory stand-in for the
// AWS APIs that local development and tests run on.
package secrets
