// Copyright 2019-2026 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Modified: measureWriteFile is nats-server v2.15.1's writeFileWithOptionalSync and
// writeAtomicallyWithTemp (server/filestore.go), with calls to the forced-order hooks added
// between their steps.

// The forced-order state writer of nats-server v2.15.1, which server.patch calls in place of the
// server's own, and the hooks of its duplicate-id window, which read fields of its own (stream.ddwin,
// v2.10.29 has none and keeps the window in the stream's config). packages/envoy/scripts/forced-order.sh
// adds it to the server package beside the shared ../zz_measure.go.
package server

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// measureWriteFile is v2.15.1's writeFileWithOptionalSync followed by writeAtomicallyWithTemp,
// unchanged except for the measure calls between their steps.
func (o *consumerFileStore) measureWriteFile(name string, data []byte, perm fs.FileMode) error {
	fst := o.fs
	sync := fst.syncAlways.Load() || fst.syncOnFlush.Load()
	tmp := name + ".tmp"
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if sync {
		flags = flags | os.O_SYNC
	}
	fst.dios.acquire()
	defer fst.dios.release()
	f, err := os.OpenFile(tmp, flags, perm)
	if err != nil {
		measure(o.mev("write.openErr", data, 0, err))
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	measure(o.mev("write.preRename", data, 0, nil))
	if err := os.Rename(tmp, name); err != nil {
		_ = os.Remove(tmp)
		measure(o.mev("write.renameErr", data, 0, err))
		return err
	}
	measure(o.mev("write.renamed", data, 0, nil))
	if sync && canFsyncDirectories {
		if err = syncDir(filepath.Dir(name)); err != nil {
			return err
		}
	}
	return nil
}

// measureDedupeReset reports a stream update whose duplicate window changes, past its re-arm of
// the stream's purge of remembered duplicate ids and before the store of the new window, so a
// purge that runs here measures the ids' ages against oldWindow, the window being replaced. The
// update re-arms that purge only while the stream remembers an id; the report comes either way,
// and a hold here parks a purge of its own only for a stream that remembers one. It reports
// nothing when the two windows are the same, since the update touches the purge only when they
// differ.
//
// It takes no lock the point's own caller holds: the update holds the stream's lock here, so the
// stream's name is the update's own (a stream is never renamed) and the store directory the
// server's. It does not wait for the purge the re-arm fires either, so the two reports can come
// in either order; both are after that re-arm and before the store of the new window.
func measureDedupeReset(mset *stream, name string, oldWindow, newWindow time.Duration, point string) {
	if MeasureHook.Load() == nil || oldWindow == newWindow {
		return
	}
	measure(&MeasureEvent{Point: point, Store: measureStoreDir(mset), Stream: name, Window: oldWindow, NewWindow: newWindow})
}

// measureDedupePurged reports a purge of remembered duplicate ids that has done its work, with
// the dedupe lock still held, so the window and the count it reports are the purge's own: the
// window it measured the ids' ages against (that lock is held for the whole purge, so no update
// can have stored another meanwhile) and the ids it kept. It must not take that lock again, and
// it takes no lock a stream update parked at dedupe.reset holds, so the report comes while that
// update waits.
func measureDedupePurged(mset *stream, point string) {
	if MeasureHook.Load() == nil {
		return
	}
	measure(&MeasureEvent{Point: point, Store: measureStoreDir(mset), Stream: mset.getCfgName(),
		Window: mset.ddwin, Remembered: len(mset.ddmap)})
}

// measureStoreDir is the store directory of the server the stream runs in, or none for a stream
// with no server. It takes the server's options lock alone, which no caller of a duplicate-window
// point holds.
func measureStoreDir(mset *stream) string {
	if mset == nil || mset.srv == nil {
		return _EMPTY_
	}
	return mset.srv.getOpts().StoreDir
}
