// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package azure

import (
	"testing"

	"github.com/cockroachdb/cockroach/pkg/cloud/externalconn"
	"github.com/cockroachdb/cockroach/pkg/cloud/externalconn/connectionpb"
	"github.com/stretchr/testify/require"
)

func TestAzureKMSConnection(t *testing.T) {
	require.Equal(t, connectionpb.ConnectionProvider_azure_kms, externalconn.ProviderForURI("azure-kms://test"))
}
