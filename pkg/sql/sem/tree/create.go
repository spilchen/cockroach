// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in licenses/BSD-vitess.txt.

// Portions of this file are additionally subject to the following
// license and copyright.
//
// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// This code was derived from https://github.com/youtube/vitess.

package tree

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/sql/lexbase"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/idxtype"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/collatedstring"
	"github.com/cockroachdb/cockroach/pkg/util/pretty"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
	"golang.org/x/text/language"
)

// CreateDatabase represents a CREATE DATABASE statement.
type CreateDatabase struct {
	IfNotExists     bool
	Name            Name
	Template        string
	Encoding        string
	Collate         string
	CType           string
	ConnectionLimit int32
	PrimaryRegion   Name
	Regions         NameList
	SurvivalGoal    SurvivalGoal
	Placement       DataPlacement
	Owner           RoleSpec
	SuperRegion     SuperRegion
	SecondaryRegion Name
}

// Format implements the NodeFormatter interface.
func (node *CreateDatabase) Format(ctx *FmtCtx) {
	ctx.WriteString("CREATE DATABASE ")
	if node.IfNotExists {
		ctx.WriteString("IF NOT EXISTS ")
	}
	ctx.FormatNode(&node.Name)
	if node.Template != "" {
		// NB: the template is not currently edited out under FmtAnonymize,
		// because we don't support custom templates. If/when custom
		// templates are supported, this should call ctx.FormatNode
		// on the template expr.
		ctx.WriteString(" TEMPLATE = ")
		lexbase.EncodeSQLStringWithFlags(&ctx.Buffer, node.Template, ctx.flags.EncodeFlags())
	}
	if node.Encoding != "" {
		// NB: the encoding is not currently edited out under FmtAnonymize,
		// because we don't support custom encodings. If/when custom
		// encodings are supported, this should call ctx.FormatNode
		// on the encoding expr.
		ctx.WriteString(" ENCODING = ")
		lexbase.EncodeSQLStringWithFlags(&ctx.Buffer, node.Encoding, ctx.flags.EncodeFlags())
	}
	if node.Collate != "" {
		// NB: the collation is not currently edited out under FmtAnonymize,
		// because we don't support custom collations. If/when custom
		// collations are supported, this should call ctx.FormatNode
		// on the collation expr.
		ctx.WriteString(" LC_COLLATE = ")
		lexbase.EncodeSQLStringWithFlags(&ctx.Buffer, node.Collate, ctx.flags.EncodeFlags())
	}
	if node.CType != "" {
		// NB: the ctype (formatting customization) is not currently
		// edited out under FmtAnonymize, because we don't support custom
		// cutomizations. If/when custom customizations are supported,
		// this should call ctx.FormatNode on the ctype expr.
		ctx.WriteString(" LC_CTYPE = ")
		lexbase.EncodeSQLStringWithFlags(&ctx.Buffer, node.CType, ctx.flags.EncodeFlags())
	}
	if node.ConnectionLimit != -1 {
		ctx.WriteString(" CONNECTION LIMIT = ")
		if ctx.flags.HasFlags(FmtHideConstants) {
			ctx.WriteByte('0')
		} else {
			// NB: use ctx.FormatNode when the connection limit becomes an expression.
			ctx.WriteString(strconv.Itoa(int(node.ConnectionLimit)))
		}
	}
	if node.PrimaryRegion != "" {
		ctx.WriteString(" PRIMARY REGION ")
		ctx.FormatNode(&node.PrimaryRegion)
	}
	if node.Regions != nil {
		ctx.WriteString(" REGIONS = ")
		ctx.FormatNode(&node.Regions)
	}
	if node.SurvivalGoal != SurvivalGoalDefault {
		ctx.WriteString(" ")
		ctx.FormatNode(&node.SurvivalGoal)
	}
	if node.Placement != DataPlacementUnspecified {
		ctx.WriteString(" ")
		ctx.FormatNode(&node.Placement)
	}

	if node.Owner.Name != "" {
		ctx.WriteString(" OWNER = ")
		ctx.FormatNode(&node.Owner)
	}

	if node.SuperRegion.Name != "" {
		ctx.FormatNode(&node.SuperRegion)
	}

	if node.SecondaryRegion != "" {
		ctx.WriteString(" SECONDARY REGION ")
		ctx.FormatNode(&node.SecondaryRegion)
	}
}

// IndexElem represents a column with a direction in a CREATE INDEX statement.
type IndexElem struct {
	// Column is set if this is a simple column reference (the common case).
	Column Name
	// Expr is set if the index element is an expression (part of an expression
	// index). If set, Column is empty.
	Expr       Expr
	Direction  Direction
	NullsOrder NullsOrder

	// OpClass is set if an index element was created using a named opclass.
	OpClass Name
}

// Format implements the NodeFormatter interface.
func (node *IndexElem) Format(ctx *FmtCtx) {
	if node.Expr == nil {
		ctx.FormatNode(&node.Column)
	} else {
		// Expressions in indexes need an extra set of parens, unless they are a
		// simple function call.
		_, isFunc := node.Expr.(*FuncExpr)
		if !isFunc {
			ctx.WriteByte('(')
		}
		ctx.FormatNode(node.Expr)
		if !isFunc {
			ctx.WriteByte(')')
		}
	}
	if node.OpClass != "" {
		ctx.WriteByte(' ')
		ctx.WriteString(node.OpClass.String())
	}
	if node.Direction != DefaultDirection {
		ctx.WriteByte(' ')
		ctx.WriteString(node.Direction.String())
	}
	if node.NullsOrder != DefaultNullsOrder {
		ctx.WriteByte(' ')
		ctx.WriteString(node.NullsOrder.String())
	}
}

func (node *IndexElem) doc(p *PrettyCfg) pretty.Doc {
	var d pretty.Doc
	if node.Expr == nil {
		d = p.Doc(&node.Column)
	} else {
		// Expressions in indexes need an extra set of parens, unless they are a
		// simple function call.
		d = p.Doc(node.Expr)
		if _, isFunc := node.Expr.(*FuncExpr); !isFunc {
			d = p.bracket("(", d, ")")
		}
	}
	if node.OpClass != "" {
		d = pretty.ConcatSpace(d, pretty.Text(node.OpClass.String()))
	}
	if node.Direction != DefaultDirection {
		d = pretty.ConcatSpace(d, pretty.Keyword(node.Direction.String()))
	}
	if node.NullsOrder != DefaultNullsOrder {
		d = pretty.ConcatSpace(d, pretty.Keyword(node.NullsOrder.String()))
	}
	return d
}

// IndexElemList is list of IndexElem.
type IndexElemList []IndexElem

// Format pretty-prints the contained names separated by commas.
// Format implements the NodeFormatter interface.
func (l *IndexElemList) Format(ctx *FmtCtx) {
	for i := range *l {
		if i > 0 {
			ctx.WriteString(", ")
		}
		ctx.FormatNode(&(*l)[i])
	}
}

// doc is part of the docer interface.
func (l *IndexElemList) doc(p *PrettyCfg) pretty.Doc {
	if l == nil || len(*l) == 0 {
		return pretty.Nil
	}
	d := make([]pretty.Doc, len(*l))
	for i := range *l {
		d[i] = p.Doc(&(*l)[i])
	}
	return p.commaSeparated(d...)
}

type IndexInvisibility struct {
	Value         float64
	FloatProvided bool
}

// CreateIndex represents a CREATE INDEX statement.
type CreateIndex struct {
	Name        Name
	Table       TableName
	Unique      bool
	Type        idxtype.T
	IfNotExists bool
	Columns     IndexElemList
	Sharded     *ShardedIndexDef
	// Extra columns to be stored together with the indexed ones as an optimization
	// for improved reading performance.
	Storing          NameList
	PartitionByIndex *PartitionByIndex
	StorageParams    StorageParams
	Predicate        Expr
	Concurrently     bool
	Invisibility     IndexInvisibility
}

// Format implements the NodeFormatter interface.
func (node *CreateIndex) Format(ctx *FmtCtx) {
	// Please also update indexForDisplay function in
	// pkg/sql/catalog/catformat/index.go if there's any update to index
	// definition components.
	ctx.WriteString("CREATE ")
	if node.Unique {
		ctx.WriteString("UNIQUE ")
	}
	switch node.Type {
	case idxtype.INVERTED:
		ctx.WriteString("INVERTED ")
	case idxtype.VECTOR:
		ctx.WriteString("VECTOR ")
	}
	ctx.WriteString("INDEX ")
	if node.Concurrently {
		ctx.WriteString("CONCURRENTLY ")
	}
	if node.IfNotExists {
		ctx.WriteString("IF NOT EXISTS ")
	}
	if node.Name != "" {
		ctx.FormatNode(&node.Name)
		ctx.WriteByte(' ')
	}
	ctx.WriteString("ON ")
	ctx.FormatNode(&node.Table)

	ctx.WriteString(" (")
	ctx.FormatNode(&node.Columns)
	ctx.WriteByte(')')
	if node.Sharded != nil {
		ctx.FormatNode(node.Sharded)
	}
	if len(node.Storing) > 0 {
		ctx.WriteString(" STORING (")
		ctx.FormatNode(&node.Storing)
		ctx.WriteByte(')')
	}
	if node.PartitionByIndex != nil {
		ctx.FormatNode(node.PartitionByIndex)
	}
	if node.StorageParams != nil {
		ctx.WriteString(" WITH (")
		ctx.FormatNode(&node.StorageParams)
		ctx.WriteString(")")
	}
	if node.Predicate != nil {
		ctx.WriteString(" WHERE ")
		ctx.FormatNode(node.Predicate)
	}
	switch {
	case node.Invisibility.FloatProvided:
		ctx.WriteString(" VISIBILITY " + fmt.Sprintf("%.2f", 1-node.Invisibility.Value))
	case node.Invisibility.Value == 1.0:
		ctx.WriteString(" NOT VISIBLE")
	}
}

