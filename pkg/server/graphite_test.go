// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package server

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

// TestGraphite tests that a server pushes metrics data to Graphite endpoint,
// if configured. In addition, it verifies that things don't fall apart when
// the endpoint goes away.
//
// TODO(obs-inf): this test takes 2m because GraphiteExporter.Push times out after
// 2m with a `write: broken pipe` error, even though it's using a DialTimeout. This
// is a waste of time.
func TestGraphite(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	s, rawDB, _ := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(context.Background())
	ctx := context.Background()

	const setQ = `SET CLUSTER SETTING "%s" = "%s"`
	const interval = 3 * time.Millisecond
	db := sqlutils.MakeSQLRunner(rawDB)
	db.Exec(t, fmt.Sprintf(setQ, graphiteIntervalKey, interval))

	listen := func() {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal("failed to open port", err)
		}
		p := lis.Addr().String()
		log.Infof(ctx, "Open port %s and listening", p)

		defer func() {
			log.Infof(ctx, "Close port %s", p)
			if err := lis.Close(); err != nil {
				t.Fatal("failed to close port", err)
			}
		}()

		db.Exec(t, fmt.Sprintf(setQ, "external.graphite.endpoint", p))
		if _, e := lis.Accept(); e != nil {
			t.Fatal("failed to receive connection", e)
		} else {
			log.Info(ctx, "received connection")
		}
	}

	listen()
	log.Info(ctx, "Make sure things don't fall apart when endpoint goes away.")
	time.Sleep(5 * interval)
	listen()
}
