// Copyright (c) 2018 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package index

import (
	goctx "context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/m3db/m3/src/dbnode/storage/bootstrap/result"
	"github.com/m3db/m3/src/dbnode/storage/namespace"
	m3ninxindex "github.com/m3db/m3/src/m3ninx/index"
	"github.com/m3db/m3/src/m3ninx/index/segment"
	"github.com/m3db/m3/src/m3ninx/index/segment/mem"
	m3ninxpersist "github.com/m3db/m3/src/m3ninx/persist"
	"github.com/m3db/m3/src/m3ninx/postings"
	"github.com/m3db/m3/src/m3ninx/search"
	"github.com/m3db/m3/src/m3ninx/search/executor"
	"github.com/m3db/m3x/context"
	xerrors "github.com/m3db/m3x/errors"
	"github.com/m3db/m3x/instrument"
	xtime "github.com/m3db/m3x/time"

	"github.com/uber-go/tally"
	"go.uber.org/atomic"
)

var (
	errUnableToWriteBlockClosed     = errors.New("unable to write, index block is closed")
	errUnableToWriteBlockSealed     = errors.New("unable to write, index block is sealed")
	errUnableToQueryBlockClosed     = errors.New("unable to query, index block is closed")
	errUnableToBootstrapBlockClosed = errors.New("unable to bootstrap, block is closed")
	errUnableToTickBlockClosed      = errors.New("unable to tick, block is closed")
	errBlockAlreadyClosed           = errors.New("unable to close, block already closed")

	errUnableToSealBlockIllegalStateFmtString  = "unable to seal, index block state: %v"
	errUnableToWriteBlockUnknownStateFmtString = "unable to write, unknown index block state: %v"
)

const (
	defaultMutableSegmentRotationSize      = 1 << 16 // TODO(prateek): migrate to options
	defaultMutableSegmentRotationMergeSize = 1 << 20 // TODO(prateek): migrate to options
	defaultMutableSegmentRotationAge       = 30 * time.Second
)

type blockState byte

const (
	blockStateClosed blockState = iota
	blockStateOpen
	blockStateSealed
)

type newExecutorFn func() (search.Executor, error)

type block struct {
	sync.RWMutex
	state               blockState
	shardRangesSegments []blockShardRangesSegments
	activeSegments      []*activeSegment
	segmentID           atomic.Int64

	// the following are used to help activeSegment rotations from map->fst Segments
	rotateCh         chan struct{}
	closeCtx         goctx.Context
	closeCtxCancelFn goctx.CancelFunc

	metrics       blockMetrics
	newExecutorFn newExecutorFn
	startTime     time.Time
	endTime       time.Time
	blockSize     time.Duration
	opts          Options
	nsMD          namespace.Metadata
}

// blockShardsSegments is a collection of segments that has a mapping of what shards
// and time ranges they completely cover, this can only ever come from computing
// from data that has come from shards, either on an index flush or a bootstrap.
type blockShardRangesSegments struct {
	shardTimeRanges result.ShardTimeRanges
	segments        []segment.Segment
}

// NewBlock returns a new Block, representing a complete reverse index for the
// duration of time specified. It is backed by one or more segments.
func NewBlock(
	startTime time.Time,
	md namespace.Metadata,
	opts Options,
) (Block, error) {
	var (
		blockSize = md.Options().IndexOptions().BlockSize()
	)

	closeCtx, closeFn := goctx.WithCancel(goctx.Background())
	b := &block{
		state:            blockStateOpen,
		rotateCh:         make(chan struct{}, 1),
		closeCtx:         closeCtx,
		closeCtxCancelFn: closeFn,
		metrics:          newBlockMetrics(opts.InstrumentOptions().MetricsScope()),
		startTime:        startTime,
		endTime:          startTime.Add(blockSize),
		blockSize:        blockSize,
		opts:             opts,
		nsMD:             md,
	}
	b.newExecutorFn = b.executorWithRLock
	b.addActiveSegmentWithLock()
	go b.monitorRotations()
	go b.reportMetrics()
	return b, nil
}

func (b *block) StartTime() time.Time {
	return b.startTime
}

func (b *block) EndTime() time.Time {
	return b.endTime
}

