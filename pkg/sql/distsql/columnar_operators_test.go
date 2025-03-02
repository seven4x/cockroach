// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package distsql

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/col/coldata"
	"github.com/cockroachdb/cockroach/pkg/col/typeconv"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec/colbuilder"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/stretchr/testify/require"
)

const nullProbability = 0.2
const randTypesProbability = 0.5

var aggregateFuncToNumArguments = map[execinfrapb.AggregatorSpec_Func]int{
	execinfrapb.AggregatorSpec_ANY_NOT_NULL:         1,
	execinfrapb.AggregatorSpec_AVG:                  1,
	execinfrapb.AggregatorSpec_BOOL_AND:             1,
	execinfrapb.AggregatorSpec_BOOL_OR:              1,
	execinfrapb.AggregatorSpec_CONCAT_AGG:           1,
	execinfrapb.AggregatorSpec_COUNT:                1,
	execinfrapb.AggregatorSpec_MAX:                  1,
	execinfrapb.AggregatorSpec_MIN:                  1,
	execinfrapb.AggregatorSpec_STDDEV:               1,
	execinfrapb.AggregatorSpec_SUM:                  1,
	execinfrapb.AggregatorSpec_SUM_INT:              1,
	execinfrapb.AggregatorSpec_VARIANCE:             1,
	execinfrapb.AggregatorSpec_XOR_AGG:              1,
	execinfrapb.AggregatorSpec_COUNT_ROWS:           0,
	execinfrapb.AggregatorSpec_SQRDIFF:              1,
	execinfrapb.AggregatorSpec_FINAL_VARIANCE:       3,
	execinfrapb.AggregatorSpec_FINAL_STDDEV:         3,
	execinfrapb.AggregatorSpec_ARRAY_AGG:            1,
	execinfrapb.AggregatorSpec_JSON_AGG:             1,
	execinfrapb.AggregatorSpec_JSONB_AGG:            1,
	execinfrapb.AggregatorSpec_STRING_AGG:           2,
	execinfrapb.AggregatorSpec_BIT_AND:              1,
	execinfrapb.AggregatorSpec_BIT_OR:               1,
	execinfrapb.AggregatorSpec_CORR:                 2,
	execinfrapb.AggregatorSpec_PERCENTILE_DISC_IMPL: 2,
	execinfrapb.AggregatorSpec_PERCENTILE_CONT_IMPL: 2,
	execinfrapb.AggregatorSpec_JSON_OBJECT_AGG:      2,
	execinfrapb.AggregatorSpec_JSONB_OBJECT_AGG:     2,
	execinfrapb.AggregatorSpec_VAR_POP:              1,
	execinfrapb.AggregatorSpec_STDDEV_POP:           1,
	execinfrapb.AggregatorSpec_ST_MAKELINE:          1,
	execinfrapb.AggregatorSpec_ST_EXTENT:            1,
	execinfrapb.AggregatorSpec_ST_UNION:             1,
	execinfrapb.AggregatorSpec_ST_COLLECT:           1,
	execinfrapb.AggregatorSpec_COVAR_POP:            2,
	execinfrapb.AggregatorSpec_COVAR_SAMP:           2,
	execinfrapb.AggregatorSpec_REGR_INTERCEPT:       2,
	execinfrapb.AggregatorSpec_REGR_R2:              2,
	execinfrapb.AggregatorSpec_REGR_SLOPE:           2,
	execinfrapb.AggregatorSpec_REGR_SXX:             2,
	execinfrapb.AggregatorSpec_REGR_SXY:             2,
	execinfrapb.AggregatorSpec_REGR_SYY:             2,
	execinfrapb.AggregatorSpec_REGR_COUNT:           2,
	execinfrapb.AggregatorSpec_REGR_AVGX:            2,
	execinfrapb.AggregatorSpec_REGR_AVGY:            2,
}

// TestAggregateFuncToNumArguments ensures that all aggregate functions are
// present in the map above.
func TestAggregateFuncToNumArguments(t *testing.T) {
	defer leaktest.AfterTest(t)()
	for aggFn, aggFnName := range execinfrapb.AggregatorSpec_Func_name {
		if _, found := aggregateFuncToNumArguments[execinfrapb.AggregatorSpec_Func(aggFn)]; !found {
			t.Fatalf("didn't find number of arguments for %s", aggFnName)
		}
	}
}

func TestAggregatorAgainstProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()

	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(context.Background())

	rng, seed := randutil.NewPseudoRand()
	nRuns := 20
	nRows := 100
	nAggFnsToTest := 5
	const (
		maxNumGroupingCols = 3
		nextGroupProb      = 0.2
	)
	groupingCols := make([]uint32, maxNumGroupingCols)
	orderingCols := make([]execinfrapb.Ordering_Column, maxNumGroupingCols)
	for i := uint32(0); i < maxNumGroupingCols; i++ {
		groupingCols[i] = i
		orderingCols[i].ColIdx = i
	}
	var da rowenc.DatumAlloc

	// We need +1 because an entry for index=6 was omitted by mistake.
	numSupportedAggFns := len(execinfrapb.AggregatorSpec_Func_name) + 1
	aggregations := make([]execinfrapb.AggregatorSpec_Aggregation, 0, nAggFnsToTest)
	for len(aggregations) < nAggFnsToTest {
		var aggFn execinfrapb.AggregatorSpec_Func
		found := false
		for !found {
			aggFn = execinfrapb.AggregatorSpec_Func(rng.Intn(numSupportedAggFns))
			if _, valid := aggregateFuncToNumArguments[aggFn]; !valid {
				continue
			}
			switch aggFn {
			case execinfrapb.AggregatorSpec_ANY_NOT_NULL:
				// We skip ANY_NOT_NULL aggregate function because it returns
				// non-deterministic results.
			case execinfrapb.AggregatorSpec_PERCENTILE_DISC_IMPL,
				execinfrapb.AggregatorSpec_PERCENTILE_CONT_IMPL:
				// We skip percentile functions because those can only be
				// planned as window functions.
			default:
				found = true
			}

		}
		aggregations = append(aggregations, execinfrapb.AggregatorSpec_Aggregation{Func: aggFn})
	}
	for _, hashAgg := range []bool{false, true} {
		filteringAggOptions := []bool{false}
		if hashAgg {
			// We currently support filtering aggregation only for hash
			// aggregator.
			filteringAggOptions = []bool{false, true}
		}
		for _, filteringAgg := range filteringAggOptions {
			numFilteringCols := 0
			if filteringAgg {
				numFilteringCols = 1
			}
			for numGroupingCols := 1; numGroupingCols <= maxNumGroupingCols; numGroupingCols++ {
				// We will be grouping based on the first numGroupingCols columns
				// (which will be of INT types) with the values for the columns set
				// manually below.
				numUtilityCols := numGroupingCols + numFilteringCols
				inputTypes := make([]*types.T, 0, numUtilityCols+len(aggregations))
				for i := 0; i < numGroupingCols; i++ {
					inputTypes = append(inputTypes, types.Int)
				}
				// Check whether we want to add a column for FILTER clause.
				var filteringColIdx uint32
				if filteringAgg {
					filteringColIdx = uint32(len(inputTypes))
					inputTypes = append(inputTypes, types.Bool)
				}
				// After all utility columns, we will have input columns for each
				// of the aggregate functions. Here, we will set up the column
				// indices, and the types will be generated below.
				numColsSoFar := numUtilityCols
				for i := range aggregations {
					numArguments := aggregateFuncToNumArguments[aggregations[i].Func]
					aggregations[i].ColIdx = make([]uint32, numArguments)
					for j := range aggregations[i].ColIdx {
						aggregations[i].ColIdx[j] = uint32(numColsSoFar)
						numColsSoFar++
					}
				}
				outputTypes := make([]*types.T, len(aggregations))

				for run := 0; run < nRuns; run++ {
					inputTypes = inputTypes[:numUtilityCols]
					var rows rowenc.EncDatumRows
					hasJSONColumn := false
					for i := range aggregations {
						aggFn := aggregations[i].Func
						aggFnInputTypes := make([]*types.T, len(aggregations[i].ColIdx))
						for {
							for j := range aggFnInputTypes {
								aggFnInputTypes[j] = rowenc.RandType(rng)
							}
							// There is a special case for some functions when at
							// least one argument is a tuple.
							// Such cases pass GetAggregateInfo check below,
							// but they are actually invalid, and during normal
							// execution it is caught during type-checking.
							// However, we don't want to do fully-fledged type
							// checking, so we hard-code an exception here.
							invalid := false
							switch aggFn {
							case execinfrapb.AggregatorSpec_CONCAT_AGG,
								execinfrapb.AggregatorSpec_STRING_AGG,
								execinfrapb.AggregatorSpec_ST_MAKELINE,
								execinfrapb.AggregatorSpec_ST_EXTENT,
								execinfrapb.AggregatorSpec_ST_UNION,
								execinfrapb.AggregatorSpec_ST_COLLECT:
								for _, typ := range aggFnInputTypes {
									if typ.Family() == types.TupleFamily {
										invalid = true
										break
									}
								}
							}
							if invalid {
								continue
							}
							for _, typ := range aggFnInputTypes {
								hasJSONColumn = hasJSONColumn || typ.Family() == types.JsonFamily
							}
							if _, outputType, err := execinfrapb.GetAggregateInfo(aggFn, aggFnInputTypes...); err == nil {
								outputTypes[i] = outputType
								break
							}
						}
						inputTypes = append(inputTypes, aggFnInputTypes...)
					}
					rows = rowenc.RandEncDatumRowsOfTypes(rng, nRows, inputTypes)
					groupIdx := 0
					for _, row := range rows {
						for i := 0; i < numGroupingCols; i++ {
							if rng.Float64() < nullProbability {
								row[i] = rowenc.EncDatum{Datum: tree.DNull}
							} else {
								row[i] = rowenc.EncDatum{Datum: tree.NewDInt(tree.DInt(groupIdx))}
								if rng.Float64() < nextGroupProb {
									groupIdx++
								}
							}
						}
					}

					// Update the specifications of aggregate functions to
					// possibly include DISTINCT and/or FILTER clauses.
					for _, aggFn := range aggregations {
						distinctProb := 0.5
						if hasJSONColumn {
							// We currently cannot encode json columns, so we
							// don't support distinct aggregation in both
							// row-by-row and vectorized engines.
							distinctProb = 0
						}
						aggFn.Distinct = rng.Float64() < distinctProb
						if filteringAgg {
							aggFn.FilterColIdx = &filteringColIdx
						} else {
							aggFn.FilterColIdx = nil
						}
					}
					aggregatorSpec := &execinfrapb.AggregatorSpec{
						Type:         execinfrapb.AggregatorSpec_NON_SCALAR,
						GroupCols:    groupingCols[:numGroupingCols],
						Aggregations: aggregations,
					}
					if hashAgg {
						// Let's shuffle the rows for the hash aggregator.
						rand.Shuffle(nRows, func(i, j int) {
							rows[i], rows[j] = rows[j], rows[i]
						})
					} else {
						aggregatorSpec.OrderedGroupCols = groupingCols[:numGroupingCols]
						orderedCols := execinfrapb.ConvertToColumnOrdering(
							execinfrapb.Ordering{Columns: orderingCols[:numGroupingCols]},
						)
						// Although we build the input rows in "non-decreasing" order, it is
						// possible that some NULL values are present here and there, so we
						// need to sort the rows to satisfy the ordering conditions.
						sort.Slice(rows, func(i, j int) bool {
							cmp, err := rows[i].Compare(inputTypes, &da, orderedCols, &evalCtx, rows[j])
							if err != nil {
								t.Fatal(err)
							}
							return cmp < 0
						})
					}
					pspec := &execinfrapb.ProcessorSpec{
						Input:       []execinfrapb.InputSyncSpec{{ColumnTypes: inputTypes}},
						Core:        execinfrapb.ProcessorCoreUnion{Aggregator: aggregatorSpec},
						ResultTypes: outputTypes,
					}
					args := verifyColOperatorArgs{
						anyOrder:   hashAgg,
						inputTypes: [][]*types.T{inputTypes},
						inputs:     []rowenc.EncDatumRows{rows},
						pspec:      pspec,
					}
					if err := verifyColOperator(t, args); err != nil {
						if strings.Contains(err.Error(), "different errors returned") {
							// Columnar and row-based aggregators are likely to hit
							// different errors, and we will swallow those and move
							// on.
							continue
						}
						fmt.Printf("--- seed = %d run = %d filter = %t hash = %t ---\n",
							seed, run, filteringAgg, hashAgg)
						var aggFnNames string
						for i, agg := range aggregations {
							if i > 0 {
								aggFnNames += " "
							}
							aggFnNames += agg.Func.String()
						}
						fmt.Printf("--- %s ---\n", aggFnNames)
						prettyPrintTypes(inputTypes, "t" /* tableName */)
						prettyPrintInput(rows, inputTypes, "t" /* tableName */)
						t.Fatal(err)
					}
				}
			}
		}
	}
}

func TestDistinctAgainstProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	var da rowenc.DatumAlloc
	evalCtx := tree.MakeTestingEvalContext(cluster.MakeTestingClusterSettings())
	defer evalCtx.Stop(context.Background())

	rng, seed := randutil.NewPseudoRand()
	nRuns := 10
	nRows := 10
	maxCols := 3
	maxNum := 3
	intTyps := make([]*types.T, maxCols)
	for i := range intTyps {
		intTyps[i] = types.Int
	}

	for run := 0; run < nRuns; run++ {
		for nCols := 1; nCols <= maxCols; nCols++ {
			for nDistinctCols := 1; nDistinctCols <= nCols; nDistinctCols++ {
				for nOrderedCols := 0; nOrderedCols <= nDistinctCols; nOrderedCols++ {
					var (
						rows       rowenc.EncDatumRows
						inputTypes []*types.T
						ordCols    []execinfrapb.Ordering_Column
					)
					if rng.Float64() < randTypesProbability {
						inputTypes = generateRandomSupportedTypes(rng, nCols)
						rows = rowenc.RandEncDatumRowsOfTypes(rng, nRows, inputTypes)
					} else {
						inputTypes = intTyps[:nCols]
						rows = rowenc.MakeRandIntRowsInRange(rng, nRows, nCols, maxNum, nullProbability)
					}
					distinctCols := make([]uint32, nDistinctCols)
					for i, distinctCol := range rng.Perm(nCols)[:nDistinctCols] {
						distinctCols[i] = uint32(distinctCol)
					}
					orderedCols := make([]uint32, nOrderedCols)
					for i, orderedColIdx := range rng.Perm(nDistinctCols)[:nOrderedCols] {
						// From the set of distinct columns we need to choose nOrderedCols
						// to be in the ordered columns set.
						orderedCols[i] = distinctCols[orderedColIdx]
					}
					ordCols = make([]execinfrapb.Ordering_Column, nOrderedCols)
					for i, col := range orderedCols {
						ordCols[i] = execinfrapb.Ordering_Column{
							ColIdx: col,
						}
					}
					sort.Slice(rows, func(i, j int) bool {
						cmp, err := rows[i].Compare(
							inputTypes, &da,
							execinfrapb.ConvertToColumnOrdering(execinfrapb.Ordering{Columns: ordCols}),
							&evalCtx, rows[j],
						)
						if err != nil {
							t.Fatal(err)
						}
						return cmp < 0
					})

					spec := &execinfrapb.DistinctSpec{
						DistinctColumns: distinctCols,
						OrderedColumns:  orderedCols,
					}
					pspec := &execinfrapb.ProcessorSpec{
						Input:       []execinfrapb.InputSyncSpec{{ColumnTypes: inputTypes}},
						Core:        execinfrapb.ProcessorCoreUnion{Distinct: spec},
						ResultTypes: inputTypes,
					}
					args := verifyColOperatorArgs{
						anyOrder:   false,
						inputTypes: [][]*types.T{inputTypes},
						inputs:     []rowenc.EncDatumRows{rows},
						pspec:      pspec,
					}
					if err := verifyColOperator(t, args); err != nil {
						fmt.Printf("--- seed = %d run = %d nCols = %d distinct cols = %v ordered cols = %v ---\n",
							seed, run, nCols, distinctCols, orderedCols)
						prettyPrintTypes(inputTypes, "t" /* tableName */)
						prettyPrintInput(rows, inputTypes, "t" /* tableName */)
						t.Fatal(err)
					}
				}
			}
		}
	}
}

func TestSorterAgainstProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(context.Background())

	rng, seed := randutil.NewPseudoRand()
	nRuns := 5
	nRows := 8 * coldata.BatchSize()
	maxCols := 5
	maxNum := 10
	intTyps := make([]*types.T, maxCols)
	for i := range intTyps {
		intTyps[i] = types.Int
	}

	for _, spillForced := range []bool{false, true} {
		for run := 0; run < nRuns; run++ {
			for nCols := 1; nCols <= maxCols; nCols++ {
				// We will try both general sort and top K sort.
				for _, topK := range []uint64{0, uint64(1 + rng.Intn(64))} {
					var (
						rows       rowenc.EncDatumRows
						inputTypes []*types.T
					)
					if rng.Float64() < randTypesProbability {
						inputTypes = generateRandomSupportedTypes(rng, nCols)
						rows = rowenc.RandEncDatumRowsOfTypes(rng, nRows, inputTypes)
					} else {
						inputTypes = intTyps[:nCols]
						rows = rowenc.MakeRandIntRowsInRange(rng, nRows, nCols, maxNum, nullProbability)
					}

					// Note: we're only generating column orderings on all nCols columns since
					// if there are columns not in the ordering, the results are not fully
					// deterministic.
					orderingCols := generateColumnOrdering(rng, nCols, nCols)
					sorterSpec := &execinfrapb.SorterSpec{
						OutputOrdering: execinfrapb.Ordering{Columns: orderingCols},
					}
					var limit, offset uint64
					if topK > 0 {
						offset = uint64(rng.Intn(int(topK)))
						limit = topK - offset
					}
					pspec := &execinfrapb.ProcessorSpec{
						Input:       []execinfrapb.InputSyncSpec{{ColumnTypes: inputTypes}},
						Core:        execinfrapb.ProcessorCoreUnion{Sorter: sorterSpec},
						Post:        execinfrapb.PostProcessSpec{Limit: limit, Offset: offset},
						ResultTypes: inputTypes,
					}
					args := verifyColOperatorArgs{
						inputTypes:     [][]*types.T{inputTypes},
						inputs:         []rowenc.EncDatumRows{rows},
						pspec:          pspec,
						forceDiskSpill: spillForced,
					}
					if spillForced {
						args.numForcedRepartitions = 2 + rng.Intn(3)
					}
					if err := verifyColOperator(t, args); err != nil {
						fmt.Printf("--- seed = %d spillForced = %t nCols = %d K = %d ---\n",
							seed, spillForced, nCols, topK)
						prettyPrintTypes(inputTypes, "t" /* tableName */)
						prettyPrintInput(rows, inputTypes, "t" /* tableName */)
						t.Fatal(err)
					}
				}
			}
		}
	}
}

func TestSortChunksAgainstProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	var da rowenc.DatumAlloc
	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(context.Background())

	rng, seed := randutil.NewPseudoRand()
	nRuns := 5
	nRows := 5 * coldata.BatchSize() / 4
	maxCols := 3
	maxNum := 10
	intTyps := make([]*types.T, maxCols)
	for i := range intTyps {
		intTyps[i] = types.Int
	}

	for _, spillForced := range []bool{false, true} {
		for run := 0; run < nRuns; run++ {
			for nCols := 2; nCols <= maxCols; nCols++ {
				for matchLen := 1; matchLen < nCols; matchLen++ {
					var (
						rows       rowenc.EncDatumRows
						inputTypes []*types.T
					)
					if rng.Float64() < randTypesProbability {
						inputTypes = generateRandomSupportedTypes(rng, nCols)
						rows = rowenc.RandEncDatumRowsOfTypes(rng, nRows, inputTypes)
					} else {
						inputTypes = intTyps[:nCols]
						rows = rowenc.MakeRandIntRowsInRange(rng, nRows, nCols, maxNum, nullProbability)
					}

					// Note: we're only generating column orderings on all nCols columns since
					// if there are columns not in the ordering, the results are not fully
					// deterministic.
					orderingCols := generateColumnOrdering(rng, nCols, nCols)
					matchedCols := execinfrapb.ConvertToColumnOrdering(execinfrapb.Ordering{Columns: orderingCols[:matchLen]})
					// Presort the input on first matchLen columns.
					sort.Slice(rows, func(i, j int) bool {
						cmp, err := rows[i].Compare(inputTypes, &da, matchedCols, &evalCtx, rows[j])
						if err != nil {
							t.Fatal(err)
						}
						return cmp < 0
					})

					sorterSpec := &execinfrapb.SorterSpec{
						OutputOrdering:   execinfrapb.Ordering{Columns: orderingCols},
						OrderingMatchLen: uint32(matchLen),
					}
					pspec := &execinfrapb.ProcessorSpec{
						Input:       []execinfrapb.InputSyncSpec{{ColumnTypes: inputTypes}},
						Core:        execinfrapb.ProcessorCoreUnion{Sorter: sorterSpec},
						ResultTypes: inputTypes,
					}
					args := verifyColOperatorArgs{
						inputTypes:     [][]*types.T{inputTypes},
						inputs:         []rowenc.EncDatumRows{rows},
						pspec:          pspec,
						forceDiskSpill: spillForced,
					}
					if err := verifyColOperator(t, args); err != nil {
						fmt.Printf("--- seed = %d spillForced = %t orderingCols = %v matchLen = %d run = %d ---\n",
							seed, spillForced, orderingCols, matchLen, run)
						prettyPrintTypes(inputTypes, "t" /* tableName */)
						prettyPrintInput(rows, inputTypes, "t" /* tableName */)
						t.Fatal(err)
					}
				}
			}
		}
	}
}

func TestHashJoinerAgainstProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	evalCtx := tree.MakeTestingEvalContext(cluster.MakeTestingClusterSettings())
	defer evalCtx.Stop(context.Background())

	type hjTestSpec struct {
		joinType        descpb.JoinType
		onExprSupported bool
	}
	testSpecs := []hjTestSpec{
		{
			joinType:        descpb.InnerJoin,
			onExprSupported: true,
		},
		{
			joinType: descpb.LeftOuterJoin,
		},
		{
			joinType: descpb.RightOuterJoin,
		},
		{
			joinType: descpb.FullOuterJoin,
		},
		{
			joinType: descpb.LeftSemiJoin,
		},
		{
			joinType: descpb.LeftAntiJoin,
		},
		{
			joinType: descpb.IntersectAllJoin,
		},
		{
			joinType: descpb.ExceptAllJoin,
		},
		{
			joinType: descpb.RightSemiJoin,
		},
		{
			joinType: descpb.RightAntiJoin,
		},
	}

	rng, seed := randutil.NewPseudoRand()
	nRuns := 3
	nRows := 10
	maxCols := 3
	maxNum := 5
	intTyps := make([]*types.T, maxCols)
	for i := range intTyps {
		intTyps[i] = types.Int
	}

	for _, spillForced := range []bool{false, true} {
		for run := 0; run < nRuns; run++ {
			for _, testSpec := range testSpecs {
				for nCols := 1; nCols <= maxCols; nCols++ {
					for nEqCols := 1; nEqCols <= nCols; nEqCols++ {
						triedWithoutOnExpr, triedWithOnExpr := false, false
						if !testSpec.onExprSupported {
							triedWithOnExpr = true
						}
						for !triedWithoutOnExpr || !triedWithOnExpr {
							var (
								lRows, rRows             rowenc.EncDatumRows
								lEqCols, rEqCols         []uint32
								lInputTypes, rInputTypes []*types.T
								usingRandomTypes         bool
							)
							if rng.Float64() < randTypesProbability {
								lInputTypes = generateRandomSupportedTypes(rng, nCols)
								lEqCols = generateEqualityColumns(rng, nCols, nEqCols)
								rInputTypes = append(rInputTypes[:0], lInputTypes...)
								rEqCols = append(rEqCols[:0], lEqCols...)
								rng.Shuffle(nEqCols, func(i, j int) {
									iColIdx, jColIdx := rEqCols[i], rEqCols[j]
									rInputTypes[iColIdx], rInputTypes[jColIdx] = rInputTypes[jColIdx], rInputTypes[iColIdx]
									rEqCols[i], rEqCols[j] = rEqCols[j], rEqCols[i]
								})
								rInputTypes = randomizeJoinRightTypes(rng, rInputTypes)
								lRows = rowenc.RandEncDatumRowsOfTypes(rng, nRows, lInputTypes)
								rRows = rowenc.RandEncDatumRowsOfTypes(rng, nRows, rInputTypes)
								usingRandomTypes = true
							} else {
								lInputTypes = intTyps[:nCols]
								rInputTypes = lInputTypes
								lRows = rowenc.MakeRandIntRowsInRange(rng, nRows, nCols, maxNum, nullProbability)
								rRows = rowenc.MakeRandIntRowsInRange(rng, nRows, nCols, maxNum, nullProbability)
								lEqCols = generateEqualityColumns(rng, nCols, nEqCols)
								rEqCols = generateEqualityColumns(rng, nCols, nEqCols)
							}

							var outputTypes []*types.T
							if testSpec.joinType.ShouldIncludeLeftColsInOutput() {
								outputTypes = append(outputTypes, lInputTypes...)
							}
							if testSpec.joinType.ShouldIncludeRightColsInOutput() {
								outputTypes = append(outputTypes, rInputTypes...)
							}
							outputColumns := make([]uint32, len(outputTypes))
							for i := range outputColumns {
								outputColumns[i] = uint32(i)
							}

							var onExpr execinfrapb.Expression
							if triedWithoutOnExpr {
								colTypes := append(lInputTypes, rInputTypes...)
								onExpr = generateFilterExpr(
									rng, nCols, nEqCols, colTypes, usingRandomTypes, false, /* forceSingleSide */
								)
							}
							hjSpec := &execinfrapb.HashJoinerSpec{
								LeftEqColumns:  lEqCols,
								RightEqColumns: rEqCols,
								OnExpr:         onExpr,
								Type:           testSpec.joinType,
							}
							pspec := &execinfrapb.ProcessorSpec{
								Input: []execinfrapb.InputSyncSpec{
									{ColumnTypes: lInputTypes},
									{ColumnTypes: rInputTypes},
								},
								Core: execinfrapb.ProcessorCoreUnion{HashJoiner: hjSpec},
								Post: execinfrapb.PostProcessSpec{
									Projection:    true,
									OutputColumns: outputColumns,
								},
								ResultTypes: outputTypes,
							}
							args := verifyColOperatorArgs{
								anyOrder:       true,
								inputTypes:     [][]*types.T{lInputTypes, rInputTypes},
								inputs:         []rowenc.EncDatumRows{lRows, rRows},
								pspec:          pspec,
								forceDiskSpill: spillForced,
								// It is possible that we have a filter that is always false, and this
								// will allow us to plan a zero operator which always returns a zero
								// batch. In such case, the spilling might not occur and that's ok.
								forcedDiskSpillMightNotOccur: !onExpr.Empty(),
								numForcedRepartitions:        2,
								rng:                          rng,
							}
							if testSpec.joinType.IsSetOpJoin() && nEqCols < nCols {
								// The output of set operation joins is not fully
								// deterministic when there are non-equality
								// columns, however, the rows must match on the
								// equality columns between vectorized and row
								// executions.
								args.colIdxsToCheckForEquality = make([]int, nEqCols)
								for i := range args.colIdxsToCheckForEquality {
									args.colIdxsToCheckForEquality[i] = int(lEqCols[i])
								}
							}

							if err := verifyColOperator(t, args); err != nil {
								fmt.Printf("--- spillForced = %t join type = %s onExpr = %q"+
									" q seed = %d run = %d ---\n",
									spillForced, testSpec.joinType.String(), onExpr.Expr, seed, run)
								fmt.Printf("--- lEqCols = %v rEqCols = %v ---\n", lEqCols, rEqCols)
								prettyPrintTypes(lInputTypes, "left_table" /* tableName */)
								prettyPrintTypes(rInputTypes, "right_table" /* tableName */)
								prettyPrintInput(lRows, lInputTypes, "left_table" /* tableName */)
								prettyPrintInput(rRows, rInputTypes, "right_table" /* tableName */)
								t.Fatal(err)
							}
							if onExpr.Expr == "" {
								triedWithoutOnExpr = true
							} else {
								triedWithOnExpr = true
							}
						}
					}
				}
			}
		}
	}
}

// generateEqualityColumns produces a random permutation of nEqCols random
// columns on a table with nCols columns, so nEqCols must be not greater than
// nCols.
func generateEqualityColumns(rng *rand.Rand, nCols int, nEqCols int) []uint32 {
	if nEqCols > nCols {
		panic("nEqCols > nCols in generateEqualityColumns")
	}
	eqCols := make([]uint32, 0, nEqCols)
	for _, eqCol := range rng.Perm(nCols)[:nEqCols] {
		eqCols = append(eqCols, uint32(eqCol))
	}
	return eqCols
}

func TestMergeJoinerAgainstProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	var da rowenc.DatumAlloc
	evalCtx := tree.MakeTestingEvalContext(cluster.MakeTestingClusterSettings())
	defer evalCtx.Stop(context.Background())

	type mjTestSpec struct {
		joinType        descpb.JoinType
		anyOrder        bool
		onExprSupported bool
	}
	testSpecs := []mjTestSpec{
		{
			joinType:        descpb.InnerJoin,
			onExprSupported: true,
		},
		{
			joinType: descpb.LeftOuterJoin,
		},
		{
			joinType: descpb.RightOuterJoin,
		},
		{
			joinType: descpb.FullOuterJoin,
			// FULL OUTER JOIN doesn't guarantee any ordering on its output (since it
			// is ambiguous), so we're comparing the outputs as sets.
			anyOrder: true,
		},
		{
			joinType: descpb.LeftSemiJoin,
		},
		{
			joinType: descpb.LeftAntiJoin,
		},
		{
			joinType: descpb.IntersectAllJoin,
		},
		{
			joinType: descpb.ExceptAllJoin,
		},
		{
			joinType: descpb.RightSemiJoin,
		},
		{
			joinType: descpb.RightAntiJoin,
		},
	}

	rng, seed := randutil.NewPseudoRand()
	nRuns := 3
	nRows := 10
	maxCols := 3
	maxNum := 5
	intTyps := make([]*types.T, maxCols)
	for i := range intTyps {
		intTyps[i] = types.Int
	}

	for run := 0; run < nRuns; run++ {
		for _, testSpec := range testSpecs {
			for nCols := 1; nCols <= maxCols; nCols++ {
				for nOrderingCols := 1; nOrderingCols <= nCols; nOrderingCols++ {
					triedWithoutOnExpr, triedWithOnExpr := false, false
					if !testSpec.onExprSupported {
						triedWithOnExpr = true
					}
					for !triedWithoutOnExpr || !triedWithOnExpr {
						var (
							lRows, rRows                 rowenc.EncDatumRows
							lInputTypes, rInputTypes     []*types.T
							lOrderingCols, rOrderingCols []execinfrapb.Ordering_Column
							usingRandomTypes             bool
						)
						if rng.Float64() < randTypesProbability {
							lInputTypes = generateRandomSupportedTypes(rng, nCols)
							lOrderingCols = generateColumnOrdering(rng, nCols, nOrderingCols)
							rInputTypes = append(rInputTypes[:0], lInputTypes...)
							rOrderingCols = append(rOrderingCols[:0], lOrderingCols...)
							rng.Shuffle(nOrderingCols, func(i, j int) {
								iColIdx, jColIdx := rOrderingCols[i].ColIdx, rOrderingCols[j].ColIdx
								rInputTypes[iColIdx], rInputTypes[jColIdx] = rInputTypes[jColIdx], rInputTypes[iColIdx]
								rOrderingCols[i], rOrderingCols[j] = rOrderingCols[j], rOrderingCols[i]
							})
							rInputTypes = randomizeJoinRightTypes(rng, rInputTypes)
							lRows = rowenc.RandEncDatumRowsOfTypes(rng, nRows, lInputTypes)
							rRows = rowenc.RandEncDatumRowsOfTypes(rng, nRows, rInputTypes)
							usingRandomTypes = true
						} else {
							lInputTypes = intTyps[:nCols]
							rInputTypes = lInputTypes
							lRows = rowenc.MakeRandIntRowsInRange(rng, nRows, nCols, maxNum, nullProbability)
							rRows = rowenc.MakeRandIntRowsInRange(rng, nRows, nCols, maxNum, nullProbability)
							lOrderingCols = generateColumnOrdering(rng, nCols, nOrderingCols)
							rOrderingCols = generateColumnOrdering(rng, nCols, nOrderingCols)
						}
						// Set the directions of both columns to be the same.
						for i, lCol := range lOrderingCols {
							rOrderingCols[i].Direction = lCol.Direction
						}

						lMatchedCols := execinfrapb.ConvertToColumnOrdering(execinfrapb.Ordering{Columns: lOrderingCols})
						rMatchedCols := execinfrapb.ConvertToColumnOrdering(execinfrapb.Ordering{Columns: rOrderingCols})
						sort.Slice(lRows, func(i, j int) bool {
							cmp, err := lRows[i].Compare(lInputTypes, &da, lMatchedCols, &evalCtx, lRows[j])
							if err != nil {
								t.Fatal(err)
							}
							return cmp < 0
						})
						sort.Slice(rRows, func(i, j int) bool {
							cmp, err := rRows[i].Compare(rInputTypes, &da, rMatchedCols, &evalCtx, rRows[j])
							if err != nil {
								t.Fatal(err)
							}
							return cmp < 0
						})
						var outputTypes []*types.T
						if testSpec.joinType.ShouldIncludeLeftColsInOutput() {
							outputTypes = append(outputTypes, lInputTypes...)
						}
						if testSpec.joinType.ShouldIncludeRightColsInOutput() {
							outputTypes = append(outputTypes, rInputTypes...)
						}
						outputColumns := make([]uint32, len(outputTypes))
						for i := range outputColumns {
							outputColumns[i] = uint32(i)
						}

						var onExpr execinfrapb.Expression
						if triedWithoutOnExpr {
							colTypes := append(lInputTypes, rInputTypes...)
							onExpr = generateFilterExpr(
								rng, nCols, nOrderingCols, colTypes, usingRandomTypes, false, /* forceSingleSide */
							)
						}
						mjSpec := &execinfrapb.MergeJoinerSpec{
							OnExpr:        onExpr,
							LeftOrdering:  execinfrapb.Ordering{Columns: lOrderingCols},
							RightOrdering: execinfrapb.Ordering{Columns: rOrderingCols},
							Type:          testSpec.joinType,
							NullEquality:  testSpec.joinType.IsSetOpJoin(),
						}
						pspec := &execinfrapb.ProcessorSpec{
							Input:       []execinfrapb.InputSyncSpec{{ColumnTypes: lInputTypes}, {ColumnTypes: rInputTypes}},
							Core:        execinfrapb.ProcessorCoreUnion{MergeJoiner: mjSpec},
							Post:        execinfrapb.PostProcessSpec{Projection: true, OutputColumns: outputColumns},
							ResultTypes: outputTypes,
						}
						args := verifyColOperatorArgs{
							anyOrder:   testSpec.anyOrder,
							inputTypes: [][]*types.T{lInputTypes, rInputTypes},
							inputs:     []rowenc.EncDatumRows{lRows, rRows},
							pspec:      pspec,
							rng:        rng,
						}
						if testSpec.joinType.IsSetOpJoin() && nOrderingCols < nCols {
							// The output of set operation joins is not fully
							// deterministic when there are non-equality
							// columns, however, the rows must match on the
							// equality columns between vectorized and row
							// executions.
							args.colIdxsToCheckForEquality = make([]int, nOrderingCols)
							for i := range args.colIdxsToCheckForEquality {
								args.colIdxsToCheckForEquality[i] = int(lOrderingCols[i].ColIdx)
							}
						}
						if err := verifyColOperator(t, args); err != nil {
							fmt.Printf("--- join type = %s onExpr = %q seed = %d run = %d ---\n",
								testSpec.joinType.String(), onExpr.Expr, seed, run)
							fmt.Printf("--- left ordering = %v right ordering = %v ---\n", lOrderingCols, rOrderingCols)
							prettyPrintTypes(lInputTypes, "left_table" /* tableName */)
							prettyPrintTypes(rInputTypes, "right_table" /* tableName */)
							prettyPrintInput(lRows, lInputTypes, "left_table" /* tableName */)
							prettyPrintInput(rRows, rInputTypes, "right_table" /* tableName */)
							t.Fatal(err)
						}
						if onExpr.Expr == "" {
							triedWithoutOnExpr = true
						} else {
							triedWithOnExpr = true
						}
					}
				}
			}
		}
	}
}

