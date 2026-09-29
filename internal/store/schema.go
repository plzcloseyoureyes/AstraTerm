package store

// coreSchemaV1 creates every core table of SPEC §5.1. Times are INTEGER unix milliseconds (UTC); booleans are 0/1;
// JSON columns are TEXT; *_enc columns hold vault ciphertext.
const coreSchemaV1 = `
CREATE TABLE users (
	id              TEXT PRIMARY KEY,
	username        TEXT NOT NULL UNIQUE COLLATE NOCASE,
	display_name    TEXT NOT NULL DEFAULT '',
	password_hash   TEXT NOT NULL,
	role            TEXT NOT NULL CHECK (role IN ('admin', 'user')),
	totp_secret_enc BLOB,
	totp_enabled    INTEGER NOT NULL DEFAULT 0,
	totp_recovery   TEXT NOT NULL DEFAULT '[]',
	totp_last_step  INTEGER NOT NULL DEFAULT 0,
	disabled        INTEGER NOT NULL DEFAULT 0,
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL,
	last_login_at   INTEGER
);

CREATE TABLE auth_sessions (
	id           TEXT PRIMARY KEY,
	user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at   INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL,
	ip           TEXT NOT NULL DEFAULT '',
	user_agent   TEXT NOT NULL DEFAULT '',
	remember     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX auth_sessions_user ON auth_sessions(user_id);
CREATE INDEX auth_sessions_expires ON auth_sessions(expires_at);

CREATE TABLE api_tokens (
	id           TEXT PRIMARY KEY,
	user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name         TEXT NOT NULL,
	token_hash   TEXT NOT NULL UNIQUE,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER,
	expires_at   INTEGER
);
CREATE INDEX api_tokens_user ON api_tokens(user_id);

CREATE TABLE folders (
	id         TEXT PRIMARY KEY,
	owner_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	parent_id  TEXT REFERENCES folders(id) ON DELETE SET NULL,
	name       TEXT NOT NULL,
	color      TEXT NOT NULL DEFAULT '',
	icon       TEXT NOT NULL DEFAULT '',
	sort_order INTEGER NOT NULL DEFAULT 0,
	shared     INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX folders_owner ON folders(owner_id);
CREATE INDEX folders_parent ON folders(parent_id);

CREATE TABLE ssh_keys (
	id              TEXT PRIMARY KEY,
	owner_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name            TEXT NOT NULL,
	type            TEXT NOT NULL DEFAULT '',
	bits            INTEGER NOT NULL DEFAULT 0,
	public_key      TEXT NOT NULL DEFAULT '',
	private_key_enc BLOB,
	passphrase_enc  BLOB,
	has_passphrase  INTEGER NOT NULL DEFAULT 0,
	fingerprint     TEXT NOT NULL DEFAULT '',
	comment         TEXT NOT NULL DEFAULT '',
	certificate     TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL
);
CREATE INDEX ssh_keys_owner ON ssh_keys(owner_id);

CREATE TABLE identities (
	id          TEXT PRIMARY KEY,
	owner_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	username    TEXT NOT NULL DEFAULT '',
	key_id      TEXT REFERENCES ssh_keys(id) ON DELETE SET NULL,
	secrets_enc BLOB,
	secret_keys TEXT NOT NULL DEFAULT '[]',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX identities_owner ON identities(owner_id);

CREATE TABLE connections (
	id           TEXT PRIMARY KEY,
	owner_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	folder_id    TEXT REFERENCES folders(id) ON DELETE SET NULL,
	name         TEXT NOT NULL,
	protocol     TEXT NOT NULL,
	host         TEXT NOT NULL DEFAULT '',
	port         INTEGER NOT NULL DEFAULT 0,
	username     TEXT NOT NULL DEFAULT '',
	identity_id  TEXT REFERENCES identities(id) ON DELETE SET NULL,
	key_id       TEXT REFERENCES ssh_keys(id) ON DELETE SET NULL,
	auth_method  TEXT NOT NULL DEFAULT 'auto',
	color        TEXT NOT NULL DEFAULT '',
	icon         TEXT NOT NULL DEFAULT '',
	tags         TEXT NOT NULL DEFAULT '[]',
	notes        TEXT NOT NULL DEFAULT '',
	favorite     INTEGER NOT NULL DEFAULT 0,
	sort_order   INTEGER NOT NULL DEFAULT 0,
	options      TEXT NOT NULL DEFAULT '{}',
	secrets_enc  BLOB,
	secret_keys  TEXT NOT NULL DEFAULT '[]',
	shared       INTEGER NOT NULL DEFAULT 0,
	last_used_at INTEGER,
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL
);
CREATE INDEX connections_owner ON connections(owner_id);
CREATE INDEX connections_folder ON connections(folder_id);
CREATE INDEX connections_shared ON connections(shared) WHERE shared = 1;
CREATE INDEX connections_identity ON connections(identity_id);
CREATE INDEX connections_key ON connections(key_id);

CREATE TABLE known_hosts (
	id          TEXT PRIMARY KEY,
	host        TEXT NOT NULL,
	port        INTEGER NOT NULL DEFAULT 22,
	key_type    TEXT NOT NULL,
	public_key  TEXT NOT NULL,
	fingerprint TEXT NOT NULL DEFAULT '',
	comment     TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL
);
CREATE INDEX known_hosts_host ON known_hosts(host, port);

CREATE TABLE snippets (
	id          TEXT PRIMARY KEY,
	owner_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	folder      TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	content     TEXT NOT NULL DEFAULT '',
	tags        TEXT NOT NULL DEFAULT '[]',
	send_mode   TEXT NOT NULL DEFAULT 'paste' CHECK (send_mode IN ('paste', 'execute')),
	shortcut    TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX snippets_owner ON snippets(owner_id);

CREATE TABLE macros (
	id         TEXT PRIMARY KEY,
	owner_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name       TEXT NOT NULL,
	steps      TEXT NOT NULL DEFAULT '[]',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX macros_owner ON macros(owner_id);

CREATE TABLE tunnels (
	id            TEXT PRIMARY KEY,
	owner_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name          TEXT NOT NULL,
	type          TEXT NOT NULL CHECK (type IN ('local', 'remote', 'dynamic')),
	connection_id TEXT NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
	bind_host     TEXT NOT NULL DEFAULT '127.0.0.1',
	bind_port     INTEGER NOT NULL DEFAULT 0,
	dest_host     TEXT NOT NULL DEFAULT '',
	dest_port     INTEGER NOT NULL DEFAULT 0,
	auto_start    INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL
);
CREATE INDEX tunnels_owner ON tunnels(owner_id);
CREATE INDEX tunnels_connection ON tunnels(connection_id);

CREATE TABLE settings (
	scope      TEXT NOT NULL,
	key        TEXT NOT NULL,
	value      TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (scope, key)
);

CREATE TABLE audit_log (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	ts       INTEGER NOT NULL,
	user_id  TEXT NOT NULL DEFAULT '',
	username TEXT NOT NULL DEFAULT '',
	action   TEXT NOT NULL,
	target   TEXT NOT NULL DEFAULT '',
	details  TEXT,
	ip       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_ts ON audit_log(ts);
CREATE INDEX audit_log_user ON audit_log(user_id, id);
CREATE INDEX audit_log_action ON audit_log(action, id);

CREATE TABLE recordings (
	id            TEXT PRIMARY KEY,
	owner_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	session_id    TEXT NOT NULL DEFAULT '',
	connection_id TEXT NOT NULL DEFAULT '',
	title         TEXT NOT NULL DEFAULT '',
	kind          TEXT NOT NULL CHECK (kind IN ('asciicast', 'log')),
	path          TEXT NOT NULL,
	size          INTEGER NOT NULL DEFAULT 0,
	cols          INTEGER NOT NULL DEFAULT 0,
	rows          INTEGER NOT NULL DEFAULT 0,
	started_at    INTEGER NOT NULL,
	ended_at      INTEGER
);
CREATE INDEX recordings_owner ON recordings(owner_id, started_at);
CREATE INDEX recordings_session ON recordings(session_id);

CREATE TABLE share_links (
	id         TEXT PRIMARY KEY,
	token_hash TEXT NOT NULL UNIQUE,
	session_id TEXT NOT NULL,
	owner_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	mode       TEXT NOT NULL CHECK (mode IN ('read', 'write')),
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX share_links_session ON share_links(session_id);

CREATE TABLE vault_meta (
	key   TEXT PRIMARY KEY,
	value BLOB NOT NULL
);
`

func init() {
	RegisterMigration("core", 1, coreSchemaV1)
}
