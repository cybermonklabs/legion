package redact

import (
	"errors"
	"strings"
)

// AWS is err, the failure of a call through the AWS SDK, as a message may print it: the service and
// operation the SDK names the failed call by (the SDK's own fixed words, such as "S3 HeadBucket", or
// "STS AssumeRoleWithWebIdentity" for the sign-in a call needed and could not get), then the status
// of the answer it failed on, as StatusLine prints it, and that answer's error code where awsCodes
// names it; for a call that drew no answer, the class of its failure (Class). Nothing else of the
// SDK's text prints. It quotes net/http's error whole, and Go parses a redirect's Location before a
// client's CheckRedirect is asked, so whatever answers at the service's address can write the
// request's credential (its X-Amz-Security-Token or its signature, the web identity token a
// sign-in's form carries) into a Location that does not parse, or into one the SDK's client
// follows (a 307 or a 308), which the error then names; and it quotes the answer's request id and
// host id headers and its message as that party wrote them. A failure this package already put in
// words keeps those words, as Class keeps them. It unwraps to err, so errors.As still reads the
// SDK's own error: a throttle's code, a missing key. AWS(nil) is nil.
func AWS(err error) error {
	if err == nil {
		return nil
	}
	var worded *classified
	if errors.As(err, &worded) {
		return &classified{worded.text, err}
	}
	// The SDK wraps every call's failure in its operation (smithy.OperationError), the answer it
	// failed on in a response error that holds its status, and the answer's code in an API error;
	// a sign-in the call needed fails as an operation of its own under the call's. The operation
	// printed is the one the answer, or the failure with none, came from: the deepest one above it.
	var call, code string
	status := 0
	for e := err; e != nil; e = errors.Unwrap(e) {
		if status == 0 {
			if op, ok := e.(interface {
				Service() string
				Operation() string
			}); ok {
				call = op.Service() + " " + op.Operation()
			}
			if answered, ok := e.(interface{ HTTPStatusCode() int }); ok {
				status = answered.HTTPStatusCode()
			}
		}
		if api, ok := e.(interface{ ErrorCode() string }); ok && code == "" {
			code = api.ErrorCode()
		}
	}
	var words []string
	if status != 0 {
		words = append(words, "the server answered HTTP "+StatusLine(status))
	}
	if awsCodes[code] {
		words = append(words, "error code "+code)
	}
	text := strings.Join(words, ", ")
	if text == "" {
		text = class(err, "the server")
	}
	if call != "" {
		text = call + ": " + text
	}
	return &classified{text, err}
}

// awsCodes are the error codes AWS (this function) prints by name: the ones the calls this module
// makes through the SDK answer (S3's object and bucket reads and writes, Bedrock's InvokeModel,
// Secrets Manager's reads, KMS's ListAliases) and the codes of a refused signature, credential or
// sign-in, a throttle and a failure of AWS's own. A code is the answering party's text, and
// whatever answers at a service's address chooses it, so any other code is not printed.
var awsCodes = func() map[string]bool {
	codes := map[string]bool{}
	for _, code := range []string{
		// A refused signature, credential or sign-in (STS's AssumeRoleWithWebIdentity among them).
		"AccessDenied", "AccessDeniedException", "UnrecognizedClientException", "InvalidSignatureException",
		"SignatureDoesNotMatch", "IncompleteSignature", "InvalidClientTokenId", "InvalidAccessKeyId",
		"ExpiredToken", "ExpiredTokenException", "RequestExpired", "RequestTimeTooSkewed",
		"MissingAuthenticationToken", "InvalidToken", "AuthorizationHeaderMalformed",
		"InvalidIdentityToken", "IDPCommunicationError", "IDPRejectedClaim", "RegionDisabledException",
		// A throttle, and a failure of AWS's own.
		"Throttling", "ThrottlingException", "ThrottledException", "TooManyRequestsException",
		"RequestLimitExceeded", "SlowDown", "LimitExceededException", "ServiceQuotaExceededException",
		"ServiceUnavailable", "ServiceUnavailableException", "InternalError", "InternalFailure",
		"InternalServiceError", "InternalServerError", "InternalServerException", "RequestTimeout",
		"RequestTimeoutException", "KMSInternalException",
		// A request the service refused, and a resource it does not hold.
		"ValidationException", "ValidationError", "InvalidParameterException", "InvalidRequestException",
		"InvalidNextTokenException", "InvalidArnException", "InvalidMarkerException",
		"ResourceNotFoundException", "NotFoundException", "DecryptionFailure",
		"KMSInvalidStateException", "DisabledException",
		// S3's own, with the codes the SDK gives a HEAD's refusal, which has no body, from its status.
		"NoSuchBucket", "NoSuchKey", "NotFound", "Forbidden", "BadRequest", "PermanentRedirect",
		"InvalidBucketName", "BadDigest", "InvalidDigest", "XAmzContentSHA256Mismatch",
		"PreconditionFailed", "InvalidRequest", "EntityTooLarge",
		// Bedrock's own.
		"ModelNotReadyException", "ModelTimeoutException", "ModelErrorException", "ConflictException",
	} {
		codes[code] = true
	}
	return codes
}()
