// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

import { DatabaseDetailsPage } from "@cockroachlabs/cluster-ui";
import { connect, ReactReduxContext } from "react-redux";
import { withRouter } from "react-router-dom";

import { mapStateToProps, mapDispatchToProps } from "./redux";

const connected = withRouter(
  connect(mapStateToProps, mapDispatchToProps, null, {
    context: ReactReduxContext,
  })(DatabaseDetailsPage),
);

export { connected as DatabaseDetailsPage };
