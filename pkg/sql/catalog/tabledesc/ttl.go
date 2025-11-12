// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tabledesc

import (
	"time"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/schemaexpr"
	"github.com/cockroachdb/cockroach/pkg/sql/parserutils"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/duration"
	"github.com/cockroachdb/errors"
	"github.com/robfig/cron/v3"
)

// ValidateRowLevelTTL validates that the TTL options are valid.
func ValidateRowLevelTTL(ttl *catpb.RowLevelTTL) error {
	if ttl == nil {
		return nil
	}
	if !ttl.HasDurationExpr() && !ttl.HasExpirationExpr() {
		return pgerror.Newf(
			pgcode.InvalidParameterValue,
			`"ttl_expire_after" and/or "ttl_expiration_expression" must be set`,
		)
	}
	if ttl.DeleteBatchSize != 0 {
		if err := ValidateTTLBatchSize("ttl_delete_batch_size", ttl.DeleteBatchSize); err != nil {
			return err
		}
	}
	if ttl.SelectBatchSize != 0 {
		if err := ValidateTTLBatchSize("ttl_select_batch_size", ttl.SelectBatchSize); err != nil {
			return err
		}
	}
	if ttl.DeletionCron != "" {
		if err := ValidateTTLCronExpr("ttl_job_cron", ttl.DeletionCron); err != nil {
			return err
		}
	}
	if ttl.SelectRateLimit != 0 {
		if err := ValidateTTLRateLimit("ttl_select_rate_limit", ttl.SelectRateLimit); err != nil {
			return err
		}
	}
	if ttl.DeleteRateLimit != 0 {
		if err := ValidateTTLRateLimit("ttl_delete_rate_limit", ttl.DeleteRateLimit); err != nil {
			return err
		}
	}
	if ttl.RowStatsPollInterval != 0 {
		if err := ValidateTTLRowStatsPollInterval("ttl_row_stats_poll_interval", ttl.RowStatsPollInterval); err != nil {
			return err
		}
	}
	return nil
}

// ValidateTTLExpirationExpr validates that the ttl_expiration_expression, if
// any, only references existing columns.
func ValidateTTLExpirationExpr(desc catalog.TableDescriptor) error {
	if !desc.HasRowLevelTTL() {
		return nil
	}
	expirationExpr := desc.GetRowLevelTTL().ExpirationExpr
	if expirationExpr == "" {
		return nil
	}
	exprs, err := parserutils.ParseExprs([]string{string(expirationExpr)})
	if err != nil {
		return errors.Wrapf(err, "ttl_expiration_expression %q must be a valid expression", expirationExpr)
	} else if len(exprs) != 1 {
		return errors.Newf(
			`ttl_expiration_expression %q must be a single expression`,
			expirationExpr,
		)
	}
	// Ideally, we would also call schemaexpr.ValidateTTLExpirationExpression
	// here, but that requires a SemaCtx which we don't have here.
	valid, err := schemaexpr.HasValidColumnReferences(desc, exprs[0])
	if err != nil {
		return err
	}
	if !valid {
		return errors.Newf("row-level TTL expiration expression %q refers to unknown columns", expirationExpr)
	}
	return nil
}

// ValidateTTLExpirationColumn validates that the ttl_expire_after setting, if
// any, is in a valid state. It requires that the TTLDefaultExpirationColumn
// exists and has DEFAULT/ON UPDATE clauses.
func ValidateTTLExpirationColumn(desc catalog.TableDescriptor) error {
	if !desc.HasRowLevelTTL() {
		return nil
	}
	if !desc.GetRowLevelTTL().HasDurationExpr() {
		return nil
	}
	intervalExpr := desc.GetRowLevelTTL().DurationExpr
	col, err := catalog.MustFindColumnByTreeName(desc, catpb.TTLDefaultExpirationColumnName)
	if err != nil {
		return errors.Wrapf(err, "expected column %s", catpb.TTLDefaultExpirationColumnName)
	}
	expectedStr := `current_timestamp():::TIMESTAMPTZ + ` + string(intervalExpr)
	if col.GetDefaultExpr() != expectedStr {
		return pgerror.Newf(
			pgcode.InvalidTableDefinition,
			"expected DEFAULT expression of %s to be %s",
			catpb.TTLDefaultExpirationColumnName,
			expectedStr,
		)
	}
	if col.GetOnUpdateExpr() != expectedStr {
		return pgerror.Newf(
			pgcode.InvalidTableDefinition,
			"expected ON UPDATE expression of %s to be %s",
			catpb.TTLDefaultExpirationColumnName,
			expectedStr,
		)
	}

	return nil
}

