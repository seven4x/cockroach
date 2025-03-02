// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/featureflag"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/colinfo"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/schemaexpr"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/rowcontainer"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqltelemetry"
	"github.com/cockroachdb/cockroach/pkg/sql/stats"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/grpcutil"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
)

// createStatsPostEvents controls the cluster setting for logging
// automatic table statistics collection to the event log.
var createStatsPostEvents = settings.RegisterPublicBoolSetting(
	"sql.stats.post_events.enabled",
	"if set, an event is logged for every CREATE STATISTICS job",
	false,
)

// featureStatsEnabled is used to enable and disable the CREATE STATISTICS and
// ANALYZE features.
var featureStatsEnabled = settings.RegisterPublicBoolSetting(
	"feature.stats.enabled",
	"set to true to enable CREATE STATISTICS/ANALYZE, false to disable; default is true",
	featureflag.FeatureFlagEnabledDefault)

const defaultHistogramBuckets = 200
const nonIndexColHistogramBuckets = 2

// createStatsNode is a planNode implemented in terms of a function. The
// startJob function starts a Job during Start, and the remainder of the
// CREATE STATISTICS planning and execution is performed within the jobs
// framework.
type createStatsNode struct {
	tree.CreateStats
	p *planner

	// runAsJob is true by default, and causes the code below to be executed,
	// which sets up a job and waits for it.
	//
	// If it is false, the flow for create statistics is planned directly; this
	// is used when the statement is under EXPLAIN or EXPLAIN ANALYZE.
	runAsJob bool

	run createStatsRun
}

// createStatsRun contains the run-time state of createStatsNode during local
// execution.
type createStatsRun struct {
	resultsCh chan tree.Datums
	errCh     chan error
}

func (n *createStatsNode) startExec(params runParams) error {
	telemetry.Inc(sqltelemetry.SchemaChangeCreateCounter("stats"))
	n.run.resultsCh = make(chan tree.Datums)
	n.run.errCh = make(chan error)
	go func() {
		err := n.startJob(params.ctx, n.run.resultsCh)
		select {
		case <-params.ctx.Done():
		case n.run.errCh <- err:
		}
		close(n.run.errCh)
		close(n.run.resultsCh)
	}()
	return nil
}

func (n *createStatsNode) Next(params runParams) (bool, error) {
	select {
	case <-params.ctx.Done():
		return false, params.ctx.Err()
	case err := <-n.run.errCh:
		return false, err
	case <-n.run.resultsCh:
		return true, nil
	}
}

func (*createStatsNode) Close(context.Context) {}
func (*createStatsNode) Values() tree.Datums   { return nil }

// startJob starts a CreateStats job to plan and execute statistics creation.
func (n *createStatsNode) startJob(ctx context.Context, resultsCh chan<- tree.Datums) error {
	record, err := n.makeJobRecord(ctx)
	if err != nil {
		return err
	}

	if n.Name == stats.AutoStatsName {
		// Don't start the job if there is already a CREATE STATISTICS job running.
		// (To handle race conditions we check this again after the job starts,
		// but this check is used to prevent creating a large number of jobs that
		// immediately fail).
		if err := checkRunningJobs(ctx, nil /* job */, n.p); err != nil {
			return err
		}
	} else {
		telemetry.Inc(sqltelemetry.CreateStatisticsUseCounter)
	}

	job, errCh, err := n.p.ExecCfg().JobRegistry.CreateAndStartJob(ctx, resultsCh, *record)
	if err != nil {
		return err
	}

	if err = <-errCh; err != nil {
		if errors.Is(err, stats.ConcurrentCreateStatsError) {
			// Delete the job so users don't see it and get confused by the error.
			const stmt = `DELETE FROM system.jobs WHERE id = $1`
			if _ /* cols */, delErr := n.p.ExecCfg().InternalExecutor.Exec(
				ctx, "delete-job", nil /* txn */, stmt, *job.ID(),
			); delErr != nil {
				log.Warningf(ctx, "failed to delete job: %v", delErr)
			}
		}
	}
	return err
}

