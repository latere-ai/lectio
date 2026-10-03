-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

DROP FUNCTION IF EXISTS lectio_exchange(text, text, timestamptz);
DROP FUNCTION IF EXISTS lectio_claim(text, integer, boolean, settings, timestamptz);
DROP FUNCTION IF EXISTS lectio_reap(text, settings, timestamptz);
DROP FUNCTION IF EXISTS lectio_settle(text, jsonb, settings, timestamptz);
DROP FUNCTION IF EXISTS lectio_correct(tasks, integer, boolean);
DROP FUNCTION IF EXISTS lectio_limited(text, text, timestamptz, interval, settings, timestamptz);
DROP FUNCTION IF EXISTS lectio_admits(integer, timestamptz, interval, timestamptz);
DROP FUNCTION IF EXISTS lectio_ceiling(integer, timestamptz, integer, interval, timestamptz);
DROP FUNCTION IF EXISTS lectio_settled(text, text, text, jsonb, jsonb, timestamptz);
DROP FUNCTION IF EXISTS lectio_cancel(text, timestamptz);
DROP FUNCTION IF EXISTS lectio_stop(text, text, jsonb, timestamptz);
DROP FUNCTION IF EXISTS lectio_submit(text, timestamptz);
DROP FUNCTION IF EXISTS lectio_register(text, timestamptz);
DROP FUNCTION IF EXISTS lectio_enqueue(text, text, text, text, timestamptz);
DROP FUNCTION IF EXISTS lectio_count(text, text, smallint, integer, integer);
DROP FUNCTION IF EXISTS lectio_configure(text);
DROP FUNCTION IF EXISTS lectio_lock();

DROP TABLE IF EXISTS sweeps;
DROP TABLE IF EXISTS pool_scopes;
DROP TABLE IF EXISTS pools;
DROP TABLE IF EXISTS project_service;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS dispatch;
DROP TABLE IF EXISTS class_service;
DROP TABLE IF EXISTS group_service;
DROP TABLE IF EXISTS groups;
DROP TABLE IF EXISTS tasks;
DROP TABLE IF EXISTS workers;
DROP TABLE IF EXISTS parses;
DROP TABLE IF EXISTS settings;