// generateColumnOrdering produces a random ordering of nOrderingCols columns
// on a table with nCols columns, so nOrderingCols must be not greater than
// nCols.
func generateColumnOrdering(
	rng *rand.Rand, nCols int, nOrderingCols int,
) []execinfrapb.Ordering_Column {
	if nOrderingCols > nCols {
		panic("nOrderingCols > nCols in generateColumnOrdering")
	}

	orderingCols := make([]execinfrapb.Ordering_Column, nOrderingCols)
	for i, col := range rng.Perm(nCols)[:nOrderingCols] {
		orderingCols[i] = execinfrapb.Ordering_Column{
			ColIdx:    uint32(col),
			Direction: execinfrapb.Ordering_Column_Direction(rng.Intn(2)),
		}
	}
	return orderingCols
}

// generateFilterExpr populates an execinfrapb.Expression that contains a
// single comparison which can be either comparing a column from the left
// against a column from the right or comparing a column from either side
// against a constant.
// If forceConstComparison is true, then the comparison against the constant
// will be used.
// If forceSingleSide is true, then the comparison of a column from the single
// side against a constant will be used ("single" meaning that the join type
// doesn't output columns from both sides).
func generateFilterExpr(
	rng *rand.Rand,
	nCols int,
	nEqCols int,
	colTypes []*types.T,
	forceConstComparison bool,
	forceSingleSide bool,
) execinfrapb.Expression {
	var comparison string
	r := rng.Float64()
	if r < 0.25 {
		comparison = "<"
	} else if r < 0.5 {
		comparison = ">"
	} else if r < 0.75 {
		comparison = "="
	} else {
		comparison = "<>"
	}
	// When all columns are used in equality comparison between inputs, there is
	// only one interesting case when a column from either side is compared
	// against a constant. The second conditional is us choosing to compare
	// against a constant.
	if nCols == nEqCols || rng.Float64() < 0.33 || forceConstComparison || forceSingleSide {
		colIdx := rng.Intn(nCols)
		if !forceSingleSide && rng.Float64() >= 0.5 {
			// Use right side.
			colIdx += nCols
		}
		constDatum := rowenc.RandDatum(rng, colTypes[colIdx], true /* nullOk */)
		constDatumString := constDatum.String()
		switch colTypes[colIdx].Family() {
		case types.FloatFamily, types.DecimalFamily:
			if strings.Contains(strings.ToLower(constDatumString), "nan") ||
				strings.Contains(strings.ToLower(constDatumString), "inf") {
				// We need to surround special numerical values with quotes.
				constDatumString = fmt.Sprintf("'%s'", constDatumString)
			}
		}
		return execinfrapb.Expression{Expr: fmt.Sprintf("@%d %s %s", colIdx+1, comparison, constDatumString)}
	}
	// We will compare a column from the left against a column from the right.
	leftColIdx := rng.Intn(nCols) + 1
	rightColIdx := rng.Intn(nCols) + nCols + 1
	return execinfrapb.Expression{Expr: fmt.Sprintf("@%d %s @%d", leftColIdx, comparison, rightColIdx)}
}

func TestWindowFunctionsAgainstProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()

	rng, seed := randutil.NewPseudoRand()
	nRows := 2 * coldata.BatchSize()
	maxCols := 4
	maxNum := 10
	typs := make([]*types.T, maxCols)
	for i := range typs {
		// TODO(yuzefovich): randomize the types of the columns once we support
		// window functions that take in arguments.
		typs[i] = types.Int
	}
	for windowFn := range colbuilder.SupportedWindowFns {
		for _, partitionBy := range [][]uint32{
			{},     // No PARTITION BY clause.
			{0},    // Partitioning on the first input column.
			{0, 1}, // Partitioning on the first and second input columns.
		} {
			for _, nOrderingCols := range []int{
				0, // No ORDER BY clause.
				1, // ORDER BY on at most one column.
				2, // ORDER BY on at most two columns.
			} {
				for nCols := 1; nCols <= maxCols; nCols++ {
					if len(partitionBy) > nCols || nOrderingCols > nCols {
						continue
					}
					inputTypes := typs[:nCols:nCols]
					rows := rowenc.MakeRandIntRowsInRange(rng, nRows, nCols, maxNum, nullProbability)

					windowerSpec := &execinfrapb.WindowerSpec{
						PartitionBy: partitionBy,
						WindowFns: []execinfrapb.WindowerSpec_WindowFn{
							{
								Func:         execinfrapb.WindowerSpec_Func{WindowFunc: &windowFn},
								Ordering:     generateOrderingGivenPartitionBy(rng, nCols, nOrderingCols, partitionBy),
								OutputColIdx: uint32(nCols),
								FilterColIdx: tree.NoColumnIdx,
							},
						},
					}
					if windowFn == execinfrapb.WindowerSpec_ROW_NUMBER &&
						len(partitionBy)+len(windowerSpec.WindowFns[0].Ordering.Columns) < nCols {
						// The output of row_number is not deterministic if there are
						// columns that are not present in either PARTITION BY or ORDER BY
						// clauses, so we skip such a configuration.
						continue
					}

					// Currently, we only support window functions that take no
					// arguments, so we leave the second argument empty.
					_, outputType, err := execinfrapb.GetWindowFunctionInfo(execinfrapb.WindowerSpec_Func{WindowFunc: &windowFn})
					require.NoError(t, err)
					pspec := &execinfrapb.ProcessorSpec{
						Input:       []execinfrapb.InputSyncSpec{{ColumnTypes: inputTypes}},
						Core:        execinfrapb.ProcessorCoreUnion{Windower: windowerSpec},
						ResultTypes: append(inputTypes, outputType),
					}
					args := verifyColOperatorArgs{
						anyOrder:   true,
						inputTypes: [][]*types.T{inputTypes},
						inputs:     []rowenc.EncDatumRows{rows},
						pspec:      pspec,
					}
					if err := verifyColOperator(t, args); err != nil {
						fmt.Printf("seed = %d\n", seed)
						prettyPrintTypes(inputTypes, "t" /* tableName */)
						prettyPrintInput(rows, inputTypes, "t" /* tableName */)
						t.Fatal(err)
					}
				}
			}
		}
	}
}

// generateRandomSupportedTypes generates nCols random types that are supported
// by the vectorized engine.
func generateRandomSupportedTypes(rng *rand.Rand, nCols int) []*types.T {
	typs := make([]*types.T, 0, nCols)
	for len(typs) < nCols {
		typ := rowenc.RandType(rng)
		if typeconv.TypeFamilyToCanonicalTypeFamily(typ.Family()) == typeconv.DatumVecCanonicalTypeFamily {
			// At the moment, we disallow datum-backed types.
			// TODO(yuzefovich): remove this.
			continue
		}
		typs = append(typs, typ)
	}
	return typs
}

// randomizeJoinRightTypes returns somewhat random types to be used for the
// right side of the join such that they would have produced equality
// conditions in the non-test environment (currently, due to #43060, we don't
// support joins of different types without pushing the mixed-type equality
// checks into the ON condition).
func randomizeJoinRightTypes(rng *rand.Rand, leftTypes []*types.T) []*types.T {
	typs := make([]*types.T, len(leftTypes))
	for i, inputType := range leftTypes {
		switch inputType.Family() {
		case types.IntFamily:
			// We want to randomize integer types because they have different
			// physical representations.
			switch rng.Intn(3) {
			case 0:
				typs[i] = types.Int2
			case 1:
				typs[i] = types.Int4
			default:
				typs[i] = types.Int
			}
		default:
			typs[i] = inputType
		}
	}
	return typs
}

// generateOrderingGivenPartitionBy produces a random ordering of up to
// nOrderingCols columns on a table with nCols columns such that only columns
// not present in partitionBy are used. This is useful to simulate how
// optimizer plans window functions - for example, with an OVER clause as
// (PARTITION BY a ORDER BY a DESC), the optimizer will omit the ORDER BY
// clause entirely.
func generateOrderingGivenPartitionBy(
	rng *rand.Rand, nCols int, nOrderingCols int, partitionBy []uint32,
) execinfrapb.Ordering {
	var ordering execinfrapb.Ordering
	if nOrderingCols == 0 || len(partitionBy) == nCols {
		return ordering
	}
	ordering = execinfrapb.Ordering{Columns: make([]execinfrapb.Ordering_Column, 0, nOrderingCols)}
	for len(ordering.Columns) == 0 {
		for _, ordCol := range generateColumnOrdering(rng, nCols, nOrderingCols) {
			usedInPartitionBy := false
			for _, p := range partitionBy {
				if p == ordCol.ColIdx {
					usedInPartitionBy = true
					break
				}
			}
			if !usedInPartitionBy {
				ordering.Columns = append(ordering.Columns, ordCol)
			}
		}
	}
	return ordering
}

// prettyPrintTypes prints out typs as a CREATE TABLE statement.
func prettyPrintTypes(typs []*types.T, tableName string) {
	fmt.Printf("CREATE TABLE %s(", tableName)
	colName := byte('a')
	for typIdx, typ := range typs {
		if typIdx < len(typs)-1 {
			fmt.Printf("%c %s, ", colName, typ.SQLStandardName())
		} else {
			fmt.Printf("%c %s);\n", colName, typ.SQLStandardName())
		}
		colName++
	}
}

// prettyPrintInput prints out rows as INSERT INTO tableName VALUES statement.
func prettyPrintInput(rows rowenc.EncDatumRows, inputTypes []*types.T, tableName string) {
	fmt.Printf("INSERT INTO %s VALUES\n", tableName)
	for rowIdx, row := range rows {
		fmt.Printf("(%s", row[0].String(inputTypes[0]))
		for i := range row[1:] {
			fmt.Printf(", %s", row[i+1].String(inputTypes[i+1]))
		}
		if rowIdx < len(rows)-1 {
			fmt.Printf("),\n")
		} else {
			fmt.Printf(");\n")
		}
	}
}