// CreateTypeVariety represents a particular variety of user defined types.
type CreateTypeVariety int

//go:generate stringer -type=CreateTypeVariety
const (
	_ CreateTypeVariety = iota
	// Enum represents an ENUM user defined type.
	Enum
	// Composite represents a composite user defined type.
	Composite
	// Range represents a RANGE user defined type.
	Range
	// Base represents a base user defined type.
	Base
	// Shell represents a shell user defined type.
	Shell
	// Domain represents a DOMAIN user defined type.
	Domain
)

// EnumValue represents a single enum value.
type EnumValue string

// Format implements the NodeFormatter interface.
func (n *EnumValue) Format(ctx *FmtCtx) {
	f := ctx.flags
	if f.HasFlags(FmtAnonymize) {
		ctx.WriteByte('_')
	} else if f.HasFlags(FmtMarkRedactionNode) {
		ctx.WriteString(string(redact.StartMarker()))
		lexbase.EncodeSQLString(&ctx.Buffer, string(*n))
		ctx.WriteString(string(redact.EndMarker()))
	} else {
		lexbase.EncodeSQLString(&ctx.Buffer, string(*n))
	}
}

// EnumValueList represents a list of enum values.
type EnumValueList []EnumValue

// Format implements the NodeFormatter interface.
func (l *EnumValueList) Format(ctx *FmtCtx) {
	for i := range *l {
		if i > 0 {
			ctx.WriteString(", ")
		}
		ctx.FormatNode(&(*l)[i])
	}
}

// CompositeTypeElem is a single element in a composite type definition.
type CompositeTypeElem struct {
	Label Name
	Type  ResolvableTypeReference
}

// CreateType represents a CREATE TYPE statement.
type CreateType struct {
	TypeName *UnresolvedObjectName
	Variety  CreateTypeVariety
	// EnumLabels is set when this represents a CREATE TYPE ... AS ENUM statement.
	EnumLabels EnumValueList
	// CompositeTypeList is set when this represents a CREATE TYPE ... AS ( )
	// statement.
	CompositeTypeList []CompositeTypeElem
	// IfNotExists is true if IF NOT EXISTS was requested.
	IfNotExists bool
}

var _ Statement = &CreateType{}

// Format implements the NodeFormatter interface.
func (node *CreateType) Format(ctx *FmtCtx) {
	ctx.WriteString("CREATE TYPE ")
	if node.IfNotExists {
		ctx.WriteString("IF NOT EXISTS ")
	}
	ctx.FormatNode(node.TypeName)
	ctx.WriteString(" ")
	switch node.Variety {
	case Enum:
		ctx.WriteString("AS ENUM (")
		ctx.FormatNode(&node.EnumLabels)
		ctx.WriteString(")")
	case Composite:
		ctx.WriteString("AS (")
		for i := range node.CompositeTypeList {
			elem := &node.CompositeTypeList[i]
			if i != 0 {
				ctx.WriteString(", ")
			}
			ctx.FormatNode(&elem.Label)
			ctx.WriteString(" ")
			ctx.FormatTypeReference(elem.Type)
		}
		ctx.WriteString(")")
	}
}

func (node *CreateType) String() string {
	return AsString(node)
}

// TableDef represents a column, index or constraint definition within a CREATE
// TABLE statement.
type TableDef interface {
	NodeFormatter
	// Placeholder function to ensure that only desired types (*TableDef) conform
	// to the TableDef interface.
	tableDef()
}

func (*ColumnTableDef) tableDef()               {}
func (*IndexTableDef) tableDef()                {}
func (*FamilyTableDef) tableDef()               {}
func (*ForeignKeyConstraintTableDef) tableDef() {}
func (*CheckConstraintTableDef) tableDef()      {}
func (*LikeTableDef) tableDef()                 {}

// TableDefs represents a list of table definitions.
type TableDefs []TableDef

// Format implements the NodeFormatter interface.
func (node *TableDefs) Format(ctx *FmtCtx) {
	for i, n := range *node {
		if i > 0 {
			ctx.WriteString(", ")
		}
		ctx.FormatNode(n)
	}
}

// Nullability represents either NULL, NOT NULL or an unspecified value (silent
// NULL).
type Nullability int

// The values for NullType.
const (
	NotNull Nullability = iota
	Null
	SilentNull
)

// RandomNullability returns a random valid Nullability, given a random number
// generator, `rand`. If nullableOnly is true, the random Nullability will not
// be `NotNull`.
func RandomNullability(rand *rand.Rand, nullableOnly bool) Nullability {
	if nullableOnly {
		// Add 1 after getting the random number to exclude the zero-value NotNull.
		return Nullability(rand.Intn(int(SilentNull)) + 1)
	}
	return Nullability(rand.Intn(int(SilentNull) + 1))
}

// GeneratedIdentityType represents either GENERATED ALWAYS AS IDENTITY
// or GENERATED BY DEFAULT AS IDENTITY.
type GeneratedIdentityType int

// The values of GeneratedIdentity.GeneratedAsIdentityType.
const (
	GeneratedAlways GeneratedIdentityType = iota
	GeneratedByDefault
)

// ColumnTableDef represents a column definition within a CREATE TABLE
// statement.
type ColumnTableDef struct {
	Name     Name
	Type     ResolvableTypeReference
	IsSerial bool
	// IsCreateAs is set to true if the Type is resolved after parsing.
	// CREATE AS statements must not display column types during formatting.
	IsCreateAs        bool
	GeneratedIdentity struct {
		IsGeneratedAsIdentity   bool
		GeneratedAsIdentityType GeneratedIdentityType
		SeqOptions              SequenceOptions
	}
	Hidden   bool
	Nullable struct {
		Nullability    Nullability
		ConstraintName Name
	}
	PrimaryKey struct {
		IsPrimaryKey  bool
		Sharded       bool
		ShardBuckets  Expr
		StorageParams StorageParams
	}
	Unique struct {
		IsUnique       bool
		WithoutIndex   bool
		ConstraintName Name
	}
	DefaultExpr struct {
		Expr           Expr
		ConstraintName Name
	}
	OnUpdateExpr struct {
		Expr           Expr
		ConstraintName Name
	}
	CheckExprs []ColumnTableDefCheckExpr
	References struct {
		Table          *TableName
		Col            Name
		ConstraintName Name
		Actions        ReferenceActions
		Match          CompositeKeyMatchMethod
	}
	Computed struct {
		Computed bool
		Expr     Expr
		Virtual  bool
	}
	Family struct {
		Name        Name
		Create      bool
		IfNotExists bool
	}
}

// ColumnTableDefCheckExpr represents a check constraint on a column definition
// within a CREATE TABLE statement.
type ColumnTableDefCheckExpr struct {
	Expr           Expr
	ConstraintName Name
}

func processCollationOnType(
	name Name, ref ResolvableTypeReference, c ColumnCollation,
) (*types.T, error) {
	// At the moment, only string types can be collated. User defined types
	//  like enums don't support collations, so check this at parse time.
	typ, ok := GetStaticallyKnownType(ref)
	if !ok {
		return nil, pgerror.Newf(pgcode.DatatypeMismatch,
			"COLLATE declaration for non-string-typed column %q", name)
	}
	switch typ.Family() {
	case types.StringFamily:
		return types.MakeCollatedString(typ, string(c)), nil
	case types.CollatedStringFamily:
		return nil, pgerror.Newf(pgcode.Syntax,
			"multiple COLLATE declarations for column %q", name)
	case types.ArrayFamily:
		elemTyp, err := processCollationOnType(name, typ.ArrayContents(), c)
		if err != nil {
			return nil, err
		}
		return types.MakeArray(elemTyp), nil
	default:
		return nil, pgerror.Newf(pgcode.DatatypeMismatch,
			"COLLATE declaration for non-string-typed column %q", name)
	}
}

