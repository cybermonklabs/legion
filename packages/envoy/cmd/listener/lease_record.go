package main

import (
	"encoding/json"
	"reflect"
	"time"

	"github.com/sjawhar/envoy/internal/store"
)

// leaseFields are the lease record's fields this build knows (leaseRecordKeys names their JSON
// keys). holder is the holding pod's instance (ENVOY_INSTANCE_ID or its hostname), so a standby
// names the active pod; boot is random per process start, so two starts of one instance differ;
// deliver_subject is the durable's deliver subject the holder receives on; service_since is when
// this machine's pods started serving without a gap, the server's timestamp of the write that
// began it; acquired_at is the server's timestamp of the holder's acquiring write (informational);
// released marks a record its holder gave up on a graceful stop.
//
// stale_binding and unsettled tell the next holder that a pod of this build may still hold
// messages or a push binding, so that a binding it finds is not a pre-lease build's and its term
// must settle (leaseAcquisition). stale_binding is set by a holder that subscribed while the
// durable was push-bound by a binding it did not wait out (an expired holder's lingering
// connection), cleared by one that finds the durable not push-bound before it subscribes. Every
// other write carries it on. unsettled is set from the acquiring write of a term that must settle
// until that term's settle hold ends, and a release keeps it as it stands, so a holder stopped
// mid-hold hands the duty on; a graceful stop that hands messages back sets it before its release
// (Unsettle), so a release after one always carries it, and the release of a term whose validity
// had run out sets it too (Release).
type leaseFields struct {
	Holder         string    `json:"holder"`
	Boot           string    `json:"boot"`
	DeliverSubject string    `json:"deliver_subject"`
	ServiceSince   time.Time `json:"service_since,omitzero"`
	AcquiredAt     time.Time `json:"acquired_at,omitzero"`
	Released       bool      `json:"released"`
	StaleBinding   bool      `json:"stale_binding"`
	Unsettled      bool      `json:"unsettled"`
}

// leaseRecordKeys are leaseFields' JSON keys; any other key a record carries is a newer build's,
// kept across every rewrite.
var leaseRecordKeys = store.JSONKeys(reflect.TypeFor[leaseFields]())

// leaseRecord is the lease's value: the fields this build knows and the JSON of every key it does
// not, which encode writes back unchanged. Its unknown map is never modified once decoded, so
// copies of a record may share it.
type leaseRecord struct {
	leaseFields
	unknown map[string]json.RawMessage
}

func decodeLeaseRecord(data []byte) (leaseRecord, error) {
	var record leaseRecord
	unknown, err := store.DecodeKeeping(data, &record.leaseFields, leaseRecordKeys)
	if err != nil {
		return leaseRecord{}, err
	}
	record.unknown = unknown
	return record, nil
}

func (r leaseRecord) encode() ([]byte, error) {
	return store.EncodeKeeping(r.leaseFields, r.unknown)
}
