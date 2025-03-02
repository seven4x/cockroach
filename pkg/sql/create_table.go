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
	"bytes"
	"context"
	"fmt"
	"go/constant"
	"strconv"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/geo/geoindex"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catalogkeys"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catalogkv"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/colinfo"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/resolver"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/schemaexpr"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/paramparse"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgnotice"
	"github.com/cockroachdb/cockroach/pkg/sql/row"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/sqltelemetry"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/errorutil/unimplemented"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/errors"
	"github.com/lib/pq/oid"
)

type createTableNode struct {
	n          *tree.CreateTable
	dbDesc     catalog.DatabaseDescriptor
	sourcePlan planNode

	run createTableRun
}

// createTableRun contains the run-time state of createTableNode
// during local execution.
type createTableRun struct {
	autoCommit autoCommitOpt

	// synthRowID indicates whether an input column needs to be synthesized to
	// provide the default value for the hidden rowid column. The optimizer's plan
	// already includes this column if a user specified PK does not exist (so
	// synthRowID is false), whereas the heuristic planner's plan does not in this
	// case (so synthRowID is true).
	synthRowID bool

	// fromHeuristicPlanner indicates whether the planning was performed by the
	// heuristic planner instead of the optimizer. This is used to determine
	// whether or not a row_id was synthesized as part of the planning stage, if a
	// user defined PK is not specified.
	fromHeuristicPlanner bool
}

// minimumTypeUsageVersions defines the minimum version needed for a new
// data type.
var minimumTypeUsageVersions = map[types.Family]clusterversion.Key{
	types.GeographyFamily: clusterversion.GeospatialType,
	types.GeometryFamily:  clusterversion.GeospatialType,
	types.Box2DFamily:     clusterversion.Box2DType,
}

// isTypeSupportedInVersion returns whether a given type is supported in the given version.
func isTypeSupportedInVersion(v clusterversion.ClusterVersion, t *types.T) (bool, error) {
	// For these checks, if we have an array, we only want to find whether
	// we support the array contents.
	if t.Family() == types.ArrayFamily {
		t = t.ArrayContents()
	}

	minVersion, ok := minimumTypeUsageVersions[t.Family()]
	if !ok {
		return true, nil
	}
	return v.IsActive(minVersion), nil
}

// ReadingOwnWrites implements the planNodeReadingOwnWrites interface.
// This is because CREATE TABLE performs multiple KV operations on descriptors
// and expects to see its own writes.
func (n *createTableNode) ReadingOwnWrites() {}

// getSchemaIDForCreate returns the ID of the schema to create an object within.
// Note that it does not handle the temporary schema -- if the requested schema
// is temporary, the caller needs to use (*planner).getOrCreateTemporarySchema.
func (p *planner) getSchemaIDForCreate(
	ctx context.Context, codec keys.SQLCodec, dbID descpb.ID, scName string,
) (descpb.ID, error) {
	_, res, err := p.ResolveUncachedSchemaDescriptor(ctx, dbID, scName, true /* required */)
	if err != nil {
		return 0, err
	}
	switch res.Kind {
	case catalog.SchemaPublic, catalog.SchemaUserDefined:
		return res.ID, nil
	case catalog.SchemaVirtual:
		return 0, pgerror.Newf(pgcode.InsufficientPrivilege, "schema cannot be modified: %q", scName)
	default:
		return 0, errors.AssertionFailedf("invalid schema kind for getSchemaIDForCreate: %d", res.Kind)
	}
}

// getTableCreateParams returns the table key needed for the new table,
// as well as the schema id. It returns valid data in the case that
// the desired object exists.
func getTableCreateParams(
	params runParams, dbID descpb.ID, persistence tree.Persistence, tableName *tree.TableName,
) (tKey catalogkeys.DescriptorKey, schemaID descpb.ID, err error) {
	// Check we are not creating a table which conflicts with an alias available
	// as a built-in type in CockroachDB but an extension type on the public
	// schema for PostgreSQL.
	if tableName.Schema() == tree.PublicSchema {
		if _, ok := types.PublicSchemaAliases[tableName.Object()]; ok {
			return nil, 0, sqlerrors.NewTypeAlreadyExistsError(tableName.String())
		}
	}

	if persistence.IsTemporary() {
		if !params.SessionData().TempTablesEnabled {
			return nil, 0, errors.WithTelemetry(
				pgerror.WithCandidateCode(
					errors.WithHint(
						errors.WithIssueLink(
							errors.Newf("temporary tables are only supported experimentally"),
							errors.IssueLink{IssueURL: build.MakeIssueURL(46260)},
						),
						"You can enable temporary tables by running `SET experimental_enable_temp_tables = 'on'`.",
					),
					pgcode.FeatureNotSupported,
				),
				"sql.schema.temp_tables_disabled",
			)
		}

		// If the table is temporary, get the temporary schema ID.
		var err error
		schemaID, err = params.p.getOrCreateTemporarySchema(params.ctx, dbID)
		if err != nil {
			return nil, 0, err
		}
		tKey = catalogkeys.NewTableKey(dbID, schemaID, tableName.Table())
	} else {
		// Otherwise, find the ID of the schema to create the table within.
		var err error
		schemaID, err = params.p.getSchemaIDForCreate(params.ctx, params.ExecCfg().Codec, dbID, tableName.Schema())
		if err != nil {
			return nil, 0, err
		}
		if schemaID != keys.PublicSchemaID {
			sqltelemetry.IncrementUserDefinedSchemaCounter(sqltelemetry.UserDefinedSchemaUsedByObject)
		}
		tKey = catalogkv.MakeObjectNameKey(params.ctx, params.ExecCfg().Settings, dbID, schemaID, tableName.Table())
	}

	if persistence.IsUnlogged() {
		telemetry.Inc(sqltelemetry.CreateUnloggedTableCounter)
		params.p.BufferClientNotice(
			params.ctx,
			pgnotice.Newf("UNLOGGED TABLE will behave as a regular table in CockroachDB"),
		)
	}

	// Check permissions on the schema.
	if err := params.p.canCreateOnSchema(
		params.ctx, schemaID, dbID, params.p.User(), skipCheckPublicSchema); err != nil {
		return nil, 0, err
	}

	exists, id, err := catalogkv.LookupObjectID(
		params.ctx, params.p.txn, params.ExecCfg().Codec, dbID, schemaID, tableName.Table())
	if err == nil && exists {
		// Try and see what kind of object we collided with.
		desc, err := catalogkv.GetAnyDescriptorByID(params.ctx, params.p.txn, params.ExecCfg().Codec, id, catalogkv.Immutable)
		if err != nil {
			return nil, 0, sqlerrors.WrapErrorWhileConstructingObjectAlreadyExistsErr(err)
		}
		// Still return data in this case.
		return tKey, schemaID, sqlerrors.MakeObjectAlreadyExistsError(desc.DescriptorProto(), tableName.Table())
	} else if err != nil {
		return nil, 0, err
	}
	return tKey, schemaID, nil
}