// makeJobRecord creates a CreateStats job record which can be used to plan and
// execute statistics creation.
func (n *createStatsNode) makeJobRecord(ctx context.Context) (*jobs.Record, error) {
	var tableDesc *tabledesc.Immutable
	var fqTableName string
	var err error
	switch t := n.Table.(type) {
	case *tree.UnresolvedObjectName:
		tableDesc, err = n.p.ResolveExistingObjectEx(ctx, t, true /*required*/, tree.ResolveRequireTableDesc)
		if err != nil {
			return nil, err
		}
		fqTableName = n.p.ResolvedName(t).FQString()

	case *tree.TableRef:
		flags := tree.ObjectLookupFlags{CommonLookupFlags: tree.CommonLookupFlags{
			AvoidCached: n.p.avoidCachedDescriptors,
		}}
		tableDesc, err = n.p.Descriptors().GetTableVersionByID(ctx, n.p.txn, descpb.ID(t.TableID), flags)
		if err != nil {
			return nil, err
		}
		fqName, err := n.p.getQualifiedTableName(ctx, tableDesc)
		if err != nil {
			return nil, err
		}
		fqTableName = fqName.FQString()
	}

	if tableDesc.IsVirtualTable() {
		return nil, pgerror.New(
			pgcode.WrongObjectType, "cannot create statistics on virtual tables",
		)
	}

	if tableDesc.IsView() {
		return nil, pgerror.New(
			pgcode.WrongObjectType, "cannot create statistics on views",
		)
	}

	if err := n.p.CheckPrivilege(ctx, tableDesc, privilege.SELECT); err != nil {
		return nil, err
	}

	// Identify which columns we should create statistics for.
	var colStats []jobspb.CreateStatsDetails_ColStat
	if len(n.ColumnNames) == 0 {
		multiColEnabled := stats.MultiColumnStatisticsClusterMode.Get(&n.p.ExecCfg().Settings.SV)
		if colStats, err = createStatsDefaultColumns(tableDesc, multiColEnabled); err != nil {
			return nil, err
		}
	} else {
		columns, err := tableDesc.FindActiveColumnsByNames(n.ColumnNames)
		if err != nil {
			return nil, err
		}

		columnIDs := make([]descpb.ColumnID, len(columns))
		for i := range columns {
			columnIDs[i] = columns[i].ID
		}
		col, err := tableDesc.FindColumnByID(columnIDs[0])
		if err != nil {
			return nil, err
		}
		isInvIndex := colinfo.ColumnTypeIsInvertedIndexable(col.Type)
		colStats = []jobspb.CreateStatsDetails_ColStat{{
			ColumnIDs: columnIDs,
			// By default, create histograms on all explicitly requested column stats
			// with a single column that doesn't use an inverted index.
			HasHistogram:        len(columnIDs) == 1 && !isInvIndex,
			HistogramMaxBuckets: defaultHistogramBuckets,
		}}
		// Make histograms for inverted index column types.
		if len(columnIDs) == 1 && isInvIndex {
			colStats = append(colStats, jobspb.CreateStatsDetails_ColStat{
				ColumnIDs:           columnIDs,
				HasHistogram:        true,
				Inverted:            true,
				HistogramMaxBuckets: defaultHistogramBuckets,
			})
		}
	}

	// Evaluate the AS OF time, if any.
	var asOf *hlc.Timestamp
	if n.Options.AsOf.Expr != nil {
		asOfTs, err := n.p.EvalAsOfTimestamp(ctx, n.Options.AsOf)
		if err != nil {
			return nil, err
		}
		asOf = &asOfTs
	}

	// Create a job to run statistics creation.
	statement := tree.AsStringWithFQNames(n, n.p.EvalContext().Annotations)
	var description string
	if n.Name == stats.AutoStatsName {
		// Use a user-friendly description for automatic statistics.
		description = fmt.Sprintf("Table statistics refresh for %s", fqTableName)
	} else {
		// This must be a user query, so use the statement (for consistency with
		// other jobs triggered by statements).
		description = statement
		statement = ""
	}
	return &jobs.Record{
		Description: description,
		Statement:   statement,
		Username:    n.p.User(),
		Details: jobspb.CreateStatsDetails{
			Name:            string(n.Name),
			FQTableName:     fqTableName,
			Table:           tableDesc.TableDescriptor,
			ColumnStats:     colStats,
			Statement:       n.String(),
			AsOf:            asOf,
			MaxFractionIdle: n.Options.Throttling,
		},
		Progress: jobspb.CreateStatsProgress{},
	}, nil
}

