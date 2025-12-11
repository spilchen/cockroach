use movr;

-- turn off auto-add of partitions so we have control of what expires
SET CLUSTER SETTING sql.ttl.partition.auto_add_partitions.enabled=false;

-- cannot have inbound foreign key on a table that is partitioned TTL
alter table vehicle_location_histories drop constraint if exists vehicle_location_histories_city_ride_id_fkey;

SHOW TABLES;
DROP TABLE IF EXISTS rides ;
  CREATE TABLE rides (
      id UUID NOT NULL,
      city VARCHAR NOT NULL,
      vehicle_city VARCHAR NULL,
      rider_id UUID NULL,
      vehicle_id UUID NULL,
      start_address VARCHAR NULL,
      end_address VARCHAR NULL,
      start_time TIMESTAMPTZ NULL,
      end_time TIMESTAMPTZ NULL,
      revenue DECIMAL(10,2) NULL,
      CONSTRAINT rides_pkey PRIMARY KEY (start_time ASC, city ASC, id ASC),
      INDEX rides_auto_index_fk_city_ref_users (city ASC, rider_id ASC),
      INDEX rides_auto_index_fk_vehicle_city_ref_vehicles (vehicle_city ASC, vehicle_id ASC)
  ) PARTITION BY RANGE (start_time) (
    PARTITION p1 VALUES FROM ('2025-11-01 00:00:00+00') TO ('2025-11-02 00:00:00+00'),
    PARTITION p2 VALUES FROM ('2025-11-02 00:00:00+00') TO ('2025-11-03 00:00:00+00'),
    PARTITION p3 VALUES FROM ('2025-11-03 00:00:00+00') TO ('2025-11-04 00:00:00+00'),
    PARTITION p4 VALUES FROM ('2025-11-04 00:00:00+00') TO ('2025-11-05 00:00:00+00'),
    PARTITION p5 VALUES FROM ('2025-11-05 00:00:00+00') TO ('2025-11-06 00:00:00+00'),
    PARTITION p6 VALUES FROM ('2025-11-06 00:00:00+00') TO ('2025-11-07 00:00:00+00'),
    PARTITION p7 VALUES FROM ('2025-11-07 00:00:00+00') TO ('2025-11-08 00:00:00+00'),
    PARTITION p8 VALUES FROM ('2025-11-08 00:00:00+00') TO ('2025-11-09 00:00:00+00'),
    PARTITION p9 VALUES FROM ('2025-11-09 00:00:00+00') TO ('2025-11-10 00:00:00+00'),
    PARTITION p10 VALUES FROM ('2025-11-10 00:00:00+00') TO ('2025-11-11 00:00:00+00')
  ) WITH (
ttl_mode = 'partition',
ttl_column = 'start_time',
ttl_retention = '7d',
ttl_granularity = '1d',
ttl_lookahead = '2d'
);

IMPORT INTO rides CSV DATA ('nodelocal://1/rides_csv/*/*');
