package backfill

import (
	"testing"

	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/stretchr/testify/require"
)

func TestStatementCreatesUniqueIndex(t *testing.T) {
	testCases := []struct {
		name       string
		statements []string
		expect     bool
	}{
		{
			name:       "non-unique create index",
			statements: []string{"CREATE INDEX idx ON t(a)"},
			expect:     false,
		},
		{
			name:       "create unique index",
			statements: []string{"CREATE UNIQUE INDEX idx ON t(a)"},
			expect:     true,
		},
		{
			name:       "alter table add unique constraint",
			statements: []string{"ALTER TABLE t ADD CONSTRAINT c UNIQUE (a)"},
			expect:     true,
		},
		{
			name:       "alter table add unique without index",
			statements: []string{"ALTER TABLE t ADD CONSTRAINT c UNIQUE WITHOUT INDEX (a)"},
			expect:     false,
		},
		{
			name:       "alter table alter primary key",
			statements: []string{"ALTER TABLE t ALTER PRIMARY KEY USING COLUMNS (a)"},
			expect:     true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var actual bool
			for _, stmt := range tc.statements {
				parsed, err := parser.ParseOne(stmt)
				require.NoError(t, err)
				if StatementCreatesUniqueIndex(parsed.AST) {
					actual = true
					break
				}
			}
			require.Equal(t, tc.expect, actual)
		})
	}
}
