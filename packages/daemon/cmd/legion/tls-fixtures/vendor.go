//go:build ignore

// vendor writes the two public test sites' own chains, certificates only, into this directory:
// vendored-emsign-chain.pem and vendored-aaa-chain.pem, each the chain its site served, leaf first,
// as a TLS client receives it. It was run once, with `go run vendor.go` from this directory, and
// what it wrote is checked in beside it, each file opening with its source, the time it was
// fetched, each certificate's end of validity and its SHA-256 fingerprint. Run it again before a
// vendored leaf's end (the leaves of 2026-11-20 end on 2027-05-03), and commit what it writes: once a
// leaf has expired, its positive control fails (probeVendoredTLSSites), the probe prints an UNKNOWN
// line for that chain and holds every client to the public site in its place.
//
// The probe serves these chains from its own server with a key of its own, since it has none of
// the sites' (tls_probe_server.go): a client refuses the chain for what it finds in it, as at the
// public site, or accepts it and then fails the server's signature, which the probe reads as an
// acceptance. So the chains need no key, and none is fetched or written.
package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	for file, host := range map[string]string{
		"vendored-emsign-chain.pem": "dv-g3b-emudhra.we-have-a-problem.com",
		"vendored-aaa-chain.pem":    "dv-g3b-aaa-emudhra.we-have-a-problem.com",
	} {
		dialer := &net.Dialer{Timeout: 30 * time.Second}
		// The chain is fetched to be refused, never trusted: verification is the clients' job.
		conn, err := tls.DialWithDialer(dialer, "tcp", host+":443", &tls.Config{ServerName: host, InsecureSkipVerify: true})
		if err != nil {
			log.Fatalf("fetch the chain of %s: %v", host, err)
		}
		certs := conn.ConnectionState().PeerCertificates
		conn.Close()
		var out strings.Builder
		fmt.Fprintf(&out, "The chain https://%s/ served on %s, leaf first, fetched by\n", host, time.Now().UTC().Format(time.RFC3339))
		fmt.Fprintf(&out, "packages/daemon/cmd/legion/tls-fixtures/vendor.go. Certificates only: the probe holds no key for them.\n")
		for index, cert := range certs {
			sum := sha256.Sum256(cert.Raw)
			fmt.Fprintf(&out, "%d: %s, issued by %s, valid until %s, SHA-256 %X\n", index, cert.Subject, cert.Issuer, cert.NotAfter.UTC().Format(time.RFC3339), sum)
		}
		for _, cert := range certs {
			out.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
		}
		if err := os.WriteFile(file, []byte(out.String()), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote", file, "with", len(certs), "certificates")
	}
}