func (b *block) WriteBatch(inserts *WriteBatch) (WriteBatchResult, error) {
	b.Lock()
	defer b.Unlock()

	if b.state != blockStateOpen {
		err := b.writeBatchErrorInvalidState(b.state)
		inserts.MarkUnmarkedEntriesError(err)
		return WriteBatchResult{
			NumError: int64(inserts.Len()),
		}, err
	}

	// NB: we're guaranteed the block has a mutable activeSegment because
	// of the state check above; the if check below is additional paranoia.
	mutableActiveSeg := b.mutableActiveSegmentWithRLock()
	mutableSeg := mutableActiveSeg.mutableSegment
	if mutableSeg == nil { // should never happen
		err := b.openBlockHasNilActiveSegmentInvariantErrorWithRLock()
		inserts.MarkUnmarkedEntriesError(err)
		return WriteBatchResult{
			NumError: int64(inserts.Len()),
		}, err
	}

	defer func() {
		if mutableSeg.Size() > defaultMutableSegmentRotationSize ||
			time.Since(mutableActiveSeg.creationTime) > defaultMutableSegmentRotationAge {
			mutableSeg.Seal() // TODO(prateek) error?
			b.addActiveSegmentWithLock()
			b.triggerRotations()
		}
	}()

	err := mutableSeg.InsertBatch(m3ninxindex.Batch{
		Docs:                inserts.PendingDocs(),
		AllowPartialUpdates: true,
	})
	if err == nil {
		inserts.MarkUnmarkedEntriesSuccess()
		return WriteBatchResult{
			NumSuccess: int64(inserts.Len()),
		}, nil
	}

	partialErr, ok := err.(*m3ninxindex.BatchPartialError)
	if !ok { // should never happen
		err := b.unknownWriteBatchInvariantError(err)
		// NB: marking all the inserts as failure, cause we don't know which ones failed
		inserts.MarkUnmarkedEntriesError(err)
		return WriteBatchResult{NumError: int64(inserts.Len())}, err
	}

	numErr := len(partialErr.Errs())
	for _, err := range partialErr.Errs() {
		// Avoid marking these as success
		inserts.MarkUnmarkedEntryError(err.Err, err.Idx)
	}

	// mark all non-error inserts success, so we don't repeatedly index them
	inserts.MarkUnmarkedEntriesSuccess()
	return WriteBatchResult{
		NumSuccess: int64(inserts.Len() - numErr),
		NumError:   int64(numErr),
	}, partialErr
}

func (b *block) executorWithRLock() (search.Executor, error) {
	expectedReaders := len(b.activeSegments)
	for _, group := range b.shardRangesSegments {
		expectedReaders += len(group.segments)
	}

	var (
		readers = make([]m3ninxindex.Reader, 0, expectedReaders)
		success = false
	)

	// cleanup in case any of the readers below fail.
	defer func() {
		if !success {
			for _, reader := range readers {
				reader.Close()
			}
		}
	}()

	// start with the segment that's being actively written to (if we have one)
	for _, seg := range b.activeSegments {
		if seg.state != fstActiveSegmentState {
			continue
		}
		reader, err := seg.Reader()
		if err != nil {
			return nil, err
		}
		readers = append(readers, reader)
	}

	// loop over the segments associated to shard time ranges
	for _, group := range b.shardRangesSegments {
		for _, seg := range group.segments {
			reader, err := seg.Reader()
			if err != nil {
				return nil, err
			}
			readers = append(readers, reader)
		}
	}

	success = true
	return executor.NewExecutor(readers), nil
}

func (b *block) Query(
	query Query,
	opts QueryOptions,
	results Results,
) (bool, error) {
	b.RLock()
	defer b.RUnlock()
	if b.state == blockStateClosed {
		return false, errUnableToQueryBlockClosed
	}

	exec, err := b.newExecutorFn()
	if err != nil {
		return false, err
	}

	// FOLLOWUP(prateek): push down QueryOptions to restrict results
	// TODO(jeromefroe): Use the idx query directly once we implement an index in m3ninx
	// and don't need to use the segments anymore.
	iter, err := exec.Execute(query.Query.SearchQuery())
	if err != nil {
		exec.Close()
		return false, err
	}

	var (
		size       = results.Size()
		brokeEarly = false
	)
	execCloser := safeCloser{closable: exec}
	iterCloser := safeCloser{closable: iter}

	defer func() {
		iterCloser.Close()
		execCloser.Close()
	}()

	for iter.Next() {
		if opts.Limit > 0 && size >= opts.Limit {
			brokeEarly = true
			break
		}
		d := iter.Current()
		_, size, err = results.Add(d)
		if err != nil {
			return false, err
		}
	}

	if err := iter.Err(); err != nil {
		return false, err
	}

	if err := iterCloser.Close(); err != nil {
		return false, err
	}

	if err := execCloser.Close(); err != nil {
		return false, err
	}

	exhaustive := !brokeEarly
	return exhaustive, nil
}

