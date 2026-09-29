package automation

import "github.com/nexterm/nexterm/internal/store"

// Module-owned tables (SPEC §3 "Migrations"). Snippets and macros live in the core tables (SPEC §5.1); everything the
// automation module adds — scripts, triggers, schedules, run history and the trigger log — is stored here.
func init() {
	store.RegisterMigration("automation", 1, schemaV1)
	store.RegisterMigration("automation", 2, schemaV2)
}

// v2: event triggers (connect / disconnect / command finished).
const schemaV2 = `
ALTER TABLE automation_triggers ADD COLUMN event TEXT NOT NULL DEFAULT 'output';
ALTER TABLE automation_triggers ADD COLUMN event_opts TEXT NOT NULL DEFAULT '{}';
`

const schemaV1 = `
CREATE TABLE automation_scripts (
	id          TEXT PRIMARY KEY,
	owner_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	content     TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX automation_scripts_owner ON automation_scripts(owner_id);

CREATE TABLE automation_triggers (
	id             TEXT PRIMARY KEY,
	owner_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name           TEXT NOT NULL,
	enabled        INTEGER NOT NULL DEFAULT 1,
	pattern        TEXT NOT NULL,
	case_sensitive INTEGER NOT NULL DEFAULT 0,
	scope          TEXT NOT NULL DEFAULT '{}',
	actions        TEXT NOT NULL DEFAULT '[]',
	cooldown_ms    INTEGER NOT NULL DEFAULT 2000,
	once           INTEGER NOT NULL DEFAULT 0,
	sort_order     INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL
);
CREATE INDEX automation_triggers_owner ON automation_triggers(owner_id);

CREATE TABLE automation_schedules (
	id             TEXT PRIMARY KEY,
	owner_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name           TEXT NOT NULL,
	enabled        INTEGER NOT NULL DEFAULT 1,
	spec           TEXT NOT NULL,
	action         TEXT NOT NULL DEFAULT '{}',
	connection_ids TEXT NOT NULL DEFAULT '[]',
	notify         TEXT NOT NULL DEFAULT 'failure',
	last_run_at    INTEGER,
	last_status    TEXT NOT NULL DEFAULT '',
	last_run_id    TEXT NOT NULL DEFAULT '',
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL
);
CREATE INDEX automation_schedules_owner ON automation_schedules(owner_id);

CREATE TABLE automation_runs (
	id          TEXT PRIMARY KEY,
	owner_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	kind        TEXT NOT NULL,
	ref_id      TEXT NOT NULL DEFAULT '',
	name        TEXT NOT NULL DEFAULT '',
	origin      TEXT NOT NULL DEFAULT 'manual',
	target      TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL,
	job_id      TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT '',
	log         TEXT NOT NULL DEFAULT '',
	results     TEXT NOT NULL DEFAULT '[]',
	summary     TEXT NOT NULL DEFAULT '{}',
	started_at  INTEGER NOT NULL,
	finished_at INTEGER
);
CREATE INDEX automation_runs_owner ON automation_runs(owner_id, started_at);

CREATE TABLE automation_trigger_log (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	owner_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	trigger_id    TEXT NOT NULL,
	trigger_name  TEXT NOT NULL DEFAULT '',
	session_id    TEXT NOT NULL DEFAULT '',
	session_title TEXT NOT NULL DEFAULT '',
	connection_id TEXT NOT NULL DEFAULT '',
	line          TEXT NOT NULL DEFAULT '',
	ts            INTEGER NOT NULL
);
CREATE INDEX automation_trigger_log_owner ON automation_trigger_log(owner_id, id);
`
