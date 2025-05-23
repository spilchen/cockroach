// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

import React from "react";

import { cockroach } from "src/js/protos";
import { LocalityTree } from "src/redux/localities";
import { LivenessStatus } from "src/redux/nodes";
import { getChildLocalities } from "src/util/localities";

import { LocalityView } from "./localityView";
import { NodeView } from "./nodeView";

type Liveness = cockroach.kv.kvserver.liveness.livenesspb.ILiveness;

const MIN_RADIUS = 150;
const PADDING = 150;

interface CircleLayoutProps {
  localityTree: LocalityTree;
  livenessStatuses: { [id: string]: LivenessStatus };
  livenesses: { [id: string]: Liveness };
  viewportSize: [number, number];
}

export class CircleLayout extends React.Component<CircleLayoutProps> {
  coordsFor(index: number, total: number, radius: number) {
    if (total === 1) {
      return [0, 0];
    }

    if (total === 2) {
      const leftOrRight = index === 0 ? -radius : radius;
      return [leftOrRight, 0];
    }

    const angle = (2 * Math.PI * index) / total - Math.PI / 2;
    return [radius * Math.cos(angle), radius * Math.sin(angle)];
  }

  render() {
    const { localityTree, viewportSize } = this.props;
    const childLocalities = getChildLocalities(localityTree);

    const total = localityTree.nodes.length + childLocalities.length;

    const calculatedRadius = Math.min(...viewportSize) / 2 - PADDING;
    const radius = Math.max(MIN_RADIUS, calculatedRadius);

    return (
      <g transform={`translate(${viewportSize[0] / 2},${viewportSize[1] / 2})`}>
        {childLocalities.map((locality, i) => (
          <g
            key={`locality-${i}`}
            transform={`translate(${this.coordsFor(i, total, radius)})`}
          >
            <LocalityView
              localityTree={locality}
              livenessStatuses={this.props.livenessStatuses}
            />
          </g>
        ))}
        {localityTree.nodes.map((node, i) => {
          return (
            <g
              key={`node-${i}`}
              transform={`translate(${this.coordsFor(
                i + childLocalities.length,
                total,
                radius,
              )})`}
            >
              <NodeView
                node={node}
                livenessStatus={this.props.livenessStatuses[node.desc.node_id]}
                liveness={this.props.livenesses[node.desc.node_id]}
              />
            </g>
          );
        })}
      </g>
    );
  }
}
