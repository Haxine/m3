// Copyright (c) 2016 Uber Technologies, Inc.
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

package node

import (
	"fmt"
	"sync"

	"github.com/m3db/m3db/network/server/tchannelthrift/convert"
	tterrors "github.com/m3db/m3db/network/server/tchannelthrift/errors"
	"github.com/m3db/m3db/network/server/tchannelthrift/thrift/gen-go/rpc"
	"github.com/m3db/m3db/storage"
	xerrors "github.com/m3db/m3db/x/errors"
	xio "github.com/m3db/m3db/x/io"
	xtime "github.com/m3db/m3db/x/time"

	"github.com/uber/tchannel-go/thrift"
)

type service struct {
	sync.RWMutex

	db     storage.Database
	health *rpc.HealthResult_
}

// NewService creates a new node TChannel Thrift service
func NewService(db storage.Database) rpc.TChanNode {
	return &service{
		db:     db,
		health: &rpc.HealthResult_{Ok: true, Status: "up"},
	}
}

func (s *service) Health(ctx thrift.Context) (*rpc.HealthResult_, error) {
	s.RLock()
	health := s.health
	s.RUnlock()
	return health, nil
}

func (s *service) Fetch(tctx thrift.Context, req *rpc.FetchRequest) (*rpc.FetchResult_, error) {
	ctx := s.db.Options().GetContextPool().Get()
	defer ctx.Close()

	start, rangeStartErr := convert.ValueToTime(req.RangeStart, req.RangeType)
	end, rangeEndErr := convert.ValueToTime(req.RangeEnd, req.RangeType)
	if rangeStartErr != nil || rangeEndErr != nil {
		return nil, tterrors.NewBadRequestError(xerrors.FirstError(rangeStartErr, rangeEndErr))
	}

	encoded, err := s.db.ReadEncoded(ctx, req.ID, start, end)
	if err != nil {
		if xerrors.IsInvalidParams(err) {
			return nil, tterrors.NewBadRequestError(err)
		}
		return nil, tterrors.NewInternalError(err)
	}

	result := rpc.NewFetchResult_()

	// Make datapoints an initialized empty array for JSON serialization as empty array than null
	result.Datapoints = make([]*rpc.Datapoint, 0)

	if encoded == nil || len(encoded.Readers()) == 0 {
		return result, nil
	}

	for _, reader := range encoded.Readers() {
		newDecoderFn := s.db.Options().GetNewDecoderFn()
		it := newDecoderFn().Decode(reader)
		for it.Next() {
			dp, _, annotation := it.Current()
			ts, tsErr := convert.TimeToValue(dp.Timestamp, req.ResultTimeType)
			if tsErr != nil {
				return nil, tterrors.NewBadRequestError(tsErr)
			}

			afterOrAtStart := !dp.Timestamp.Before(start)
			beforeOrAtEnd := !dp.Timestamp.After(end)
			if afterOrAtStart && beforeOrAtEnd {
				datapoint := rpc.NewDatapoint()
				datapoint.Timestamp = ts
				datapoint.Value = dp.Value
				datapoint.Annotation = annotation
				result.Datapoints = append(result.Datapoints, datapoint)
			}
		}
		if err := it.Err(); err != nil {
			return nil, tterrors.NewInternalError(err)
		}
	}

	return result, nil
}

func (s *service) FetchRawBatch(tctx thrift.Context, req *rpc.FetchRawBatchRequest) (*rpc.FetchRawBatchResult_, error) {
	ctx := s.db.Options().GetContextPool().Get()
	defer ctx.Close()

	start, rangeStartErr := convert.ValueToTime(req.RangeStart, req.RangeType)
	end, rangeEndErr := convert.ValueToTime(req.RangeEnd, req.RangeType)
	if rangeStartErr != nil || rangeEndErr != nil {
		return nil, tterrors.NewBadRequestError(xerrors.FirstError(rangeStartErr, rangeEndErr))
	}

	result := rpc.NewFetchRawBatchResult_()
	for i := range req.Ids {
		encoded, err := s.db.ReadEncoded(ctx, req.Ids[i], start, end)
		if err != nil {
			if xerrors.IsInvalidParams(err) {
				return nil, tterrors.NewBadRequestError(err)
			}
			return nil, tterrors.NewInternalError(err)
		}
		rawResult := rpc.NewFetchRawResult_()
		sgrs, err := xio.GetSegmentReaders(encoded)
		if err != nil {
			return nil, tterrors.NewInternalError(err)
		}
		segments := make([]*rpc.Segment, 0, len(sgrs))
		for _, sgr := range sgrs {
			seg := sgr.Segment()
			segments = append(segments, &rpc.Segment{Head: seg.Head, Tail: seg.Tail})
		}
		rawResult.Segments = segments
		result.Elements = append(result.Elements, rawResult)
	}

	return result, nil
}

func (s *service) Write(tctx thrift.Context, req *rpc.WriteRequest) error {
	ctx := s.db.Options().GetContextPool().Get()
	defer ctx.Close()

	if req.Datapoint == nil {
		return tterrors.NewBadRequestWriteError(fmt.Errorf("requires datapoint"))
	}
	unit, unitErr := convert.TimeTypeToUnit(req.Datapoint.TimestampType)
	if unitErr != nil {
		return tterrors.NewBadRequestWriteError(unitErr)
	}
	d, err := unit.Value()
	if err != nil {
		return tterrors.NewBadRequestWriteError(err)
	}
	ts := xtime.FromNormalizedTime(req.Datapoint.Timestamp, d)
	err = s.db.Write(ctx, req.ID, ts, req.Datapoint.Value, unit, req.Datapoint.Annotation)
	if err != nil {
		if xerrors.IsInvalidParams(err) {
			return tterrors.NewBadRequestWriteError(err)
		}
		return tterrors.NewWriteError(err)
	}
	return nil
}

func (s *service) WriteBatch(tctx thrift.Context, req *rpc.WriteBatchRequest) error {
	ctx := s.db.Options().GetContextPool().Get()
	defer ctx.Close()

	var errs []*rpc.WriteBatchError
	for i, elem := range req.Elements {
		unit, unitErr := convert.TimeTypeToUnit(elem.Datapoint.TimestampType)
		if unitErr != nil {
			errs = append(errs, tterrors.NewBadRequestWriteBatchError(i, unitErr))
			continue
		}
		d, err := unit.Value()
		if err != nil {
			errs = append(errs, tterrors.NewBadRequestWriteBatchError(i, err))
			continue
		}
		ts := xtime.FromNormalizedTime(elem.Datapoint.Timestamp, d)
		err = s.db.Write(ctx, elem.ID, ts, elem.Datapoint.Value, unit, elem.Datapoint.Annotation)
		if err != nil {
			if xerrors.IsInvalidParams(err) {
				errs = append(errs, tterrors.NewBadRequestWriteBatchError(i, err))
			} else {
				errs = append(errs, tterrors.NewWriteBatchError(i, err))
			}
		}
	}

	if len(errs) > 0 {
		batchErrs := rpc.NewWriteBatchErrors()
		batchErrs.Errors = errs
		return batchErrs
	}
	return nil
}