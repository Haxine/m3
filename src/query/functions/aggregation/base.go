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

package aggregation

import (
	"fmt"

	"github.com/m3db/m3/src/query/block"
	"github.com/m3db/m3/src/query/executor/transform"
	"github.com/m3db/m3/src/query/models"
	"github.com/m3db/m3/src/query/parser"
)

// NodeParams contains additional parameters required for aggregation ops
type NodeParams struct {
	Matching []string
	Without  bool
}

type withKeysID func(tags models.Tags, matching []string) uint64

func includeKeysID(tags models.Tags, matching []string) uint64 {
	return tags.IDWithKeys(matching...)
}

func excludeKeysID(tags models.Tags, matching []string) uint64 {
	return tags.IDWithExcludes(matching...)
}

type withKeysTags func(tags models.Tags, matching []string) models.Tags

func includeKeysTags(tags models.Tags, matching []string) models.Tags {
	return tags.TagsWithKeys(matching)
}

func excludeKeysTags(tags models.Tags, matching []string) models.Tags {
	return tags.TagsWithoutKeys(matching)
}

// create the output, by tags,
// returns a list of seriesMeta for the combined series,
// and a list of [index lists].
// Input series that exist in an index list are mapped to the
// relevant index in the combined series meta.
func collectSeries(params NodeParams, opName string, metas []block.SeriesMeta) ([][]int, []block.SeriesMeta) {
	without, matching := params.Without, params.Matching

	var idFunc withKeysID
	var tagsFunc withKeysTags
	if without {
		idFunc = excludeKeysID
		tagsFunc = excludeKeysTags
	} else {
		idFunc = includeKeysID
		tagsFunc = includeKeysTags
	}

	type tagMatch struct {
		indices []int
		tags    models.Tags
	}

	tagMap := make(map[uint64]*tagMatch)
	for i, meta := range metas {
		id := idFunc(meta.Tags, matching)
		if val, ok := tagMap[id]; ok {
			val.indices = append(val.indices, i)
		} else {
			tagMap[id] = &tagMatch{
				indices: []int{i},
				tags:    tagsFunc(meta.Tags, matching),
			}
		}
	}

	collectedIndices := make([][]int, len(tagMap))
	collectedMetas := make([]block.SeriesMeta, len(tagMap))
	i := 0
	for _, v := range tagMap {
		collectedIndices[i] = v.indices
		collectedMetas[i] = block.SeriesMeta{
			Tags: v.tags,
			Name: opName,
		}
		i++
	}

	return collectedIndices, collectedMetas
}

type aggregationFn func(values []float64, indices [][]int) []float64

var aggregationFunctions = map[string]aggregationFn{
	SumType:               sumFn,
	MinType:               minFn,
	MaxType:               maxFn,
	AverageType:           averageFn,
	StandardDeviationType: stddevFn,
	StandardVarianceType:  varianceFn,
	CountType:             countFn,
}

// NewAggregationOp creates a new aggregation operation
func NewAggregationOp(
	opType string,
	params NodeParams,
) (parser.Params, error) {
	if fn, ok := aggregationFunctions[opType]; ok {
		return newBaseOp(params, opType, fn), nil
	}
	return baseOp{}, fmt.Errorf("operator not supported: %s", opType)
}

// baseOp stores required properties for count
type baseOp struct {
	params NodeParams
	opType string
	aggFn  aggregationFn
}

// OpType for the operator
func (o baseOp) OpType() string {
	return o.opType
}

// String representation
func (o baseOp) String() string {
	return fmt.Sprintf("type: %s", o.OpType())
}

// Node creates an execution node
func (o baseOp) Node(controller *transform.Controller) transform.OpNode {
	return &baseNode{
		op:         o,
		controller: controller,
	}
}

func newBaseOp(params NodeParams, opType string, aggFn aggregationFn) baseOp {
	return baseOp{
		params: params,
		opType: opType,
		aggFn:  aggFn,
	}
}

type baseNode struct {
	op         baseOp
	controller *transform.Controller
}

// Process the block
func (n *baseNode) Process(ID parser.NodeID, b block.Block) error {
	stepIter, err := b.StepIter()
	if err != nil {
		return err
	}

	params := n.op.params
	indices, metas := collectSeries(params, n.op.opType, stepIter.SeriesMeta())

	builder, err := n.controller.BlockBuilder(stepIter.Meta(), metas)
	if err != nil {
		return err
	}

	if err := builder.AddCols(stepIter.StepCount()); err != nil {
		return err
	}

	for index := 0; stepIter.Next(); index++ {
		step, err := stepIter.Current()
		if err != nil {
			return err
		}

		values := step.Values()
		aggregatedValues := n.op.aggFn(values, indices)
		builder.AppendValues(index, aggregatedValues)
	}

	nextBlock := builder.Build()
	defer nextBlock.Close()
	return n.controller.Process(nextBlock)
}