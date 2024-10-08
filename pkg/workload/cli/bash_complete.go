// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cli

import "github.com/spf13/cobra"

func init() {
	AddSubCmd(func(userFacing bool) *cobra.Command {
		var bashCmd = SetCmdDefaults(&cobra.Command{
			Use:   `gen-bash-completions <output-file>`,
			Short: `generate bash completions for workload command`,
			Args:  cobra.ExactArgs(1),
		})
		bashCmd.Run = func(cmd *cobra.Command, args []string) {
			for parent := cmd.Parent(); parent != nil; parent = cmd.Parent() {
				cmd = parent
			}
			if err := cmd.GenBashCompletionFile(args[0]); err != nil {
				panic(err)
			}
		}
		bashCmd.Hidden = userFacing
		return bashCmd
	})
}