func (b *block) AddResults(
	results result.IndexBlock,
) error {
	b.Lock()
	defer b.Unlock()

	// NB(prateek): we have to allow bootstrap to succeed even if we're Sealed because
	// of topology changes. i.e. if the current m3db process is assigned new shards,
	// we need to include their data in the index.

	// i.e. the only state we do not accept bootstrapped data is if we are closed.
	if b.state == blockStateClosed {
		return errUnableToBootstrapBlockClosed
	}

	// First check fulfilled is correct
	min, max := results.Fulfilled().MinMax()
	if min.Before(b.startTime) || max.After(b.endTime) {
		blockRange := xtime.Range{Start: b.startTime, End: b.endTime}
		return fmt.Errorf("fulfilled range %s is outside of index block range: %s",
			results.Fulfilled().SummaryString(), blockRange.String())
	}

	// NB: need to check if the current block has been marked 'Sealed' and if so,
	// mark all incoming mutable segments the same.
	isSealed := b.IsSealedWithRLock()

	var multiErr xerrors.MultiError
	for _, seg := range results.Segments() {
		if x, ok := seg.(segment.MutableSegment); ok {
			if isSealed {
				_, err := x.Seal()
				if err != nil {
					// if this happens it means a Mutable segment was marked sealed
					// in the bootstrappers, this should never happen.
					err := b.bootstrappingSealedMutableSegmentInvariant(err)
					multiErr = multiErr.Add(err)
				}
			}
		}
	}

	entry := blockShardRangesSegments{
		shardTimeRanges: results.Fulfilled(),
		segments:        results.Segments(),
	}

	// First see if this block can cover all our current blocks covering shard
	// time ranges
	currFulfilled := make(result.ShardTimeRanges)
	for _, existing := range b.shardRangesSegments {
		currFulfilled.AddRanges(existing.shardTimeRanges)
	}

	unfulfilledBySegments := currFulfilled.Copy()
	unfulfilledBySegments.Subtract(results.Fulfilled())
	if !unfulfilledBySegments.IsEmpty() {
		// This is the case where it cannot wholly replace the current set of blocks
		// so simply append the segments in this case
		b.shardRangesSegments = append(b.shardRangesSegments, entry)
		return multiErr.FinalError()
	}

	// This is the case where the new segments can wholly replace the
	// current set of blocks since unfullfilled by the new segments is zero
	for i, group := range b.shardRangesSegments {
		for _, seg := range group.segments {
			// Make sure to close the existing segments
			multiErr = multiErr.Add(seg.Close())
		}
		b.shardRangesSegments[i] = blockShardRangesSegments{}
	}
	b.shardRangesSegments = append(b.shardRangesSegments[:0], entry)

	return multiErr.FinalError()
}

func (b *block) Tick(c context.Cancellable, tickStart time.Time) (BlockTickResult, error) {
	b.RLock()
	defer b.RUnlock()
	result := BlockTickResult{}
	if b.state == blockStateClosed {
		return result, errUnableToTickBlockClosed
	}

	// active segment, can be nil incase we've evicted it already.
	for _, seg := range b.activeSegments {
		result.NumSegments++
		result.NumDocs += seg.Size()
	}

	// any other segments
	for _, group := range b.shardRangesSegments {
		for _, seg := range group.segments {
			result.NumSegments++
			result.NumDocs += seg.Size()
		}
	}

	return result, nil
}