// NewColumnTableDef constructs a column definition for a CreateTable statement.
func NewColumnTableDef(
	name Name,
	typRef ResolvableTypeReference,
	isSerial bool,
	qualifications []NamedColumnQualification,
) (*ColumnTableDef, error) {
	d := &ColumnTableDef{
		Name:     name,
		Type:     typRef,
		IsSerial: isSerial,
	}
	d.Nullable.Nullability = SilentNull
	for _, c := range qualifications {
		switch t := c.Qualification.(type) {
		case ColumnCollation:
			locale := string(t)
			// In postgres, all strings have collations defaulting to "default".
			// In CRDB, collated strings are treated separately to string family types.
			// To most behave like postgres, set the CollatedString type if a non-"default"
			// collation is used.
			if locale != collatedstring.DefaultCollationTag {
				_, err := language.Parse(locale)
				if err != nil {
					return nil, pgerror.Wrapf(err, pgcode.Syntax, "invalid locale %s", locale)
				}
				collatedTyp, err := processCollationOnType(name, d.Type, t)
				if err != nil {
					return nil, err
				}
				d.Type = collatedTyp
			}
		case *ColumnDefault:
			if d.HasDefaultExpr() || d.GeneratedIdentity.IsGeneratedAsIdentity {
				return nil, pgerror.Newf(pgcode.Syntax,
					"multiple default values specified for column %q", name)
			}
			d.DefaultExpr.Expr = t.Expr
			d.DefaultExpr.ConstraintName = c.Name
		case *ColumnOnUpdate:
			if d.HasOnUpdateExpr() {
				return nil, pgerror.Newf(pgcode.Syntax,
					"multiple ON UPDATE values specified for column %q", name)
			}
			if d.GeneratedIdentity.IsGeneratedAsIdentity {
				return nil, pgerror.Newf(pgcode.Syntax,
					"both generated identity and on update expression specified for column %q",
					name)
			}
			d.OnUpdateExpr.Expr = t.Expr
			d.OnUpdateExpr.ConstraintName = c.Name
		case *GeneratedAlwaysAsIdentity, *GeneratedByDefAsIdentity:
			if typ, ok := typRef.(*types.T); !ok || typ.InternalType.Family != types.IntFamily {
				return nil, pgerror.Newf(
					pgcode.InvalidParameterValue,
					"identity column type must be an INT",
				)
			}
			if d.GeneratedIdentity.IsGeneratedAsIdentity {
				return nil, pgerror.Newf(pgcode.Syntax,
					"multiple identity specifications for column %q", name)
			}
			if d.HasDefaultExpr() {
				return nil, pgerror.Newf(pgcode.Syntax,
					"multiple default values specified for column %q", name)
			}
			if d.Computed.Computed {
				return nil, pgerror.Newf(pgcode.Syntax,
					"both generated identity and computed expression specified for column %q", name)
			}
			if d.Nullable.Nullability == Null {
				return nil, pgerror.Newf(pgcode.Syntax,
					"conflicting NULL/NOT NULL declarations for column %q", name)
			}
			if d.HasOnUpdateExpr() {
				return nil, pgerror.Newf(pgcode.Syntax,
					"both generated identity and on update expression specified for column %q",
					name)
			}
			d.GeneratedIdentity.IsGeneratedAsIdentity = true
			d.Nullable.Nullability = NotNull
			switch c.Qualification.(type) {
			case *GeneratedAlwaysAsIdentity:
				d.GeneratedIdentity.GeneratedAsIdentityType = GeneratedAlways
				d.GeneratedIdentity.SeqOptions = t.(*GeneratedAlwaysAsIdentity).SeqOptions
			case *GeneratedByDefAsIdentity:
				d.GeneratedIdentity.GeneratedAsIdentityType = GeneratedByDefault
				d.GeneratedIdentity.SeqOptions = t.(*GeneratedByDefAsIdentity).SeqOptions
			}
		case HiddenConstraint:
			d.Hidden = true
		case NotNullConstraint:
			if d.Nullable.Nullability == Null {
				return nil, pgerror.Newf(pgcode.Syntax,
					"conflicting NULL/NOT NULL declarations for column %q", name)
			}
			d.Nullable.Nullability = NotNull
			d.Nullable.ConstraintName = c.Name
		case NullConstraint:
			if d.Nullable.Nullability == NotNull {
				return nil, pgerror.Newf(pgcode.Syntax,
					"conflicting NULL/NOT NULL declarations for column %q", name)
			}
			d.Nullable.Nullability = Null
			d.Nullable.ConstraintName = c.Name
		case PrimaryKeyConstraint:
			d.PrimaryKey.IsPrimaryKey = true
			d.PrimaryKey.StorageParams = c.Qualification.(PrimaryKeyConstraint).StorageParams
			d.Unique.ConstraintName = c.Name
		case ShardedPrimaryKeyConstraint:
			d.PrimaryKey.IsPrimaryKey = true
			constraint := c.Qualification.(ShardedPrimaryKeyConstraint)
			d.PrimaryKey.Sharded = true
			d.PrimaryKey.ShardBuckets = constraint.ShardBuckets
			d.PrimaryKey.StorageParams = constraint.StorageParams
			d.Unique.ConstraintName = c.Name
		case UniqueConstraint:
			d.Unique.IsUnique = true
			d.Unique.WithoutIndex = t.WithoutIndex
			d.Unique.ConstraintName = c.Name
		case *ColumnCheckConstraint:
			d.CheckExprs = append(d.CheckExprs, ColumnTableDefCheckExpr{
				Expr:           t.Expr,
				ConstraintName: c.Name,
			})
		case *ColumnFKConstraint:
			if d.HasFKConstraint() {
				return nil, pgerror.Newf(pgcode.InvalidTableDefinition,
					"multiple foreign key constraints specified for column %q", name)
			}
			d.References.Table = &t.Table
			d.References.Col = t.Col
			d.References.ConstraintName = c.Name
			d.References.Actions = t.Actions
			d.References.Match = t.Match
		case *ColumnComputedDef:
			if d.GeneratedIdentity.IsGeneratedAsIdentity {
				return nil, pgerror.Newf(pgcode.Syntax,
					"both generated identity and computed expression specified for column %q", name)
			}
			d.Computed.Computed = true
			d.Computed.Expr = t.Expr
			d.Computed.Virtual = t.Virtual
		case *ColumnFamilyConstraint:
			if d.HasColumnFamily() {
				return nil, pgerror.Newf(pgcode.InvalidTableDefinition,
					"multiple column families specified for column %q", name)
			}
			d.Family.Name = t.Family
			d.Family.Create = t.Create
			d.Family.IfNotExists = t.IfNotExists
		default:
			return nil, errors.AssertionFailedf("unexpected column qualification: %T", c)
		}
	}

	return d, nil
}

// HasDefaultExpr returns if the ColumnTableDef has a default expression.
func (node *ColumnTableDef) HasDefaultExpr() bool {
	return node.DefaultExpr.Expr != nil
}

// HasOnUpdateExpr returns if the ColumnTableDef has an ON UPDATE expression.
func (node *ColumnTableDef) HasOnUpdateExpr() bool {
	return node.OnUpdateExpr.Expr != nil
}

// HasFKConstraint returns if the ColumnTableDef has a foreign key constraint.
func (node *ColumnTableDef) HasFKConstraint() bool {
	return node.References.Table != nil
}

// IsComputed returns if the ColumnTableDef is a computed column.
func (node *ColumnTableDef) IsComputed() bool {
	return node.Computed.Computed
}

// IsVirtual returns if the ColumnTableDef is a virtual column.
func (node *ColumnTableDef) IsVirtual() bool {
	return node.Computed.Virtual
}

// HasColumnFamily returns if the ColumnTableDef has a column family.
func (node *ColumnTableDef) HasColumnFamily() bool {
	return node.Family.Name != "" || node.Family.Create
}

// Format implements the NodeFormatter interface.
func (node *ColumnTableDef) Format(ctx *FmtCtx) {
	ctx.FormatNode(&node.Name)

	// ColumnTableDef node type will not be specified if it represents a CREATE
	// TABLE ... AS query.
	if !node.IsCreateAs && node.Type != nil {
		ctx.WriteByte(' ')
		node.formatColumnType(ctx)
	}

	if node.Nullable.Nullability != SilentNull && node.Nullable.ConstraintName != "" {
		ctx.WriteString(" CONSTRAINT ")
		ctx.FormatNode(&node.Nullable.ConstraintName)
	}
	switch node.Nullable.Nullability {
	case Null:
		ctx.WriteString(" NULL")
	case NotNull:
		ctx.WriteString(" NOT NULL")
	}
	if node.Hidden {
		ctx.WriteString(" NOT VISIBLE")
	}
	if node.PrimaryKey.IsPrimaryKey || node.Unique.IsUnique {
		if node.Unique.ConstraintName != "" {
			ctx.WriteString(" CONSTRAINT ")
			ctx.FormatNode(&node.Unique.ConstraintName)
		}
		if node.PrimaryKey.IsPrimaryKey {
			ctx.WriteString(" PRIMARY KEY")

			// Always prefer to output hash sharding bucket count as a storage param.
			pkStorageParams := node.PrimaryKey.StorageParams
			if node.PrimaryKey.Sharded {
				ctx.WriteString(" USING HASH")
				bcStorageParam := node.PrimaryKey.StorageParams.GetVal(`bucket_count`)
				if _, ok := node.PrimaryKey.ShardBuckets.(DefaultVal); !ok && bcStorageParam == nil {
					pkStorageParams = append(
						pkStorageParams,
						StorageParam{
							Key:   `bucket_count`,
							Value: node.PrimaryKey.ShardBuckets,
						},
					)
				}
			}
			if len(pkStorageParams) > 0 {
				ctx.WriteString(" WITH (")
				ctx.FormatNode(&pkStorageParams)
				ctx.WriteString(")")
			}
		} else if node.Unique.IsUnique {
			ctx.WriteString(" UNIQUE")
			if node.Unique.WithoutIndex {
				ctx.WriteString(" WITHOUT INDEX")
			}
		}
	}
	if node.HasDefaultExpr() {
		if node.DefaultExpr.ConstraintName != "" {
			ctx.WriteString(" CONSTRAINT ")
			ctx.FormatNode(&node.DefaultExpr.ConstraintName)
		}
		ctx.WriteString(" DEFAULT ")
		ctx.FormatNode(node.DefaultExpr.Expr)
	}
	if node.HasOnUpdateExpr() {
		if node.OnUpdateExpr.ConstraintName != "" {
			ctx.WriteString(" CONSTRAINT ")
			ctx.FormatNode(&node.OnUpdateExpr.ConstraintName)
		}
		ctx.WriteString(" ON UPDATE ")
		ctx.FormatNode(node.OnUpdateExpr.Expr)
	}
	if node.GeneratedIdentity.IsGeneratedAsIdentity {
		switch node.GeneratedIdentity.GeneratedAsIdentityType {
		case GeneratedAlways:
			ctx.WriteString(" GENERATED ALWAYS AS IDENTITY")
		case GeneratedByDefault:
			ctx.WriteString(" GENERATED BY DEFAULT AS IDENTITY")
		}
		if genSeqOpt := node.GeneratedIdentity.SeqOptions; genSeqOpt != nil {
			ctx.WriteString(" (")
			// TODO(janexing): remove the leading and ending space of the
			// sequence option expression.
			genSeqOpt.Format(ctx)
			ctx.WriteString(" ) ")
		}
	}
	for _, checkExpr := range node.CheckExprs {
		if checkExpr.ConstraintName != "" {
			ctx.WriteString(" CONSTRAINT ")
			ctx.FormatNode(&checkExpr.ConstraintName)
		}
		ctx.WriteString(" CHECK (")
		ctx.FormatNode(checkExpr.Expr)
		ctx.WriteByte(')')
	}
	if node.HasFKConstraint() {
		if node.References.ConstraintName != "" {
			ctx.WriteString(" CONSTRAINT ")
			ctx.FormatNode(&node.References.ConstraintName)
		}
		ctx.WriteString(" REFERENCES ")
		ctx.FormatNode(node.References.Table)
		if node.References.Col != "" {
			ctx.WriteString(" (")
			ctx.FormatNode(&node.References.Col)
			ctx.WriteByte(')')
		}
		if node.References.Match != MatchSimple {
			ctx.WriteByte(' ')
			ctx.WriteString(node.References.Match.String())
		}
		ctx.FormatNode(&node.References.Actions)
	}
	if node.IsComputed() {
		ctx.WriteString(" AS (")
		ctx.FormatNode(node.Computed.Expr)
		if node.Computed.Virtual {
			ctx.WriteString(") VIRTUAL")
		} else {
			ctx.WriteString(") STORED")
		}
	}
	if node.HasColumnFamily() {
		if node.Family.Create {
			ctx.WriteString(" CREATE")
			if node.Family.IfNotExists {
				ctx.WriteString(" IF NOT EXISTS")
			}
		}
		ctx.WriteString(" FAMILY")
		if len(node.Family.Name) > 0 {
			ctx.WriteByte(' ')
			ctx.FormatNode(&node.Family.Name)
		}
	}
}

