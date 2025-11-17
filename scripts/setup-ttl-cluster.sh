#!/bin/bash

cluster=${CLUSTER:-local}

roachprod put $cluster cockroach cockroach
roachprod put $cluster last-ttl-partition-maintenance-job.sql last-ttl-partition-maintenance-job.sql
roachprod put $cluster last-row-level-ttl-job.sql last-row-level-ttl-job.sql
roachprod put $cluster last-hybrid-ttl-job.sql last-hybrid-ttl-job.sql
roachprod put $cluster partition-ttl-1-week-static.sql partition-ttl-1-week-static.sql
roachprod put $cluster row-ttl-1-week-static.sql row-ttl-1-week-static.sql
roachprod put $cluster partition-ttl-schedule.sql partition-ttl-schedule.sql
roachprod put $cluster row-ttl-schedule.sql row-ttl-schedule.sql
roachprod start $cluster
roachprod ssh $cluster:1 "./cockroach workload init movr {pgurl:1}"
roachprod sql $cluster:1 -e "SET CLUSTER SETTING jobs.registry.interval.adopt = '500ms'"