// maxNonIndexCols is the maximum number of non-index columns that we will use
// when choosing a default set of column statistics.
const maxNonIndexCols = 100

// createStatsDefaultColumns creates column statistics on a default set of
// column lists when no columns were specified by the caller.
//
// To determine a useful set of default column statistics, we rely on
// information provided by the schema. In particular, the presence of an index
// on a particular set of columns indicates that the workload likely contains
// queries that involve those columns (e.g., for filters), and it would be
// useful to have statistics on prefixes of those columns. For example, if a
// table abc contains indexes on (a ASC, b ASC) and (b ASC, c ASC), we will
// collect statistics on a, {a, b}, b, and {b, c}. (But if multiColEnabled is
// false, we will only collect stats on a and b). Columns in partial index
// predicate expressions are also likely to appear in query filters, so stats
// are collected for those columns as well.
//
// In addition to the index columns, we collect stats on up to maxNonIndexCols
// other columns from the table. We only collect histograms for index columns,
// plus any other boolean or enum columns (where the "histogram" is tiny).
func createStatsDefaultColumns(
	desc *tabledesc.Immutable, multiColEnabled bool,
) ([]jobspb.CreateStatsDetails_ColStat, error) {
	colStats := make([]jobspb.CreateStatsDetails_ColStat, 0, len(desc.Indexes)+1)

	requestedStats := make(map[string]struct{})

	// trackStatsIfNotExists adds the given column IDs as a set to the
	// requestedStats set. If the columnIDs were not already in the set, it
	// returns true.
	trackStatsIfNotExists := func(colIDs []descpb.ColumnID) bool {
		key := makeColStatKey(colIDs)
		if _, ok := requestedStats[key]; ok {
			return false
		}
		requestedStats[key] = struct{}{}
		return true
	}

	// addIndexColumnStatsIfNotExists appends column stats for the given column
	// ID if they have not already been added. Histogram stats are collected for
	// every indexed column.
	addIndexColumnStatsIfNotExists := func(colID descpb.ColumnID, isInverted bool) {
		colList := []descpb.ColumnID{colID}

		// Check for existing stats and remember the requested stats.
		if !trackStatsIfNotExists(colList) {
			return
		}

		colStat := jobspb.CreateStatsDetails_ColStat{
			ColumnIDs:           colList,
			HasHistogram:        !isInverted,
			HistogramMaxBuckets: defaultHistogramBuckets,
		}
		colStats = append(colStats, colStat)

		// Generate histograms for inverted indexes. The above
		// colStat append is still needed for a basic sketch of
		// the column. The following colStat is needed for the
		// sampling and sketch of the inverted index keys of
		// the column.
		if isInverted {
			colStat.Inverted = true
			colStat.HasHistogram = true
			colStats = append(colStats, colStat)
		}
	}

	// Add column stats for the primary key.
	for i := range desc.PrimaryIndex.ColumnIDs {
		// Generate stats for each column in the primary key.
		addIndexColumnStatsIfNotExists(desc.PrimaryIndex.ColumnIDs[i], false /* isInverted */)

		// Only collect multi-column stats if enabled.
		if i == 0 || !multiColEnabled {
			continue
		}

		colIDs := desc.PrimaryIndex.ColumnIDs[: i+1 : i+1]

		// Remember the requested stats so we don't request duplicates.
		trackStatsIfNotExists(colIDs)

		// Only generate non-histogram multi-column stats.
		colStats = append(colStats, jobspb.CreateStatsDetails_ColStat{
			ColumnIDs:    colIDs,
			HasHistogram: false,
		})
	}

	// Add column stats for each secondary index.
	for i := range desc.Indexes {
		isInverted := desc.Indexes[i].Type == descpb.IndexDescriptor_INVERTED

		for j := range desc.Indexes[i].ColumnIDs {
			// Generate stats for each indexed column.
			addIndexColumnStatsIfNotExists(desc.Indexes[i].ColumnIDs[j], isInverted)

			// Only collect multi-column stats if enabled.
			if j == 0 || !multiColEnabled {
				continue
			}

			colIDs := desc.Indexes[i].ColumnIDs[: j+1 : j+1]

			// Check for existing stats and remember the requested stats.
			if !trackStatsIfNotExists(colIDs) {
				continue
			}

			// Only generate non-histogram multi-column stats.
			colStats = append(colStats, jobspb.CreateStatsDetails_ColStat{
				ColumnIDs:    colIDs,
				HasHistogram: false,
			})
		}

		// Add columns referenced in partial index predicate expressions.
		if desc.Indexes[i].IsPartial() {
			expr, err := parser.ParseExpr(desc.Indexes[i].Predicate)
			if err != nil {
				return nil, err
			}

			// Extract the IDs of columns referenced in the predicate.
			colIDs, err := schemaexpr.ExtractColumnIDs(desc, expr)
			if err != nil {
				return nil, err
			}

			// Generate stats for each column individually.
			for _, colID := range colIDs.Ordered() {
				addIndexColumnStatsIfNotExists(colID, isInverted)
			}
		}
	}

	// Add all remaining columns in the table, up to maxNonIndexCols.
	nonIdxCols := 0
	for i := 0; i < len(desc.Columns) && nonIdxCols < maxNonIndexCols; i++ {
		col := &desc.Columns[i]
		colList := []descpb.ColumnID{col.ID}

		if !trackStatsIfNotExists(colList) {
			continue
		}

		// Non-index columns have very small histograms since it's not worth the
		// overhead of storing large histograms for these columns. Since bool and
		// enum types only have a few values anyway, include all possible values
		// for those types, up to defaultHistogramBuckets.
		maxHistBuckets := uint32(nonIndexColHistogramBuckets)
		if col.Type.Family() == types.BoolFamily || col.Type.Family() == types.EnumFamily {
			maxHistBuckets = defaultHistogramBuckets
		}
		colStats = append(colStats, jobspb.CreateStatsDetails_ColStat{
			ColumnIDs:           colList,
			HasHistogram:        !colinfo.ColumnTypeIsInvertedIndexable(col.Type),
			HistogramMaxBuckets: maxHistBuckets,
		})
		nonIdxCols++
	}

	return colStats, nil
}

