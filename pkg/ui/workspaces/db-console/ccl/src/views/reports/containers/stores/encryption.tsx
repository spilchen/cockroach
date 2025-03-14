// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

import { util } from "@cockroachlabs/cluster-ui";
import * as protosccl from "@cockroachlabs/crdb-protobuf-client-ccl";
import isEmpty from "lodash/isEmpty";
import Long from "long";
import moment from "moment-timezone";
import React from "react";

import { EncryptionStatusProps } from "oss/src/views/reports/containers/stores/encryption";
import * as protos from "src/js/protos";
import { FixLong } from "src/util/fixLong";

const dateFormat = "Y-MM-DD HH:mm:ss";

export default class EncryptionStatus {
  props: EncryptionStatusProps;

  constructor(props: EncryptionStatusProps) {
    this.props = props;
  }

  renderHeaderRow(header: string) {
    return (
      <tr className="stores-table__row">
        <td
          colSpan={2}
          className="stores-table__cell stores-table__cell--header--row"
        >
          {header}
        </td>
      </tr>
    );
  }

  renderSimpleRow(header: string, value: string) {
    return (
      <tr className="stores-table__row">
        <th className="stores-table__cell stores-table__cell--header">
          {header}
        </th>
        <td className="stores-table__cell" title={value}>
          {value}
        </td>
      </tr>
    );
  }

  renderStoreKey(key: protosccl.cockroach.storage.enginepb.IKeyInfo) {
    // Get the enum name from its value (eg: "AES128_CTR" for 1).
    const encryptionType =
      protosccl.cockroach.storage.enginepb.EncryptionType[key.encryption_type];
    const createdAt = moment
      .unix(FixLong(key.creation_time).toNumber())
      .utc()
      .format(dateFormat);

    return [
      this.renderHeaderRow("Active Store Key: user specified"),
      this.renderSimpleRow("Algorithm", encryptionType),
      this.renderSimpleRow("Key ID", key.key_id),
      this.renderSimpleRow("Created", createdAt),
      this.renderSimpleRow("Source", key.source),
    ];
  }

  renderDataKey(key: protosccl.cockroach.storage.enginepb.IKeyInfo) {
    // Get the enum name from its value (eg: "AES128_CTR" for 1).
    const encryptionType =
      protosccl.cockroach.storage.enginepb.EncryptionType[key.encryption_type];
    const createdAt = moment
      .unix(key.creation_time.toNumber())
      .utc()
      .format(dateFormat);

    return [
      this.renderHeaderRow("Active Data Key: automatically generated"),
      this.renderSimpleRow("Algorithm", encryptionType),
      this.renderSimpleRow("Key ID", key.key_id),
      this.renderSimpleRow("Created", createdAt),
      this.renderSimpleRow("Parent Key ID", key.parent_key_id),
    ];
  }

  calculatePercentage(active: Long, total: Long): number {
    if (active.eq(total)) {
      return 100;
    }
    return Long.fromInt(100).mul(active).toNumber() / total.toNumber();
  }

  renderFileStats(stats: protos.cockroach.server.serverpb.IStoreDetails) {
    const { Bytes } = util;
    const totalFiles = FixLong(stats.total_files);
    const totalBytes = FixLong(stats.total_bytes);
    if (totalFiles.eq(0) && totalBytes.eq(0)) {
      return null;
    }

    const activeFiles = FixLong(stats.active_key_files);
    const activeBytes = FixLong(stats.active_key_bytes);

    let fileDetails =
      this.calculatePercentage(activeFiles, totalFiles).toFixed(2) + "%";
    fileDetails += " (" + activeFiles + "/" + totalFiles + ")";

    let byteDetails =
      this.calculatePercentage(activeBytes, totalBytes).toFixed(2) + "%";
    byteDetails +=
      " (" +
      Bytes(activeBytes.toNumber()) +
      "/" +
      Bytes(totalBytes.toNumber()) +
      ")";

    return [
      this.renderHeaderRow(
        "Encryption Progress: fraction encrypted using the active data key",
      ),
      this.renderSimpleRow("Files", fileDetails),
      this.renderSimpleRow("Bytes", byteDetails),
    ];
  }

  getEncryptionRows() {
    const { store } = this.props;
    const rawStatus = store.encryption_status;
    if (isEmpty(rawStatus)) {
      return [this.renderSimpleRow("Encryption status", "Not encrypted")];
    }

    let decodedStatus;

    // Attempt to decode protobuf.
    try {
      decodedStatus =
        protosccl.cockroach.storage.enginepb.EncryptionStatus.decode(rawStatus);
    } catch (e) {
      return [
        this.renderSimpleRow(
          "Encryption status",
          "Error decoding protobuf: " + e.toString(),
        ),
      ];
    }

    return [
      this.renderStoreKey(decodedStatus.active_store_key),
      this.renderDataKey(decodedStatus.active_data_key),
      this.renderFileStats(store),
    ];
  }
}