// ValidateTTLBatchSize validates the batch size of a TTL.
func ValidateTTLBatchSize(key string, val int64) error {
	if val < 0 {
		return pgerror.Newf(
			pgcode.InvalidParameterValue,
			`"%s" must be at least 0`,
			key,
		)
	}
	return nil
}

// ValidateTTLCronExpr validates the cron expression of TTL.
func ValidateTTLCronExpr(key string, str string) error {
	if _, err := cron.ParseStandard(str); err != nil {
		return pgerror.Wrapf(
			err,
			pgcode.InvalidParameterValue,
			`invalid cron expression for "%s"`,
			key,
		)
	}
	return nil
}

// ValidateTTLRowStatsPollInterval validates the automatic statistics field
// of TTL.
func ValidateTTLRowStatsPollInterval(key string, val time.Duration) error {
	if val < 0 {
		return pgerror.Newf(
			pgcode.InvalidParameterValue,
			`"%s" must be at least 0`,
			key,
		)
	}
	return nil
}

// ValidateTTLRateLimit validates the rate limit parameters of TTL.
func ValidateTTLRateLimit(key string, val int64) error {
	if val < 0 {
		return pgerror.Newf(
			pgcode.InvalidParameterValue,
			`"%s" must be at least 0`,
			key,
		)
	}
	return nil
}

