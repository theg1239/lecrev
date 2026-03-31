alter table hosts
  add column if not exists available_full_network_slots integer not null default 0;
