// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Code generated by generate-acceptance-tests, DO NOT EDIT.

package acceptance

import (
	"testing"

	"github.com/cockroachdb/cockroach/pkg/util/log"
)

func TestDockerCLI_test_audit_log(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_audit_log", "../cli/interactive_tests/test_audit_log.tcl")
}

func TestDockerCLI_test_auto_trace(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_auto_trace", "../cli/interactive_tests/test_auto_trace.tcl")
}

func TestDockerCLI_test_cert_advisory_validation(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_cert_advisory_validation", "../cli/interactive_tests/test_cert_advisory_validation.tcl")
}

func TestDockerCLI_test_changefeed(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_changefeed", "../cli/interactive_tests/test_changefeed.tcl")
}

func TestDockerCLI_test_client_side_checking(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_client_side_checking", "../cli/interactive_tests/test_client_side_checking.tcl")
}

func TestDockerCLI_test_cluster_name(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_cluster_name", "../cli/interactive_tests/test_cluster_name.tcl")
}

func TestDockerCLI_test_contextual_help(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_contextual_help", "../cli/interactive_tests/test_contextual_help.tcl")
}

func TestDockerCLI_test_copy(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_copy", "../cli/interactive_tests/test_copy.tcl")
}

func TestDockerCLI_test_demo(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_demo", "../cli/interactive_tests/test_demo.tcl")
}

func TestDockerCLI_test_demo_changefeeds(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_demo_changefeeds", "../cli/interactive_tests/test_demo_changefeeds.tcl")
}

func TestDockerCLI_test_demo_cli_integration(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_demo_cli_integration", "../cli/interactive_tests/test_demo_cli_integration.tcl")
}

func TestDockerCLI_test_demo_global(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_demo_global", "../cli/interactive_tests/test_demo_global.tcl")
}

func TestDockerCLI_test_demo_global_insecure(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_demo_global_insecure", "../cli/interactive_tests/test_demo_global_insecure.tcl")
}

func TestDockerCLI_test_demo_locality_error(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_demo_locality_error", "../cli/interactive_tests/test_demo_locality_error.tcl")
}

func TestDockerCLI_test_demo_memory_warning(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_demo_memory_warning", "../cli/interactive_tests/test_demo_memory_warning.tcl")
}

func TestDockerCLI_test_demo_networking(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_demo_networking", "../cli/interactive_tests/test_demo_networking.tcl")
}

func TestDockerCLI_test_demo_node_cmds(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_demo_node_cmds", "../cli/interactive_tests/test_demo_node_cmds.tcl")
}

func TestDockerCLI_test_demo_partitioning(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_demo_partitioning", "../cli/interactive_tests/test_demo_partitioning.tcl")
}

func TestDockerCLI_test_demo_telemetry(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_demo_telemetry", "../cli/interactive_tests/test_demo_telemetry.tcl")
}

func TestDockerCLI_test_demo_workload(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_demo_workload", "../cli/interactive_tests/test_demo_workload.tcl")
}

func TestDockerCLI_test_disable_replication(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_disable_replication", "../cli/interactive_tests/test_disable_replication.tcl")
}

func TestDockerCLI_test_distinguished_name_validation(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_distinguished_name_validation", "../cli/interactive_tests/test_distinguished_name_validation.tcl")
}

func TestDockerCLI_test_dump_sig(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_dump_sig", "../cli/interactive_tests/test_dump_sig.tcl")
}

func TestDockerCLI_test_encryption(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_encryption", "../cli/interactive_tests/test_encryption.tcl")
}

func TestDockerCLI_test_error_handling(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_error_handling", "../cli/interactive_tests/test_error_handling.tcl")
}

func TestDockerCLI_test_error_hints(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_error_hints", "../cli/interactive_tests/test_error_hints.tcl")
}

func TestDockerCLI_test_example_data(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_example_data", "../cli/interactive_tests/test_example_data.tcl")
}

func TestDockerCLI_test_exec_log(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_exec_log", "../cli/interactive_tests/test_exec_log.tcl")
}

func TestDockerCLI_test_explain_analyze(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_explain_analyze", "../cli/interactive_tests/test_explain_analyze.tcl")
}

func TestDockerCLI_test_explain_analyze_debug(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_explain_analyze_debug", "../cli/interactive_tests/test_explain_analyze_debug.tcl")
}

func TestDockerCLI_test_extern_dir(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_extern_dir", "../cli/interactive_tests/test_extern_dir.tcl")
}

func TestDockerCLI_test_flags(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_flags", "../cli/interactive_tests/test_flags.tcl")
}

func TestDockerCLI_test_high_verbosity(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_high_verbosity", "../cli/interactive_tests/test_high_verbosity.tcl")
}

func TestDockerCLI_test_history(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_history", "../cli/interactive_tests/test_history.tcl")
}

func TestDockerCLI_test_init_command(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_init_command", "../cli/interactive_tests/test_init_command.tcl")
}

