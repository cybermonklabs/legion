// Package holdtest is the writer hold's test surrounding, for the tests of writerhold and of the
// commands that run inside a hold (envoy-infra buckets): the whole stack a hold acts on (Stack:
// the broker's hold routes over a test Postgres, a cluster of nats-servers behind the NATS auth
// service, and that auth service reading the hold row), and the clustered hub's Kubernetes objects
// over that cluster (Kube: its StatefulSet, pods, claims and their volumes, its evictions, and the
// port-forwards that reach each member). It is an ordinary package so every package's tests can
// import it, and it imports nothing of writerhold, so writerhold's own tests can too.
package holdtest
