package aws

import "github.com/aws/aws-sdk-go-v2/aws"

// The writer's internals its external tests call directly.
var (
	LoadClients   = loadClients
	StoreFailure  = awsStoreFailure
	SignInFailure = awsSignInFailure
	Words         = awsWords
	FileName      = awsFileName

	CallPermissions = awsCallPermissions
	Permissions     = awsPermissions

	StoreOn         = storeOn
	WriterClientsOn = writerClientsOn
)

// WebIdentityToken is the reader the SDK reads AWS_WEB_IDENTITY_TOKEN_FILE through.
type WebIdentityToken = webIdentityToken

// SourceWords are the words of the source awsSourceOf finds in sources: its phrase, how to choose
// another, and the shell assignments a printed command carries.
func SourceWords(sources []aws.CredentialSource) (phrase, another, prefix string) {
	s := awsSourceOf(sources)
	return s.phrase, s.another, s.prefix
}
