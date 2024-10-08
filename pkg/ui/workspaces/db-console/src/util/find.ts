// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

import React from "react";

/**
 * findChildrenOfType performs a DFS of the supplied React children collection,
 * returning all children which are ReactElements of the supplied type.
 */
export function findChildrenOfType<P>(
  children: React.ReactChild,
  type: string | React.ComponentClass<P> | React.SFC<P>,
): React.ReactElement<P>[] {
  const matchingChildren: React.ReactElement<P>[] = [];
  const childrenToSearch = React.Children.toArray(children);
  while (childrenToSearch.length > 0) {
    const child: React.ReactChild =
      childrenToSearch.shift() as React.ReactChild;
    if (!isReactElement(child)) {
      continue;
    } else {
      if (child.type === type) {
        matchingChildren.push(child);
      }
      const { props } = child;
      if (props.children) {
        // Children added to front of search array for DFS.
        childrenToSearch.unshift(...React.Children.toArray(props.children));
      }
    }
  }

  return matchingChildren;
}

/**
 * Predicate function to determine if a react child is a ReactElement.
 */
function isReactElement(
  child: React.ReactChild,
): child is React.ReactElement<any> {
  return (<React.ReactElement<any>>child).type !== undefined;
}
