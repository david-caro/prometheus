// Copyright 2014 The Prometheus Authors
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

package local

import (
	"container/list"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/storage/metric"
)

// DefaultChunkEncoding can be changed via a flag.
var DefaultChunkEncoding = DoubleDelta

var errChunkBoundsExceeded = errors.New("attempted access outside of chunk boundaries")

// ChunkEncoding defintes which encoding we are using, delta, doubledelta, or varbit
type ChunkEncoding byte

// String implements flag.Value.
func (ce ChunkEncoding) String() string {
	return fmt.Sprintf("%d", ce)
}

// Set implements flag.Value.
func (ce *ChunkEncoding) Set(s string) error {
	switch s {
	case "0":
		*ce = Delta
	case "1":
		*ce = DoubleDelta
	case "2":
		*ce = Varbit
	default:
		return fmt.Errorf("invalid chunk encoding: %s", s)
	}
	return nil
}

const (
	// Delta encoding
	Delta ChunkEncoding = iota
	// DoubleDelta encoding
	DoubleDelta
	// Varbit encoding
	Varbit
)

// ChunkDesc contains meta-data for a chunk. Pay special attention to the
// documented requirements for calling its methods concurrently (WRT pinning and
// locking). The doc comments spell out the requirements for each method, but
// here is an overview and general explanation:
//
// Everything that changes the pinning of the underlying chunk or deals with its
// eviction is protected by a mutex. This affects the following methods: pin,
// unpin, refCount, isEvicted, maybeEvict. These methods can be called at any
// time without further prerequisites.
//
// Another group of methods acts on (or sets) the underlying chunk. These
// methods involve no locking. They may only be called if the caller has pinned
// the chunk (to guarantee the chunk is not evicted concurrently). Also, the
// caller must make sure nobody else will call these methods concurrently,
// either by holding the sole reference to the chunkDesc (usually during loading
// or creation) or by locking the fingerprint of the series the chunkDesc
// belongs to. The affected methods are: add, maybePopulateLastTime, setChunk.
//
// Finally, there are the special cases firstTime and lastTime. lastTime requires
// to have locked the fingerprint of the series but the chunk does not need to
// be pinned. That's because the chunkLastTime field in chunkDesc gets populated
// upon completion of the chunk (when it is still pinned, and which happens
// while the series's fingerprint is locked). Once that has happened, calling
// lastTime does not require the chunk to be loaded anymore. Before that has
// happened, the chunk is pinned anyway. The chunkFirstTime field in chunkDesc
// is populated upon creation of a chunkDesc, so it is alway safe to call
// firstTime. The firstTime method is arguably not needed and only there for
// consistency with lastTime.
type ChunkDesc struct {
	sync.Mutex           // Protects pinning.
	c              Chunk // nil if chunk is evicted.
	rCnt           int
	chunkFirstTime model.Time // Populated at creation. Immutable.
	chunkLastTime  model.Time // Populated on closing of the chunk, model.Earliest if unset.

	// evictListElement is nil if the chunk is not in the evict list.
	// evictListElement is _not_ protected by the chunkDesc mutex.
	// It must only be touched by the evict list handler in MemorySeriesStorage.
	evictListElement *list.Element
}

// NewChunkDesc creates a new chunkDesc pointing to the provided chunk. The
// provided chunk is assumed to be not persisted yet. Therefore, the refCount of
// the new chunkDesc is 1 (preventing eviction prior to persisting).
func NewChunkDesc(c Chunk, firstTime model.Time) *ChunkDesc {
	chunkOps.WithLabelValues(createAndPin).Inc()
	atomic.AddInt64(&numMemChunks, 1)
	numMemChunkDescs.Inc()
	return &ChunkDesc{
		c:              c,
		rCnt:           1,
		chunkFirstTime: firstTime,
		chunkLastTime:  model.Earliest,
	}
}

// Add adds a sample pair to the underlying chunk. For safe concurrent access,
// The chunk must be pinned, and the caller must have locked the fingerprint of
// the series.
func (cd *ChunkDesc) Add(s model.SamplePair) ([]Chunk, error) {
	return cd.c.Add(s)
}