func (node *ColumnTableDef) formatColumnType(ctx *FmtCtx) {
	if replaced, ok := node.replacedSerialTypeName(); ok {
		ctx.WriteString(replaced)
	} else {
		ctx.FormatTypeReference(node.Type)
	}
}

func (node *ColumnTableDef) replacedSerialTypeName() (string, bool) {
	if node.IsSerial {
		// Map INT types to SERIAL keyword.
		// TODO (rohany): This should be pushed until type resolution occurs.
		//  However, the argument is that we deal with serial at parse time only,
		//  so we handle those cases here.
		switch MustBeStaticallyKnownType(node.Type).Width() {
		case 16:
			return "SERIAL2", true
		case 32:
			return "SERIAL4", true
		default:
			return "SERIAL8", true
		}
	}
	return "", false
}

// String implements the fmt.Stringer interface.
func (node *ColumnTableDef) String() string { return AsString(node) }

// NamedColumnQualification wraps a NamedColumnQualification with a name.
type NamedColumnQualification struct {
	Name          Name
	Qualification ColumnQualification
}

// ColumnQualification represents a constraint on a column.
type ColumnQualification interface {
	columnQualification()
}

func (ColumnCollation) columnQualification()             {}
func (*ColumnDefault) columnQualification()              {}
func (*ColumnOnUpdate) columnQualification()             {}
func (NotNullConstraint) columnQualification()           {}
func (NullConstraint) columnQualification()              {}
func (HiddenConstraint) columnQualification()            {}
func (PrimaryKeyConstraint) columnQualification()        {}
func (ShardedPrimaryKeyConstraint) columnQualification() {}
func (UniqueConstraint) columnQualification()            {}
func (*ColumnCheckConstraint) columnQualification()      {}
func (*ColumnComputedDef) columnQualification()          {}
func (*ColumnFKConstraint) columnQualification()         {}
func (*ColumnFamilyConstraint) columnQualification()     {}
func (*GeneratedAlwaysAsIdentity) columnQualification()  {}
func (*GeneratedByDefAsIdentity) columnQualification()   {}

// ColumnCollation represents a COLLATE clause for a column.
type ColumnCollation string

// ColumnDefault represents a DEFAULT clause for a column.
type ColumnDefault struct {
	Expr Expr
}

// ColumnOnUpdate represents a ON UPDATE clause for a column.
type ColumnOnUpdate struct {
	Expr Expr
}

// GeneratedAlwaysAsIdentity represents a column generated always as identity.
type GeneratedAlwaysAsIdentity struct {
	SeqOptions SequenceOptions
}

// GeneratedByDefAsIdentity represents a column generated by default as identity.
type GeneratedByDefAsIdentity struct {
	SeqOptions SequenceOptions
}

// NotNullConstraint represents NOT NULL on a column.
type NotNullConstraint struct{}

// NullConstraint represents NULL on a column.
type NullConstraint struct{}

// HiddenConstraint represents HIDDEN on a column.
type HiddenConstraint struct{}

// PrimaryKeyConstraint represents PRIMARY KEY on a column.
type PrimaryKeyConstraint struct {
	StorageParams StorageParams
}

// ShardedPrimaryKeyConstraint represents `PRIMARY KEY .. USING HASH..`
// on a column.
type ShardedPrimaryKeyConstraint struct {
	Sharded       bool
	ShardBuckets  Expr
	StorageParams StorageParams
}

// UniqueConstraint represents UNIQUE on a column.
type UniqueConstraint struct {
	WithoutIndex bool
}

// ColumnCheckConstraint represents either a check on a column.
type ColumnCheckConstraint struct {
	Expr Expr
}

// ColumnFKConstraint represents a FK-constaint on a column.
type ColumnFKConstraint struct {
	Table   TableName
	Col     Name // empty-string means use PK
	Actions ReferenceActions
	Match   CompositeKeyMatchMethod
}

// ColumnComputedDef represents the description of a computed column.
type ColumnComputedDef struct {
	Expr    Expr
	Virtual bool
}

// ColumnFamilyConstraint represents FAMILY on a column.
type ColumnFamilyConstraint struct {
	Family      Name
	Create      bool
	IfNotExists bool
}

// IndexTableDef represents an index definition within a CREATE TABLE
// statement.
type IndexTableDef struct {
	Name             Name
	Columns          IndexElemList
	Sharded          *ShardedIndexDef
	Storing          NameList
	Type             idxtype.T
	PartitionByIndex *PartitionByIndex
	StorageParams    StorageParams
	Predicate        Expr
	Invisibility     IndexInvisibility
}

// Format implements the NodeFormatter interface.
func (node *IndexTableDef) Format(ctx *FmtCtx) {
	switch node.Type {
	case idxtype.INVERTED:
		ctx.WriteString("INVERTED ")
	case idxtype.VECTOR:
		ctx.WriteString("VECTOR ")
	}
	ctx.WriteString("INDEX ")
	if node.Name != "" {
		ctx.FormatNode(&node.Name)
		ctx.WriteByte(' ')
	}
	ctx.WriteByte('(')
	ctx.FormatNode(&node.Columns)
	ctx.WriteByte(')')
	if node.Sharded != nil {
		ctx.FormatNode(node.Sharded)
	}
	if node.Storing != nil {
		ctx.WriteString(" STORING (")
		ctx.FormatNode(&node.Storing)
		ctx.WriteByte(')')
	}
	if node.PartitionByIndex != nil {
		ctx.FormatNode(node.PartitionByIndex)
	}
	if node.StorageParams != nil {
		ctx.WriteString(" WITH (")
		ctx.FormatNode(&node.StorageParams)
		ctx.WriteString(")")
	}
	if node.Predicate != nil {
		ctx.WriteString(" WHERE ")
		ctx.FormatNode(node.Predicate)
	}
	switch {
	case node.Invisibility.FloatProvided:
		ctx.WriteString(" VISIBILITY " + fmt.Sprintf("%.2f", 1-node.Invisibility.Value))
	case node.Invisibility.Value == 1.0:
		ctx.WriteString(" NOT VISIBLE")
	}
}

// ConstraintTableDef represents a constraint definition within a CREATE TABLE
// statement.
type ConstraintTableDef interface {
	TableDef
	// Placeholder function to ensure that only desired types
	// (*ConstraintTableDef) conform to the ConstraintTableDef interface.
	constraintTableDef()

	// SetName replaces the name of the definition in-place. Used in the parser.
	SetName(name Name)

	// SetIfNotExists sets this definition as coming from an
	// ADD CONSTRAINT IF NOT EXISTS statement. Used in the parser.
	SetIfNotExists()
}

func (*UniqueConstraintTableDef) constraintTableDef()     {}
func (*ForeignKeyConstraintTableDef) constraintTableDef() {}
func (*CheckConstraintTableDef) constraintTableDef()      {}

// UniqueConstraintTableDef represents a unique constraint within a CREATE
// TABLE statement.
type UniqueConstraintTableDef struct {
	IndexTableDef
	PrimaryKey   bool
	WithoutIndex bool
	IfNotExists  bool
	// FormatAsIndex indicates if the constraint should be formatted as an index
	// definition. This is needed since indexes support syntax for things like
	// storage parameters and sharding, while constraints do not.
	FormatAsIndex bool
}

// SetName implements the TableDef interface.
func (node *UniqueConstraintTableDef) SetName(name Name) {
	node.Name = name
}

// SetIfNotExists implements the ConstraintTableDef interface.
func (node *UniqueConstraintTableDef) SetIfNotExists() {
	node.IfNotExists = true
}

// Format implements the NodeFormatter interface.
func (node *UniqueConstraintTableDef) Format(ctx *FmtCtx) {
	if node.Name != "" && !node.FormatAsIndex {
		ctx.WriteString("CONSTRAINT ")
		if node.IfNotExists {
			ctx.WriteString("IF NOT EXISTS ")
		}
		ctx.FormatNode(&node.Name)
		ctx.WriteByte(' ')
	}
	if node.PrimaryKey {
		ctx.WriteString("PRIMARY KEY ")
	} else {
		ctx.WriteString("UNIQUE ")
		if node.FormatAsIndex {
			ctx.WriteString("INDEX ")
			if node.Name != "" {
				ctx.FormatNode(&node.Name)
				ctx.WriteByte(' ')
			}
		}
	}
	if node.WithoutIndex {
		ctx.WriteString("WITHOUT INDEX ")
	}
	ctx.WriteByte('(')
	ctx.FormatNode(&node.Columns)
	ctx.WriteByte(')')
	if node.Sharded != nil {
		ctx.FormatNode(node.Sharded)
	}
	if node.Storing != nil {
		ctx.WriteString(" STORING (")
		ctx.FormatNode(&node.Storing)
		ctx.WriteByte(')')
	}
	if node.PartitionByIndex != nil {
		ctx.FormatNode(node.PartitionByIndex)
	}
	if node.Predicate != nil {
		ctx.WriteString(" WHERE ")
		ctx.FormatNode(node.Predicate)
	}
	switch {
	case node.Invisibility.FloatProvided:
		ctx.WriteString(" VISIBILITY " + fmt.Sprintf("%.2f", 1-node.Invisibility.Value))
	case node.Invisibility.Value == 1.0:
		ctx.WriteString(" NOT VISIBLE")
	}
	if node.StorageParams != nil {
		ctx.WriteString(" WITH (")
		ctx.FormatNode(&node.StorageParams)
		ctx.WriteString(")")
	}
}

