-- MHC (Mahachai) dataset — landing tables. Run BEFORE loading the dumps.
-- The dumps (MHC_Ai2.sql, MHC_machineIQF.sql) are bare INSERTs with no CREATE TABLE.
--
-- ponytail: tables are created under the dumps' OWN names (public.ais,
-- public.iqf_recoards) so the 150MB of dump text is loaded byte-for-byte with no
-- sed/-replace pass; scripts/mhc-registry.sql renames them to mhc_* afterwards.
-- Neither name collides with an app table, which is the only reason NJ5 needed the
-- rename-while-loading trick (its dump said public.machines).

-- Production counters. time_stamp is epoch SECONDS and is the reading time.
-- created_at_ts is the batch-insert time (344,925 distinct values across 791,049
-- rows, several machines sharing one value) so it is NOT usable as the time axis.
CREATE TABLE IF NOT EXISTS ais (
  time_stamp    bigint,
  machine_name  text,
  area          text,
  count_fg      bigint,
  count_ng      bigint,
  sku_id        text,
  created_at    bigint,
  updated_at    bigint,
  created_at_ts timestamptz
);

-- Freezer sensors. No epoch column at all: created_at_ts is the only time axis
-- (258,371 rows, zero duplicate timestamps). Column name typo is the upstream's.
CREATE TABLE IF NOT EXISTS iqf_recoards (
  machine_type  text,
  machine_name  text,
  area          text,
  room_temp     real,
  freezing_time real,
  coil_temp     real,
  speed_belt    real,
  fan01         real,
  fan02         real,
  fan03         real,
  fan04         real,
  fail_network  boolean,
  created_at_ts timestamptz,
  updated_at    timestamptz
);