// pin increments the refCount by one. Upon increment from 0 to 1, this
// chunkDesc is removed from the evict list. To enable the latter, the
// evictRequests channel has to be provided. This method can be called
// concurrently at any time.
func (cd *ChunkDesc) pin(evictRequests chan<- evictRequest) {
	cd.Lock()
	defer cd.Unlock()

	if cd.rCnt == 0 {
		// Remove ourselves from the evict list.
		evictRequests <- evictRequest{cd, false}
	}
	cd.rCnt++
}

// unpin decrements the refCount by one. Upon decrement from 1 to 0, this
// chunkDesc is added to the evict list. To enable the latter, the evictRequests
// channel has to be provided. This method can be called concurrently at any
// time.
func (cd *ChunkDesc) unpin(evictRequests chan<- evictRequest) {
	cd.Lock()
	defer cd.Unlock()

	if cd.rCnt == 0 {
		panic("cannot unpin already unpinned chunk")
	}
	cd.rCnt--
	if cd.rCnt == 0 {
		// Add ourselves to the back of the evict list.
		evictRequests <- evictRequest{cd, true}
	}
}

// refCount returns the number of pins. This method can be called concurrently
// at any time.
func (cd *ChunkDesc) refCount() int {
	cd.Lock()
	defer cd.Unlock()

	return cd.rCnt
}

// firstTime returns the timestamp of the first sample in the chunk. This method
// can be called concurrently at any time. It only returns the immutable
// cd.chunkFirstTime without any locking. Arguably, this method is
// useless. However, it provides consistency with the lastTime method.
func (cd *ChunkDesc) firstTime() model.Time {
	return cd.chunkFirstTime
}

// lastTime returns the timestamp of the last sample in the chunk. For safe
// concurrent access, this method requires the fingerprint of the time series to
// be locked.
func (cd *ChunkDesc) lastTime() (model.Time, error) {
	if cd.chunkLastTime != model.Earliest || cd.c == nil {
		return cd.chunkLastTime, nil
	}
	return cd.c.NewIterator().LastTimestamp()
}

// maybePopulateLastTime populates the chunkLastTime from the underlying chunk
// if it has not yet happened. Call this method directly after having added the
// last sample to a chunk or after closing a head chunk due to age. For safe
// concurrent access, the chunk must be pinned, and the caller must have locked
// the fingerprint of the series.
func (cd *ChunkDesc) maybePopulateLastTime() error {
	if cd.chunkLastTime == model.Earliest && cd.c != nil {
		t, err := cd.c.NewIterator().LastTimestamp()
		if err != nil {
			return err
		}
		cd.chunkLastTime = t
	}
	return nil
}

// isEvicted returns whether the chunk is evicted. For safe concurrent access,
// the caller must have locked the fingerprint of the series.
func (cd *ChunkDesc) isEvicted() bool {
	// Locking required here because we do not want the caller to force
	// pinning the chunk first, so it could be evicted while this method is
	// called.
	cd.Lock()
	defer cd.Unlock()

	return cd.c == nil
}

// setChunk sets the underlying chunk. The caller must have locked the
// fingerprint of the series and must have "pre-pinned" the chunk (i.e. first
// call pin and then set the chunk).
func (cd *ChunkDesc) setChunk(c Chunk) {
	if cd.c != nil {
		panic("chunk already set")
	}
	cd.c = c
}

// maybeEvict evicts the chunk if the refCount is 0. It returns whether the chunk
// is now evicted, which includes the case that the chunk was evicted even
// before this method was called. It can be called concurrently at any time.
func (cd *ChunkDesc) maybeEvict() bool {
	cd.Lock()
	defer cd.Unlock()

	if cd.c == nil {
		return true
	}
	if cd.rCnt != 0 {
		return false
	}
	if cd.chunkLastTime == model.Earliest {
		// This must never happen.
		panic("chunkLastTime not populated for evicted chunk")
	}
	cd.c = nil
	chunkOps.WithLabelValues(evict).Inc()
	atomic.AddInt64(&numMemChunks, -1)
	return true
}