// ForeignKeyConstraintTableDef represents a FOREIGN KEY constraint in the AST.
type ForeignKeyConstraintTableDef struct {
	Name        Name
	Table       TableName
	FromCols    NameList
	ToCols      NameList
	Actions     ReferenceActions
	Match       CompositeKeyMatchMethod
	IfNotExists bool
}

// Format implements the NodeFormatter interface.
func (node *ForeignKeyConstraintTableDef) Format(ctx *FmtCtx) {
	if node.Name != "" {
		ctx.WriteString("CONSTRAINT ")
		if node.IfNotExists {
			ctx.WriteString("IF NOT EXISTS ")
		}
		ctx.FormatNode(&node.Name)
		ctx.WriteByte(' ')
	}
	ctx.WriteString("FOREIGN KEY (")
	ctx.FormatNode(&node.FromCols)
	ctx.WriteString(") REFERENCES ")
	ctx.FormatNode(&node.Table)

	if len(node.ToCols) > 0 {
		ctx.WriteByte(' ')
		ctx.WriteByte('(')
		ctx.FormatNode(&node.ToCols)
		ctx.WriteByte(')')
	}

	if node.Match != MatchSimple {
		ctx.WriteByte(' ')
		ctx.WriteString(node.Match.String())
	}

	ctx.FormatNode(&node.Actions)
}

// SetName implements the ConstraintTableDef interface.
func (node *ForeignKeyConstraintTableDef) SetName(name Name) {
	node.Name = name
}

// SetIfNotExists implements the ConstraintTableDef interface.
func (node *ForeignKeyConstraintTableDef) SetIfNotExists() {
	node.IfNotExists = true
}

// CheckConstraintTableDef represents a check constraint within a CREATE
// TABLE statement.
type CheckConstraintTableDef struct {
	Name                  Name
	Expr                  Expr
	FromHashShardedColumn bool
	IfNotExists           bool
}

// SetName implements the ConstraintTableDef interface.
func (node *CheckConstraintTableDef) SetName(name Name) {
	node.Name = name
}

// SetIfNotExists implements the ConstraintTableDef interface.
func (node *CheckConstraintTableDef) SetIfNotExists() {
	node.IfNotExists = true
}

// Format implements the NodeFormatter interface.
func (node *CheckConstraintTableDef) Format(ctx *FmtCtx) {
	if node.Name != "" {
		ctx.WriteString("CONSTRAINT ")
		if node.IfNotExists {
			ctx.WriteString("IF NOT EXISTS ")
		}
		ctx.FormatNode(&node.Name)
		ctx.WriteByte(' ')
	}
	ctx.WriteString("CHECK (")
	ctx.FormatNode(node.Expr)
	ctx.WriteByte(')')
}

// FamilyTableDef represents a family definition within a CREATE TABLE
// statement.
type FamilyTableDef struct {
	Name    Name
	Columns NameList
}

// Format implements the NodeFormatter interface.
func (node *FamilyTableDef) Format(ctx *FmtCtx) {
	ctx.WriteString("FAMILY ")
	if node.Name != "" {
		ctx.FormatNode(&node.Name)
		ctx.WriteByte(' ')
	}
	ctx.WriteByte('(')
	ctx.FormatNode(&node.Columns)
	ctx.WriteByte(')')
}

// ShardedIndexDef represents a hash sharded secondary index definition within a CREATE
// TABLE or CREATE INDEX statement.
type ShardedIndexDef struct {
	ShardBuckets Expr
}

// Format implements the NodeFormatter interface.
func (node *ShardedIndexDef) Format(ctx *FmtCtx) {
	if _, ok := node.ShardBuckets.(DefaultVal); ok {
		ctx.WriteString(" USING HASH")
		return
	}
	ctx.WriteString(" USING HASH WITH BUCKET_COUNT = ")
	ctx.FormatNode(node.ShardBuckets)
}

// PartitionByType is an enum of each type of partitioning (LIST/RANGE).
type PartitionByType string

const (
	// PartitionByList indicates a PARTITION BY LIST clause.
	PartitionByList PartitionByType = "LIST"
	// PartitionByRange indicates a PARTITION BY LIST clause.
	PartitionByRange PartitionByType = "RANGE"
)

// PartitionByIndex represents a PARTITION BY definition within
// a CREATE/ALTER INDEX statement.
type PartitionByIndex struct {
	*PartitionBy
}

// ContainsPartitions determines if the partition by table contains
// a partition clause which is not PARTITION BY NOTHING.
func (node *PartitionByIndex) ContainsPartitions() bool {
	return node != nil && node.PartitionBy != nil
}

// ContainsPartitioningClause determines if the partition by table contains
// a partitioning clause, including PARTITION BY NOTHING.
func (node *PartitionByIndex) ContainsPartitioningClause() bool {
	return node != nil
}

// PartitionByTable represents a PARTITION [ALL] BY definition within
// a CREATE/ALTER TABLE statement.
type PartitionByTable struct {
	// All denotes PARTITION ALL BY.
	All bool

	*PartitionBy
}

// Format implements the NodeFormatter interface.
func (node *PartitionByTable) Format(ctx *FmtCtx) {
	if node == nil {
		ctx.WriteString(` PARTITION BY NOTHING`)
		return
	}
	ctx.WriteString(` PARTITION `)
	if node.All {
		ctx.WriteString(`ALL `)
	}
	ctx.WriteString(`BY `)
	node.PartitionBy.formatListOrRange(ctx)
}

// ContainsPartitions determines if the partition by table contains
// a partition clause which is not PARTITION BY NOTHING.
func (node *PartitionByTable) ContainsPartitions() bool {
	return node != nil && node.PartitionBy != nil
}

// ContainsPartitioningClause determines if the partition by table contains
// a partitioning clause, including PARTITION BY NOTHING.
func (node *PartitionByTable) ContainsPartitioningClause() bool {
	return node != nil
}

// PartitionBy represents an PARTITION BY definition within a CREATE/ALTER
// TABLE/INDEX statement or within a subpartition statement.
// This is wrapped by top level PartitionByTable/PartitionByIndex
// structs for table and index definitions respectively.
type PartitionBy struct {
	Fields NameList
	// Exactly one of List or Range is required to be non-empty.
	List  []ListPartition
	Range []RangePartition
}

// Format implements the NodeFormatter interface.
func (node *PartitionBy) Format(ctx *FmtCtx) {
	ctx.WriteString(` PARTITION BY `)
	node.formatListOrRange(ctx)
}

func (node *PartitionBy) formatListOrRange(ctx *FmtCtx) {
	if node == nil {
		ctx.WriteString(`NOTHING`)
		return
	}
	if len(node.List) > 0 {
		ctx.WriteString(`LIST (`)
	} else if len(node.Range) > 0 {
		ctx.WriteString(`RANGE (`)
	}
	ctx.FormatNode(&node.Fields)
	ctx.WriteString(`) (`)
	for i := range node.List {
		if i > 0 {
			ctx.WriteString(", ")
		}
		ctx.FormatNode(&node.List[i])
	}
	for i := range node.Range {
		if i > 0 {
			ctx.WriteString(", ")
		}
		ctx.FormatNode(&node.Range[i])
	}
	ctx.WriteString(`)`)
}

// ListPartition represents a PARTITION definition within a PARTITION BY LIST.
type ListPartition struct {
	Name         Name
	Exprs        Exprs
	Subpartition *PartitionBy
}

// Format implements the NodeFormatter interface.
func (node *ListPartition) Format(ctx *FmtCtx) {
	ctx.WriteString(`PARTITION `)
	ctx.FormatNode(&node.Name)
	ctx.WriteString(` VALUES IN (`)
	ctx.FormatNode(&node.Exprs)
	ctx.WriteByte(')')
	if node.Subpartition != nil {
		ctx.FormatNode(node.Subpartition)
	}
}

// RangePartition represents a PARTITION definition within a PARTITION BY RANGE.
type RangePartition struct {
	Name         Name
	From         Exprs
	To           Exprs
	Subpartition *PartitionBy
}

// Format implements the NodeFormatter interface.
func (node *RangePartition) Format(ctx *FmtCtx) {
	ctx.WriteString(`PARTITION `)
	ctx.FormatNode(&node.Name)
	ctx.WriteString(` VALUES FROM (`)
	ctx.FormatNode(&node.From)
	ctx.WriteString(`) TO (`)
	ctx.FormatNode(&node.To)
	ctx.WriteByte(')')
	if node.Subpartition != nil {
		ctx.FormatNode(node.Subpartition)
	}
}

// StorageParam is a key-value parameter for table storage.
type StorageParam struct {
	Key   string
	Value Expr
}

// StorageParams is a list of StorageParams.
type StorageParams []StorageParam

// Format implements the NodeFormatter interface.
func (o *StorageParams) Format(ctx *FmtCtx) {
	buf, f := &ctx.Buffer, ctx.flags
	for i := range *o {
		n := &(*o)[i]
		if i > 0 {
			ctx.WriteString(", ")
		}
		lexbase.EncodeSQLStringWithFlags(buf, n.Key, f.EncodeFlags())
		if n.Value != nil {
			ctx.WriteString(` = `)
			ctx.FormatNode(n.Value)
		}
	}
}

