#!/bin/bash

cluster=${CLUSTER:-local}
total=500000
chunk=100000

base_ts="timestamp '2025-11-01 00:00:00+00'"
range_int="interval '9 days 23 hours 59 minutes 59 seconds'"  # inclusive upper bound handling

roachprod ssh $cluster:1 -- 'rm -rf data/extern/rides_csv'


for ((offset=0; offset<total; offset+=chunk)); do
  start=$((offset+1))
  end=$((offset+chunk))
  echo "Exporting rows $start to $end..."
  roachprod sql $cluster:1 -- -e "
  EXPORT INTO CSV 'nodelocal://1/rides_csv/rides_${start}_${end}'
  FROM
  SELECT
      gen_random_uuid() AS id,
      'nyc' AS city,
      'nyc' AS vehicle_city,
      gen_random_uuid() AS rider_id,
      gen_random_uuid() AS vehicle_id,
      concat('Start_', g) AS start_address,
      concat('End_', g) AS end_address,
      (${base_ts} + (random() * ${range_int})) AS start_time,
      (${base_ts} + (random() * ${range_int})) AS end_time,
      round((random() * 30)::numeric, 2) AS revenue
  FROM generate_series($start, $end) AS g;"
done