// Chunk is the interface for all chunks. Chunks are generally not
// goroutine-safe.
type Chunk interface {
	// add adds a SamplePair to the chunks, performs any necessary
	// re-encoding, and adds any necessary overflow chunks. It returns the
	// new version of the original chunk, followed by overflow chunks, if
	// any. The first chunk returned might be the same as the original one
	// or a newly allocated version. In any case, take the returned chunk as
	// the relevant one and discard the original chunk.
	Add(sample model.SamplePair) ([]Chunk, error)
	Clone() Chunk
	FirstTime() model.Time
	NewIterator() ChunkIterator
	Marshal(io.Writer) error
	MarshalToBuf([]byte) error
	Unmarshal(io.Reader) error
	UnmarshalFromBuf([]byte) error
	Encoding() ChunkEncoding
}

// ChunkIterator enables efficient access to the content of a chunk. It is
// generally not safe to use a chunkIterator concurrently with or after chunk
// mutation.
type ChunkIterator interface {
	// Gets the last timestamp in the chunk.
	LastTimestamp() (model.Time, error)
	// Whether a given timestamp is contained between first and last value
	// in the chunk.
	Contains(model.Time) (bool, error)
	// Scans the next value in the chunk. Directly after the iterator has
	// been created, the next value is the first value in the
	// chunk. Otherwise, it is the value following the last value scanned or
	// found (by one of the find... methods). Returns false if either the
	// end of the chunk is reached or an error has occurred.
	Scan() bool
	// Finds the most recent value at or before the provided time. Returns
	// false if either the chunk contains no value at or before the provided
	// time, or an error has occurred.
	FindAtOrBefore(model.Time) bool
	// Finds the oldest value at or after the provided time. Returns false
	// if either the chunk contains no value at or after the provided time,
	// or an error has occurred.
	FindAtOrAfter(model.Time) bool
	// Returns the last value scanned (by the scan method) or found (by one
	// of the find... methods). It returns ZeroSamplePair before any of
	// those methods were called.
	Value() model.SamplePair
	// Returns the last error encountered. In general, an error signals data
	// corruption in the chunk and requires quarantining.
	Err() error
}

// rangeValues is a utility function that retrieves all values within the given
// range from a chunkIterator.
func rangeValues(it ChunkIterator, in metric.Interval) ([]model.SamplePair, error) {
	result := []model.SamplePair{}
	if !it.FindAtOrAfter(in.OldestInclusive) {
		return result, it.Err()
	}
	for !it.Value().Timestamp.After(in.NewestInclusive) {
		result = append(result, it.Value())
		if !it.Scan() {
			break
		}
	}
	return result, it.Err()
}

// addToOverflowChunk is a utility function that creates a new chunk as overflow
// chunk, adds the provided sample to it, and returns a chunk slice containing
// the provided old chunk followed by the new overflow chunk.
func addToOverflowChunk(c Chunk, s model.SamplePair) ([]Chunk, error) {
	overflowChunks, err := NewChunk().Add(s)
	if err != nil {
		return nil, err
	}
	return []Chunk{c, overflowChunks[0]}, nil
}

// transcodeAndAdd is a utility function that transcodes the dst chunk into the
// provided src chunk (plus the necessary overflow chunks) and then adds the
// provided sample. It returns the new chunks (transcoded plus overflow) with
// the new sample at the end.
func transcodeAndAdd(dst Chunk, src Chunk, s model.SamplePair) ([]Chunk, error) {
	chunkOps.WithLabelValues(transcode).Inc()

	var (
		head            = dst
		body, NewChunks []Chunk
		err             error
	)

	it := src.NewIterator()
	for it.Scan() {
		if NewChunks, err = head.Add(it.Value()); err != nil {
			return nil, err
		}
		body = append(body, NewChunks[:len(NewChunks)-1]...)
		head = NewChunks[len(NewChunks)-1]
	}
	if it.Err() != nil {
		return nil, it.Err()
	}

	if NewChunks, err = head.Add(s); err != nil {
		return nil, err
	}
	return append(body, NewChunks...), nil
}