// GetVal returns corresponding value if a key exists, otherwise nil is
// returned.
func (o *StorageParams) GetVal(key string) Expr {
	for _, param := range *o {
		if param.Key == key {
			return param.Value
		}
	}
	return nil
}

// CreateTableOnCommitSetting represents the CREATE TABLE ... ON COMMIT <action>
// parameters.
type CreateTableOnCommitSetting uint32

const (
	// CreateTableOnCommitUnset indicates that ON COMMIT was unset.
	CreateTableOnCommitUnset CreateTableOnCommitSetting = iota
	// CreateTableOnCommitPreserveRows indicates that ON COMMIT PRESERVE ROWS was set.
	CreateTableOnCommitPreserveRows
)

// CreateTable represents a CREATE TABLE statement.
type CreateTable struct {
	IfNotExists      bool
	Table            TableName
	PartitionByTable *PartitionByTable
	Persistence      Persistence
	StorageParams    StorageParams
	OnCommit         CreateTableOnCommitSetting
	// In CREATE...AS queries, Defs represents a list of ColumnTableDefs, one for
	// each column, and a ConstraintTableDef for each constraint on a subset of
	// these columns.
	Defs     TableDefs
	AsSource *Select
	Locality *Locality
}

// As returns true if this table represents a CREATE TABLE ... AS statement,
// false otherwise.
func (node *CreateTable) As() bool {
	return node.AsSource != nil
}

// AsHasUserSpecifiedPrimaryKey returns true if a CREATE TABLE ... AS statement
// has a PRIMARY KEY constraint specified.
func (node *CreateTable) AsHasUserSpecifiedPrimaryKey() bool {
	if node.As() {
		for _, def := range node.Defs {
			if d, ok := def.(*ColumnTableDef); !ok {
				return false
			} else if d.PrimaryKey.IsPrimaryKey {
				return true
			}
		}
	}
	return false
}

// Format implements the NodeFormatter interface.
func (node *CreateTable) Format(ctx *FmtCtx) {
	ctx.WriteString("CREATE ")
	switch node.Persistence {
	case PersistenceTemporary:
		ctx.WriteString("TEMPORARY ")
	case PersistenceUnlogged:
		ctx.WriteString("UNLOGGED ")
	}
	ctx.WriteString("TABLE ")
	if node.IfNotExists {
		ctx.WriteString("IF NOT EXISTS ")
	}
	ctx.FormatNode(&node.Table)
	node.FormatBody(ctx)
}

// FormatBody formats the "body" of the create table definition - everything
// but the CREATE TABLE tableName part.
func (node *CreateTable) FormatBody(ctx *FmtCtx) {
	if node.As() {
		if len(node.Defs) > 0 {
			ctx.WriteString(" (")
			ctx.FormatNode(&node.Defs)
			ctx.WriteByte(')')
		}
		if node.StorageParams != nil {
			ctx.WriteString(` WITH (`)
			ctx.FormatNode(&node.StorageParams)
			ctx.WriteByte(')')
		}
		ctx.WriteString(" AS ")
		ctx.FormatNode(node.AsSource)
	} else {
		ctx.WriteString(" (")
		ctx.FormatNode(&node.Defs)
		ctx.WriteByte(')')
		if node.PartitionByTable != nil {
			ctx.FormatNode(node.PartitionByTable)
		}
		if node.StorageParams != nil {
			ctx.WriteString(` WITH (`)
			ctx.FormatNode(&node.StorageParams)
			ctx.WriteByte(')')
		}
		if node.Locality != nil {
			ctx.WriteString(" ")
			ctx.FormatNode(node.Locality)
		}
	}
	switch node.OnCommit {
	case CreateTableOnCommitUnset:
	case CreateTableOnCommitPreserveRows:
		ctx.WriteString(" ON COMMIT PRESERVE ROWS")
	default:
		panic(errors.AssertionFailedf("unexpected CreateTableOnCommitSetting: %d", node.OnCommit))
	}
}

// HoistConstraints finds column check and foreign key constraints defined
// inline with their columns and makes them table-level constraints, stored in
// n.Defs. For example, the foreign key constraint in
//
//	CREATE TABLE foo (a INT REFERENCES bar(a))
//
// gets pulled into a top-level constraint like:
//
//	CREATE TABLE foo (a INT, FOREIGN KEY (a) REFERENCES bar(a))
//
// Similarly, the CHECK constraint in
//
//	CREATE TABLE foo (a INT CHECK (a < 1), b INT)
//
// gets pulled into a top-level constraint like:
//
//	CREATE TABLE foo (a INT, b INT, CHECK (a < 1))
//
// Note that some SQL databases require that a constraint attached to a column
// to refer only to the column it is attached to. We follow Postgres' behavior,
// however, in omitting this restriction by blindly hoisting all column
// constraints. For example, the following table definition is accepted in
// CockroachDB and Postgres, but not necessarily other SQL databases:
//
//	CREATE TABLE foo (a INT CHECK (a < b), b INT)
//
// Unique constraints are not hoisted.
func (node *CreateTable) HoistConstraints() {
	for _, d := range node.Defs {
		if col, ok := d.(*ColumnTableDef); ok {
			for _, checkExpr := range col.CheckExprs {
				node.Defs = append(node.Defs,
					&CheckConstraintTableDef{
						Expr: checkExpr.Expr,
						Name: checkExpr.ConstraintName,
					},
				)
			}
			col.CheckExprs = nil
			if col.HasFKConstraint() {
				var targetCol NameList
				if col.References.Col != "" {
					targetCol = append(targetCol, col.References.Col)
				}
				node.Defs = append(node.Defs, &ForeignKeyConstraintTableDef{
					Table:    *col.References.Table,
					FromCols: NameList{col.Name},
					ToCols:   targetCol,
					Name:     col.References.ConstraintName,
					Actions:  col.References.Actions,
					Match:    col.References.Match,
				})
				col.References.Table = nil
			}
		}
	}
}

// CreateSchema represents a CREATE SCHEMA statement.
type CreateSchema struct {
	IfNotExists bool
	AuthRole    RoleSpec
	Schema      ObjectNamePrefix
}

// Format implements the NodeFormatter interface.
func (node *CreateSchema) Format(ctx *FmtCtx) {
	ctx.WriteString("CREATE SCHEMA")

	if node.IfNotExists {
		ctx.WriteString(" IF NOT EXISTS")
	}

	if node.Schema.ExplicitSchema {
		ctx.WriteString(" ")
		ctx.FormatNode(&node.Schema)
	}

	if !node.AuthRole.Undefined() {
		ctx.WriteString(" AUTHORIZATION ")
		ctx.FormatNode(&node.AuthRole)
	}
}

// CreateSequence represents a CREATE SEQUENCE statement.
type CreateSequence struct {
	IfNotExists bool
	Name        TableName
	Persistence Persistence
	Options     SequenceOptions
}

// Format implements the NodeFormatter interface.
func (node *CreateSequence) Format(ctx *FmtCtx) {
	ctx.WriteString("CREATE ")

	if node.Persistence == PersistenceTemporary {
		ctx.WriteString("TEMPORARY ")
	}

	ctx.WriteString("SEQUENCE ")

	if node.IfNotExists {
		ctx.WriteString("IF NOT EXISTS ")
	}
	ctx.FormatNode(&node.Name)
	ctx.FormatNode(&node.Options)
}

// SequenceOptions represents a list of sequence options.
type SequenceOptions []SequenceOption

// Format implements the NodeFormatter interface.
func (node *SequenceOptions) Format(ctx *FmtCtx) {
	for i := range *node {
		option := &(*node)[i]
		ctx.WriteByte(' ')
		switch option.Name {
		case SeqOptAs:
			ctx.WriteString(option.Name)
			ctx.WriteByte(' ')
			ctx.WriteString(option.AsIntegerType.SQLString())
		case SeqOptCycle, SeqOptNoCycle:
			ctx.WriteString(option.Name)
		case SeqOptCacheNode, SeqOptCacheSession:
			ctx.WriteString(option.Name)
			ctx.WriteByte(' ')
			// TODO(knz): replace all this with ctx.FormatNode if/when
			// the cache option supports expressions.
			if ctx.flags.HasFlags(FmtHideConstants) {
				ctx.WriteByte('0')
			} else {
				ctx.Printf("%d", *option.IntVal)
			}
		case SeqOptMaxValue, SeqOptMinValue:
			if option.IntVal == nil {
				ctx.WriteString("NO ")
				ctx.WriteString(option.Name)
			} else {
				ctx.WriteString(option.Name)
				ctx.WriteByte(' ')
				// TODO(knz): replace all this with ctx.FormatNode if/when
				// the min/max value options support expressions.
				if ctx.flags.HasFlags(FmtHideConstants) {
					ctx.WriteByte('0')
				} else {
					ctx.Printf("%d", *option.IntVal)
				}
			}
		case SeqOptStart:
			ctx.WriteString(option.Name)
			ctx.WriteByte(' ')
			if option.OptionalWord {
				ctx.WriteString("WITH ")
			}
			// TODO(knz): replace all this with ctx.FormatNode if/when
			// the start option supports expressions.
			if ctx.flags.HasFlags(FmtHideConstants) {
				ctx.WriteByte('0')
			} else {
				ctx.Printf("%d", *option.IntVal)
			}
		case SeqOptRestart:
			ctx.WriteString(option.Name)
			if option.IntVal != nil {
				ctx.WriteByte(' ')
				if option.OptionalWord {
					ctx.WriteString("WITH ")
				}
				if ctx.flags.HasFlags(FmtHideConstants) {
					ctx.WriteByte('0')
				} else {
					ctx.Printf("%d", *option.IntVal)
				}
			}
		case SeqOptIncrement:
			ctx.WriteString(option.Name)
			ctx.WriteByte(' ')
			if option.OptionalWord {
				ctx.WriteString("BY ")
			}
			// TODO(knz): replace all this with ctx.FormatNode if/when
			// the increment option supports expressions.
			if ctx.flags.HasFlags(FmtHideConstants) {
				ctx.WriteByte('0')
			} else {
				ctx.Printf("%d", *option.IntVal)
			}
		case SeqOptVirtual:
			ctx.WriteString(option.Name)
		case SeqOptOwnedBy:
			ctx.WriteString(option.Name)
			ctx.WriteByte(' ')
			switch option.ColumnItemVal {
			case nil:
				ctx.WriteString("NONE")
			default:
				ctx.FormatNode(option.ColumnItemVal)
			}
		default:
			panic(errors.AssertionFailedf("unexpected SequenceOption: %v", option))
		}
	}
}

