#!/bin/bash

set -o xtrace;
set -o errexit;

dev build short roachtest
roachprod destroy local || :
time roachtest run schemachange/bulkingest/nodes=2/rows=4000000000/create_index --local --debug-always
