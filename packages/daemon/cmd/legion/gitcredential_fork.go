package main

import "time"

// leakguardCurl is the curl the guard's curl reads run in this binary: empty in the binary, where
// the guard then runs /usr/bin/curl; only tests set it.
var leakguardCurl string

// forkReadBound is the bound of the credential helper's whole fork read, the token read included;
// only tests shorten it.
var forkReadBound = 10 * time.Second