func (n *createTableNode) startExec(params runParams) error {
	telemetry.Inc(sqltelemetry.SchemaChangeCreateCounter("table"))

	tKey, schemaID, err := getTableCreateParams(params, n.dbDesc.GetID(), n.n.Persistence, &n.n.Table)
	if err != nil {
		if sqlerrors.IsRelationAlreadyExistsError(err) && n.n.IfNotExists {
			return nil
		}
		return err
	}

	if n.n.Interleave != nil {
		telemetry.Inc(sqltelemetry.CreateInterleavedTableCounter)
		params.p.BufferClientNotice(
			params.ctx,
			errors.WithIssueLink(
				pgnotice.Newf("interleaved tables and indexes are deprecated in 20.2 and will be removed in 21.2"),
				errors.IssueLink{IssueURL: build.MakeIssueURL(52009)},
			),
		)
	}
	if n.n.Persistence.IsTemporary() {
		telemetry.Inc(sqltelemetry.CreateTempTableCounter)

		// TODO(#46556): support ON COMMIT DROP and DELETE ROWS on TEMPORARY TABLE.
		// If we do this, the n.n.OnCommit variable should probably be stored on the
		// table descriptor.
		// Note UNSET / PRESERVE ROWS behave the same way so we do not need to do that for now.
		switch n.n.OnCommit {
		case tree.CreateTableOnCommitUnset, tree.CreateTableOnCommitPreserveRows:
		default:
			return errors.AssertionFailedf("ON COMMIT value %d is unrecognized", n.n.OnCommit)
		}
	} else if n.n.OnCommit != tree.CreateTableOnCommitUnset {
		return pgerror.Newf(
			pgcode.InvalidTableDefinition,
			"ON COMMIT can only be used on temporary tables",
		)
	}

	// Warn against creating non-partitioned indexes on a partitioned table,
	// which is undesirable in most cases.
	if n.n.PartitionBy != nil {
		for _, def := range n.n.Defs {
			if d, ok := def.(*tree.IndexTableDef); ok {
				if d.PartitionBy == nil {
					params.p.BufferClientNotice(
						params.ctx,
						errors.WithHint(
							pgnotice.Newf("creating non-partitioned index on partitioned table may not be performant"),
							"Consider modifying the index such that it is also partitioned.",
						),
					)
				}
			}
		}
	}

	id, err := catalogkv.GenerateUniqueDescID(params.ctx, params.p.ExecCfg().DB, params.p.ExecCfg().Codec)
	if err != nil {
		return err
	}

	privs := CreateInheritedPrivilegesFromDBDesc(n.dbDesc, params.SessionData().User())

	var asCols colinfo.ResultColumns
	var desc *tabledesc.Mutable
	var affected map[descpb.ID]*tabledesc.Mutable
	// creationTime is initialized to a zero value and populated at read time.
	// See the comment in desc.MaybeIncrementVersion.
	//
	// TODO(ajwerner): remove the timestamp from newTableDesc and its friends,
	// it's	currently relied on in import and restore code and tests.
	var creationTime hlc.Timestamp
	if n.n.As() {
		asCols = planColumns(n.sourcePlan)
		if !n.run.fromHeuristicPlanner && !n.n.AsHasUserSpecifiedPrimaryKey() {
			// rowID column is already present in the input as the last column if it
			// was planned by the optimizer and the user did not specify a PRIMARY
			// KEY. So ignore it for the purpose of creating column metadata (because
			// newTableDescIfAs does it automatically).
			asCols = asCols[:len(asCols)-1]
		}

		desc, err = newTableDescIfAs(params,
			n.n, n.dbDesc.GetID(), schemaID, id, creationTime, asCols, privs, params.p.EvalContext())
		if err != nil {
			return err
		}

		// If we have an implicit txn we want to run CTAS async, and consequently
		// ensure it gets queued as a SchemaChange.
		if params.p.ExtendedEvalContext().TxnImplicit {
			desc.State = descpb.DescriptorState_ADD
		}
	} else {
		affected = make(map[descpb.ID]*tabledesc.Mutable)
		desc, err = newTableDesc(params, n.n, n.dbDesc.GetID(), schemaID, id, creationTime, privs, affected)
		if err != nil {
			return err
		}

		if desc.Adding() {
			// if this table and all its references are created in the same
			// transaction it can be made PUBLIC.
			refs, err := desc.FindAllReferences()
			if err != nil {
				return err
			}
			var foundExternalReference bool
			for id := range refs {
				if t := params.p.Descriptors().GetUncommittedTableByID(id); t == nil || !t.IsNew() {
					foundExternalReference = true
					break
				}
			}
			if !foundExternalReference {
				desc.State = descpb.DescriptorState_PUBLIC
			}
		}
	}

	// Descriptor written to store here.
	if err := params.p.createDescriptorWithID(
		params.ctx, tKey.Key(params.ExecCfg().Codec), id, desc, params.EvalContext().Settings,
		tree.AsStringWithFQNames(n.n, params.Ann()),
	); err != nil {
		return err
	}

	for _, updated := range affected {
		if err := params.p.writeSchemaChange(
			params.ctx, updated, descpb.InvalidMutationID,
			fmt.Sprintf("updating referenced FK table %s(%d) for table %s(%d)",
				updated.Name, updated.ID, desc.Name, desc.ID,
			),
		); err != nil {
			return err
		}
	}

	for _, index := range desc.AllNonDropIndexes() {
		if len(index.Interleave.Ancestors) > 0 {
			if err := params.p.finalizeInterleave(params.ctx, desc, index); err != nil {
				return err
			}
		}
	}

	// Install back references to types used by this table.
	if err := params.p.addBackRefsFromAllTypesInTable(params.ctx, desc); err != nil {
		return err
	}

	dg := catalogkv.NewOneLevelUncachedDescGetter(params.p.txn, params.ExecCfg().Codec)
	if err := desc.Validate(params.ctx, dg); err != nil {
		return err
	}

	// Log Create Table event. This is an auditable log event and is
	// recorded in the same transaction as the table descriptor update.
	if err := MakeEventLogger(params.extendedEvalCtx.ExecCfg).InsertEventRecord(
		params.ctx,
		params.p.txn,
		EventLogCreateTable,
		int32(desc.ID),
		int32(params.extendedEvalCtx.NodeID.SQLInstanceID()),
		struct {
			TableName string
			Statement string
			User      string
		}{n.n.Table.FQString(), n.n.String(), params.p.User().Normalized()},
	); err != nil {
		return err
	}

	// If we are in an explicit txn or the source has placeholders, we execute the
	// CTAS query synchronously.
	if n.n.As() && !params.p.ExtendedEvalContext().TxnImplicit {
		err = func() error {
			// The data fill portion of CREATE AS must operate on a read snapshot,
			// so that it doesn't end up observing its own writes.
			prevMode := params.p.Txn().ConfigureStepping(params.ctx, kv.SteppingEnabled)
			defer func() { _ = params.p.Txn().ConfigureStepping(params.ctx, prevMode) }()

			// This is a very simplified version of the INSERT logic: no CHECK
			// expressions, no FK checks, no arbitrary insertion order, no
			// RETURNING, etc.

			// Instantiate a row inserter and table writer. It has a 1-1
			// mapping to the definitions in the descriptor.
			ri, err := row.MakeInserter(
				params.ctx,
				params.p.txn,
				params.ExecCfg().Codec,
				desc.ImmutableCopy().(*tabledesc.Immutable),
				desc.Columns,
				params.p.alloc)
			if err != nil {
				return err
			}
			ti := tableInserterPool.Get().(*tableInserter)
			*ti = tableInserter{ri: ri}
			tw := tableWriter(ti)
			if n.run.autoCommit == autoCommitEnabled {
				tw.enableAutoCommit()
			}
			defer func() {
				tw.close(params.ctx)
				*ti = tableInserter{}
				tableInserterPool.Put(ti)
			}()
			if err := tw.init(params.ctx, params.p.txn, params.p.EvalContext()); err != nil {
				return err
			}

			// Prepare the buffer for row values. At this point, one more column has
			// been added by ensurePrimaryKey() to the list of columns in sourcePlan, if
			// a PRIMARY KEY is not specified by the user.
			rowBuffer := make(tree.Datums, len(desc.Columns))
			pkColIdx := len(desc.Columns) - 1

			// The optimizer includes the rowID expression as part of the input
			// expression. But the heuristic planner does not do this, so construct
			// a rowID expression to be evaluated separately.
			var defTypedExpr tree.TypedExpr
			if n.run.synthRowID {
				// Prepare the rowID expression.
				defExprSQL := *desc.Columns[pkColIdx].DefaultExpr
				defExpr, err := parser.ParseExpr(defExprSQL)
				if err != nil {
					return err
				}
				defTypedExpr, err = params.p.analyzeExpr(
					params.ctx,
					defExpr,
					nil, /*sources*/
					tree.IndexedVarHelper{},
					types.Any,
					false, /*requireType*/
					"CREATE TABLE AS")
				if err != nil {
					return err
				}
			}

			for {
				if err := params.p.cancelChecker.Check(); err != nil {
					return err
				}
				if next, err := n.sourcePlan.Next(params); !next {
					if err != nil {
						return err
					}
					if err := tw.finalize(params.ctx); err != nil {
						return err
					}
					break
				}

				// Populate the buffer and generate the PK value.
				copy(rowBuffer, n.sourcePlan.Values())
				if n.run.synthRowID {
					rowBuffer[pkColIdx], err = defTypedExpr.Eval(params.p.EvalContext())
					if err != nil {
						return err
					}
				}

				// CREATE TABLE AS does not copy indexes from the input table.
				// An empty row.PartialIndexUpdateHelper is used here because
				// there are no indexes, partial or otherwise, to update.
				var pm row.PartialIndexUpdateHelper
				if err := tw.row(params.ctx, rowBuffer, pm, params.extendedEvalCtx.Tracing.KVTracingEnabled()); err != nil {
					return err
				}
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}

	return nil
}

func (*createTableNode) Next(runParams) (bool, error) { return false, nil }
func (*createTableNode) Values() tree.Datums          { return tree.Datums{} }

func (n *createTableNode) Close(ctx context.Context) {
	if n.sourcePlan != nil {
		n.sourcePlan.Close(ctx)
		n.sourcePlan = nil
	}
}

func qualifyFKColErrorWithDB(
	ctx context.Context, txn *kv.Txn, codec keys.SQLCodec, tbl *tabledesc.Mutable, col string,
) string {
	if txn == nil {
		return tree.ErrString(tree.NewUnresolvedName(tbl.Name, col))
	}

	// TODO(solon): this ought to use a database cache.
	db, err := catalogkv.MustGetDatabaseDescByID(ctx, txn, codec, tbl.ParentID)
	if err != nil {
		return tree.ErrString(tree.NewUnresolvedName(tbl.Name, col))
	}
	schema, err := resolver.ResolveSchemaNameByID(ctx, txn, codec, db.GetID(), tbl.GetParentSchemaID())
	if err != nil {
		return tree.ErrString(tree.NewUnresolvedName(tbl.Name, col))
	}
	return tree.ErrString(tree.NewUnresolvedName(db.GetName(), schema, tbl.Name, col))
}

// FKTableState is the state of the referencing table ResolveFK() is called on.
type FKTableState int

const (
	// NewTable represents a new table, where the FK constraint is specified in the
	// CREATE TABLE
	NewTable FKTableState = iota
	// EmptyTable represents an existing table that is empty
	EmptyTable
	// NonEmptyTable represents an existing non-empty table
	NonEmptyTable
)

// MaybeUpgradeDependentOldForeignKeyVersionTables upgrades the on-disk foreign key descriptor
// version of all table descriptors that have foreign key relationships with desc. This is intended
// to catch upgrade 19.1 version table descriptors that haven't been upgraded yet before an operation
// like drop index which could cause them to lose FK information in the old representation.
func (p *planner) MaybeUpgradeDependentOldForeignKeyVersionTables(
	ctx context.Context, desc *tabledesc.Mutable,
) error {
	// In order to avoid having old version foreign key descriptors that depend on this
	// index lose information when this index is dropped, ensure that they get updated.
	maybeUpgradeFKRepresentation := func(id descpb.ID) error {
		// Read the referenced table and see if the foreign key representation has changed. If it has, write
		// the upgraded descriptor back to disk.
		desc, err := catalogkv.GetDescriptorByID(ctx, p.txn, p.ExecCfg().Codec, id,
			catalogkv.Mutable, catalogkv.TableDescriptorKind, true /* required */)
		if err != nil {
			return err
		}
		tbl := desc.(*tabledesc.Mutable)
		changes := tbl.GetPostDeserializationChanges()
		if changes.UpgradedForeignKeyRepresentation {
			err := p.writeSchemaChange(ctx, tbl, descpb.InvalidMutationID,
				fmt.Sprintf("updating foreign key references on table %s(%d)",
					tbl.Name, tbl.ID),
			)
			if err != nil {
				return err
			}
		}
		return nil
	}
	for i := range desc.OutboundFKs {
		if err := maybeUpgradeFKRepresentation(desc.OutboundFKs[i].ReferencedTableID); err != nil {
			return err
		}
	}
	for i := range desc.InboundFKs {
		if err := maybeUpgradeFKRepresentation(desc.InboundFKs[i].OriginTableID); err != nil {
			return err
		}
	}
	return nil
}

// ResolveFK looks up the tables and columns mentioned in a `REFERENCES`
// constraint and adds metadata representing that constraint to the descriptor.
// It may, in doing so, add to or alter descriptors in the passed in `backrefs`
// map of other tables that need to be updated when this table is created.
// Constraints that are not known to hold for existing data are created
// "unvalidated", but when table is empty (e.g. during creation), no existing
// data implies no existing violations, and thus the constraint can be created
// without the unvalidated flag.
//
// The caller should pass an instance of fkSelfResolver as
// SchemaResolver, so that FK references can find the newly created
// table for self-references.
//
// The caller must also ensure that the SchemaResolver is configured to
// bypass caching and enable visibility of just-added descriptors.
// If there are any FKs, the descriptor of the depended-on table must
// be looked up uncached, and we'll allow FK dependencies on tables
// that were just added.
//
// The passed Txn is used to lookup databases to qualify names in error messages
// but if nil, will result in unqualified names in those errors.
//
// The passed validationBehavior is used to determine whether or not preexisting
// entries in the table need to be validated against the foreign key being added.
// This only applies for existing tables, not new tables.
func ResolveFK(
	ctx context.Context,
	txn *kv.Txn,
	sc resolver.SchemaResolver,
	tbl *tabledesc.Mutable,
	d *tree.ForeignKeyConstraintTableDef,
	backrefs map[descpb.ID]*tabledesc.Mutable,
	ts FKTableState,
	validationBehavior tree.ValidationBehavior,
	evalCtx *tree.EvalContext,
) error {
	var originColSet catalog.TableColSet
	originCols := make([]*descpb.ColumnDescriptor, len(d.FromCols))
	for i, col := range d.FromCols {
		col, err := tbl.FindActiveOrNewColumnByName(col)
		if err != nil {
			return err
		}
		if err := col.CheckCanBeFKRef(); err != nil {
			return err
		}
		// Ensure that the origin columns don't have duplicates.
		if originColSet.Contains(col.ID) {
			return pgerror.Newf(pgcode.InvalidForeignKey,
				"foreign key contains duplicate column %q", col.Name)
		}
		originColSet.Add(col.ID)
		originCols[i] = col
	}

	target, err := resolver.ResolveMutableExistingTableObject(ctx, sc, &d.Table, true /*required*/, tree.ResolveRequireTableDesc)
	if err != nil {
		return err
	}
	if target.ParentID != tbl.ParentID {
		if !allowCrossDatabaseFKs.Get(&evalCtx.Settings.SV) {
			return pgerror.Newf(pgcode.InvalidForeignKey,
				"foreign references between databases are not allowed (see the '%s' cluster setting)",
				allowCrossDatabaseFKsSetting,
			)
		}
	}
	if tbl.Temporary != target.Temporary {
		persistenceType := "permanent"
		if tbl.Temporary {
			persistenceType = "temporary"
		}
		return pgerror.Newf(
			pgcode.InvalidTableDefinition,
			"constraints on %s tables may reference only %s tables",
			persistenceType,
			persistenceType,
		)
	}
	if target.ID == tbl.ID {
		// When adding a self-ref FK to an _existing_ table, we want to make sure
		// we edit the same copy.
		target = tbl
	} else {
		// Since this FK is referencing another table, this table must be created in
		// a non-public "ADD" state and made public only after all leases on the
		// other table are updated to include the backref, if it does not already
		// exist.
		if ts == NewTable {
			tbl.State = descpb.DescriptorState_ADD
		}

		// If we resolve the same table more than once, we only want to edit a
		// single instance of it, so replace target with previously resolved table.
		if prev, ok := backrefs[target.ID]; ok {
			target = prev
		} else {
			backrefs[target.ID] = target
		}
	}

	referencedColNames := d.ToCols
	// If no columns are specified, attempt to default to PK.
	if len(referencedColNames) == 0 {
		referencedColNames = make(tree.NameList, len(target.PrimaryIndex.ColumnNames))
		for i, n := range target.PrimaryIndex.ColumnNames {
			referencedColNames[i] = tree.Name(n)
		}
	}

	referencedCols, err := target.FindActiveColumnsByNames(referencedColNames)
	if err != nil {
		return err
	}

	if len(referencedCols) != len(originCols) {
		return pgerror.Newf(pgcode.Syntax,
			"%d columns must reference exactly %d columns in referenced table (found %d)",
			len(originCols), len(originCols), len(referencedCols))
	}

	for i := range originCols {
		if s, t := originCols[i], referencedCols[i]; !s.Type.Equivalent(t.Type) {
			return pgerror.Newf(pgcode.DatatypeMismatch,
				"type of %q (%s) does not match foreign key %q.%q (%s)",
				s.Name, s.Type.String(), target.Name, t.Name, t.Type.String())
		}
	}

	// Verify we are not writing a constraint over the same name.
	// This check is done in Verify(), but we must do it earlier
	// or else we can hit other checks that break things with
	// undesired error codes, e.g. #42858.
	// It may be removable after #37255 is complete.
	constraintInfo, err := tbl.GetConstraintInfo(ctx, nil)
	if err != nil {
		return err
	}
	constraintName := string(d.Name)
	if constraintName == "" {
		constraintName = tabledesc.GenerateUniqueConstraintName(
			fmt.Sprintf("fk_%s_ref_%s", string(d.FromCols[0]), target.Name),
			func(p string) bool {
				_, ok := constraintInfo[p]
				return ok
			},
		)
	} else {
		if _, ok := constraintInfo[constraintName]; ok {
			return pgerror.Newf(pgcode.DuplicateObject, "duplicate constraint name: %q", constraintName)
		}
	}

	originColumnIDs := make(descpb.ColumnIDs, len(originCols))
	for i, col := range originCols {
		originColumnIDs[i] = col.ID
	}

	targetColIDs := make(descpb.ColumnIDs, len(referencedCols))
	for i := range referencedCols {
		targetColIDs[i] = referencedCols[i].ID
	}

	// Don't add a SET NULL action on an index that has any column that is NOT
	// NULL.
	if d.Actions.Delete == tree.SetNull || d.Actions.Update == tree.SetNull {
		for _, originColumn := range originCols {
			if !originColumn.Nullable {
				col := qualifyFKColErrorWithDB(ctx, txn, evalCtx.Codec, tbl, originColumn.Name)
				return pgerror.Newf(pgcode.InvalidForeignKey,
					"cannot add a SET NULL cascading action on column %q which has a NOT NULL constraint", col,
				)
			}
		}
	}

	// Don't add a SET DEFAULT action on an index that has any column that has
	// a DEFAULT expression of NULL and a NOT NULL constraint.
	if d.Actions.Delete == tree.SetDefault || d.Actions.Update == tree.SetDefault {
		for _, originColumn := range originCols {
			// Having a default expression of NULL, and a constraint of NOT NULL is a
			// contradiction and should never be allowed.
			if originColumn.DefaultExpr == nil && !originColumn.Nullable {
				col := qualifyFKColErrorWithDB(ctx, txn, evalCtx.Codec, tbl, originColumn.Name)
				return pgerror.Newf(pgcode.InvalidForeignKey,
					"cannot add a SET DEFAULT cascading action on column %q which has a "+
						"NOT NULL constraint and a NULL default expression", col,
				)
			}
		}
	}

	// Check if the version is high enough to stop creating origin indexes.
	if evalCtx.Settings != nil &&
		!evalCtx.Settings.Version.IsActive(ctx, clusterversion.NoOriginFKIndexes) {
		// Search for an index on the origin table that matches. If one doesn't exist,
		// we create one automatically if the table to alter is new or empty. We also
		// search if an index for the set of columns was created in this transaction.
		_, err = tabledesc.FindFKOriginIndexInTxn(tbl, originColumnIDs)
		// If there was no error, we found a suitable index.
		if err != nil {
			// No existing suitable index was found.
			if ts == NonEmptyTable {
				var colNames bytes.Buffer
				colNames.WriteString(`("`)
				for i, id := range originColumnIDs {
					if i != 0 {
						colNames.WriteString(`", "`)
					}
					col, err := tbl.FindColumnByID(id)
					if err != nil {
						return err
					}
					colNames.WriteString(col.Name)
				}
				colNames.WriteString(`")`)
				return pgerror.Newf(pgcode.ForeignKeyViolation,
					"foreign key requires an existing index on columns %s", colNames.String())
			}
			_, err := addIndexForFK(ctx, tbl, originCols, constraintName, ts)
			if err != nil {
				return err
			}
		}
	}

	// Ensure that there is an index on the referenced side to use.
	_, err = tabledesc.FindFKReferencedIndex(target, targetColIDs)
	if err != nil {
		return err
	}

	var validity descpb.ConstraintValidity
	if ts != NewTable {
		if validationBehavior == tree.ValidationSkip {
			validity = descpb.ConstraintValidity_Unvalidated
		} else {
			validity = descpb.ConstraintValidity_Validating
		}
	}

	ref := descpb.ForeignKeyConstraint{
		OriginTableID:       tbl.ID,
		OriginColumnIDs:     originColumnIDs,
		ReferencedColumnIDs: targetColIDs,
		ReferencedTableID:   target.ID,
		Name:                constraintName,
		Validity:            validity,
		OnDelete:            descpb.ForeignKeyReferenceActionValue[d.Actions.Delete],
		OnUpdate:            descpb.ForeignKeyReferenceActionValue[d.Actions.Update],
		Match:               descpb.CompositeKeyMatchMethodValue[d.Match],
	}

	if ts == NewTable {
		tbl.OutboundFKs = append(tbl.OutboundFKs, ref)
		target.InboundFKs = append(target.InboundFKs, ref)
	} else {
		tbl.AddForeignKeyMutation(&ref, descpb.DescriptorMutation_ADD)
	}

	return nil
}

// Adds an index to a table descriptor (that is in the process of being created)
// that will support using `srcCols` as the referencing (src) side of an FK.
func addIndexForFK(
	ctx context.Context,
	tbl *tabledesc.Mutable,
	srcCols []*descpb.ColumnDescriptor,
	constraintName string,
	ts FKTableState,
) (descpb.IndexID, error) {
	autoIndexName := tabledesc.GenerateUniqueConstraintName(
		fmt.Sprintf("%s_auto_index_%s", tbl.Name, constraintName),
		func(name string) bool {
			return tbl.ValidateIndexNameIsUnique(name) != nil
		},
	)
	// No existing index for the referencing columns found, so we add one.
	idx := descpb.IndexDescriptor{
		Name:             autoIndexName,
		ColumnNames:      make([]string, len(srcCols)),
		ColumnDirections: make([]descpb.IndexDescriptor_Direction, len(srcCols)),
	}
	for i, c := range srcCols {
		idx.ColumnDirections[i] = descpb.IndexDescriptor_ASC
		idx.ColumnNames[i] = c.Name
	}

	if ts == NewTable {
		if err := tbl.AddIndex(idx, false); err != nil {
			return 0, err
		}
		if err := tbl.AllocateIDs(ctx); err != nil {
			return 0, err
		}
		added := tbl.Indexes[len(tbl.Indexes)-1]
		return added.ID, nil
	}

	// TODO (lucy): In the EmptyTable case, we add an index mutation, making this
	// the only case where a foreign key is added to an index being added.
	// Allowing FKs to be added to other indexes/columns also being added should
	// be a generalization of this special case.
	if err := tbl.AddIndexMutation(&idx, descpb.DescriptorMutation_ADD); err != nil {
		return 0, err
	}
	if err := tbl.AllocateIDs(ctx); err != nil {
		return 0, err
	}
	id := tbl.Mutations[len(tbl.Mutations)-1].GetIndex().ID
	return id, nil
}

func (p *planner) addInterleave(
	ctx context.Context,
	desc *tabledesc.Mutable,
	index *descpb.IndexDescriptor,
	interleave *tree.InterleaveDef,
) error {
	return addInterleave(ctx, p.txn, p, desc, index, interleave)
}

// addInterleave marks an index as one that is interleaved in some parent data
// according to the given definition.
func addInterleave(
	ctx context.Context,
	txn *kv.Txn,
	vt resolver.SchemaResolver,
	desc *tabledesc.Mutable,
	index *descpb.IndexDescriptor,
	interleave *tree.InterleaveDef,
) error {
	if interleave.DropBehavior != tree.DropDefault {
		return unimplemented.NewWithIssuef(
			7854, "unsupported shorthand %s", interleave.DropBehavior)
	}

	parentTable, err := resolver.ResolveExistingTableObject(
		ctx, vt, &interleave.Parent, tree.ObjectLookupFlagsWithRequiredTableKind(tree.ResolveRequireTableDesc),
	)
	if err != nil {
		return err
	}
	parentIndex := parentTable.PrimaryIndex

	// typeOfIndex is used to give more informative error messages.
	var typeOfIndex string
	if index.ID == desc.PrimaryIndex.ID {
		typeOfIndex = "primary key"
	} else {
		typeOfIndex = "index"
	}

	if len(interleave.Fields) != len(parentIndex.ColumnIDs) {
		return pgerror.Newf(
			pgcode.InvalidSchemaDefinition,
			"declared interleaved columns (%s) must match the parent's primary index (%s)",
			&interleave.Fields,
			strings.Join(parentIndex.ColumnNames, ", "),
		)
	}
	if len(interleave.Fields) > len(index.ColumnIDs) {
		return pgerror.Newf(
			pgcode.InvalidSchemaDefinition,
			"declared interleaved columns (%s) must be a prefix of the %s columns being interleaved (%s)",
			&interleave.Fields,
			typeOfIndex,
			strings.Join(index.ColumnNames, ", "),
		)
	}

	for i, targetColID := range parentIndex.ColumnIDs {
		targetCol, err := parentTable.FindColumnByID(targetColID)
		if err != nil {
			return err
		}
		col, err := desc.FindColumnByID(index.ColumnIDs[i])
		if err != nil {
			return err
		}
		if string(interleave.Fields[i]) != col.Name {
			return pgerror.Newf(
				pgcode.InvalidSchemaDefinition,
				"declared interleaved columns (%s) must refer to a prefix of the %s column names being interleaved (%s)",
				&interleave.Fields,
				typeOfIndex,
				strings.Join(index.ColumnNames, ", "),
			)
		}
		if !col.Type.Identical(targetCol.Type) || index.ColumnDirections[i] != parentIndex.ColumnDirections[i] {
			return pgerror.Newf(
				pgcode.InvalidSchemaDefinition,
				"declared interleaved columns (%s) must match type and sort direction of the parent's primary index (%s)",
				&interleave.Fields,
				strings.Join(parentIndex.ColumnNames, ", "),
			)
		}
	}

	ancestorPrefix := append(
		[]descpb.InterleaveDescriptor_Ancestor(nil), parentIndex.Interleave.Ancestors...)
	intl := descpb.InterleaveDescriptor_Ancestor{
		TableID:         parentTable.ID,
		IndexID:         parentIndex.ID,
		SharedPrefixLen: uint32(len(parentIndex.ColumnIDs)),
	}
	for _, ancestor := range ancestorPrefix {
		intl.SharedPrefixLen -= ancestor.SharedPrefixLen
	}
	index.Interleave = descpb.InterleaveDescriptor{Ancestors: append(ancestorPrefix, intl)}

	desc.State = descpb.DescriptorState_ADD
	return nil
}

// finalizeInterleave creates backreferences from an interleaving parent to the
// child data being interleaved.
func (p *planner) finalizeInterleave(
	ctx context.Context, desc *tabledesc.Mutable, index *descpb.IndexDescriptor,
) error {
	// TODO(dan): This is similar to finalizeFKs. Consolidate them
	if len(index.Interleave.Ancestors) == 0 {
		return nil
	}
	// Only the last ancestor needs the backreference.
	ancestor := index.Interleave.Ancestors[len(index.Interleave.Ancestors)-1]
	var ancestorTable *tabledesc.Mutable
	if ancestor.TableID == desc.ID {
		ancestorTable = desc
	} else {
		var err error
		ancestorTable, err = p.Descriptors().GetMutableTableVersionByID(ctx, ancestor.TableID, p.txn)
		if err != nil {
			return err
		}
	}
	ancestorIndex, err := ancestorTable.FindIndexByID(ancestor.IndexID)
	if err != nil {
		return err
	}
	ancestorIndex.InterleavedBy = append(ancestorIndex.InterleavedBy,
		descpb.ForeignKeyReference{Table: desc.ID, Index: index.ID})

	if err := p.writeSchemaChange(
		ctx, ancestorTable, descpb.InvalidMutationID,
		fmt.Sprintf(
			"updating ancestor table %s(%d) for table %s(%d)",
			ancestorTable.Name, ancestorTable.ID, desc.Name, desc.ID,
		),
	); err != nil {
		return err
	}

	if desc.State == descpb.DescriptorState_ADD {
		desc.State = descpb.DescriptorState_PUBLIC

		// No job description, since this is presumably part of some larger schema change.
		if err := p.writeSchemaChange(
			ctx, desc, descpb.InvalidMutationID, "",
		); err != nil {
			return err
		}
	}

	return nil
}

// CreatePartitioning constructs the partitioning descriptor for an index that
// is partitioned into ranges, each addressable by zone configs.
func CreatePartitioning(
	ctx context.Context,
	st *cluster.Settings,
	evalCtx *tree.EvalContext,
	tableDesc *tabledesc.Mutable,
	indexDesc *descpb.IndexDescriptor,
	partBy *tree.PartitionBy,
) (descpb.PartitioningDescriptor, error) {
	if partBy == nil {
		// No CCL necessary if we're looking at PARTITION BY NOTHING.
		return descpb.PartitioningDescriptor{}, nil
	}
	return CreatePartitioningCCL(ctx, st, evalCtx, tableDesc, indexDesc, partBy)
}

// CreatePartitioningCCL is the public hook point for the CCL-licensed
// partitioning creation code.
var CreatePartitioningCCL = func(
	ctx context.Context,
	st *cluster.Settings,
	evalCtx *tree.EvalContext,
	tableDesc *tabledesc.Mutable,
	indexDesc *descpb.IndexDescriptor,
	partBy *tree.PartitionBy,
) (descpb.PartitioningDescriptor, error) {
	return descpb.PartitioningDescriptor{}, sqlerrors.NewCCLRequiredError(errors.New(
		"creating or manipulating partitions requires a CCL binary"))
}

func getFinalSourceQuery(source *tree.Select, evalCtx *tree.EvalContext) string {
	// Ensure that all the table names pretty-print as fully qualified, so we
	// store that in the table descriptor.
	//
	// The traversal will update the TableNames in-place, so the changes are
	// persisted in n.n.AsSource. We exploit the fact that planning step above
	// has populated any missing db/schema details in the table names in-place.
	// We use tree.FormatNode merely as a traversal method; its output buffer is
	// discarded immediately after the traversal because it is not needed
	// further.
	f := tree.NewFmtCtx(tree.FmtSerializable)
	f.SetReformatTableNames(
		func(_ *tree.FmtCtx, tn *tree.TableName) {
			// Persist the database prefix expansion.
			if tn.SchemaName != "" {
				// All CTE or table aliases have no schema
				// information. Those do not turn into explicit.
				tn.ExplicitSchema = true
				tn.ExplicitCatalog = true
			}
		},
	)
	f.FormatNode(source)
	f.Close()

	// Substitute placeholders with their values.
	ctx := tree.NewFmtCtx(tree.FmtSerializable)
	ctx.SetPlaceholderFormat(func(ctx *tree.FmtCtx, placeholder *tree.Placeholder) {
		d, err := placeholder.Eval(evalCtx)
		if err != nil {
			panic(errors.AssertionFailedf("failed to serialize placeholder: %s", err))
		}
		d.Format(ctx)
	})
	ctx.FormatNode(source)

	return ctx.CloseAndGetString()
}

// newTableDescIfAs is the NewTableDesc method for when we have a table
// that is created with the CREATE AS format.
func newTableDescIfAs(
	params runParams,
	p *tree.CreateTable,
	parentID, parentSchemaID, id descpb.ID,
	creationTime hlc.Timestamp,
	resultColumns []colinfo.ResultColumn,
	privileges *descpb.PrivilegeDescriptor,
	evalContext *tree.EvalContext,
) (desc *tabledesc.Mutable, err error) {
	colResIndex := 0
	// TableDefs for a CREATE TABLE ... AS AST node comprise of a ColumnTableDef
	// for each column, and a ConstraintTableDef for any constraints on those
	// columns.
	for _, defs := range p.Defs {
		var d *tree.ColumnTableDef
		var ok bool
		if d, ok = defs.(*tree.ColumnTableDef); ok {
			d.Type = resultColumns[colResIndex].Typ
			colResIndex++
		}
	}

	// If there are no TableDefs defined by the parser, then we construct a
	// ColumnTableDef for each column using resultColumns.
	if len(p.Defs) == 0 {
		for _, colRes := range resultColumns {
			var d *tree.ColumnTableDef
			var ok bool
			var tableDef tree.TableDef = &tree.ColumnTableDef{Name: tree.Name(colRes.Name), Type: colRes.Typ}
			if d, ok = tableDef.(*tree.ColumnTableDef); !ok {
				return nil, errors.Errorf("failed to cast type to ColumnTableDef\n")
			}
			d.Nullable.Nullability = tree.SilentNull
			p.Defs = append(p.Defs, tableDef)
		}
	}

	desc, err = newTableDesc(
		params,
		p,
		parentID, parentSchemaID, id,
		creationTime,
		privileges,
		nil, /* affected */
	)
	if err != nil {
		return nil, err
	}
	desc.CreateQuery = getFinalSourceQuery(p.AsSource, evalContext)
	return desc, nil
}

// NewTableDesc creates a table descriptor from a CreateTable statement.
//
// txn and vt can be nil if the table to be created does not contain references
// to other tables (e.g. foreign keys or interleaving). This is useful at
// bootstrap when creating descriptors for virtual tables.
//
// parentID refers to the databaseID under which the descriptor is being
// created and parentSchemaID refers to the schemaID of the schema under which
// the descriptor is being created.
//
// evalCtx can be nil if the table to be created has no default expression for
// any of the columns and no partitioning expression.
//
// semaCtx can be nil if the table to be created has no default expression on
// any of the columns and no check constraints.
//
// The caller must also ensure that the SchemaResolver is configured
// to bypass caching and enable visibility of just-added descriptors.
// This is used to resolve sequence and FK dependencies. Also see the
// comment at the start of ResolveFK().
//
// If the table definition *may* use the SERIAL type, the caller is
// also responsible for processing serial types using
// processSerialInColumnDef() on every column definition, and creating
// the necessary sequences in KV before calling NewTableDesc().
func NewTableDesc(
	ctx context.Context,
	txn *kv.Txn,
	vt resolver.SchemaResolver,
	st *cluster.Settings,
	n *tree.CreateTable,
	parentID, parentSchemaID, id descpb.ID,
	creationTime hlc.Timestamp,
	privileges *descpb.PrivilegeDescriptor,
	affected map[descpb.ID]*tabledesc.Mutable,
	semaCtx *tree.SemaContext,
	evalCtx *tree.EvalContext,
	sessionData *sessiondata.SessionData,
	persistence tree.Persistence,
) (*tabledesc.Mutable, error) {
	// Used to delay establishing Column/Sequence dependency until ColumnIDs have
	// been populated.
	columnDefaultExprs := make([]tree.TypedExpr, len(n.Defs))

	desc := tabledesc.InitTableDescriptor(
		id, parentID, parentSchemaID, n.Table.Table(), creationTime, privileges, persistence,
	)

	if err := paramparse.ApplyStorageParameters(
		ctx,
		semaCtx,
		evalCtx,
		n.StorageParams,
		&paramparse.TableStorageParamObserver{},
	); err != nil {
		return nil, err
	}

	indexEncodingVersion := descpb.SecondaryIndexFamilyFormatVersion
	// We can't use st.Version.IsActive because this method is used during
	// server setup before the cluster version has been initialized.
	version := st.Version.ActiveVersionOrEmpty(ctx)
	if version != (clusterversion.ClusterVersion{}) {
		if version.IsActive(clusterversion.EmptyArraysInInvertedIndexes) {
			indexEncodingVersion = descpb.EmptyArraysInInvertedIndexesVersion
		}
	}

	for i, def := range n.Defs {
		if d, ok := def.(*tree.ColumnTableDef); ok {
			// NewTableDesc is called sometimes with a nil SemaCtx (for example
			// during bootstrapping). In order to not panic, pass a nil TypeResolver
			// when attempting to resolve the columns type.
			defType, err := tree.ResolveType(ctx, d.Type, semaCtx.GetTypeResolver())
			if err != nil {
				return nil, err
			}
			if !desc.IsVirtualTable() {
				switch defType.Oid() {
				case oid.T_int2vector, oid.T_oidvector:
					return nil, pgerror.Newf(
						pgcode.FeatureNotSupported,
						"VECTOR column types are unsupported",
					)
				}
			}
			if supported, err := isTypeSupportedInVersion(version, defType); err != nil {
				return nil, err
			} else if !supported {
				return nil, pgerror.Newf(
					pgcode.FeatureNotSupported,
					"type %s is not supported until version upgrade is finalized",
					defType.SQLString(),
				)
			}
			if d.PrimaryKey.Sharded {
				if !sessionData.HashShardedIndexesEnabled {
					return nil, hashShardedIndexesDisabledError
				}
				if n.PartitionBy != nil {
					return nil, pgerror.New(pgcode.FeatureNotSupported, "sharded indexes don't support partitioning")
				}
				if n.Interleave != nil {
					return nil, pgerror.New(pgcode.FeatureNotSupported, "interleaved indexes cannot also be hash sharded")
				}
				buckets, err := tabledesc.EvalShardBucketCount(ctx, semaCtx, evalCtx, d.PrimaryKey.ShardBuckets)
				if err != nil {
					return nil, err
				}
				shardCol, _, err := maybeCreateAndAddShardCol(int(buckets), &desc,
					[]string{string(d.Name)}, true /* isNewTable */)
				if err != nil {
					return nil, err
				}
				checkConstraint, err := makeShardCheckConstraintDef(&desc, int(buckets), shardCol)
				if err != nil {
					return nil, err
				}
				// Add the shard's check constraint to the list of TableDefs to treat it
				// like it's been "hoisted" like the explicitly added check constraints.
				// It'll then be added to this table's resulting table descriptor below in
				// the constraint pass.
				n.Defs = append(n.Defs, checkConstraint)
				columnDefaultExprs = append(columnDefaultExprs, nil)
			}
			if d.IsVirtual() {
				return nil, unimplemented.NewWithIssue(57608, "virtual computed columns")
			}

			col, idx, expr, err := tabledesc.MakeColumnDefDescs(ctx, d, semaCtx, evalCtx)
			if err != nil {
				return nil, err
			}

			// Do not include virtual tables in these statistics.
			if !descpb.IsVirtualTable(id) {
				incTelemetryForNewColumn(d, col)
			}

			desc.AddColumn(col)
			if d.HasDefaultExpr() {
				// This resolution must be delayed until ColumnIDs have been populated.
				columnDefaultExprs[i] = expr
			} else {
				columnDefaultExprs[i] = nil
			}

			if idx != nil {
				idx.Version = indexEncodingVersion
				if err := desc.AddIndex(*idx, d.PrimaryKey.IsPrimaryKey); err != nil {
					return nil, err
				}
			}

			if d.HasColumnFamily() {
				// Pass true for `create` and `ifNotExists` because when we're creating
				// a table, we always want to create the specified family if it doesn't
				// exist.
				err := desc.AddColumnToFamilyMaybeCreate(col.Name, string(d.Family.Name), true, true)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	// Now that we've constructed our columns, we pop into any of our computed
	// columns so that we can dequalify any column references.
	sourceInfo := colinfo.NewSourceInfoForSingleTable(
		n.Table, colinfo.ResultColumnsFromColDescs(desc.GetID(), desc.Columns),
	)

	for i := range desc.Columns {
		col := &desc.Columns[i]
		if col.IsComputed() {
			expr, err := parser.ParseExpr(*col.ComputeExpr)
			if err != nil {
				return nil, err
			}

			deqExpr, err := schemaexpr.DequalifyColumnRefs(ctx, sourceInfo, expr)
			if err != nil {
				return nil, err
			}
			col.ComputeExpr = &deqExpr
		}
	}

	var primaryIndexColumnSet map[string]struct{}
	setupShardedIndexForNewTable := func(d *tree.IndexTableDef, idx *descpb.IndexDescriptor) error {
		if n.PartitionBy != nil {
			return pgerror.New(pgcode.FeatureNotSupported, "sharded indexes don't support partitioning")
		}
		shardCol, newColumn, err := setupShardedIndex(
			ctx,
			evalCtx,
			semaCtx,
			sessionData.HashShardedIndexesEnabled,
			&d.Columns,
			d.Sharded.ShardBuckets,
			&desc,
			idx,
			true /* isNewTable */)
		if err != nil {
			return err
		}
		if newColumn {
			buckets, err := tabledesc.EvalShardBucketCount(ctx, semaCtx, evalCtx, d.Sharded.ShardBuckets)
			if err != nil {
				return err
			}
			checkConstraint, err := makeShardCheckConstraintDef(&desc, int(buckets), shardCol)
			if err != nil {
				return err
			}
			n.Defs = append(n.Defs, checkConstraint)
			columnDefaultExprs = append(columnDefaultExprs, nil)
		}
		return nil
	}
	idxValidator := schemaexpr.MakeIndexPredicateValidator(ctx, n.Table, &desc, semaCtx)
	for _, def := range n.Defs {
		switch d := def.(type) {
		case *tree.ColumnTableDef, *tree.LikeTableDef:
			// pass, handled above.

		case *tree.IndexTableDef:
			idx := descpb.IndexDescriptor{
				Name:             string(d.Name),
				StoreColumnNames: d.Storing.ToStrings(),
				Version:          indexEncodingVersion,
			}
			if d.Inverted {
				if !sessionData.EnableMultiColumnInvertedIndexes && len(d.Columns) > 1 {
					return nil, pgerror.New(pgcode.FeatureNotSupported, "indexing more than one column with an inverted index is not supported")
				}
				idx.Type = descpb.IndexDescriptor_INVERTED
			}
			if d.Sharded != nil {
				if d.Interleave != nil {
					return nil, pgerror.New(pgcode.FeatureNotSupported, "interleaved indexes cannot also be hash sharded")
				}
				if err := setupShardedIndexForNewTable(d, &idx); err != nil {
					return nil, err
				}
			}
			if err := idx.FillColumns(d.Columns); err != nil {
				return nil, err
			}
			if d.Inverted {
				columnDesc, _, err := desc.FindColumnByName(tree.Name(idx.InvertedColumnName()))
				if err != nil {
					return nil, err
				}
				switch columnDesc.Type.Family() {
				case types.GeometryFamily:
					config, err := geoindex.GeometryIndexConfigForSRID(columnDesc.Type.GeoSRIDOrZero())
					if err != nil {
						return nil, err
					}
					idx.GeoConfig = *config
				case types.GeographyFamily:
					idx.GeoConfig = *geoindex.DefaultGeographyIndexConfig()
				}
			}
			if d.PartitionBy != nil {
				partitioning, err := CreatePartitioning(ctx, st, evalCtx, &desc, &idx, d.PartitionBy)
				if err != nil {
					return nil, err
				}
				idx.Partitioning = partitioning
			}
			if d.Predicate != nil {
				expr, err := idxValidator.Validate(d.Predicate)
				if err != nil {
					return nil, err
				}
				idx.Predicate = expr
				telemetry.Inc(sqltelemetry.PartialIndexCounter)
			}
			if err := paramparse.ApplyStorageParameters(
				ctx,
				semaCtx,
				evalCtx,
				d.StorageParams,
				&paramparse.IndexStorageParamObserver{IndexDesc: &idx},
			); err != nil {
				return nil, err
			}

			if err := desc.AddIndex(idx, false); err != nil {
				return nil, err
			}
			if d.Interleave != nil {
				return nil, unimplemented.NewWithIssue(9148, "use CREATE INDEX to make interleaved indexes")
			}
		case *tree.UniqueConstraintTableDef:
			if d.WithoutIndex {
				return nil, pgerror.New(pgcode.FeatureNotSupported,
					"unique constraints without an index are not yet supported",
				)
			}
			idx := descpb.IndexDescriptor{
				Name:             string(d.Name),
				Unique:           true,
				StoreColumnNames: d.Storing.ToStrings(),
				Version:          indexEncodingVersion,
			}
			if d.Sharded != nil {
				if n.Interleave != nil && d.PrimaryKey {
					return nil, pgerror.New(pgcode.FeatureNotSupported, "interleaved indexes cannot also be hash sharded")
				}
				if err := setupShardedIndexForNewTable(&d.IndexTableDef, &idx); err != nil {
					return nil, err
				}
			}
			if err := idx.FillColumns(d.Columns); err != nil {
				return nil, err
			}
			if d.PartitionBy != nil {
				partitioning, err := CreatePartitioning(ctx, st, evalCtx, &desc, &idx, d.PartitionBy)
				if err != nil {
					return nil, err
				}
				idx.Partitioning = partitioning
			}
			if d.Predicate != nil {
				expr, err := idxValidator.Validate(d.Predicate)
				if err != nil {
					return nil, err
				}
				idx.Predicate = expr
				telemetry.Inc(sqltelemetry.PartialIndexCounter)
			}
			if err := desc.AddIndex(idx, d.PrimaryKey); err != nil {
				return nil, err
			}
			if d.PrimaryKey {
				if d.Interleave != nil {
					return nil, unimplemented.NewWithIssue(
						45710,
						"interleave not supported in primary key constraint definition",
					)
				}
				primaryIndexColumnSet = make(map[string]struct{})
				for _, c := range d.Columns {
					primaryIndexColumnSet[string(c.Column)] = struct{}{}
				}
			}
			if d.Interleave != nil {
				return nil, unimplemented.NewWithIssue(9148, "use CREATE INDEX to make interleaved indexes")
			}
		case *tree.CheckConstraintTableDef, *tree.ForeignKeyConstraintTableDef, *tree.FamilyTableDef:
			// pass, handled below.

		default:
			return nil, errors.Errorf("unsupported table def: %T", def)
		}
	}

	// If explicit primary keys are required, error out since a primary key was not supplied.
	if len(desc.PrimaryIndex.ColumnNames) == 0 && desc.IsPhysicalTable() && evalCtx != nil &&
		evalCtx.SessionData != nil && evalCtx.SessionData.RequireExplicitPrimaryKeys {
		return nil, errors.Errorf(
			"no primary key specified for table %s (require_explicit_primary_keys = true)", desc.Name)
	}

	if primaryIndexColumnSet != nil {
		// Primary index columns are not nullable.
		for i := range desc.Columns {
			if _, ok := primaryIndexColumnSet[desc.Columns[i].Name]; ok {
				desc.Columns[i].Nullable = false
			}
		}
	}

	// Now that all columns are in place, add any explicit families (this is done
	// here, rather than in the constraint pass below since we want to pick up
	// explicit allocations before AllocateIDs adds implicit ones).
	columnsInExplicitFamilies := map[string]bool{}
	for _, def := range n.Defs {
		if d, ok := def.(*tree.FamilyTableDef); ok {
			fam := descpb.ColumnFamilyDescriptor{
				Name:        string(d.Name),
				ColumnNames: d.Columns.ToStrings(),
			}
			for _, c := range fam.ColumnNames {
				columnsInExplicitFamilies[c] = true
			}
			desc.AddFamily(fam)
		}
	}

	// Assign any implicitly added shard columns to the column family of the first column
	// in their corresponding set of index columns.
	for _, index := range desc.AllNonDropIndexes() {
		if index.IsSharded() && !columnsInExplicitFamilies[index.Sharded.Name] {
			// Ensure that the shard column wasn't explicitly assigned a column family
			// during table creation (this will happen when a create statement is
			// "roundtripped", for example).
			family := tabledesc.GetColumnFamilyForShard(&desc, index.Sharded.ColumnNames)
			if family != "" {
				if err := desc.AddColumnToFamilyMaybeCreate(index.Sharded.Name, family, false, false); err != nil {
					return nil, err
				}
			}
		}
	}

	if err := desc.AllocateIDs(ctx); err != nil {
		return nil, err
	}

	for i := range desc.Indexes {
		idx := &desc.Indexes[i]
		// Increment the counter if this index could be storing data across multiple column families.
		if len(idx.StoreColumnNames) > 1 && len(desc.Families) > 1 {
			telemetry.Inc(sqltelemetry.SecondaryIndexColumnFamiliesCounter)
		}
	}

	if n.Interleave != nil {
		if err := addInterleave(ctx, txn, vt, &desc, &desc.PrimaryIndex, n.Interleave); err != nil {
			return nil, err
		}
	}

	if n.PartitionBy != nil {
		partitioning, err := CreatePartitioning(
			ctx, st, evalCtx, &desc, &desc.PrimaryIndex, n.PartitionBy)
		if err != nil {
			return nil, err
		}
		desc.PrimaryIndex.Partitioning = partitioning
	}

	// Once all the IDs have been allocated, we can add the Sequence dependencies
	// as maybeAddSequenceDependencies requires ColumnIDs to be correct.
	// Elements in n.Defs are not necessarily column definitions, so use a separate
	// counter to map ColumnDefs to columns.
	colIdx := 0
	for i := range n.Defs {
		if _, ok := n.Defs[i].(*tree.ColumnTableDef); ok {
			if expr := columnDefaultExprs[i]; expr != nil {
				changedSeqDescs, err := maybeAddSequenceDependencies(ctx, vt, &desc, &desc.Columns[colIdx], expr, affected)
				if err != nil {
					return nil, err
				}
				for _, changedSeqDesc := range changedSeqDescs {
					affected[changedSeqDesc.ID] = changedSeqDesc
				}
			}
			colIdx++
		}
	}

	// With all structural elements in place and IDs allocated, we can resolve the
	// constraints and qualifications.
	// FKs are resolved after the descriptor is otherwise complete and IDs have
	// been allocated since the FKs will reference those IDs. Resolution also
	// accumulates updates to other tables (adding backreferences) in the passed
	// map -- anything in that map should be saved when the table is created.
	//

	// We use a fkSelfResolver so that name resolution can find the newly created
	// table.
	fkResolver := &fkSelfResolver{
		SchemaResolver: vt,
		newTableDesc:   &desc,
		newTableName:   &n.Table,
	}

	ckBuilder := schemaexpr.MakeCheckConstraintBuilder(ctx, n.Table, &desc, semaCtx)
	for _, def := range n.Defs {
		switch d := def.(type) {
		case *tree.ColumnTableDef:
			// Check after all ResolveFK calls.

		case *tree.IndexTableDef, *tree.UniqueConstraintTableDef, *tree.FamilyTableDef, *tree.LikeTableDef:
			// Pass, handled above.

		case *tree.CheckConstraintTableDef:
			ck, err := ckBuilder.Build(d)
			if err != nil {
				return nil, err
			}
			desc.Checks = append(desc.Checks, ck)

		case *tree.ForeignKeyConstraintTableDef:
			if err := ResolveFK(
				ctx, txn, fkResolver, &desc, d, affected, NewTable, tree.ValidationDefault, evalCtx,
			); err != nil {
				return nil, err
			}

		default:
			return nil, errors.Errorf("unsupported table def: %T", def)
		}
	}

	// Now that we have all the other columns set up, we can validate
	// any computed columns.
	computedColValidator := schemaexpr.MakeComputedColumnValidator(
		ctx,
		&desc,
		semaCtx,
		&n.Table,
	)
	for _, def := range n.Defs {
		switch d := def.(type) {
		case *tree.ColumnTableDef:
			if d.IsComputed() {
				if err := computedColValidator.Validate(d); err != nil {
					return nil, err
				}
			}
		}
	}

	// AllocateIDs mutates its receiver. `return desc, desc.AllocateIDs()`
	// happens to work in gc, but does not work in gccgo.
	//
	// See https://github.com/golang/go/issues/23188.
	if err := desc.AllocateIDs(ctx); err != nil {
		return nil, err
	}

	// Record the types of indexes that the table has.
	if err := desc.ForeachNonDropIndex(func(idx *descpb.IndexDescriptor) error {
		if idx.IsSharded() {
			telemetry.Inc(sqltelemetry.HashShardedIndexCounter)
		}
		if idx.Type == descpb.IndexDescriptor_INVERTED {
			telemetry.Inc(sqltelemetry.InvertedIndexCounter)
			if !geoindex.IsEmptyConfig(&idx.GeoConfig) {
				if geoindex.IsGeographyConfig(&idx.GeoConfig) {
					telemetry.Inc(sqltelemetry.GeographyInvertedIndexCounter)
				} else if geoindex.IsGeometryConfig(&idx.GeoConfig) {
					telemetry.Inc(sqltelemetry.GeometryInvertedIndexCounter)
				}
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	if n.Locality != nil {
		db, err := catalogkv.MustGetDatabaseDescByID(ctx, txn, evalCtx.Codec, parentID)
		if err != nil {
			return nil, errors.Wrap(err, "error fetching database descriptor for locality checks")
		}

		desc.LocalityConfig = &descpb.TableDescriptor_LocalityConfig{}
		switch n.Locality.LocalityLevel {
		case tree.LocalityLevelGlobal:
			desc.LocalityConfig.Locality = &descpb.TableDescriptor_LocalityConfig_Global_{
				Global: &descpb.TableDescriptor_LocalityConfig_Global{},
			}
		case tree.LocalityLevelTable:
			l := &descpb.TableDescriptor_LocalityConfig_RegionalByTable_{
				RegionalByTable: &descpb.TableDescriptor_LocalityConfig_RegionalByTable{},
			}
			if n.Locality.TableRegion != "" {
				region := descpb.Region(n.Locality.TableRegion)
				l.RegionalByTable.Region = &region
			}
			desc.LocalityConfig.Locality = l
		case tree.LocalityLevelRow:
			desc.LocalityConfig.Locality = &descpb.TableDescriptor_LocalityConfig_RegionalByRow_{
				RegionalByRow: &descpb.TableDescriptor_LocalityConfig_RegionalByRow{},
			}
		default:
			return nil, errors.Newf("unknown locality level: %v", n.Locality.LocalityLevel)
		}

		if err := tabledesc.ValidateTableLocalityConfig(
			n.Table.Table(),
			desc.LocalityConfig,
			db,
		); err != nil {
			return nil, err
		}
	}

	return &desc, nil
}

// newTableDesc creates a table descriptor from a CreateTable statement.
func newTableDesc(
	params runParams,
	n *tree.CreateTable,
	parentID, parentSchemaID, id descpb.ID,
	creationTime hlc.Timestamp,
	privileges *descpb.PrivilegeDescriptor,
	affected map[descpb.ID]*tabledesc.Mutable,
) (ret *tabledesc.Mutable, err error) {
	// Process any SERIAL columns to remove the SERIAL type,
	// as required by NewTableDesc.
	createStmt := n
	ensureCopy := func() {
		if createStmt == n {
			newCreateStmt := *n
			n.Defs = append(tree.TableDefs(nil), n.Defs...)
			createStmt = &newCreateStmt
		}
	}
	newDefs, err := replaceLikeTableOpts(n, params)
	if err != nil {
		return nil, err
	}

	if newDefs != nil {
		// If we found any LIKE table defs, we actually modified the list of
		// defs during iteration, so we re-assign the resultant list back to
		// n.Defs.
		n.Defs = newDefs
	}

	for i, def := range n.Defs {
		d, ok := def.(*tree.ColumnTableDef)
		if !ok {
			continue
		}
		newDef, seqDbDesc, seqName, seqOpts, err := params.p.processSerialInColumnDef(params.ctx, d, &n.Table)
		if err != nil {
			return nil, err
		}
		// TODO (lucy): Have more consistent/informative names for dependent jobs.
		if seqName != nil {
			if err := doCreateSequence(
				params,
				n.String(),
				seqDbDesc,
				parentSchemaID,
				seqName,
				n.Persistence,
				seqOpts,
				fmt.Sprintf("creating sequence %s for new table %s", seqName, n.Table.Table()),
			); err != nil {
				return nil, err
			}
		}
		if d != newDef {
			ensureCopy()
			n.Defs[i] = newDef
		}
	}

	// We need to run NewTableDesc with caching disabled, because
	// it needs to pull in descriptors from FK depended-on tables
	// and interleaved parents using their current state in KV.
	// See the comment at the start of NewTableDesc() and ResolveFK().
	params.p.runWithOptions(resolveFlags{skipCache: true, contextDatabaseID: parentID}, func() {
		ret, err = NewTableDesc(
			params.ctx,
			params.p.txn,
			params.p,
			params.p.ExecCfg().Settings,
			n,
			parentID,
			parentSchemaID,
			id,
			creationTime,
			privileges,
			affected,
			&params.p.semaCtx,
			params.EvalContext(),
			params.SessionData(),
			n.Persistence,
		)
	})

	return ret, err
}

// replaceLikeTableOps processes the TableDefs in the input CreateTableNode,
// searching for LikeTableDefs. If any are found, each LikeTableDef will be
// replaced in the output tree.TableDefs (which will be a copy of the input
// node's TableDefs) by an equivalent set of TableDefs pulled from the
// LikeTableDef's target table.
// If no LikeTableDefs are found, the output tree.TableDefs will be nil.
func replaceLikeTableOpts(n *tree.CreateTable, params runParams) (tree.TableDefs, error) {
	var newDefs tree.TableDefs
	for i, def := range n.Defs {
		d, ok := def.(*tree.LikeTableDef)
		if !ok {
			if newDefs != nil {
				newDefs = append(newDefs, def)
			}
			continue
		}
		// We're definitely going to be editing n.Defs now, so make a copy of it.
		if newDefs == nil {
			newDefs = make(tree.TableDefs, 0, len(n.Defs))
			newDefs = append(newDefs, n.Defs[:i]...)
		}
		td, err := params.p.ResolveMutableTableDescriptor(params.ctx, &d.Name, true, tree.ResolveRequireTableDesc)
		if err != nil {
			return nil, err
		}
		opts := tree.LikeTableOpt(0)
		// Process ons / offs.
		for _, opt := range d.Options {
			if opt.Excluded {
				opts &^= opt.Opt
			} else {
				opts |= opt.Opt
			}
		}

		defs := make(tree.TableDefs, 0)
		// Add all columns. Columns are always added.
		for i := range td.Columns {
			c := &td.Columns[i]
			if c.Hidden {
				// Hidden columns automatically get added by the system; we don't need
				// to add them ourselves here.
				continue
			}
			def := tree.ColumnTableDef{
				Name: tree.Name(c.Name),
				Type: c.Type,
			}
			if c.Nullable {
				def.Nullable.Nullability = tree.Null
			} else {
				def.Nullable.Nullability = tree.NotNull
			}
			if c.DefaultExpr != nil {
				if opts.Has(tree.LikeTableOptDefaults) {
					def.DefaultExpr.Expr, err = parser.ParseExpr(*c.DefaultExpr)
					if err != nil {
						return nil, err
					}
				}
			}
			if c.ComputeExpr != nil {
				if opts.Has(tree.LikeTableOptGenerated) {
					def.Computed.Computed = true
					def.Computed.Expr, err = parser.ParseExpr(*c.ComputeExpr)
					if err != nil {
						return nil, err
					}
				}
			}
			defs = append(defs, &def)
		}
		if opts.Has(tree.LikeTableOptConstraints) {
			for _, c := range td.Checks {
				def := tree.CheckConstraintTableDef{
					Name:   tree.Name(c.Name),
					Hidden: c.Hidden,
				}
				def.Expr, err = parser.ParseExpr(c.Expr)
				if err != nil {
					return nil, err
				}
				defs = append(defs, &def)
			}
		}
		if opts.Has(tree.LikeTableOptIndexes) {
			for _, idx := range td.AllNonDropIndexes() {
				indexDef := tree.IndexTableDef{
					Name:     tree.Name(idx.Name),
					Inverted: idx.Type == descpb.IndexDescriptor_INVERTED,
					Storing:  make(tree.NameList, 0, len(idx.StoreColumnNames)),
					Columns:  make(tree.IndexElemList, 0, len(idx.ColumnNames)),
				}
				columnNames := idx.ColumnNames
				if idx.IsSharded() {
					indexDef.Sharded = &tree.ShardedIndexDef{
						ShardBuckets: tree.NewDInt(tree.DInt(idx.Sharded.ShardBuckets)),
					}
					columnNames = idx.Sharded.ColumnNames
				}
				for i, name := range columnNames {
					elem := tree.IndexElem{
						Column:    tree.Name(name),
						Direction: tree.Ascending,
					}
					if idx.ColumnDirections[i] == descpb.IndexDescriptor_DESC {
						elem.Direction = tree.Descending
					}
					indexDef.Columns = append(indexDef.Columns, elem)
				}
				for _, name := range idx.StoreColumnNames {
					indexDef.Storing = append(indexDef.Storing, tree.Name(name))
				}
				var def tree.TableDef = &indexDef
				if idx.Unique {
					isPK := idx.ID == td.PrimaryIndex.ID
					if isPK && td.IsPrimaryIndexDefaultRowID() {
						continue
					}

					def = &tree.UniqueConstraintTableDef{
						IndexTableDef: indexDef,
						PrimaryKey:    isPK,
					}
				}
				if idx.IsPartial() {
					indexDef.Predicate, err = parser.ParseExpr(idx.Predicate)
					if err != nil {
						return nil, err
					}
				}
				defs = append(defs, def)
			}
		}
		newDefs = append(newDefs, defs...)
	}
	return newDefs, nil
}

// makeShardColumnDesc returns a new column descriptor for a hidden computed shard column
// based on all the `colNames`.
func makeShardColumnDesc(colNames []string, buckets int) (*descpb.ColumnDescriptor, error) {
	col := &descpb.ColumnDescriptor{
		Hidden:   true,
		Nullable: false,
		Type:     types.Int4,
	}
	col.Name = tabledesc.GetShardColumnName(colNames, int32(buckets))
	col.ComputeExpr = makeHashShardComputeExpr(colNames, buckets)
	return col, nil
}

// makeHashShardComputeExpr creates the serialized computed expression for a hash shard
// column based on the column names and the number of buckets. The expression will be
// of the form:
//
//    mod(fnv32(colNames[0]::STRING)+fnv32(colNames[1])+...,buckets)
//
func makeHashShardComputeExpr(colNames []string, buckets int) *string {
	unresolvedFunc := func(funcName string) tree.ResolvableFunctionReference {
		return tree.ResolvableFunctionReference{
			FunctionReference: &tree.UnresolvedName{
				NumParts: 1,
				Parts:    tree.NameParts{funcName},
			},
		}
	}
	hashedColumnExpr := func(colName string) tree.Expr {
		return &tree.FuncExpr{
			Func: unresolvedFunc("fnv32"),
			Exprs: tree.Exprs{
				// NB: We have created the hash shard column as NOT NULL so we need
				// to coalesce NULLs into something else. There's a variety of different
				// reasonable choices here. We could pick some outlandish value, we
				// could pick a zero value for each type, or we can do the simple thing
				// we do here, however the empty string seems pretty reasonable. At worst
				// we'll have a collision for every combination of NULLable string
				// columns. That seems just fine.
				&tree.CoalesceExpr{
					Name: "COALESCE",
					Exprs: tree.Exprs{
						&tree.CastExpr{
							Type: types.String,
							Expr: &tree.ColumnItem{ColumnName: tree.Name(colName)},
						},
						tree.NewDString(""),
					},
				},
			},
		}
	}

	// Construct an expression which is the sum of all of the casted and hashed
	// columns.
	var expr tree.Expr
	for i := len(colNames) - 1; i >= 0; i-- {
		c := colNames[i]
		if expr == nil {
			expr = hashedColumnExpr(c)
		} else {
			expr = &tree.BinaryExpr{
				Left:     hashedColumnExpr(c),
				Operator: tree.Plus,
				Right:    expr,
			}
		}
	}
	str := tree.Serialize(&tree.FuncExpr{
		Func: unresolvedFunc("mod"),
		Exprs: tree.Exprs{
			expr,
			tree.NewDInt(tree.DInt(buckets)),
		},
	})
	return &str
}

func makeShardCheckConstraintDef(
	desc *tabledesc.Mutable, buckets int, shardCol *descpb.ColumnDescriptor,
) (*tree.CheckConstraintTableDef, error) {
	values := &tree.Tuple{}
	for i := 0; i < buckets; i++ {
		const negative = false
		values.Exprs = append(values.Exprs, tree.NewNumVal(
			constant.MakeInt64(int64(i)),
			strconv.Itoa(i),
			negative))
	}
	return &tree.CheckConstraintTableDef{
		Expr: &tree.ComparisonExpr{
			Operator: tree.In,
			Left: &tree.ColumnItem{
				ColumnName: tree.Name(shardCol.Name),
			},
			Right: values,
		},
		Hidden: true,
	}, nil
}

// incTelemetryForNewColumn increments relevant telemetry every time a new column
// is added to a table.
func incTelemetryForNewColumn(def *tree.ColumnTableDef, desc *descpb.ColumnDescriptor) {
	switch desc.Type.Family() {
	case types.EnumFamily:
		sqltelemetry.IncrementEnumCounter(sqltelemetry.EnumInTable)
	default:
		telemetry.Inc(sqltelemetry.SchemaNewTypeCounter(desc.Type.TelemetryName()))
	}
	if desc.IsComputed() {
		telemetry.Inc(sqltelemetry.SchemaNewColumnTypeQualificationCounter("computed"))
	}
	if desc.HasDefault() {
		telemetry.Inc(sqltelemetry.SchemaNewColumnTypeQualificationCounter("default_expr"))
	}
	if def.Unique.IsUnique {
		if def.Unique.WithoutIndex {
			telemetry.Inc(sqltelemetry.SchemaNewColumnTypeQualificationCounter("unique_without_index"))
		} else {
			telemetry.Inc(sqltelemetry.SchemaNewColumnTypeQualificationCounter("unique"))
		}
	}
}

// CreateInheritedPrivilegesFromDBDesc creates privileges with the appropriate
// owner (node for system, the restoring user otherwise.)
func CreateInheritedPrivilegesFromDBDesc(
	dbDesc catalog.DatabaseDescriptor, user security.SQLUsername,
) *descpb.PrivilegeDescriptor {
	// If a new system table is being created (which should only be doable by
	// an internal user account), make sure it gets the correct privileges.
	if dbDesc.GetID() == keys.SystemDatabaseID {
		return descpb.NewDefaultPrivilegeDescriptor(security.NodeUserName())
	}

	privs := dbDesc.GetPrivileges()
	privs.SetOwner(user)

	return privs
}
