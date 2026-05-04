// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package descs

import (
	"context"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/internal/catkv"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/schemadesc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catconstants"
)

// getTemporarySchemaByName resolves a "pg_temp_*" schema name against this
// session's temporary-schema mapping (DatabaseIDToTempSchemaID +
// TemporarySchemaName). When session data records a schemaID for dbID, the
// function verifies the namespace entry still exists before returning a
// synthetic descriptor. Without this check a rolled-back CREATE TEMP would
// leave a stale schemaID in session data and produce a phantom-schema
// reference (#168966). Detection of temporary schemas owned by *other*
// sessions is handled by getOtherSessionTemporarySchemaByID, not here.
//
// The avoidFurtherLookups return is a hint to the caller's lookup chain:
//   - true means this session owns the name and no other layer should be
//     consulted (either the descriptor is returned, or session data
//     authoritatively says no temp schema is registered for dbID).
//   - false means the caller should keep looking, either because the name
//     doesn't belong to this session at all or because the session-data
//     mapping turned out to be stale and lookupStoredID needs to confirm
//     against the namespace table.
func (tc *Collection) getTemporarySchemaByName(
	ctx context.Context, txn *kv.Txn, dbID descpb.ID, schemaName string,
) (avoidFurtherLookups bool, _ catalog.SchemaDescriptor, _ error) {
	// If a temp schema is requested, check if it's for the current session, or
	// else fall back to reading from the store.
	if !tc.temporarySchemaProvider.HasTemporarySchema() {
		return false, nil, nil
	}
	tempSchemaName := tc.temporarySchemaProvider.GetTemporarySchemaName()
	if schemaName != catconstants.PgTempSchemaName && schemaName != tempSchemaName {
		return false, nil, nil
	}
	schemaID := tc.temporarySchemaProvider.GetTemporarySchemaIDForDB(dbID)
	if schemaID == descpb.InvalidID {
		return true, nil, nil
	}
	// Verify the namespace entry recorded in session data still exists. If a
	// previous CREATE TEMP ... txn rolled back, the schemaID survives in
	// session data but the namespace entry does not. Returning a synthetic
	// descriptor in that case produces a phantom-schema reference that breaks
	// subsequent CREATE TEMP statements (#168966).
	exists, err := tc.tempSchemaNamespaceEntryExists(ctx, txn, dbID, schemaID, tempSchemaName)
	if err != nil {
		return false, nil, err
	}
	if !exists {
		// Fall through to lookupStoredID, which will agree the entry is gone.
		return false, nil, nil
	}
	return true, schemadesc.NewTemporarySchema(
		tempSchemaName,
		schemaID,
		dbID,
	), nil
}

// getTemporarySchemaByID returns the schema descriptor if it is temporary and
// belongs to the current session.
//
// Unlike getTemporarySchemaByName, this path is not staleness-guarded: callers
// that obtained schemaID from a live catalog reference are expected to be
// looking at consistent state, and the by-ID lookup has no fall-through layer
// equivalent to lookupStoredID that we could defer to on a false negative.
func (tc *Collection) getTemporarySchemaByID(schemaID descpb.ID) catalog.SchemaDescriptor {
	dbID := tc.temporarySchemaProvider.MaybeGetDatabaseForTemporarySchemaID(schemaID)
	if dbID == descpb.InvalidID {
		return nil
	}
	return schemadesc.NewTemporarySchema(
		tc.temporarySchemaProvider.GetTemporarySchemaName(),
		schemaID,
		dbID,
	)
}

// tempSchemaNamespaceEntryExists checks system.namespace via the supplied txn
// for the temporary-schema name entry described by (dbID, schemaID,
// tempSchemaName). It returns true iff that exact entry exists.
//
// We bypass tc.cr (the cached catalog reader) deliberately: the cached reader
// memoizes by-name lookups, so a prior negative lookup of "pg_temp_xxx" (for
// example from a CREATE TEMP that hadn't yet written its namespace entry)
// would short-circuit a subsequent read even after the fresh insert in this
// same txn. It also wouldn't reflect in-txn buffered deletes of the entry.
// The uncached reader does a real KV Get on the namespace key against the
// supplied txn, which sees both inserts and deletes buffered in this txn.
//
// The cost is one KV Get per call. The function only fires when
// getTemporarySchemaByName is resolving a "pg_temp_*" name and session data
// already records a schemaID for the database, i.e. only on temp-schema name
// resolution, which is rare outside of sessions that use temporary objects.
func (tc *Collection) tempSchemaNamespaceEntryExists(
	ctx context.Context, txn *kv.Txn, dbID, schemaID descpb.ID, tempSchemaName string,
) (bool, error) {
	ni := descpb.NameInfo{ParentID: dbID, Name: tempSchemaName}
	uncached := catkv.NewUncachedCatalogReader(tc.codec())
	read, err := uncached.GetByNames(ctx, txn, []descpb.NameInfo{ni})
	if err != nil {
		return false, err
	}
	e := read.LookupNamespaceEntry(ni)
	return e != nil && e.GetID() == schemaID, nil
}

// getOtherSessionTemporarySchemaByID checks the catalog reader's cached
// namespace entries for a temporary schema from another session with the
// given ID. This is a fallback for when getTemporarySchemaByID returns nil
// because the schema doesn't belong to the current session.
func (tc *Collection) getOtherSessionTemporarySchemaByID(id descpb.ID) catalog.SchemaDescriptor {
	e := tc.cr.Cache().LookupNamespaceEntryByID(id)
	if e == nil {
		return nil
	}
	// Verify this is actually a schema namespace entry: it must have a valid
	// parent database ID and no parent schema ID.
	if e.GetParentID() == descpb.InvalidID || e.GetParentSchemaID() != descpb.InvalidID {
		return nil
	}
	if !strings.HasPrefix(e.GetName(), catconstants.PgTempSchemaName) {
		return nil
	}
	return schemadesc.NewTemporarySchema(e.GetName(), id, e.GetParentID())
}
