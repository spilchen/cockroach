// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tree

import "strings"

var _ Statement = &AlterPolicy{}
var _ Statement = &CreatePolicy{}
var _ Statement = &DropPolicy{}

// RLSTableAction represents different actions that you can do to the table for
// row-level security.
type RLSTableAction int

// RLSTableAction values
const (
	RLSTableEnable RLSTableAction = iota
	RLSTableDisable
	RLSTableForce
	RLSTableNoForce
)

var rlsTableActionName = [...]string{
	RLSTableEnable:  "ENABLE",
	RLSTableDisable: "DISABLE",
	RLSTableForce:   "FORCE",
	RLSTableNoForce: "NO FORCE",
}

func (r RLSTableAction) String() string {
	return rlsTableActionName[r]
}

func (r RLSTableAction) TelemetryName() string {
	return strings.ReplaceAll(strings.ToLower(r.String()), " ", "_")
}

// PolicyCommand represents the type of commands a row-level security policy
// will apply against.
type PolicyCommand int

// PolicyCommand values
const (
	PolicyCommandDefault PolicyCommand = iota
	PolicyCommandAll
	PolicyCommandSelect
	PolicyCommandInsert
	PolicyCommandUpdate
	PolicyCommandDelete
)

var policyCommandName = [...]string{
	PolicyCommandDefault: "",
	PolicyCommandAll:     "ALL",
	PolicyCommandSelect:  "SELECT",
	PolicyCommandInsert:  "INSERT",
	PolicyCommandUpdate:  "UPDATE",
	PolicyCommandDelete:  "DELETE",
}

func (p PolicyCommand) String() string { return policyCommandName[p] }

// PolicyType represents the type of the row-level security policy.
type PolicyType int

// PolicyType values
const (
	PolicyTypeDefault PolicyType = iota
	PolicyTypePermissive
	PolicyTypeRestrictive
)

var policyTypeName = [...]string{
	PolicyTypeDefault:     "",
	PolicyTypePermissive:  "PERMISSIVE",
	PolicyTypeRestrictive: "RESTRICTIVE",
}

func (p PolicyType) String() string { return policyTypeName[p] }

// AlterPolicy is a tree struct for the ALTER POLICY DDL statement
type AlterPolicy struct {
	Policy    Name
	Table     TableName
	NewPolicy Name
	Roles     RoleSpecList
	Using     Expr
	WithCheck Expr
}

// Format implements the NodeFormatter interface.
func (node *AlterPolicy) Format(ctx *FmtCtx) {
	ctx.WriteString("ALTER POLICY ")
	ctx.FormatName(string(node.Policy))
	ctx.WriteString(" ON ")
	ctx.FormatNode(&node.Table)

	if node.NewPolicy != "" {
		ctx.WriteString(" RENAME TO ")
		ctx.FormatName(string(node.NewPolicy))
		return
	}

	if len(node.Roles) > 0 {
		ctx.WriteString(" TO ")
		ctx.FormatNode(&node.Roles)
	}

	if node.Using != nil {
		ctx.WriteString(" USING (")
		ctx.FormatNode(node.Using)
		ctx.WriteString(")")
	}

	if node.WithCheck != nil {
		ctx.WriteString(" WITH CHECK (")
		ctx.FormatNode(node.WithCheck)
		ctx.WriteString(")")
	}
}

// CreatePolicy is a tree struct for the CREATE POLICY DDL statement
type CreatePolicy struct {
	Policy    Name
	Table     TableName
	Type      PolicyType
	Cmd       PolicyCommand
	Roles     RoleSpecList
	Using     Expr
	WithCheck Expr
}

// Format implements the NodeFormatter interface.
func (node *CreatePolicy) Format(ctx *FmtCtx) {
	ctx.WriteString("CREATE POLICY ")
	ctx.FormatName(string(node.Policy))
	ctx.WriteString(" ON ")
	ctx.FormatNode(&node.Table)
	if node.Type != PolicyTypeDefault {
		ctx.WriteString(" AS ")
		ctx.WriteString(node.Type.String())
	}
	if node.Cmd != PolicyCommandDefault {
		ctx.WriteString(" FOR ")
		ctx.WriteString(node.Cmd.String())
	}

	if len(node.Roles) > 0 {
		ctx.WriteString(" TO ")
		ctx.FormatNode(&node.Roles)
	}

	if node.Using != nil {
		ctx.WriteString(" USING (")
		ctx.FormatNode(node.Using)
		ctx.WriteString(")")
	}

	if node.WithCheck != nil {
		ctx.WriteString(" WITH CHECK (")
		ctx.FormatNode(node.WithCheck)
		ctx.WriteString(")")
	}
}

// DropPolicy is a tree struct for the DROP POLICY DDL statement
type DropPolicy struct {
	Policy       Name
	Table        *UnresolvedObjectName
	DropBehavior DropBehavior
	IfExists     bool
}

// Format implements the NodeFormatter interface.
func (node *DropPolicy) Format(ctx *FmtCtx) {
	ctx.WriteString("DROP POLICY ")
	if node.IfExists {
		ctx.WriteString("IF EXISTS ")
	}
	ctx.FormatName(string(node.Policy))
	ctx.WriteString(" ON ")
	ctx.FormatNode(node.Table)
	if node.DropBehavior != DropDefault {
		ctx.WriteString(" ")
		ctx.WriteString(node.DropBehavior.String())
	}
}