func (b *block) Seal() error {
	b.Lock()
	defer b.Unlock()

	// ensure we only Seal if we're marked Open
	if b.state != blockStateOpen {
		return fmt.Errorf(errUnableToSealBlockIllegalStateFmtString, b.state)
	}
	b.state = blockStateSealed

	var multiErr xerrors.MultiError
	// seal active mutable segments.
	for _, seg := range b.activeSegments {
		if seg.state == mutableActiveSegmentState {
			_, err := seg.mutableSegment.Seal()
			multiErr = multiErr.Add(err)
		}
	}

	// loop over any added mutable segments and seal them too.
	for _, group := range b.shardRangesSegments {
		for _, seg := range group.segments {
			if unsealed, ok := seg.(segment.MutableSegment); ok {
				_, err := unsealed.Seal()
				multiErr = multiErr.Add(err)
			}
		}
	}

	return multiErr.FinalError()
}

func (b *block) IsSealedWithRLock() bool {
	return b.state == blockStateSealed
}

func (b *block) IsSealed() bool {
	b.RLock()
	defer b.RUnlock()
	return b.IsSealedWithRLock()
}

func (b *block) NeedsMutableSegmentsEvicted() bool {
	b.RLock()
	defer b.RUnlock()
	anyMutableSegmentNeedsEviction := false

	// loop thru active segments and see if they require to be flushed
	for _, seg := range b.activeSegments {
		anyMutableSegmentNeedsEviction = anyMutableSegmentNeedsEviction || seg.Size() > 0
	}

	// can early terminate if we already know we need to flush.
	if anyMutableSegmentNeedsEviction {
		return true
	}

	// otherwise we check all the boostrapped segments and to see if any of them need a flush
	for _, shardRangeSegments := range b.shardRangesSegments {
		for _, seg := range shardRangeSegments.segments {
			if mutableSeg, ok := seg.(segment.MutableSegment); ok {
				anyMutableSegmentNeedsEviction = anyMutableSegmentNeedsEviction || mutableSeg.Size() > 0
			}
		}
	}

	return anyMutableSegmentNeedsEviction
}

func (b *block) EvictActiveSegments() (EvictActiveSegmentResults, error) {
	var results EvictActiveSegmentResults
	b.Lock()
	defer b.Unlock()
	if b.state != blockStateSealed {
		return results, fmt.Errorf("unable to evict mutable segments, block must be sealed, found: %v", b.state)
	}
	var multiErr xerrors.MultiError

	// close active segments
	for _, seg := range b.activeSegments {
		results.NumActiveSegments++
		results.NumDocs += seg.Size()
		multiErr = multiErr.Add(seg.Close())
	}
	// clear any references to active segments
	b.activeSegments = nil

	// close any other mutable segments too.
	for idx := range b.shardRangesSegments {
		segments := make([]segment.Segment, 0, len(b.shardRangesSegments[idx].segments))
		for _, seg := range b.shardRangesSegments[idx].segments {
			mutableSeg, ok := seg.(segment.MutableSegment)
			if !ok {
				segments = append(segments, seg)
				continue
			}
			results.NumActiveSegments++
			results.NumDocs += mutableSeg.Size()
			multiErr = multiErr.Add(mutableSeg.Close())
		}
		b.shardRangesSegments[idx].segments = segments
	}

	return results, multiErr.FinalError()
}

func (b *block) Close() error {
	b.Lock()
	defer b.Unlock()
	if b.state == blockStateClosed {
		return errBlockAlreadyClosed
	}
	b.state = blockStateClosed

	var multiErr xerrors.MultiError

	// cancel any rotations that might be happening.
	b.closeCtxCancelFn()

	// close any active segments.
	for _, seg := range b.activeSegments {
		multiErr = multiErr.Add(seg.Close())
	}
	b.activeSegments = nil

	// close any other added segments too.
	for _, group := range b.shardRangesSegments {
		for _, seg := range group.segments {
			multiErr = multiErr.Add(seg.Close())
		}
	}
	b.shardRangesSegments = nil

	return multiErr.FinalError()
}

func (b *block) triggerRotations() {
	select {
	case b.rotateCh <- struct{}{}: // all good, we enqueued
	default: // i.e. there's already a rotation enqueued, so we're good.
	}
}

// monitorRotations monitors rotateCh and triggers activeSegment rotations when signaled.
func (b *block) monitorRotations() {
	for {
		select {
		case <-b.closeCtx.Done():
			return
		case <-b.rotateCh:
			b.rotateMutableActiveSegments()
		}
	}
}

