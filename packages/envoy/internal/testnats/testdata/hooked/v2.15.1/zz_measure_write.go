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
// server's own. packages/envoy/scripts/forced-order.sh adds it to the server package beside the
// shared ../zz_measure.go.
package server

import (
	"io/fs"
	"os"
	"path/filepath"
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
