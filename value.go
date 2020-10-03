/*
 * Copyright 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package badger

import (
	"bytes"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/badger/v2/y"
	"github.com/dgraph-io/ristretto/z"
	"github.com/pkg/errors"
	"golang.org/x/net/trace"
)

// maxVlogFileSize is the maximum size of the vlog file which can be created. Vlog Offset is of
// uint32, so limiting at max uint32.
var maxVlogFileSize = math.MaxUint32

// Values have their first byte being byteData or byteDelete. This helps us distinguish between
// a key that has never been seen and a key that has been explicitly deleted.
const (
	bitDelete                 byte = 1 << 0 // Set if the key has been deleted.
	bitValuePointer           byte = 1 << 1 // Set if the value is NOT stored directly next to key.
	bitDiscardEarlierVersions byte = 1 << 2 // Set if earlier versions can be discarded.
	// Set if item shouldn't be discarded via compactions (used by merge operator)
	bitMergeEntry byte = 1 << 3
	// The MSB 2 bits are for transactions.
	bitTxn    byte = 1 << 6 // Set if the entry is part of a txn.
	bitFinTxn byte = 1 << 7 // Set if the entry is to indicate end of txn in value log.

	mi int64 = 1 << 20

	// size of vlog header.
	// +----------------+------------------+
	// | keyID(8 bytes) |  baseIV(12 bytes)|
	// +----------------+------------------+
	vlogHeaderSize = 20
)

var errStop = errors.New("Stop iteration")
var errTruncate = errors.New("Do truncate")
var errDeleteVlogFile = errors.New("Delete vlog file")

type logEntry func(e Entry, vp valuePointer) error

type safeRead struct {
	k []byte
	v []byte

	recordOffset uint32
	lf           *logFile
}

// hashReader implements io.Reader, io.ByteReader interfaces. It also keeps track of the number
// bytes read. The hashReader writes to h (hash) what it reads from r.
type hashReader struct {
	r         io.Reader
	h         hash.Hash32
	bytesRead int // Number of bytes read.
}

func newHashReader(r io.Reader) *hashReader {
	hash := crc32.New(y.CastagnoliCrcTable)
	return &hashReader{
		r: r,
		h: hash,
	}
}

// Read reads len(p) bytes from the reader. Returns the number of bytes read, error on failure.
func (t *hashReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil {
		return n, err
	}
	t.bytesRead += n
	return t.h.Write(p[:n])
}

// ReadByte reads exactly one byte from the reader. Returns error on failure.
func (t *hashReader) ReadByte() (byte, error) {
	b := make([]byte, 1)
	_, err := t.Read(b)
	return b[0], err
}

// Sum32 returns the sum32 of the underlying hash.
func (t *hashReader) Sum32() uint32 {
	return t.h.Sum32()
}

// Entry reads an entry from the provided reader. It also validates the checksum for every entry
// read. Returns error on failure.
func (r *safeRead) Entry(reader io.Reader) (*Entry, error) {
	tee := newHashReader(reader)
	var h header
	hlen, err := h.DecodeFrom(tee)
	if err != nil {
		return nil, err
	}
	if h.klen > uint32(1<<16) { // Key length must be below uint16.
		return nil, errTruncate
	}
	kl := int(h.klen)
	if cap(r.k) < kl {
		r.k = make([]byte, 2*kl)
	}
	vl := int(h.vlen)
	if cap(r.v) < vl {
		r.v = make([]byte, 2*vl)
	}

	e := &Entry{}
	e.offset = r.recordOffset
	e.hlen = hlen
	buf := make([]byte, h.klen+h.vlen)
	if _, err := io.ReadFull(tee, buf[:]); err != nil {
		if err == io.EOF {
			err = errTruncate
		}
		return nil, err
	}
	if r.lf.encryptionEnabled() {
		if buf, err = r.lf.decryptKV(buf[:], r.recordOffset); err != nil {
			return nil, err
		}
	}
	e.Key = buf[:h.klen]
	e.Value = buf[h.klen:]
	var crcBuf [crc32.Size]byte
	if _, err := io.ReadFull(reader, crcBuf[:]); err != nil {
		if err == io.EOF {
			err = errTruncate
		}
		return nil, err
	}
	crc := y.BytesToU32(crcBuf[:])
	if crc != tee.Sum32() {
		return nil, errTruncate
	}
	e.meta = h.meta
	e.UserMeta = h.userMeta
	e.ExpiresAt = h.expiresAt
	return e, nil
}

func (vlog *valueLog) rewrite(f *logFile, tr trace.Trace) error {
	vlog.filesLock.RLock()
	maxFid := vlog.maxFid
	vlog.filesLock.RUnlock()
	y.AssertTruef(uint32(f.fid) < maxFid, "fid to move: %d. Current max fid: %d", f.fid, maxFid)
	tr.LazyPrintf("Rewriting fid: %d", f.fid)

	wb := make([]*Entry, 0, 1000)
	var size int64

	y.AssertTrue(vlog.db != nil)
	var count, moved int
	fe := func(e Entry) error {
		count++
		if count%100000 == 0 {
			tr.LazyPrintf("Processing entry %d", count)
		}

		vs, err := vlog.db.get(e.Key)
		if err != nil {
			return err
		}
		if discardEntry(e, vs, vlog.db) {
			return nil
		}

		// Value is still present in value log.
		if len(vs.Value) == 0 {
			return errors.Errorf("Empty value: %+v", vs)
		}
		var vp valuePointer
		vp.Decode(vs.Value)

		// If the entry found from the LSM Tree points to a newer vlog file, don't do anything.
		if vp.Fid > f.fid {
			return nil
		}
		// If the entry found from the LSM Tree points to an offset greater than the one
		// read from vlog, don't do anything.
		if vp.Offset > e.offset {
			return nil
		}
		// If the entry read from LSM Tree and vlog file point to the same vlog file and offset,
		// insert them back into the DB.
		// NOTE: It might be possible that the entry read from the LSM Tree points to
		// an older vlog file. See the comments in the else part.
		if vp.Fid == f.fid && vp.Offset == e.offset {
			moved++
			// This new entry only contains the key, and a pointer to the value.
			ne := new(Entry)
			ne.meta = 0 // Remove all bits. Different keyspace doesn't need these bits.
			ne.UserMeta = e.UserMeta
			ne.ExpiresAt = e.ExpiresAt
			ne.Key = append([]byte{}, e.Key...)
			ne.Value = append([]byte{}, e.Value...)
			es := int64(ne.estimateSize(vlog.opt.ValueThreshold))
			// Consider size of value as well while considering the total size
			// of the batch. There have been reports of high memory usage in
			// rewrite because we don't consider the value size. See #1292.
			es += int64(len(e.Value))

			// Ensure length and size of wb is within transaction limits.
			if int64(len(wb)+1) >= vlog.opt.maxBatchCount ||
				size+es >= vlog.opt.maxBatchSize {
				tr.LazyPrintf("request has %d entries, size %d", len(wb), size)
				if err := vlog.db.batchSet(wb); err != nil {
					return err
				}
				size = 0
				wb = wb[:0]
			}
			wb = append(wb, ne)
			size += es
		} else {
			// It might be possible that the entry read from LSM Tree points to
			// an older vlog file.  This can happen in the following situation.
			// Assume DB is opened with
			// numberOfVersionsToKeep=1
			//
			// Now, if we have ONLY one key in the system "FOO" which has been
			// updated 3 times and the same key has been garbage collected 3
			// times, we'll have 3 versions of the movekey
			// for the same key "FOO".
			//
			// NOTE: moveKeyi is the gc'ed version of the original key with version i
			// We're calling the gc'ed keys as moveKey to simplify the
			// explanantion. We used to add move keys but we no longer do that.
			//
			// Assume we have 3 move keys in L0.
			// - moveKey1 (points to vlog file 10),
			// - moveKey2 (points to vlog file 14) and
			// - moveKey3 (points to vlog file 15).
			//
			// Also, assume there is another move key "moveKey1" (points to
			// vlog file 6) (this is also a move Key for key "FOO" ) on upper
			// levels (let's say 3). The move key "moveKey1" on level 0 was
			// inserted because vlog file 6 was GCed.
			//
			// Here's what the arrangement looks like
			// L0 => (moveKey1 => vlog10), (moveKey2 => vlog14), (moveKey3 => vlog15)
			// L1 => ....
			// L2 => ....
			// L3 => (moveKey1 => vlog6)
			//
			// When L0 compaction runs, it keeps only moveKey3 because the number of versions
			// to keep is set to 1. (we've dropped moveKey1's latest version)
			//
			// The new arrangement of keys is
			// L0 => ....
			// L1 => (moveKey3 => vlog15)
			// L2 => ....
			// L3 => (moveKey1 => vlog6)
			//
			// Now if we try to GC vlog file 10, the entry read from vlog file
			// will point to vlog10 but the entry read from LSM Tree will point
			// to vlog6. The move key read from LSM tree will point to vlog6
			// because we've asked for version 1 of the move key.
			//
			// This might seem like an issue but it's not really an issue
			// because the user has set the number of versions to keep to 1 and
			// the latest version of moveKey points to the correct vlog file
			// and offset. The stale move key on L3 will be eventually dropped
			// by compaction because there is a newer versions in the upper
			// levels.
		}
		return nil
	}

	_, err := f.iterate(vlog.opt.ReadOnly, 0, func(e Entry, vp valuePointer) error {
		return fe(e)
	})
	if err != nil {
		return err
	}

	tr.LazyPrintf("request has %d entries, size %d", len(wb), size)
	batchSize := 1024
	var loops int
	for i := 0; i < len(wb); {
		loops++
		if batchSize == 0 {
			vlog.db.opt.Warningf("We shouldn't reach batch size of zero.")
			return ErrNoRewrite
		}
		end := i + batchSize
		if end > len(wb) {
			end = len(wb)
		}
		if err := vlog.db.batchSet(wb[i:end]); err != nil {
			if err == ErrTxnTooBig {
				// Decrease the batch size to half.
				batchSize = batchSize / 2
				tr.LazyPrintf("Dropped batch size to %d", batchSize)
				continue
			}
			return err
		}
		i += batchSize
	}
	tr.LazyPrintf("Processed %d entries in %d loops", len(wb), loops)
	tr.LazyPrintf("Total entries: %d. Moved: %d", count, moved)
	tr.LazyPrintf("Removing fid: %d", f.fid)
	var deleteFileNow bool
	// Entries written to LSM. Remove the older file now.
	{
		vlog.filesLock.Lock()
		// Just a sanity-check.
		if _, ok := vlog.filesMap[f.fid]; !ok {
			vlog.filesLock.Unlock()
			return errors.Errorf("Unable to find fid: %d", f.fid)
		}
		if vlog.iteratorCount() == 0 {
			delete(vlog.filesMap, f.fid)
			deleteFileNow = true
		} else {
			vlog.filesToBeDeleted = append(vlog.filesToBeDeleted, f.fid)
		}
		vlog.filesLock.Unlock()
	}

	if deleteFileNow {
		if err := vlog.deleteLogFile(f); err != nil {
			return err
		}
	}

	return nil
}

func (vlog *valueLog) incrIteratorCount() {
	atomic.AddInt32(&vlog.numActiveIterators, 1)
}

func (vlog *valueLog) iteratorCount() int {
	return int(atomic.LoadInt32(&vlog.numActiveIterators))
}

func (vlog *valueLog) decrIteratorCount() error {
	num := atomic.AddInt32(&vlog.numActiveIterators, -1)
	if num != 0 {
		return nil
	}

	vlog.filesLock.Lock()
	lfs := make([]*logFile, 0, len(vlog.filesToBeDeleted))
	for _, id := range vlog.filesToBeDeleted {
		lfs = append(lfs, vlog.filesMap[id])
		delete(vlog.filesMap, id)
	}
	vlog.filesToBeDeleted = nil
	vlog.filesLock.Unlock()

	for _, lf := range lfs {
		if err := vlog.deleteLogFile(lf); err != nil {
			return err
		}
	}
	return nil
}

func (vlog *valueLog) deleteLogFile(lf *logFile) error {
	if lf == nil {
		return nil
	}
	lf.lock.Lock()
	defer lf.lock.Unlock()

	return lf.Delete()
}

func (vlog *valueLog) dropAll() (int, error) {
	// If db is opened in InMemory mode, we don't need to do anything since there are no vlog files.
	if vlog.db.opt.InMemory {
		return 0, nil
	}
	// We don't want to block dropAll on any pending transactions. So, don't worry about iterator
	// count.
	var count int
	deleteAll := func() error {
		vlog.filesLock.Lock()
		defer vlog.filesLock.Unlock()
		for _, lf := range vlog.filesMap {
			if err := vlog.deleteLogFile(lf); err != nil {
				return err
			}
			count++
		}
		vlog.filesMap = make(map[uint32]*logFile)
		vlog.maxFid = 0
		return nil
	}
	if err := deleteAll(); err != nil {
		return count, err
	}

	vlog.db.opt.Infof("Value logs deleted. Creating value log file: 1")
	if _, err := vlog.createVlogFile(); err != nil { // Called while writes are stopped.
		return count, err
	}
	return count, nil
}

type valueLog struct {
	dirPath string

	// guards our view of which files exist, which to be deleted, how many active iterators
	filesLock        sync.RWMutex
	filesMap         map[uint32]*logFile
	maxFid           uint32
	filesToBeDeleted []uint32
	// A refcount of iterators -- when this hits zero, we can delete the filesToBeDeleted.
	numActiveIterators int32

	db                *DB
	writableLogOffset uint32 // read by read, written by write. Must access via atomics.
	numEntriesWritten uint32
	opt               Options

	garbageCh    chan struct{}
	discardStats *discardStats
}

func vlogFilePath(dirPath string, fid uint32) string {
	return fmt.Sprintf("%s%s%06d.vlog", dirPath, string(os.PathSeparator), fid)
}

func (vlog *valueLog) fpath(fid uint32) string {
	return vlogFilePath(vlog.dirPath, fid)
}

func (vlog *valueLog) populateFilesMap() error {
	vlog.filesMap = make(map[uint32]*logFile)

	files, err := ioutil.ReadDir(vlog.dirPath)
	if err != nil {
		return errFile(err, vlog.dirPath, "Unable to open log dir.")
	}

	found := make(map[uint64]struct{})
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".vlog") {
			continue
		}
		fsz := len(file.Name())
		fid, err := strconv.ParseUint(file.Name()[:fsz-5], 10, 32)
		if err != nil {
			return errFile(err, file.Name(), "Unable to parse log id.")
		}
		if _, ok := found[fid]; ok {
			return errFile(err, file.Name(), "Duplicate file found. Please delete one.")
		}
		found[fid] = struct{}{}

		lf := &logFile{
			fid:      uint32(fid),
			path:     vlog.fpath(uint32(fid)),
			registry: vlog.db.registry,
		}
		vlog.filesMap[uint32(fid)] = lf
		if vlog.maxFid < uint32(fid) {
			vlog.maxFid = uint32(fid)
		}
	}
	return nil
}

func (vlog *valueLog) createVlogFile() (*logFile, error) {
	fid := vlog.maxFid + 1
	path := vlog.fpath(fid)
	lf := &logFile{
		fid:      fid,
		path:     path,
		registry: vlog.db.registry,
		writeAt:  vlogHeaderSize,
	}
	err := lf.open(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, vlog.opt)
	if err != z.NewFile && err != nil {
		return nil, err
	}

	vlog.filesLock.Lock()
	vlog.filesMap[fid] = lf
	y.AssertTrue(vlog.maxFid < fid)
	vlog.maxFid = fid
	// writableLogOffset is only written by write func, by read by Read func.
	// To avoid a race condition, all reads and updates to this variable must be
	// done via atomics.
	atomic.StoreUint32(&vlog.writableLogOffset, vlogHeaderSize)
	vlog.numEntriesWritten = 0
	vlog.filesLock.Unlock()

	return lf, nil
}

func errFile(err error, path string, msg string) error {
	return fmt.Errorf("%s. Path=%s. Error=%v", msg, path, err)
}

// func (db *DB) replayLog(replayFn logEntry) error {
// 	lf := db.wal
// 	fi, err := lf.Fd.Stat()
// 	if err != nil {
// 		return errFile(err, lf.path, "Unable to run file.Stat")
// 	}

// 	// Alright, let's iterate now.
// 	endOffset, err := lf.iterate(db.opt.ReadOnly, 0, replayFn)
// 	if err != nil {
// 		return errFile(err, lf.path, "Unable to replay logfile")
// 	}
// 	if int64(endOffset) == fi.Size() {
// 		return nil
// 	}

// 	// End offset is different from file size. So, we should truncate the file
// 	// to that size.
// 	if !db.opt.Truncate {
// 		db.opt.Warningf("Truncate Needed. File %s size: %d Endoffset: %d",
// 			lf.Fd.Name(), fi.Size(), endOffset)
// 		return ErrTruncateNeeded
// 	}

// 	// TODO: DO we need this?
// 	// // The entire file should be truncated (i.e. it should be deleted).
// 	// // If fid == maxFid then it's okay to truncate the entire file since it will be
// 	// // used for future additions. Also, it's okay if the last file has size zero.
// 	// // We mmap 2*opt.ValueLogSize for the last file. See vlog.Open() function
// 	// // if endOffset <= vlogHeaderSize && lf.fid != vlog.maxFid {
// 	// if endOffset <= vlogHeaderSize {
// 	// 	if lf.fid != vlog.maxFid {
// 	// 		return errDeleteVlogFile
// 	// 	}
// 	// 	return lf.bootstrap()
// 	// }

// 	db.opt.Infof("Truncating vlog file %s to offset: %d", lf.Fd.Name(), endOffset)
// 	if err := lf.Fd.Truncate(int64(endOffset)); err != nil {
// 		return errFile(err, lf.path, fmt.Sprintf(
// 			"Truncation needed at offset %d. Can be done manually as well.", endOffset))
// 	}
// 	return nil
// }

// init initializes the value log struct. This initialization needs to happen
// before compactions start.
func (vlog *valueLog) init(db *DB) {
	vlog.opt = db.opt
	vlog.db = db
	// We don't need to open any vlog files or collect stats for GC if DB is opened
	// in InMemory mode. InMemory mode doesn't create any files/directories on disk.
	if vlog.opt.InMemory {
		return
	}
	vlog.dirPath = vlog.opt.ValueDir

	vlog.garbageCh = make(chan struct{}, 1) // Only allow one GC at a time.
	lf, err := initDiscardStats(vlog.opt)
	y.Check(err)
	vlog.discardStats = lf
}

func (vlog *valueLog) open(db *DB) error {
	// We don't need to open any vlog files or collect stats for GC if DB is opened
	// in InMemory mode. InMemory mode doesn't create any files/directories on disk.
	if db.opt.InMemory {
		return nil
	}

	if err := vlog.populateFilesMap(); err != nil {
		return err
	}
	// If no files are found, then create a new file.
	if len(vlog.filesMap) == 0 {
		_, err := vlog.createVlogFile()
		return y.Wrapf(err, "Error while creating log file in valueLog.open")
	}
	fids := vlog.sortedFids()
	for _, fid := range fids {
		lf, ok := vlog.filesMap[fid]
		y.AssertTrue(ok)

		// We cannot mmap the files upfront here. Windows does not like mmapped files to be
		// truncated. We might need to truncate files during a replay.
		var err error
		// Just open in RDWR mode. This should not create a new log file.
		if err = lf.open(vlog.fpath(fid), os.O_RDWR, vlog.opt); err != nil {
			return errors.Wrapf(err, "Open existing file: %q", lf.path)
		}

		// TODO: Use this for the WAL file.
		// TODO: Deal with value log replay later. We do this, just to truncate the latest log file,
		// which might have been corrupted or something.
		//
		// vlog.db.opt.Infof("Replaying file id: %d at offset: %d\n", fid, offset)
		// now := time.Now()
		// // Replay and possible truncation done. Now we can open the file as per
		// // user specified options.
		// if err := vlog.replayLog(lf, offset, replayFn); err != nil {
		// 	// Log file is corrupted. Delete it.
		// 	if err == errDeleteVlogFile {
		// 		delete(vlog.filesMap, fid)
		// 		// Close the fd of the file before deleting the file otherwise windows complaints.
		// 		if err := lf.Fd.Close(); err != nil {
		// 			return errors.Wrapf(err, "failed to close vlog file %s", lf.Fd.Name())
		// 		}
		// 		path := vlog.fpath(lf.fid)
		// 		if err := os.Remove(path); err != nil {
		// 			return y.Wrapf(err, "failed to delete empty value log file: %q", path)
		// 		}
		// 		continue
		// 	}
		// 	return err
		// }
		// vlog.db.opt.Infof("Replay took: %s\n", time.Since(now))

		// if fid < vlog.maxFid {
		// 	// For maxFid, the mmap would be done by the specially written code below.
		// 	if err := lf.init(); err != nil {
		// 		return err
		// 	}
		// }
	}
	// Seek to the end to start writing.
	last, ok := vlog.filesMap[vlog.maxFid]
	y.AssertTrue(ok)
	lastOff, err := last.iterate(vlog.opt.ReadOnly, vlogHeaderSize, func(_ Entry, vp valuePointer) error {
		return nil
	})
	if err != nil {
		return y.Wrapf(err, "while iterating over: %s", last.path)
	}
	if err := last.Truncate(int64(lastOff)); err != nil {
		return y.Wrapf(err, "while truncating last value log file: %s", last.path)
	}

	// Don't write to the old log file. Always create a new one.
	if _, err := vlog.createVlogFile(); err != nil {
		return y.Wrapf(err, "Error while creating log file in valueLog.open")
	}
	return nil
}

// func (lf *logFile) init() error {
// 	fstat, err := lf.Fd.Stat()
// 	if err != nil {
// 		return errors.Wrapf(err, "Unable to check stat for %q", lf.path)
// 	}
// 	sz := fstat.Size()
// 	if sz == 0 {
// 		// File is empty. We don't need to mmap it. Return.
// 		return nil
// 	}
// 	y.AssertTrue(sz <= math.MaxUint32)
// 	lf.size = uint32(sz)
// 	if err = lf.mmap(sz); err != nil {
// 		_ = lf.Fd.Close()
// 		return errors.Wrapf(err, "Unable to map file: %q", fstat.Name())
// 	}
// 	return nil
// }

func (vlog *valueLog) Close() error {
	if vlog == nil || vlog.db == nil || vlog.db.opt.InMemory {
		return nil
	}

	vlog.opt.Debugf("Stopping garbage collection of values.")

	var err error
	for id, lf := range vlog.filesMap {
		lf.lock.Lock() // We won’t release the lock.
		offset := int64(-1)

		if !vlog.opt.ReadOnly && id == vlog.maxFid {
			offset = int64(vlog.woffset())
		}
		if terr := lf.Close(offset); terr != nil && err == nil {
			err = terr
		}
	}
	return err
}

// sortedFids returns the file id's not pending deletion, sorted.  Assumes we have shared access to
// filesMap.
func (vlog *valueLog) sortedFids() []uint32 {
	toBeDeleted := make(map[uint32]struct{})
	for _, fid := range vlog.filesToBeDeleted {
		toBeDeleted[fid] = struct{}{}
	}
	ret := make([]uint32, 0, len(vlog.filesMap))
	for fid := range vlog.filesMap {
		if _, ok := toBeDeleted[fid]; !ok {
			ret = append(ret, fid)
		}
	}
	sort.Slice(ret, func(i, j int) bool {
		return ret[i] < ret[j]
	})
	return ret
}

type request struct {
	// Input values
	Entries []*Entry
	// Output values and wait group stuff below
	Ptrs []valuePointer
	Wg   sync.WaitGroup
	Err  error
	ref  int32
}

func (req *request) reset() {
	req.Entries = req.Entries[:0]
	req.Ptrs = req.Ptrs[:0]
	req.Wg = sync.WaitGroup{}
	req.Err = nil
	req.ref = 0
}

func (req *request) IncrRef() {
	atomic.AddInt32(&req.ref, 1)
}

func (req *request) DecrRef() {
	nRef := atomic.AddInt32(&req.ref, -1)
	if nRef > 0 {
		return
	}
	req.Entries = nil
	requestPool.Put(req)
}

func (req *request) Wait() error {
	req.Wg.Wait()
	err := req.Err
	req.DecrRef() // DecrRef after writing to DB.
	return err
}

type requests []*request

func (reqs requests) DecrRef() {
	for _, req := range reqs {
		req.DecrRef()
	}
}

func (reqs requests) IncrRef() {
	for _, req := range reqs {
		req.IncrRef()
	}
}

// sync function syncs content of latest value log file to disk. Syncing of value log directory is
// not required here as it happens every time a value log file rotation happens(check createVlogFile
// function). During rotation, previous value log file also gets synced to disk. It only syncs file
// if fid >= vlog.maxFid. In some cases such as replay(while opening db), it might be called with
// fid < vlog.maxFid. To sync irrespective of file id just call it with math.MaxUint32.
func (vlog *valueLog) sync() error {
	if vlog.opt.SyncWrites || vlog.opt.InMemory {
		return nil
	}

	vlog.filesLock.RLock()
	maxFid := vlog.maxFid
	curlf := vlog.filesMap[maxFid]
	// Sometimes it is possible that vlog.maxFid has been increased but file creation
	// with same id is still in progress and this function is called. In those cases
	// entry for the file might not be present in vlog.filesMap.
	if curlf == nil {
		vlog.filesLock.RUnlock()
		return nil
	}
	curlf.lock.RLock()
	vlog.filesLock.RUnlock()

	err := curlf.sync()
	curlf.lock.RUnlock()
	return err
}

func (vlog *valueLog) woffset() uint32 {
	return atomic.LoadUint32(&vlog.writableLogOffset)
}

// validateWrites will check whether the given requests can fit into 4GB vlog file.
// NOTE: 4GB is the maximum size we can create for vlog because value pointer offset is of type
// uint32. If we create more than 4GB, it will overflow uint32. So, limiting the size to 4GB.
func (vlog *valueLog) validateWrites(reqs []*request) error {
	vlogOffset := uint64(vlog.woffset())
	for _, req := range reqs {
		// calculate size of the request.
		size := estimateRequestSize(req)
		estimatedVlogOffset := vlogOffset + size
		if estimatedVlogOffset > uint64(maxVlogFileSize) {
			return errors.Errorf("Request size offset %d is bigger than maximum offset %d",
				estimatedVlogOffset, maxVlogFileSize)
		}

		if estimatedVlogOffset >= uint64(vlog.opt.ValueLogFileSize) {
			// We'll create a new vlog file if the estimated offset is greater or equal to
			// max vlog size. So, resetting the vlogOffset.
			vlogOffset = 0
			continue
		}
		// Estimated vlog offset will become current vlog offset if the vlog is not rotated.
		vlogOffset = estimatedVlogOffset
	}
	return nil
}

// estimateRequestSize returns the size that needed to be written for the given request.
func estimateRequestSize(req *request) uint64 {
	size := uint64(0)
	for _, e := range req.Entries {
		size += uint64(maxHeaderSize + len(e.Key) + len(e.Value) + crc32.Size)
	}
	return size
}

// write is thread-unsafe by design and should not be called concurrently.
func (vlog *valueLog) write(reqs []*request) error {
	if vlog.db.opt.InMemory {
		return nil
	}
	// Validate writes before writing to vlog. Because, we don't want to partially write and return
	// an error.
	if err := vlog.validateWrites(reqs); err != nil {
		return errors.Wrapf(err, "while validating writes")
	}

	vlog.filesLock.RLock()
	maxFid := vlog.maxFid
	curlf := vlog.filesMap[maxFid]
	vlog.filesLock.RUnlock()

	defer func() {
		if vlog.opt.SyncWrites {
			if err := curlf.sync(); err != nil {
				vlog.opt.Errorf("Error while curlf sync: %v\n", err)
			}
		}
	}()

	write := func(buf *bytes.Buffer) error {
		if buf.Len() == 0 {
			return nil
		}
		n := uint32(buf.Len())
		endOffset := atomic.AddUint32(&vlog.writableLogOffset, n)
		vlog.opt.Debugf("n: %d endOffset: %d\n", n, endOffset)
		if int(endOffset) >= len(curlf.Data) {
			return errors.Wrapf(ErrTxnTooBig, "endOffset: %d len: %d\n", endOffset, len(curlf.Data))
			// return ErrTxnTooBig
		}

		start := int(endOffset - n)
		y.AssertTrue(copy(curlf.Data[start:], buf.Bytes()) == int(n))

		atomic.StoreUint32(&curlf.size, vlog.writableLogOffset)
		return nil
	}

	toDisk := func() error {
		if vlog.woffset() > uint32(vlog.opt.ValueLogFileSize) ||
			vlog.numEntriesWritten > vlog.opt.ValueLogMaxEntries {
			if err := curlf.doneWriting(vlog.woffset()); err != nil {
				return err
			}

			newlf, err := vlog.createVlogFile()
			if err != nil {
				return err
			}
			curlf = newlf
			atomic.AddInt32(&vlog.db.logRotates, 1)
		}
		return nil
	}

	buf := new(bytes.Buffer)
	for i := range reqs {
		b := reqs[i]
		b.Ptrs = b.Ptrs[:0]
		var written, bytesWritten int
		for j := range b.Entries {
			buf.Reset()

			e := b.Entries[j]
			if vlog.db.opt.skipVlog(e) {
				b.Ptrs = append(b.Ptrs, valuePointer{})
				continue
			}
			var p valuePointer

			p.Fid = curlf.fid
			// Use the offset including buffer length so far.
			p.Offset = vlog.woffset() + uint32(buf.Len())
			plen, err := curlf.encodeEntry(buf, e, p.Offset) // Now encode the entry into buffer.
			if err != nil {
				return err
			}
			p.Len = uint32(plen)
			b.Ptrs = append(b.Ptrs, p)
			if err := write(buf); err != nil {
				return err
			}
			written++
			bytesWritten += buf.Len()

			// No need to flush anything, we write to file directly via mmap.
		}
		y.NumWrites.Add(int64(written))
		y.NumBytesWritten.Add(int64(bytesWritten))

		vlog.numEntriesWritten += uint32(written)
		// We write to disk here so that all entries that are part of the same transaction are
		// written to the same vlog file.
		if err := toDisk(); err != nil {
			return err
		}
	}
	return toDisk()
}

// Gets the logFile and acquires and RLock() for the mmap. You must call RUnlock on the file
// (if non-nil)
func (vlog *valueLog) getFileRLocked(vp valuePointer) (*logFile, error) {
	vlog.filesLock.RLock()
	defer vlog.filesLock.RUnlock()
	ret, ok := vlog.filesMap[vp.Fid]
	if !ok {
		// log file has gone away, we can't do anything. Return.
		return nil, errors.Errorf("file with ID: %d not found", vp.Fid)
	}

	// Check for valid offset if we are reading from writable log.
	maxFid := vlog.maxFid
	if vp.Fid == maxFid {
		currentOffset := vlog.woffset()
		if vp.Offset >= currentOffset {
			return nil, errors.Errorf(
				"Invalid value pointer offset: %d greater than current offset: %d",
				vp.Offset, currentOffset)
		}
	}

	ret.lock.RLock()
	return ret, nil
}

// Read reads the value log at a given location.
// TODO: Make this read private.
func (vlog *valueLog) Read(vp valuePointer, s *y.Slice) ([]byte, func(), error) {
	buf, lf, err := vlog.readValueBytes(vp, s)
	// log file is locked so, decide whether to lock immediately or let the caller to
	// unlock it, after caller uses it.
	cb := vlog.getUnlockCallback(lf)
	if err != nil {
		return nil, cb, err
	}

	if vlog.opt.VerifyValueChecksum {
		hash := crc32.New(y.CastagnoliCrcTable)
		if _, err := hash.Write(buf[:len(buf)-crc32.Size]); err != nil {
			runCallback(cb)
			return nil, nil, errors.Wrapf(err, "failed to write hash for vp %+v", vp)
		}
		// Fetch checksum from the end of the buffer.
		checksum := buf[len(buf)-crc32.Size:]
		if hash.Sum32() != y.BytesToU32(checksum) {
			runCallback(cb)
			return nil, nil, errors.Wrapf(y.ErrChecksumMismatch, "value corrupted for vp: %+v", vp)
		}
	}
	var h header
	headerLen := h.Decode(buf)
	kv := buf[headerLen:]
	if lf.encryptionEnabled() {
		kv, err = lf.decryptKV(kv, vp.Offset)
		if err != nil {
			return nil, cb, err
		}
	}
	if uint32(len(kv)) < h.klen+h.vlen {
		vlog.db.opt.Logger.Errorf("Invalid read: vp: %+v", vp)
		return nil, nil, errors.Errorf("Invalid read: Len: %d read at:[%d:%d]",
			len(kv), h.klen, h.klen+h.vlen)
	}
	return kv[h.klen : h.klen+h.vlen], cb, nil
}

// getUnlockCallback will returns a function which unlock the logfile if the logfile is mmaped.
// otherwise, it unlock the logfile and return nil.
func (vlog *valueLog) getUnlockCallback(lf *logFile) func() {
	if lf == nil {
		return nil
	}
	return lf.lock.RUnlock
}

// readValueBytes return vlog entry slice and read locked log file. Caller should take care of
// logFile unlocking.
func (vlog *valueLog) readValueBytes(vp valuePointer, s *y.Slice) ([]byte, *logFile, error) {
	lf, err := vlog.getFileRLocked(vp)
	if err != nil {
		return nil, nil, err
	}

	buf, err := lf.read(vp, s)
	return buf, lf, err
}

func (vlog *valueLog) pickLog(tr trace.Trace) (files []*logFile) {
	vlog.filesLock.RLock()
	defer vlog.filesLock.RUnlock()
	fids := vlog.sortedFids()
	if len(fids) <= 1 {
		tr.LazyPrintf("Only one or less value log file.")
		return nil
	}
	maxFid := atomic.LoadUint32(&vlog.maxFid)

	// Pick a candidate that contains the largest amount of discardable data
	fid, discard := vlog.discardStats.MaxDiscard()
	if fid < maxFid {
		vlog.opt.Infof("Found value log max discard fid: %d discard: %d\n", fid, discard)
		files = append(files, vlog.filesMap[fid])
	}

	// Fallback to randomly picking a log file
	var idxHead int
	for i, fid := range fids {
		if fid >= maxFid {
			idxHead = i
			break
		}
	}
	if idxHead == 0 { // Not found or first file
		return nil
	}
	idx := rand.Intn(idxHead) // Don’t include head.Fid. We pick a random file before it.
	if idx > 0 {
		idx = rand.Intn(idx + 1) // Another level of rand to favor smaller fids.
	}
	vlog.opt.Infof("Randomly chose fid: %d", fids[idx])
	files = append(files, vlog.filesMap[fids[idx]])
	return files
}

func discardEntry(e Entry, vs y.ValueStruct, db *DB) bool {
	if vs.Version != y.ParseTs(e.Key) {
		// Version not found. Discard.
		return true
	}
	if isDeletedOrExpired(vs.Meta, vs.ExpiresAt) {
		return true
	}
	if (vs.Meta & bitValuePointer) == 0 {
		// Key also stores the value in LSM. Discard.
		return true
	}
	if (vs.Meta & bitFinTxn) > 0 {
		// Just a txn finish entry. Discard.
		return true
	}
	return false
}

type reason struct {
	total   float64
	discard float64
	count   int
}

func (vlog *valueLog) doRunGC(lf *logFile, discardRatio float64, tr trace.Trace) (err error) {
	// Update stats before exiting
	defer func() {
		if err == nil {
			vlog.discardStats.Update(lf.fid, -1)
		}
	}()
	s := &sampler{
		lf:            lf,
		tr:            tr,
		countRatio:    0.01, // 1% of num entries.
		sizeRatio:     0.1,  // 10% of the file as window.
		fromBeginning: false,
	}

	if _, err = vlog.sample(s, discardRatio); err != nil {
		return err
	}

	if err = vlog.rewrite(lf, tr); err != nil {
		return err
	}
	tr.LazyPrintf("Done rewriting.")
	return nil
}

type sampler struct {
	lf            *logFile
	tr            trace.Trace
	sizeRatio     float64
	countRatio    float64
	fromBeginning bool
}

func (vlog *valueLog) sample(samp *sampler, discardRatio float64) (*reason, error) {
	tr := samp.tr
	sizePercent := samp.sizeRatio
	countPercent := samp.countRatio
	lf := samp.lf

	fi, err := lf.Fd.Stat()
	if err != nil {
		tr.LazyPrintf("Error while finding file size: %v", err)
		tr.SetError()
		return nil, err
	}

	// Set up the sampling window sizes.
	sizeWindow := float64(fi.Size()) * sizePercent
	sizeWindowM := sizeWindow / (1 << 20) // in MBs.
	countWindow := int(float64(vlog.opt.ValueLogMaxEntries) * countPercent)
	tr.LazyPrintf("Size window: %5.2f. Count window: %d.", sizeWindow, countWindow)

	var skipFirstM float64
	// Skip data only if fromBeginning is set to false. Pick a random start point.
	if !samp.fromBeginning {
		// Pick a random start point for the log.
		skipFirstM := float64(rand.Int63n(fi.Size())) // Pick a random starting location.
		skipFirstM -= sizeWindow                      // Avoid hitting EOF by moving back by window.
		skipFirstM /= float64(mi)                     // Convert to MBs.
		tr.LazyPrintf("Skip first %5.2f MB of file of size: %d MB", skipFirstM, fi.Size()/mi)
	}
	var skipped float64

	var r reason
	start := time.Now()
	y.AssertTrue(vlog.db != nil)
	s := new(y.Slice)
	var numIterations int
	_, err = lf.iterate(vlog.opt.ReadOnly, 0, func(e Entry, vp valuePointer) error {
		numIterations++
		esz := float64(vp.Len) / (1 << 20) // in MBs.
		if skipped < skipFirstM {
			skipped += esz
			return nil
		}

		// Sample until we reach the window sizes or exceed 10 seconds.
		if r.count > countWindow {
			tr.LazyPrintf("Stopping sampling after %d entries.", countWindow)
			return errStop
		}
		if r.total > sizeWindowM {
			tr.LazyPrintf("Stopping sampling after reaching window size.")
			return errStop
		}
		if time.Since(start) > 10*time.Second {
			tr.LazyPrintf("Stopping sampling after 10 seconds.")
			return errStop
		}
		r.total += esz
		r.count++

		vs, err := vlog.db.get(e.Key)
		if err != nil {
			return err
		}
		if discardEntry(e, vs, vlog.db) {
			r.discard += esz
			return nil
		}

		// Value is still present in value log.
		y.AssertTrue(len(vs.Value) > 0)
		vp.Decode(vs.Value)

		if vp.Fid > lf.fid {
			// Value is present in a later log. Discard.
			r.discard += esz
			return nil
		}
		if vp.Offset > e.offset {
			// Value is present in a later offset, but in the same log.
			r.discard += esz
			return nil
		}
		if vp.Fid == lf.fid && vp.Offset == e.offset {
			// This is still the active entry. This would need to be rewritten.

		} else {
			// Todo(ibrahim): Can this ever happen? See the else in rewrite function.
			vlog.opt.Debugf("Reason=%+v\n", r)
			buf, lf, err := vlog.readValueBytes(vp, s)
			// we need to decide, whether to unlock the lock file immediately based on the
			// loading mode. getUnlockCallback will take care of it.
			cb := vlog.getUnlockCallback(lf)
			if err != nil {
				runCallback(cb)
				return errStop
			}
			ne, err := lf.decodeEntry(buf, vp.Offset)
			if err != nil {
				runCallback(cb)
				return errStop
			}
			ne.print("Latest Entry Header in LSM")
			e.print("Latest Entry in Log")
			runCallback(cb)
			return errors.Errorf("This shouldn't happen. Latest Pointer:%+v. Meta:%v.",
				vp, vs.Meta)
		}
		return nil
	})

	if err != nil {
		tr.LazyPrintf("Error while iterating for RunGC: %v", err)
		tr.SetError()
		return nil, err
	}
	tr.LazyPrintf("Fid: %d. Skipped: %5.2fMB Num iterations: %d. Data status=%+v\n",
		lf.fid, skipped, numIterations, r)
	// If we couldn't sample at least a 1000 KV pairs or at least 75% of the window size,
	// and what we can discard is below the threshold, we should skip the rewrite.
	if (r.count < countWindow && r.total < sizeWindowM*0.75) || r.discard < discardRatio*r.total {
		tr.LazyPrintf("Skipping GC on fid: %d", lf.fid)
		return nil, ErrNoRewrite
	}
	return &r, nil
}

func (vlog *valueLog) waitOnGC(lc *z.Closer) {
	defer lc.Done()

	<-lc.HasBeenClosed() // Wait for lc to be closed.

	// Block any GC in progress to finish, and don't allow any more writes to runGC by filling up
	// the channel of size 1.
	vlog.garbageCh <- struct{}{}
}

func (vlog *valueLog) runGC(discardRatio float64) error {
	select {
	case vlog.garbageCh <- struct{}{}:
		// Pick a log file for GC.
		tr := trace.New("Badger.ValueLog", "GC")
		tr.SetMaxEvents(100)
		defer func() {
			tr.Finish()
			<-vlog.garbageCh
		}()

		var err error
		files := vlog.pickLog(tr)
		if len(files) == 0 {
			tr.LazyPrintf("PickLog returned zero results.")
			return ErrNoRewrite
		}
		tried := make(map[uint32]bool)
		for _, lf := range files {
			if _, done := tried[lf.fid]; done {
				continue
			}
			tried[lf.fid] = true
			if err = vlog.doRunGC(lf, discardRatio, tr); err == nil {
				return nil
			}
		}
		return err
	default:
		return ErrRejected
	}
}

func (vlog *valueLog) updateDiscardStats(stats map[uint32]int64) {
	if vlog.opt.InMemory {
		return
	}
	for fid, discard := range stats {
		vlog.discardStats.Update(fid, discard)
	}
}

func (vlog *valueLog) cleanVlog() error {
	// Check if gc is not already running.
	select {
	case vlog.garbageCh <- struct{}{}:
	default:
		return errors.New("Gc already running")
	}
	defer func() { <-vlog.garbageCh }()

	vlog.filesLock.RLock()
	filesToGC := vlog.sortedFids()
	vlog.filesLock.RUnlock()

	head := atomic.LoadUint32(&vlog.maxFid)
	for _, fid := range filesToGC {
		tr := trace.New("Badger.ValueLog Sampling", "Sampling")
		start := time.Now()
		// stop on the head fid.
		if fid == head {
			break
		}
		vlog.filesLock.RLock()
		lf, ok := vlog.filesMap[fid]
		vlog.filesLock.RUnlock()
		if !ok {
			return errors.Errorf("Unknown fid %d", fid)
		}

		s := &sampler{
			lf:            lf,
			tr:            tr,
			countRatio:    1, // 1% of num entries.
			sizeRatio:     1, // 10% of the file as window.
			fromBeginning: true,
		}
		vlog.db.opt.Logger.Infof("Sampling fid %d", fid)
		r, err := vlog.sample(s, 0)
		if err != nil {
			return err
		}
		vlog.db.opt.Logger.Infof("Sampled fid %d. Took: %s", fid, time.Since(start))

		// Skip vlog files with < 0.5 discard ratio.
		if r.discard/r.total < 0.5 {
			continue
		}
		vlog.db.opt.Logger.Infof("Rewriting fid %d", fid)
		start = time.Now()
		if err := vlog.rewrite(lf, tr); err != nil {
			return errors.Wrapf(err, "file: %s", lf.Fd.Name())
		}
		vlog.db.opt.Logger.Infof("Rewritten fid %d. Took: %s", fid, time.Since(start))
	}

	return nil
}

type sampleResult struct {
	Fid          uint32
	FileSize     int64
	DiscardRatio float64
}

// getDiscardStats is used to collect and return the discard stats for all the files.
func (vlog *valueLog) getDiscardStats() ([]sampleResult, error) {
	vlog.filesLock.RLock()
	filesToSample := vlog.sortedFids()
	vlog.filesLock.RUnlock()

	head := atomic.LoadUint32(&vlog.maxFid)

	tr := trace.New("Badger.ValueLog Sampling", "Sampling")
	tr.SetMaxEvents(100)
	samp := &sampler{
		countRatio:    1,    // 100% of entries in the file.
		sizeRatio:     1,    // 100% of the file size.
		fromBeginning: true, // Start reading from the start.
		tr:            tr,
	}

	var result []sampleResult
	for _, fid := range filesToSample {
		// Skip the head file since it is actively being written.
		if fid == head {
			continue
		}
		start := time.Now()
		vlog.db.opt.Logger.Infof("Sampling fid %d", fid)

		vlog.filesLock.RLock()
		lf, ok := vlog.filesMap[fid]
		vlog.filesLock.RUnlock()
		if !ok {
			return nil, errors.Errorf("Unknown fid %d", fid)
		}
		samp.lf = lf
		// Set discard ratio to 0 so that sample never returns a ErrNoRewrite error.
		r, err := vlog.sample(samp, 0)
		if err != nil {
			return nil, errors.Wrapf(err, "file: %s", lf.Fd.Name())
		}

		fstat, err := lf.Fd.Stat()
		if err != nil {
			return nil, errors.Wrapf(err, "Unable to check stat for %q", lf.path)
		}

		result = append(result, sampleResult{
			Fid:          fid,
			DiscardRatio: r.discard / r.total,
			FileSize:     fstat.Size()})
		vlog.db.opt.Logger.Infof("Sampled fid %d. Took: %s", fid, time.Since(start))
	}
	return result, nil
}