func (b *block) rotateMutableActiveSegments() {
	// find group of active segments which need to be rotated
	var (
		activeSegmentsToRotate                   []*activeSegment
		segmentsToRotate                         []segment.Segment
		accumulatedSize                          int64
		numAccumulatedMutable, numAccumulatedFST int
	)
	b.Lock()
	// NB: sort the segments into ascending order by size, to guarantee we merge as many as we can
	sort.Slice(b.activeSegments, func(i, j int) bool {
		return b.activeSegments[i].Size() < b.activeSegments[j].Size()
	})
	for _, seg := range b.activeSegments {
		size := seg.Size()
		// if adding this segment is going to overflow the size, don't add it, and terminate the loop
		// because we know there are no smaller segments to be added (due to the sort above).
		if accumulatedSize+size >= defaultMutableSegmentRotationMergeSize {
			break
		}
		if seg.state == mutableActiveSegmentState && seg.mutableSegment.IsSealed() {
			seg.state = rotatingActiveSegmentState // mark the segment as being rotated
			accumulatedSize += size
			segmentsToRotate = append(segmentsToRotate, seg.mutableSegment)
			activeSegmentsToRotate = append(activeSegmentsToRotate, seg)
			numAccumulatedMutable++
		} else if seg.state == fstActiveSegmentState {
			accumulatedSize += size
			segmentsToRotate = append(segmentsToRotate, seg.fstSegment)
			activeSegmentsToRotate = append(activeSegmentsToRotate, seg)
			numAccumulatedFST++
		}
	}
	b.Unlock()

	// i.e. no active segments need rotation, so we can terminate early
	if len(segmentsToRotate) == 0 || (len(segmentsToRotate) == 1 && numAccumulatedMutable == 0) {
		return
	}

	start := time.Now()
	// merge segments to rotate
	postingsOffset := postings.ID(b.segmentID.Inc())
	mergedMutableSegment := mem.NewSegment(postingsOffset, b.opts.MemSegmentOptions())
	if err := mem.Merge(mergedMutableSegment, segmentsToRotate...); err != nil {
		// TODO(prateek): only log for now, maybe we should add retries?
		b.opts.InstrumentOptions().Logger().Errorf("unable to merge simple segments, err = %v", err)
		return
	}
	mergeTime := time.Since(start)
	if _, err := mergedMutableSegment.Seal(); err != nil {
		// TODO(prateek): only log for now, maybe we should add retries?
		b.opts.InstrumentOptions().Logger().Errorf("unable to seal merged segment, err = %v", err)
		return
	}

	fstSegment, err := m3ninxpersist.TransformAndMmap(mergedMutableSegment, b.opts.FSTSegmentOptions())
	if err != nil {
		b.opts.InstrumentOptions().Logger().Errorf("unable to fst-ify merged segment, err = %v", err)
		return
	}
	compactionTime := time.Since(start)
	b.opts.InstrumentOptions().Logger().Infof("took [%v to compact, %v to merge] %d segments of total size %d",
		compactionTime, mergeTime, len(segmentsToRotate), fstSegment.Size())

	mergedMutableSegment.Close() // can skip error checking here as we've got the equivalent FST
	newActiveSegment := &activeSegment{
		creationTime: b.opts.ClockOptions().NowFn()(),
		state:        fstActiveSegmentState,
		fstSegment:   fstSegment,
	}

	// swap the successfully converted activeSegments with the newly created FST
	b.Lock()
	segments := make([]*activeSegment, 0, len(b.activeSegments))
	for _, seg := range b.activeSegments {
		// skip all the activeSegments which have been converted above
		isRotatedSegment := false
		for _, aseg := range activeSegmentsToRotate {
			if seg == aseg {
				isRotatedSegment = true
				break
			}
		}
		if !isRotatedSegment {
			// add all other activeSegments
			segments = append(segments, seg)
		}
	}
	// finally, add the newly created activeSegment and update active segments
	segments = append(segments, newActiveSegment)
	b.activeSegments = segments
	b.Unlock()

	// release resources from all merged segments
	for _, seg := range segmentsToRotate {
		seg.Close() // can skip error checking here as we've got the equivalent FST
	}
}