// NewChunk creates a new chunk according to the encoding set by the
// DefaultChunkEncoding flag.
func NewChunk() Chunk {
	chunk, err := NewChunkForEncoding(DefaultChunkEncoding)
	if err != nil {
		panic(err)
	}
	return chunk
}

// NewChunkForEncoding allows configuring what chunk type you want
func NewChunkForEncoding(encoding ChunkEncoding) (Chunk, error) {
	switch encoding {
	case Delta:
		return newDeltaEncodedChunk(d1, d0, true, chunkLen), nil
	case DoubleDelta:
		return newDoubleDeltaEncodedChunk(d1, d0, true, chunkLen), nil
	case Varbit:
		return newVarbitChunk(varbitZeroEncoding), nil
	default:
		return nil, fmt.Errorf("unknown chunk encoding: %v", encoding)
	}
}

// indexAccessor allows accesses to samples by index.
type indexAccessor interface {
	timestampAtIndex(int) model.Time
	sampleValueAtIndex(int) model.SampleValue
	err() error
}

// indexAccessingChunkIterator is a chunk iterator for chunks for which an
// indexAccessor implementation exists.
type indexAccessingChunkIterator struct {
	len       int
	pos       int
	lastValue model.SamplePair
	acc       indexAccessor
}

func newIndexAccessingChunkIterator(len int, acc indexAccessor) *indexAccessingChunkIterator {
	return &indexAccessingChunkIterator{
		len:       len,
		pos:       -1,
		lastValue: ZeroSamplePair,
		acc:       acc,
	}
}

// lastTimestamp implements chunkIterator.
func (it *indexAccessingChunkIterator) LastTimestamp() (model.Time, error) {
	return it.acc.timestampAtIndex(it.len - 1), it.acc.err()
}

// contains implements chunkIterator.
func (it *indexAccessingChunkIterator) Contains(t model.Time) (bool, error) {
	return !t.Before(it.acc.timestampAtIndex(0)) &&
		!t.After(it.acc.timestampAtIndex(it.len-1)), it.acc.err()
}

// scan implements chunkIterator.
func (it *indexAccessingChunkIterator) Scan() bool {
	it.pos++
	if it.pos >= it.len {
		return false
	}
	it.lastValue = model.SamplePair{
		Timestamp: it.acc.timestampAtIndex(it.pos),
		Value:     it.acc.sampleValueAtIndex(it.pos),
	}
	return it.acc.err() == nil
}

// findAtOrBefore implements chunkIterator.
func (it *indexAccessingChunkIterator) FindAtOrBefore(t model.Time) bool {
	i := sort.Search(it.len, func(i int) bool {
		return it.acc.timestampAtIndex(i).After(t)
	})
	if i == 0 || it.acc.err() != nil {
		return false
	}
	it.pos = i - 1
	it.lastValue = model.SamplePair{
		Timestamp: it.acc.timestampAtIndex(i - 1),
		Value:     it.acc.sampleValueAtIndex(i - 1),
	}
	return true
}

// findAtOrAfter implements chunkIterator.
func (it *indexAccessingChunkIterator) FindAtOrAfter(t model.Time) bool {
	i := sort.Search(it.len, func(i int) bool {
		return !it.acc.timestampAtIndex(i).Before(t)
	})
	if i == it.len || it.acc.err() != nil {
		return false
	}
	it.pos = i
	it.lastValue = model.SamplePair{
		Timestamp: it.acc.timestampAtIndex(i),
		Value:     it.acc.sampleValueAtIndex(i),
	}
	return true
}

// value implements chunkIterator.
func (it *indexAccessingChunkIterator) Value() model.SamplePair {
	return it.lastValue
}

// err implements chunkIterator.
func (it *indexAccessingChunkIterator) Err() error {
	return it.acc.err()
}