// SequenceOption represents an option on a CREATE SEQUENCE statement.
type SequenceOption struct {
	Name string

	// AsIntegerType specifies default min and max values of a sequence.
	AsIntegerType *types.T

	IntVal *int64

	OptionalWord bool

	ColumnItemVal *ColumnItem
}

// Names of options on CREATE SEQUENCE.
const (
	SeqOptAs           = "AS"
	SeqOptCycle        = "CYCLE"
	SeqOptNoCycle      = "NO CYCLE"
	SeqOptOwnedBy      = "OWNED BY"
	SeqOptCacheNode    = "PER NODE CACHE"
	SeqOptCacheSession = "PER SESSION CACHE"
	SeqOptIncrement    = "INCREMENT"
	SeqOptMinValue     = "MINVALUE"
	SeqOptMaxValue     = "MAXVALUE"
	SeqOptStart        = "START"
	SeqOptRestart      = "RESTART"
	SeqOptVirtual      = "VIRTUAL"

	// Avoid unused warning for constants.
	_ = SeqOptAs
)

// LikeTableDef represents a LIKE table declaration on a CREATE TABLE statement.
type LikeTableDef struct {
	Name    TableName
	Options []LikeTableOption
}

// LikeTableOption represents an individual INCLUDING / EXCLUDING statement
// on a LIKE table declaration.
type LikeTableOption struct {
	Excluded bool
	Opt      LikeTableOpt
}

// Format implements the NodeFormatter interface.
func (def *LikeTableDef) Format(ctx *FmtCtx) {
	ctx.WriteString("LIKE ")
	ctx.FormatNode(&def.Name)
	for _, o := range def.Options {
		ctx.WriteString(" ")
		ctx.FormatNode(o)
	}
}

// Format implements the NodeFormatter interface.
func (l LikeTableOption) Format(ctx *FmtCtx) {
	if l.Excluded {
		ctx.WriteString("EXCLUDING ")
	} else {
		ctx.WriteString("INCLUDING ")
	}
	ctx.WriteString(l.Opt.String())
}

// LikeTableOpt represents one of the types of things that can be included or
// excluded in a LIKE table declaration. It's a bitmap, where each of the Opt
// values is a single enabled bit in the map.
type LikeTableOpt int

// The values for LikeTableOpt.
const (
	LikeTableOptConstraints LikeTableOpt = 1 << iota
	LikeTableOptDefaults
	LikeTableOptGenerated
	LikeTableOptIndexes

	// Make sure this field stays last!
	likeTableOptInvalid
)

// LikeTableOptAll is the full LikeTableOpt bitmap.
const LikeTableOptAll = ^likeTableOptInvalid

// Has returns true if the receiver has all of the given option bits set.
func (o LikeTableOpt) Has(other LikeTableOpt) bool {
	return int(o)&int(other) == int(other)
}

func (o LikeTableOpt) String() string {
	switch o {
	case LikeTableOptConstraints:
		return "CONSTRAINTS"
	case LikeTableOptDefaults:
		return "DEFAULTS"
	case LikeTableOptGenerated:
		return "GENERATED"
	case LikeTableOptIndexes:
		return "INDEXES"
	case LikeTableOptAll:
		return "ALL"
	default:
		panic("unknown like table opt" + strconv.Itoa(int(o)))
	}
}

func (o *KVOptions) formatAsRoleOptions(ctx *FmtCtx) {
	for _, option := range *o {
		ctx.WriteByte(' ')
		// Role option keys are always sequences of keywords separated
		// by spaces.
		ctx.WriteString(strings.ToUpper(string(option.Key)))

		// Password is a special case.
		if strings.HasSuffix(string(option.Key), "password") {
			ctx.WriteByte(' ')
			if ctx.flags.HasFlags(FmtShowPasswords) {
				ctx.FormatNode(option.Value)
			} else {
				ctx.WriteString(PasswordSubstitution)
			}
		} else if option.Value != nil {
			ctx.WriteByte(' ')
			if ctx.HasFlags(FmtHideConstants) {
				ctx.WriteString("'_'")
			} else {
				ctx.FormatNode(option.Value)
			}
		}
	}
}

// CreateRole represents a CREATE ROLE statement.
type CreateRole struct {
	Name        RoleSpec
	IfNotExists bool
	IsRole      bool
	KVOptions   KVOptions
}

// Format implements the NodeFormatter interface.
func (node *CreateRole) Format(ctx *FmtCtx) {
	ctx.WriteString("CREATE")
	if node.IsRole {
		ctx.WriteString(" ROLE ")
	} else {
		ctx.WriteString(" USER ")
	}
	if node.IfNotExists {
		ctx.WriteString("IF NOT EXISTS ")
	}
	ctx.FormatNode(&node.Name)

	if len(node.KVOptions) > 0 {
		ctx.WriteString(" WITH")
		node.KVOptions.formatAsRoleOptions(ctx)
	}
}

// ViewOptions represents options for CREATE VIEW statements.
type ViewOptions struct {
	SecurityInvoker bool
}

// Format implements the NodeFormatter interface.
func (node *ViewOptions) Format(ctx *FmtCtx) {
	if node.SecurityInvoker {
		ctx.WriteString("security_invoker = true")
	} else {
		ctx.WriteString("security_invoker = false")
	}
}

func (node *ViewOptions) doc(p *PrettyCfg) pretty.Doc {
	if node.SecurityInvoker {
		return pretty.Text("security_invoker = true")
	} else {
		return pretty.Text("security_invoker = false")
	}
}

// CreateView represents a CREATE VIEW statement.
type CreateView struct {
	Name         TableName
	ColumnNames  NameList
	AsSource     *Select
	IfNotExists  bool
	Persistence  Persistence
	Options      *ViewOptions
	Replace      bool
	Materialized bool
	WithData     bool
}

// Format implements the NodeFormatter interface.
func (node *CreateView) Format(ctx *FmtCtx) {
	ctx.WriteString("CREATE ")

	if node.Replace {
		ctx.WriteString("OR REPLACE ")
	}

	if node.Persistence == PersistenceTemporary {
		ctx.WriteString("TEMPORARY ")
	}

	if node.Materialized {
		ctx.WriteString("MATERIALIZED ")
	}

	ctx.WriteString("VIEW ")

	if node.IfNotExists {
		ctx.WriteString("IF NOT EXISTS ")
	}
	ctx.FormatNode(&node.Name)

	if len(node.ColumnNames) > 0 {
		ctx.WriteByte(' ')
		ctx.WriteByte('(')
		ctx.FormatNode(&node.ColumnNames)
		ctx.WriteByte(')')
	}

	if node.Options != nil {
		ctx.WriteString(` WITH ( `)
		ctx.FormatNode(node.Options)
		ctx.WriteString(` )`)
	}

	ctx.WriteString(" AS ")
	ctx.FormatNode(node.AsSource)
	if node.Materialized && node.WithData {
		ctx.WriteString(" WITH DATA")
	} else if node.Materialized && !node.WithData {
		ctx.WriteString(" WITH NO DATA")
	}
}

// RefreshMaterializedView represents a REFRESH MATERIALIZED VIEW statement.
type RefreshMaterializedView struct {
	Name              *UnresolvedObjectName
	Concurrently      bool
	RefreshDataOption RefreshDataOption
	AsOf              AsOfClause
}

// RefreshDataOption corresponds to arguments for the REFRESH MATERIALIZED VIEW
// statement.
type RefreshDataOption int

const (
	// RefreshDataDefault refers to no option provided to the REFRESH MATERIALIZED
	// VIEW statement.
	RefreshDataDefault RefreshDataOption = iota
	// RefreshDataWithData refers to the WITH DATA option provided to the REFRESH
	// MATERIALIZED VIEW statement.
	RefreshDataWithData
	// RefreshDataClear refers to the WITH NO DATA option provided to the REFRESH
	// MATERIALIZED VIEW statement.
	RefreshDataClear
)

// Format implements the NodeFormatter interface.
func (node *RefreshMaterializedView) Format(ctx *FmtCtx) {
	ctx.WriteString("REFRESH MATERIALIZED VIEW ")
	if node.Concurrently {
		ctx.WriteString("CONCURRENTLY ")
	}
	ctx.FormatNode(node.Name)
	if node.AsOf.Expr != nil {
		ctx.WriteString(" ")
		ctx.FormatNode(&node.AsOf)
	}
	switch node.RefreshDataOption {
	case RefreshDataWithData:
		ctx.WriteString(" WITH DATA")
	case RefreshDataClear:
		ctx.WriteString(" WITH NO DATA")
	}
}

// CreateStats represents a CREATE STATISTICS statement.
type CreateStats struct {
	Name        Name
	ColumnNames NameList
	Table       TableExpr
	Options     CreateStatsOptions
}

// Format implements the NodeFormatter interface.
func (node *CreateStats) Format(ctx *FmtCtx) {
	ctx.WriteString("CREATE STATISTICS ")
	ctx.FormatNode(&node.Name)

	if len(node.ColumnNames) > 0 {
		ctx.WriteString(" ON ")
		ctx.FormatNode(&node.ColumnNames)
	}

	ctx.WriteString(" FROM ")
	ctx.FormatNode(node.Table)

	if !node.Options.Empty() {
		ctx.WriteString(" WITH OPTIONS")
		ctx.FormatNode(&node.Options)
	}
}

