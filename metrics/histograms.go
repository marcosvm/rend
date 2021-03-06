// Copyright 2015 Netflix, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package metrics

import (
	"math"
	"sync"
	"sync/atomic"
)

const (
	maxNumHists = 1024
	buflen      = 0x7FFF // max index, 32769 entries
	bhistlen    = 65
)

var (
	hnames    = make([]string, maxNumHists)
	hsampled  = make([]bool, maxNumHists)
	hists     = make([]*hist, maxNumHists)
	bhists    = make([]*bhist, maxNumHists)
	curHistID = new(uint32)
)

func init() {
	// start at "-1" so the first ID is 0
	atomic.StoreUint32(curHistID, 0)
}

// The hist struct holds a primary and secondary data structure so the reader of
// the histograms will get to read the data out while new observations are made.
// As well, pulling and resetting the histogram does not require a malloc in the
// path of pulling the data, and the large circular buffers can be reused.
type hist struct {
	lock sync.RWMutex
	prim *hdat
	sec  *hdat
}
type hdat struct {
	count uint64
	kept  uint64
	total uint64
	min   uint64
	max   uint64
	buf   []uint64
}

func newHist() *hist {
	return &hist{
		// read: primary and secondary data structures
		prim: newHdat(),
		sec:  newHdat(),
	}
}
func newHdat() *hdat {
	ret := &hdat{
		buf: make([]uint64, buflen+1),
	}
	atomic.StoreUint64(&ret.min, math.MaxUint64)
	return ret
}

type bhist struct {
	buckets []uint64
}

func newBHist() *bhist {
	return &bhist{
		// Holds enough for the entire length of a uint64
		buckets: make([]uint64, 65),
	}
}

func AddHistogram(name string, sampled bool) uint32 {
	idx := atomic.AddUint32(curHistID, 1) - 1

	if idx >= maxNumHists {
		panic("Too many histograms")
	}

	hnames[idx] = name
	hsampled[idx] = sampled
	hists[idx] = newHist()
	bhists[idx] = newBHist()

	return idx
}

func ObserveHist(id uint32, value uint64) {
	h := hists[id]

	// We lock here to ensure that the min and max values are true to this time
	// period, meaning extractAndReset won't pull the data out from under us
	// while the current observation is being compared. Otherwise, min and max
	// could come from the previous period on the next read. Same with average.
	h.lock.RLock()

	// Keep a running total for average
	atomic.AddUint64(&h.prim.total, value)

	// Set max and min (if needed) in an atomic fashion
	for {
		max := atomic.LoadUint64(&h.prim.max)
		if value < max || atomic.CompareAndSwapUint64(&h.prim.max, max, value) {
			break
		}
	}
	for {
		min := atomic.LoadUint64(&h.prim.min)
		if value > min || atomic.CompareAndSwapUint64(&h.prim.min, min, value) {
			break
		}
	}

	// Record the bucketized histograms
	bucket := lzcnt(value)
	atomic.AddUint64(&bhists[id].buckets[bucket], 1)

	// Count and possibly return for sampling
	c := atomic.AddUint64(&h.prim.count, 1)
	if hsampled[id] {
		// Sample, keep every 4th observation
		if (c & 0x3) > 0 {
			h.lock.RUnlock()
			return
		}
	}

	// Get the current index as the count % buflen
	idx := atomic.AddUint64(&h.prim.kept, 1) & buflen

	// Add observation
	h.prim.buf[idx] = value

	// No longer "reading"
	h.lock.RUnlock()
}

func getAllHistograms() map[string]*hdat {
	n := int(atomic.LoadUint32(curHistID))

	ret := make(map[string]*hdat)

	for i := 0; i < n; i++ {
		ret[hnames[i]] = extractAndReset(hists[i])
	}

	return ret
}

func extractAndReset(h *hist) *hdat {
	h.lock.Lock()

	// flip and reset the count
	h.prim, h.sec = h.sec, h.prim

	atomic.StoreUint64(&h.prim.count, 0)
	atomic.StoreUint64(&h.prim.kept, 0)
	atomic.StoreUint64(&h.prim.total, 0)
	atomic.StoreUint64(&h.prim.max, 0)
	atomic.StoreUint64(&h.prim.min, math.MaxUint64)

	h.lock.Unlock()

	return h.sec
}

func getAllBucketHistograms() map[string][]uint64 {
	n := int(atomic.LoadUint32(curHistID))

	ret := make(map[string][]uint64)

	for i := 0; i < n; i++ {
		ret[hnames[i]] = extractBHist(bhists[i])
	}

	return ret
}

func extractBHist(b *bhist) []uint64 {
	ret := make([]uint64, bhistlen)
	for i := 0; i < bhistlen; i++ {
		ret[i] = atomic.LoadUint64(&b.buckets[i])
	}
	return ret
}
