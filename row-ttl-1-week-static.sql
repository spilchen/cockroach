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
    CONSTRAINT rides_pkey PRIMARY KEY (city ASC, id ASC) -- ,
      --INDEX rides_auto_index_fk_city_ref_users (city ASC, rider_id ASC),
      --INDEX rides_auto_index_fk_vehicle_city_ref_vehicles (vehicle_city ASC, vehicle_id ASC)
) WITH (
    ttl_expiration_expression = "start_time + INTERVAL '1 day'",
    ttl_job_cron = '* * * * *',
    ttl_select_batch_size = 2500000,
    ttl_delete_batch_size = 2500000,
    ttl_select_rate_limit = 0,
    ttl_delete_rate_limit = 0
);

IMPORT INTO rides CSV DATA ('nodelocal://1/rides_csv/*/*');