// makeColStatKey constructs a unique key representing cols that can be used
// as the key in a map.
func makeColStatKey(cols []descpb.ColumnID) string {
	var colSet util.FastIntSet
	for _, c := range cols {
		colSet.Add(int(c))
	}
	return colSet.String()
}

// createStatsResumer implements the jobs.Resumer interface for CreateStats
// jobs. A new instance is created for each job.
type createStatsResumer struct {
	job     *jobs.Job
	tableID descpb.ID
}

var _ jobs.Resumer = &createStatsResumer{}

// Resume is part of the jobs.Resumer interface.
func (r *createStatsResumer) Resume(
	ctx context.Context, execCtx interface{}, resultsCh chan<- tree.Datums,
) error {
	p := execCtx.(JobExecContext)
	details := r.job.Details().(jobspb.CreateStatsDetails)
	if details.Name == stats.AutoStatsName {
		// We want to make sure there is only one automatic CREATE STATISTICS job
		// running at a time.
		if err := checkRunningJobs(ctx, r.job, p); err != nil {
			return err
		}
	}

	r.tableID = details.Table.ID
	evalCtx := p.ExtendedEvalContext()

	ci := colinfo.ColTypeInfoFromColTypes([]*types.T{})
	rows := rowcontainer.NewRowContainer(evalCtx.Mon.MakeBoundAccount(), ci)
	defer func() {
		if rows != nil {
			rows.Close(ctx)
		}
	}()

	dsp := p.DistSQLPlanner()
	if err := p.ExecCfg().DB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		// Set the transaction on the EvalContext to this txn. This allows for
		// use of the txn during processor setup during the execution of the flow.
		evalCtx.Txn = txn

		if details.AsOf != nil {
			p.SemaCtx().AsOfTimestamp = details.AsOf
			p.ExtendedEvalContext().SetTxnTimestamp(details.AsOf.GoTime())
			txn.SetFixedTimestamp(ctx, *details.AsOf)
		}

		planCtx := dsp.NewPlanningCtx(ctx, evalCtx, nil /* planner */, txn, true /* distribute */)
		if err := dsp.planAndRunCreateStats(
			ctx, evalCtx, planCtx, txn, r.job, NewRowResultWriter(rows),
		); err != nil {
			// Check if this was a context canceled error and restart if it was.
			if grpcutil.IsContextCanceled(err) {
				return jobs.NewRetryJobError("node failure")
			}

			// If the job was canceled, any of the distsql processors could have been
			// the first to encounter the .Progress error. This error's string is sent
			// through distsql back here, so we can't examine the err type in this case
			// to see if it's a jobs.InvalidStatusError. Instead, attempt to update the
			// job progress to coerce out the correct error type. If the update succeeds
			// then return the original error, otherwise return this error instead so
			// it can be cleaned up at a higher level.
			if jobErr := r.job.FractionProgressed(
				ctx,
				func(ctx context.Context, _ jobspb.ProgressDetails) float32 {
					// The job failed so the progress value here doesn't really matter.
					return 0
				},
			); jobErr != nil {
				return jobErr
			}
			return err
		}

		return nil
	}); err != nil {
		return err
	}

	// Refresh the local cache if Gossip is not available.
	if _, ok := evalCtx.ExecCfg.Gossip.Optional(47925); !ok {
		evalCtx.ExecCfg.TableStatsCache.RefreshTableStats(ctx, r.tableID)
	}

	// Record this statistics creation in the event log.
	if !createStatsPostEvents.Get(&evalCtx.Settings.SV) {
		return nil
	}

	// TODO(rytaft): This creates a new transaction for the CREATE STATISTICS
	// event. It must be different from the CREATE STATISTICS transaction,
	// because that transaction must be read-only. In the future we may want
	// to use the transaction that inserted the new stats into the
	// system.table_statistics table, but that would require calling
	// MakeEventLogger from the distsqlrun package.
	return evalCtx.ExecCfg.DB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		return MakeEventLogger(evalCtx.ExecCfg).InsertEventRecord(
			ctx,
			txn,
			EventLogCreateStatistics,
			int32(details.Table.ID),
			int32(evalCtx.NodeID.SQLInstanceID()),
			struct {
				TableName string
				Statement string
			}{details.FQTableName, details.Statement},
		)
	})
}