func TestDockerCLI_test_init_virtualized_command(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_init_virtualized_command", "../cli/interactive_tests/test_init_virtualized_command.tcl")
}

func TestDockerCLI_test_interrupt(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_interrupt", "../cli/interactive_tests/test_interrupt.tcl")
}

func TestDockerCLI_test_last_statement(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_last_statement", "../cli/interactive_tests/test_last_statement.tcl")
}

func TestDockerCLI_test_local_cmds(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_local_cmds", "../cli/interactive_tests/test_local_cmds.tcl")
}

func TestDockerCLI_test_log_config_msg(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_log_config_msg", "../cli/interactive_tests/test_log_config_msg.tcl")
}

func TestDockerCLI_test_log_flags(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_log_flags", "../cli/interactive_tests/test_log_flags.tcl")
}

func TestDockerCLI_test_missing_log_output(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_missing_log_output", "../cli/interactive_tests/test_missing_log_output.tcl")
}

func TestDockerCLI_test_multiline_statements(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_multiline_statements", "../cli/interactive_tests/test_multiline_statements.tcl")
}

func TestDockerCLI_test_multiple_nodes(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_multiple_nodes", "../cli/interactive_tests/test_multiple_nodes.tcl")
}

func TestDockerCLI_test_notice(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_notice", "../cli/interactive_tests/test_notice.tcl")
}

func TestDockerCLI_test_password(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_password", "../cli/interactive_tests/test_password.tcl")
}

func TestDockerCLI_test_pretty(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_pretty", "../cli/interactive_tests/test_pretty.tcl")
}

func TestDockerCLI_test_read_only(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_read_only", "../cli/interactive_tests/test_read_only.tcl")
}

func TestDockerCLI_test_reconnect(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_reconnect", "../cli/interactive_tests/test_reconnect.tcl")
}

func TestDockerCLI_test_replication_protocol(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_replication_protocol", "../cli/interactive_tests/test_replication_protocol.tcl")
}

func TestDockerCLI_test_sb_recreate(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_sb_recreate", "../cli/interactive_tests/test_sb_recreate.tcl")
}

func TestDockerCLI_test_sb_recreate_fks(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_sb_recreate_fks", "../cli/interactive_tests/test_sb_recreate_fks.tcl")
}

func TestDockerCLI_test_sb_recreate_mr(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_sb_recreate_mr", "../cli/interactive_tests/test_sb_recreate_mr.tcl")
}

func TestDockerCLI_test_sb_recreate_rls(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_sb_recreate_rls", "../cli/interactive_tests/test_sb_recreate_rls.tcl")
}

func TestDockerCLI_test_secure(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_secure", "../cli/interactive_tests/test_secure.tcl")
}

func TestDockerCLI_test_secure_ocsp(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_secure_ocsp", "../cli/interactive_tests/test_secure_ocsp.tcl")
}

func TestDockerCLI_test_server_restart(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_server_restart", "../cli/interactive_tests/test_server_restart.tcl")
}

func TestDockerCLI_test_server_sig(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_server_sig", "../cli/interactive_tests/test_server_sig.tcl")
}

func TestDockerCLI_test_socket_name(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_socket_name", "../cli/interactive_tests/test_socket_name.tcl")
}

func TestDockerCLI_test_sql_demo_node_cmds(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_sql_demo_node_cmds", "../cli/interactive_tests/test_sql_demo_node_cmds.tcl")
}

func TestDockerCLI_test_sql_mem_monitor(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_sql_mem_monitor", "../cli/interactive_tests/test_sql_mem_monitor.tcl")
}

func TestDockerCLI_test_sql_safe_updates(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_sql_safe_updates", "../cli/interactive_tests/test_sql_safe_updates.tcl")
}

func TestDockerCLI_test_sql_version_reporting(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_sql_version_reporting", "../cli/interactive_tests/test_sql_version_reporting.tcl")
}

func TestDockerCLI_test_style_enabled(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_style_enabled", "../cli/interactive_tests/test_style_enabled.tcl")
}

func TestDockerCLI_test_temp_dir(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_temp_dir", "../cli/interactive_tests/test_temp_dir.tcl")
}

func TestDockerCLI_test_timing(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_timing", "../cli/interactive_tests/test_timing.tcl")
}

func TestDockerCLI_test_txn_prompt(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_txn_prompt", "../cli/interactive_tests/test_txn_prompt.tcl")
}

func TestDockerCLI_test_url_db_override(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_url_db_override", "../cli/interactive_tests/test_url_db_override.tcl")
}

func TestDockerCLI_test_url_login(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_url_login", "../cli/interactive_tests/test_url_login.tcl")
}

func TestDockerCLI_test_workload(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_workload", "../cli/interactive_tests/test_workload.tcl")
}

func TestDockerCLI_test_zero_directory(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_zero_directory", "../cli/interactive_tests/test_zero_directory.tcl")
}

func TestDockerCLI_test_zip_filter(t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	runTestDockerCLI(t, "test_zip_filter", "../cli/interactive_tests/test_zip_filter.tcl")
}
