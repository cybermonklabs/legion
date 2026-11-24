// Copyright 2019-2025 The NATS Authors
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
// Modified: measureWriteFile is nats-server v2.10.29's writeFileWithOptionalSync and
// writeFileWithSync (server/filestore.go), with calls to the forced-order hooks added between
// their steps.

// The forced-order state writer of nats-server v2.10.29, which server.patch calls in place of the
// server's own. packages/envoy/scripts/forced-order.sh adds it to the server package beside the
// shared ../zz_measure.go.
package server

import (
	"io/fs"
	"os"
)

// measureWriteFile is v2.10.29's writeFileWithOptionalSync: writeFileWithSync under SyncAlways,
// otherwise os.WriteFile under the dios semaphore. Both write the final path directly
// (O_WRONLY|O_CREATE|O_TRUNC), so this server has no temporary file and no rename, and no
// write.preRename point. Unchanged except for the measure calls between the steps.
func (o *consumerFileStore) measureWriteFile(name string, data []byte, perm fs.FileMode) error {
	<-dios
	defer func() {
		dios <- struct{}{}
	}()
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if o.fs.fcfg.SyncAlways {
		flags |= os.O_SYNC
	}
	f, err := os.OpenFile(name, flags, perm)
	if err != nil {
		measure(o.mev("write.openErr", data, 0, err))
		return err
	}
	_, err = f.Write(data)
	if err1 := f.Close(); err1 != nil && err == nil {
		err = err1
	}
	measure(o.mev("write.written", data, 0, err))
	return err
}