// checkRunningJobs checks whether there are any other CreateStats jobs in the
// pending, running, or paused status that started earlier than this one. If
// there are, checkRunningJobs returns an error. If job is nil, checkRunningJobs
// just checks if there are any pending, running, or paused CreateStats jobs.
func checkRunningJobs(ctx context.Context, job *jobs.Job, p JobExecContext) error {
	var jobID int64
	if job != nil {
		jobID = *job.ID()
	}
	const stmt = `SELECT id, payload FROM system.jobs WHERE status IN ($1, $2, $3) ORDER BY created`

	rows, err := p.ExecCfg().InternalExecutor.Query(
		ctx,
		"get-jobs",
		nil, /* txn */
		stmt,
		jobs.StatusPending,
		jobs.StatusRunning,
		jobs.StatusPaused,
	)
	if err != nil {
		return err
	}

	for _, row := range rows {
		payload, err := jobs.UnmarshalPayload(row[1])
		if err != nil {
			return err
		}

		if payload.Type() == jobspb.TypeCreateStats || payload.Type() == jobspb.TypeAutoCreateStats {
			id := (*int64)(row[0].(*tree.DInt))
			if *id == jobID {
				break
			}

			// This is not the first CreateStats job running. This job should fail
			// so that the earlier job can succeed.
			return stats.ConcurrentCreateStatsError
		}
	}
	return nil
}

// OnFailOrCancel is part of the jobs.Resumer interface.
func (r *createStatsResumer) OnFailOrCancel(context.Context, interface{}) error { return nil }

func init() {
	createResumerFn := func(job *jobs.Job, settings *cluster.Settings) jobs.Resumer {
		return &createStatsResumer{job: job}
	}
	jobs.RegisterConstructor(jobspb.TypeCreateStats, createResumerFn)
	jobs.RegisterConstructor(jobspb.TypeAutoCreateStats, createResumerFn)
}