// CreateStatsOptions contains options for CREATE STATISTICS.
type CreateStatsOptions struct {
	// Throttling enables throttling and indicates the fraction of time we are
	// idling (between 0 and 1).
	Throttling float64

	// AsOf performs a historical read at the given timestamp.
	// Note that the timestamp will be moved up during the operation if it gets
	// too old (in order to avoid problems with TTL expiration).
	AsOf AsOfClause

	// UsingExtremes is true when the statistics collection is at
	// extreme values of the table or the index specified.
	UsingExtremes bool

	// Where will specify statistics collection in a set of rows of the table
	// or index specified.
	Where *Where
}

// Empty returns true if no options were provided.
func (o *CreateStatsOptions) Empty() bool {
	return o.Throttling == 0 && o.AsOf.Expr == nil && o.Where == nil && !o.UsingExtremes
}

// Format implements the NodeFormatter interface.
func (o *CreateStatsOptions) Format(ctx *FmtCtx) {
	if o.UsingExtremes {
		ctx.WriteString(" USING EXTREMES")
	}
	if o.Where != nil {
		ctx.WriteByte(' ')
		ctx.FormatNode(o.Where)
	}
	if o.Throttling != 0 {
		ctx.WriteString(" THROTTLING ")
		// TODO(knz): Remove all this with ctx.FormatNode()
		// if/when throttling supports full expressions.
		if ctx.flags.HasFlags(FmtHideConstants) {
			// Using the value '0.001' instead of '0.0', because
			// when using '0.0' the statement does not get
			// formatted with the THROTTLING option.
			ctx.WriteString("0.001")
		} else {
			fmt.Fprintf(ctx, "%g", o.Throttling)
		}
	}
	if o.AsOf.Expr != nil {
		ctx.WriteByte(' ')
		ctx.FormatNode(&o.AsOf)
	}
}

// CombineWith combines two options, erroring out if the two options contain
// incompatible settings.
func (o *CreateStatsOptions) CombineWith(other *CreateStatsOptions) error {
	if other.Throttling != 0 {
		if o.Throttling != 0 {
			return errors.New("THROTTLING specified multiple times")
		}
		o.Throttling = other.Throttling
	}
	if other.AsOf.Expr != nil {
		if o.AsOf.Expr != nil {
			return errors.New("AS OF specified multiple times")
		}
		o.AsOf = other.AsOf
	}
	if other.UsingExtremes {
		if o.UsingExtremes {
			return errors.New("USING EXTREMES specified multiple times")
		}
		o.UsingExtremes = other.UsingExtremes
	}
	if other.Where != nil {
		if o.Where != nil {
			return errors.New("WHERE specified multiple times")
		}
		o.Where = other.Where
	}
	if other.Where != nil && o.UsingExtremes || o.Where != nil && other.UsingExtremes {
		return errors.New("USING EXTREMES and WHERE may not be specified together")
	}
	return nil
}

// CreateExtension represents a CREATE EXTENSION statement.
type CreateExtension struct {
	Name        Name
	IfNotExists bool
}

// Format implements the NodeFormatter interface.
func (node *CreateExtension) Format(ctx *FmtCtx) {
	ctx.WriteString("CREATE EXTENSION ")
	if node.IfNotExists {
		ctx.WriteString("IF NOT EXISTS ")
	}
	// NB: we do not anonymize the extension name
	// because 1) we assume that extension names
	// do not contain sensitive information and
	// 2) we want to get telemetry on which extensions
	// users attempt to load.
	ctx.WithFlags(ctx.flags&^FmtAnonymize, func() {
		ctx.FormatNode(&node.Name)
	})
}

// CreateExternalConnection represents a CREATE EXTERNAL CONNECTION statement.
type CreateExternalConnection struct {
	ConnectionLabelSpec LabelSpec
	As                  Expr
}

var _ Statement = &CreateExternalConnection{}

// Format implements the Statement interface.
func (node *CreateExternalConnection) Format(ctx *FmtCtx) {
	ctx.WriteString("CREATE EXTERNAL CONNECTION")
	ctx.FormatNode(&node.ConnectionLabelSpec)
	ctx.WriteString(" AS ")
	ctx.FormatURI(node.As)
}

// CreateTenant represents a CREATE VIRTUAL CLUSTER statement.
type CreateTenant struct {
	IfNotExists bool
	TenantSpec  *TenantSpec
}

// Format implements the NodeFormatter interface.
func (node *CreateTenant) Format(ctx *FmtCtx) {
	ctx.WriteString("CREATE VIRTUAL CLUSTER ")
	if node.IfNotExists {
		ctx.WriteString("IF NOT EXISTS ")
	}
	ctx.FormatNode(node.TenantSpec)
}

// CreateTenantFromReplication represents a CREATE VIRTUAL CLUSTER...FROM REPLICATION
// statement.
type CreateTenantFromReplication struct {
	IfNotExists bool
	TenantSpec  *TenantSpec

	// ReplicationSourceTenantName is the name of the tenant that
	// we are replicating into the newly created tenant.
	// Note: even though this field can only be a name
	// (this is guaranteed during parsing), we still want
	// to use the TenantSpec type. This supports the auto-promotion
	// of simple identifiers to strings.
	ReplicationSourceTenantName *TenantSpec
	// ReplicationSourceConnUri is the address of the source cluster that we are
	// replicating data from.
	ReplicationSourceConnUri Expr

	Options TenantReplicationOptions
}

// TenantReplicationOptions  options for the CREATE/ALTER VIRTUAL CLUSTER FROM REPLICATION command.
type TenantReplicationOptions struct {
	Retention          Expr
	ExpirationWindow   Expr
	EnableReaderTenant *DBool
}

var _ NodeFormatter = &TenantReplicationOptions{}

// Format implements the NodeFormatter interface.
func (node *CreateTenantFromReplication) Format(ctx *FmtCtx) {
	ctx.WriteString("CREATE VIRTUAL CLUSTER ")
	if node.IfNotExists {
		ctx.WriteString("IF NOT EXISTS ")
	}
	// NB: we do not anonymize the tenant name because we assume that tenant names
	// do not contain sensitive information.
	ctx.FormatNode(node.TenantSpec)

	if node.ReplicationSourceConnUri != nil {
		ctx.WriteString(" FROM REPLICATION OF ")
		ctx.FormatNode(node.ReplicationSourceTenantName)
		ctx.WriteString(" ON ")
		_, canOmitParentheses := node.ReplicationSourceConnUri.(alreadyDelimitedAsSyntacticDExpr)
		if !canOmitParentheses {
			ctx.WriteByte('(')
		}
		ctx.FormatNode(node.ReplicationSourceConnUri)
		if !canOmitParentheses {
			ctx.WriteByte(')')
		}

	}
	if !node.Options.IsDefault() {
		ctx.WriteString(" WITH ")
		ctx.FormatNode(&node.Options)
	}
}

// Format implements the NodeFormatter interface
func (o *TenantReplicationOptions) Format(ctx *FmtCtx) {
	var addSep bool
	maybeAddSep := func() {
		if addSep {
			ctx.WriteString(", ")
		}
		addSep = true
	}
	if o.Retention != nil {
		maybeAddSep()
		ctx.WriteString("RETENTION = ")
		_, canOmitParentheses := o.Retention.(alreadyDelimitedAsSyntacticDExpr)
		if !canOmitParentheses {
			ctx.WriteByte('(')
		}
		ctx.FormatNode(o.Retention)
		if !canOmitParentheses {
			ctx.WriteByte(')')
		}
	}
	if o.ExpirationWindow != nil {
		maybeAddSep()
		ctx.WriteString("EXPIRATION WINDOW = ")
		_, canOmitParentheses := o.ExpirationWindow.(alreadyDelimitedAsSyntacticDExpr)
		if !canOmitParentheses {
			ctx.WriteByte('(')
		}
		ctx.FormatNode(o.ExpirationWindow)
		if !canOmitParentheses {
			ctx.WriteByte(')')
		}
	}
	if o.EnableReaderTenant != nil {
		maybeAddSep()
		ctx.WriteString("READ VIRTUAL CLUSTER")
	}
}

// CombineWith merges other TenantReplicationOptions into this struct.
// An error is returned if the same option merged multiple times.
func (o *TenantReplicationOptions) CombineWith(other *TenantReplicationOptions) error {
	if o.Retention != nil {
		if other.Retention != nil {
			return errors.New("RETENTION option specified multiple times")
		}
	} else {
		o.Retention = other.Retention
	}

	if o.ExpirationWindow != nil {
		if other.ExpirationWindow != nil {
			return errors.New("EXPIRATION WINDOW option specified multiple times")
		}
	} else {
		o.ExpirationWindow = other.ExpirationWindow
	}

	if o.EnableReaderTenant != nil {
		if other.EnableReaderTenant != nil {
			return errors.New("READ VIRTUAL CLUSTER option specified multiple times")
		} else {
			o.EnableReaderTenant = other.EnableReaderTenant
		}
	}

	return nil
}

// IsDefault returns true if this backup options struct has default value.
func (o TenantReplicationOptions) IsDefault() bool {
	options := TenantReplicationOptions{}
	return o.Retention == options.Retention &&
		o.ExpirationWindow == options.ExpirationWindow &&
		o.EnableReaderTenant == options.EnableReaderTenant
}

func (o TenantReplicationOptions) ExpirationWindowSet() bool {
	options := TenantReplicationOptions{}
	return o.ExpirationWindow != options.ExpirationWindow
}

type SuperRegion struct {
	Name    Name
	Regions NameList
}

func (node *SuperRegion) Format(ctx *FmtCtx) {
	ctx.WriteString(" SUPER REGION ")
	ctx.FormatNode(&node.Name)
	ctx.WriteString(" VALUES ")
	for i := range node.Regions {
		if i != 0 {
			ctx.WriteString(",")
		}
		ctx.FormatNode(&node.Regions[i])
	}
}
