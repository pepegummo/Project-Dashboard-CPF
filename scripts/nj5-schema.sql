-- NJ5 dataset — table definitions (run BEFORE loading the dumps).
-- The dumps (NJ5_ai.sql, NJ5_machineIQF2/3.sql) are bare INSERT statements with no
-- CREATE TABLE, so we create the tables here. Renamed nj5_* to avoid colliding with
-- the app's own `machines` table. See scripts/nj5-views.sql for indexes/views (run AFTER load).

CREATE TABLE IF NOT EXISTS nj5_machines (
  time_stamp    bigint,
  country       text,
  plant         text,
  plant_id      text,
  machine_type  text,
  machine_name  text,
  area          text,
  count_fg      bigint,
  count_ng      bigint,
  sku_id        text,
  created_at    bigint,
  updated_at    bigint,
  created_at_ts timestamptz
);

CREATE TABLE IF NOT EXISTS nj5_iqf2 (
  time_stamp    bigint,
  country       text,
  plant         text,
  plant_id      integer,
  machine_type  text,
  machine_name  text,
  area          text,
  air_b_fan1a   real,
  air_b_fan2a   real,
  evap_temp     real,
  rail_temp     real,
  room_temp     real,
  freezing_time real,
  speed_belts   real,
  fail_network  boolean,
  created_at_ts timestamptz,
  created_at    bigint
);

CREATE TABLE IF NOT EXISTS nj5_iqf3 (
  time_stamp    bigint,
  country       text,
  plant         text,
  plant_id      integer,
  machine_type  text,
  machine_name  text,
  area          text,
  air_b_fan1a   real,
  air_b_fan2a   real,
  evap_temp     real,
  rail_temp     real,
  room_temp     real,
  freezing_time real,
  speed_belts   real,
  fail_network  boolean,
  created_at_ts timestamptz,
  created_at    bigint
);
