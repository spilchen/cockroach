// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package builtins_test

import (
	"context"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/cockroachdb/cockroach/pkg/sql/randgen"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/builtins"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/stretchr/testify/require"
)

func TestGeoBuiltinsInfo(t *testing.T) {
	defer leaktest.AfterTest(t)()
	testForBuiltin := func(builtinName string, builtinOverloads []tree.Overload) {
		t.Run(builtinName, func(t *testing.T) {
			for i, overload := range builtinOverloads {
				t.Run(strconv.Itoa(i+1), func(t *testing.T) {
					infoFirstLine := strings.Trim(strings.Split(overload.Info, "\n\n")[0], "\t\n ")
					require.True(t, infoFirstLine[len(infoFirstLine)-1] == '.', "first line of info must end with a `.` character")
					require.True(t, unicode.IsUpper(rune(infoFirstLine[0])), "first character of info start with an uppercase letter.")
				})
			}
		})
	}
	builtins.IterateGeoBuiltinOverloads(testForBuiltin)
}

// TestGeoBuiltinsPointEmptyArgs tests POINT EMPTY arguments do not cause panics.
func TestGeoBuiltinsPointEmptyArgs(t *testing.T) {
	defer leaktest.AfterTest(t)()

	emptyGeometry, err := tree.ParseDGeometry("POINT EMPTY")
	require.NoError(t, err)
	emptyGeography, err := tree.ParseDGeography("POINT EMPTY")
	require.NoError(t, err)

	rng := rand.New(rand.NewSource(0))
	testForBuiltin := func(builtinName string, builtinOverloads []tree.Overload) {
		t.Run(builtinName, func(t *testing.T) {
			for i, overload := range builtinOverloads {
				t.Run("overload_"+strconv.Itoa(i+1), func(t *testing.T) {
					for overloadIdx := 0; overloadIdx < overload.Types.Length(); overloadIdx++ {
						switch overload.Types.GetAt(overloadIdx).Family() {
						case types.GeometryFamily, types.GeographyFamily:
							t.Run("idx_"+strconv.Itoa(overloadIdx), func(t *testing.T) {
								var datums tree.Datums
								for i := 0; i < overload.Types.Length(); i++ {
									if i == overloadIdx {
										switch overload.Types.GetAt(i).Family() {
										case types.GeometryFamily:
											datums = append(datums, emptyGeometry)
										case types.GeographyFamily:
											datums = append(datums, emptyGeography)
										default:
											panic("unexpected condition")
										}
									} else {
										datums = append(datums, randgen.RandDatum(rng, overload.Types.GetAt(i), false))
									}
								}
								var call strings.Builder
								call.WriteString(builtinName)
								call.WriteByte('(')
								for i, arg := range datums {
									if i > 0 {
										call.WriteString(", ")
									}
									call.WriteString(arg.String())
								}
								call.WriteByte(')')
								t.Logf("calling: %s", call.String())
								if overload.Fn != nil {
									_, _ = overload.Fn.(eval.FnOverload)(context.Background(), &eval.Context{}, datums)
								} else if overload.Generator != nil {
									_, _ = overload.Generator.(eval.GeneratorOverload)(context.Background(), &eval.Context{}, datums)
								} else if overload.GeneratorWithExprs != nil {
									exprs := make(tree.Exprs, len(datums))
									for i := range datums {
										exprs[i] = datums[i]
									}
									_, _ = overload.GeneratorWithExprs.(eval.GeneratorWithExprsOverload)(context.Background(), &eval.Context{}, exprs)
								}
							})
						}
					}
				})
			}
		})
	}
	builtins.IterateGeoBuiltinOverloads(testForBuiltin)
}