func (b *block) addActiveSegmentWithLock() {
	postingsOffset := postings.ID(b.segmentID.Inc())
	mutableSeg := mem.NewSegment(postingsOffset, b.opts.MemSegmentOptions())

	// move the new mutable segment to the front to make it easier for writes
	// to find a mutable segment.
	b.activeSegments = append([]*activeSegment{
		&activeSegment{
			creationTime:   b.opts.ClockOptions().NowFn()(),
			state:          mutableActiveSegmentState,
			mutableSegment: mutableSeg,
		},
	}, b.activeSegments...)

	logger := b.opts.InstrumentOptions().Logger()
	logger.Infof("[blockStart: %v] stats after adding active segment", b.StartTime())
	for i, seg := range b.activeSegments {
		logger.Infof("%d) state [%v] size [%d]", i, seg.state.String(), seg.Size())
	}
}

// mutableActiveSegmentWithRLock returns any activeSegment marked as a mutableSegment.
// NB: it can return nil if no such segment exists.
func (b *block) mutableActiveSegmentWithRLock() *activeSegment {
	for _, seg := range b.activeSegments {
		if seg.state == mutableActiveSegmentState {
			return seg
		}
	}
	return nil
}

func (b *block) reportMetrics() {
	reportInterval := b.opts.InstrumentOptions().ReportInterval()
	for {
		select {
		case <-b.closeCtx.Done():
			return
		default:
			b.RLock()
			numDocs := float64(0)
			numSegments := float64(0)
			numSegmentsCompacting := float64(0)
			for _, seg := range b.activeSegments {
				numDocs += float64(seg.Size())
				numSegments++
				// if seg.compacting {
				// 	numSegmentsCompacting++
				// }
				b.metrics.segmentSizeHistogram.RecordValue(float64(seg.Size()))
			}
			b.RUnlock()
			b.metrics.numDocs.Update(numDocs)
			b.metrics.numSegments.Update(numSegments)
			b.metrics.numSegmentsCompacting.Update(numSegmentsCompacting)
		}
		time.Sleep(reportInterval)
	}
}

type blockMetrics struct {
	numSegmentsCompacting tally.Gauge
	numSegments           tally.Gauge
	numDocs               tally.Gauge
	segmentSizeHistogram  tally.Histogram
	compactionLatency     tally.Timer
}

func newBlockMetrics(scope tally.Scope) blockMetrics {
	buckets := tally.MustMakeExponentialValueBuckets(1, 10, 7)
	return blockMetrics{
		numSegmentsCompacting: scope.Gauge("num-segments-compacting"),
		numSegments:           scope.Gauge("num-segments"),
		numDocs:               scope.Gauge("num-docs"),
		segmentSizeHistogram:  scope.Histogram("segment-size", buckets),
		compactionLatency:     scope.Timer("compaction-latency"),
	}
}

func (b *block) writeBatchErrorInvalidState(state blockState) error {
	switch state {
	case blockStateClosed:
		return errUnableToWriteBlockClosed
	case blockStateSealed:
		return errUnableToWriteBlockSealed
	default: // should never happen
		err := fmt.Errorf(errUnableToWriteBlockUnknownStateFmtString, state)
		instrument.EmitInvariantViolationAndGetLogger(b.opts.InstrumentOptions()).Errorf(err.Error())
		return err
	}
}

func (b *block) unknownWriteBatchInvariantError(err error) error {
	wrappedErr := fmt.Errorf("unexpected non-BatchPartialError from m3ninx InsertBatch: %v", err)
	instrument.EmitInvariantViolationAndGetLogger(b.opts.InstrumentOptions()).Errorf(wrappedErr.Error())
	return wrappedErr
}

func (b *block) bootstrappingSealedMutableSegmentInvariant(err error) error {
	wrapped := fmt.Errorf("internal error: bootstrapping a mutable segment already marked sealed: %v", err)
	instrument.EmitInvariantViolationAndGetLogger(b.opts.InstrumentOptions()).Errorf(wrapped.Error())
	return wrapped
}

func (b *block) openBlockHasNilActiveSegmentInvariantErrorWithRLock() error {
	err := fmt.Errorf("internal error: block has open block state [%v] has no mutable active segment", b.state)
	instrument.EmitInvariantViolationAndGetLogger(b.opts.InstrumentOptions()).Errorf(err.Error())
	return err
}

type closable interface {
	Close() error
}

type safeCloser struct {
	closable
	closed bool
}

func (c *safeCloser) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	return c.closable.Close()
}