// ValidatePartitionTTL validates that the partition TTL options are valid.
func ValidatePartitionTTL(ttl *catpb.PartitionTTLConfig) error {
	if ttl == nil {
		return nil
	}
	// Only ttl_column is required; other fields have defaults.
	if ttl.ColumnName == "" {
		return pgerror.Newf(
			pgcode.InvalidParameterValue,
			`"ttl_column" must be set when using partition TTL`,
		)
	}

	// Validate duration relationships if all fields are set.
	// Note: Some fields may be empty at this point if defaults haven't been applied yet.
	if ttl.Retention != "" && ttl.Granularity != "" {
		retention, err := tree.ParseDInterval(duration.IntervalStyle_POSTGRES, ttl.Retention)
		if err != nil {
			return pgerror.Wrapf(
				err,
				pgcode.InvalidParameterValue,
				`"ttl_retention" must be a valid interval`,
			)
		}

		// Validate that retention is positive (check this first for better error messages).
		if retention.Duration.Compare(duration.MakeDuration(0, 0, 0)) <= 0 {
			return pgerror.Newf(
				pgcode.InvalidParameterValue,
				`"ttl_retention" must be greater than zero`,
			)
		}

		granularity, err := tree.ParseDInterval(duration.IntervalStyle_POSTGRES, ttl.Granularity)
		if err != nil {
			return pgerror.Wrapf(
				err,
				pgcode.InvalidParameterValue,
				`"ttl_granularity" must be a valid interval`,
			)
		}

		// Validate that granularity is positive.
		if granularity.Duration.Compare(duration.MakeDuration(0, 0, 0)) <= 0 {
			return pgerror.Newf(
				pgcode.InvalidParameterValue,
				`"ttl_granularity" must be greater than zero`,
			)
		}

		// Validate that granularity <= retention.
		if granularity.Duration.Compare(retention.Duration) > 0 {
			return pgerror.Newf(
				pgcode.InvalidParameterValue,
				`"ttl_granularity" (%s) must be less than or equal to "ttl_retention" (%s)`,
				ttl.Granularity,
				ttl.Retention,
			)
		}
	}

	// Validate lookahead if both granularity and lookahead are set.
	if ttl.Granularity != "" && ttl.Lookahead != "" {
		granularity, err := tree.ParseDInterval(duration.IntervalStyle_POSTGRES, ttl.Granularity)
		if err != nil {
			return pgerror.Wrapf(
				err,
				pgcode.InvalidParameterValue,
				`"ttl_granularity" must be a valid interval`,
			)
		}

		lookahead, err := tree.ParseDInterval(duration.IntervalStyle_POSTGRES, ttl.Lookahead)
		if err != nil {
			return pgerror.Wrapf(
				err,
				pgcode.InvalidParameterValue,
				`"ttl_lookahead" must be a valid interval`,
			)
		}

		// Validate that lookahead is positive (check this first for better error messages).
		if lookahead.Duration.Compare(duration.MakeDuration(0, 0, 0)) <= 0 {
			return pgerror.Newf(
				pgcode.InvalidParameterValue,
				`"ttl_lookahead" must be greater than zero`,
			)
		}

		// Validate that lookahead >= granularity/2.
		halfGranularity := granularity.Duration.Div(2)
		if lookahead.Duration.Compare(halfGranularity) < 0 {
			return pgerror.Newf(
				pgcode.InvalidParameterValue,
				`"ttl_lookahead" (%s) must be at least half of "ttl_granularity" (%s)`,
				ttl.Lookahead,
				ttl.Granularity,
			)
		}
	}

	// Validate that retention is positive if set.
	if ttl.Retention != "" {
		retention, err := tree.ParseDInterval(duration.IntervalStyle_POSTGRES, ttl.Retention)
		if err != nil {
			return pgerror.Wrapf(
				err,
				pgcode.InvalidParameterValue,
				`"ttl_retention" must be a valid interval`,
			)
		}

		if retention.Duration.Compare(duration.MakeDuration(0, 0, 0)) <= 0 {
			return pgerror.Newf(
				pgcode.InvalidParameterValue,
				`"ttl_retention" must be greater than zero`,
			)
		}
	}

	return nil
}

// ValidatePartitionTTLColumn validates that the partition TTL column exists,
// is of the correct type (TIMESTAMP/TIMESTAMPTZ), and is NOT NULL.
func ValidatePartitionTTLColumn(desc catalog.TableDescriptor) error {
	if desc.GetPartitionTTL() == nil {
		return nil
	}

	columnName := desc.GetPartitionTTL().ColumnName
	if columnName == "" {
		return nil
	}

	col, err := catalog.MustFindColumnByTreeName(desc, tree.Name(columnName))
	if err != nil {
		return pgerror.Wrapf(
			err,
			pgcode.InvalidTableDefinition,
			"ttl_column %q does not exist",
			columnName,
		)
	}

	// Check if the column is TIMESTAMP or TIMESTAMPTZ.
	if col.GetType().Family() != types.TimestampFamily && col.GetType().Family() != types.TimestampTZFamily {
		return pgerror.Newf(
			pgcode.InvalidTableDefinition,
			"ttl_column %q must be of type TIMESTAMP or TIMESTAMPTZ, got %s",
			columnName,
			col.GetType().String(),
		)
	}

	// Check if the column is NOT NULL.
	if col.IsNullable() {
		return pgerror.Newf(
			pgcode.InvalidTableDefinition,
			"ttl_column %q must be NOT NULL",
			columnName,
		)
	}

	return nil
}

// ValidatePartitionTTLNoInboundFKs validates that tables with partition TTL
// do not have inbound foreign key constraints.
func ValidatePartitionTTLNoInboundFKs(desc catalog.TableDescriptor) error {
	if desc.GetPartitionTTL() == nil {
		return nil
	}

	if len(desc.GetInboundFKs()) > 0 {
		return pgerror.Newf(
			pgcode.InvalidTableDefinition,
			"cannot enable partition TTL on a table with inbound foreign key constraints",
		)
	}

	return nil
}
