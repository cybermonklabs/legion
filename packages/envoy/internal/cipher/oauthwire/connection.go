package oauthwire

// ConnectionProofMethod and ConnectionProofTarget are the 'htm' and 'htu' of the proof a session
// signs to connect to NATS with its access token: an RFC 9449-shaped DPoP proof over the server's
// own per-connection nonce, carried in the CONNECT's signature field beside the token. A NATS
// connection is no HTTP request, so the pair names the connection rather than a method and a URL;
// a proof signed for an HTTP request can never be presented on a connection, nor the other way
// round. The signer (internal/cipherclient, ConnectionProof) and the checker
// (internal/ciphertoken, VerifyConnectionProof) read the pair from here, as the two halves of one
// wire contract read every other name of this package.
const (
	ConnectionProofMethod = "CONNECT"
	ConnectionProofTarget = "nats:connect"
)
