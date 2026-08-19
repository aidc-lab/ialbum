CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL COLLATE NOCASE UNIQUE,
    password_hash TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash BLOB NOT NULL UNIQUE,
    csrf_hash BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value_json TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS storage_connections (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    type TEXT NOT NULL CHECK(type IN ('local','webdav','baidu')),
    config_version INTEGER NOT NULL DEFAULT 1,
    config_json TEXT NOT NULL,
    secret_ciphertext BLOB,
    secret_nonce BLOB,
    status TEXT NOT NULL DEFAULT 'unknown',
    status_message TEXT NOT NULL DEFAULT '',
    last_checked_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    removed_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_storage_connections_active ON storage_connections(removed_at, type);

CREATE TABLE IF NOT EXISTS baidu_device_flows (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    config_json TEXT NOT NULL,
    secret_ciphertext BLOB NOT NULL,
    secret_nonce BLOB NOT NULL,
    device_code_ciphertext BLOB NOT NULL,
    device_code_nonce BLOB NOT NULL,
    user_code TEXT NOT NULL,
    verification_url TEXT NOT NULL,
    qr_url TEXT NOT NULL DEFAULT '',
    poll_interval_seconds INTEGER NOT NULL,
    status TEXT NOT NULL,
    error_message TEXT NOT NULL DEFAULT '',
    expires_at INTEGER NOT NULL,
    next_poll_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS albums (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    cover_media_id TEXT,
    backup_enabled INTEGER NOT NULL DEFAULT 1,
    backup_mode TEXT NOT NULL DEFAULT 'safe' CHECK(backup_mode IN ('safe','mirror')),
    sync_on_upload INTEGER NOT NULL DEFAULT 1,
    scan_interval_seconds INTEGER,
    last_scan_at INTEGER,
    scan_status TEXT NOT NULL DEFAULT 'pending',
    scan_error TEXT NOT NULL DEFAULT '',
    generation INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    removed_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_albums_active_created ON albums(removed_at, created_at DESC);

CREATE TABLE IF NOT EXISTS album_storage_bindings (
    id TEXT PRIMARY KEY,
    album_id TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    storage_id TEXT NOT NULL REFERENCES storage_connections(id),
    role TEXT NOT NULL CHECK(role IN ('primary','backup')),
    root_path TEXT NOT NULL,
    marker_version INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    UNIQUE(album_id, role)
);
CREATE INDEX IF NOT EXISTS idx_bindings_storage ON album_storage_bindings(storage_id, root_path);

CREATE TABLE IF NOT EXISTS tags (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    normalized_name TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS album_tags (
    album_id TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    tag_id TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY(album_id, tag_id)
);

CREATE TABLE IF NOT EXISTS media_items (
    id TEXT PRIMARY KEY,
    album_id TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    relative_path TEXT NOT NULL,
    normalized_path TEXT NOT NULL,
    provider_object_id TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL CHECK(kind IN ('image','video')),
    mime_type TEXT NOT NULL,
    size INTEGER NOT NULL,
    modified_at INTEGER NOT NULL,
    etag TEXT NOT NULL DEFAULT '',
    native_checksum TEXT NOT NULL DEFAULT '',
    sha256 TEXT NOT NULL DEFAULT '',
    width INTEGER,
    height INTEGER,
    duration_ms INTEGER,
    taken_at INTEGER,
    source_version TEXT NOT NULL,
    processing_status TEXT NOT NULL DEFAULT 'pending',
    missing_scans INTEGER NOT NULL DEFAULT 0,
    first_missing_at INTEGER,
    delete_suppressed INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(album_id, normalized_path)
);
CREATE INDEX IF NOT EXISTS idx_media_album_path ON media_items(album_id, normalized_path);
CREATE INDEX IF NOT EXISTS idx_media_album_kind ON media_items(album_id, kind, taken_at DESC);

CREATE TABLE IF NOT EXISTS scan_runs (
    id TEXT PRIMARY KEY,
    album_id TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    status TEXT NOT NULL,
    discovered_count INTEGER NOT NULL DEFAULT 0,
    missing_count INTEGER NOT NULL DEFAULT 0,
    error_message TEXT NOT NULL DEFAULT '',
    started_at INTEGER NOT NULL,
    completed_at INTEGER
);

CREATE TABLE IF NOT EXISTS media_replicas (
    id TEXT PRIMARY KEY,
    media_id TEXT NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    storage_id TEXT NOT NULL REFERENCES storage_connections(id),
    relative_path TEXT NOT NULL,
    provider_object_id TEXT NOT NULL DEFAULT '',
    source_version TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    last_error TEXT NOT NULL DEFAULT '',
    verified_at INTEGER,
    delete_after INTEGER,
    delete_suppressed INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(media_id, storage_id)
);
CREATE INDEX IF NOT EXISTS idx_replicas_status ON media_replicas(status, delete_after);

CREATE TABLE IF NOT EXISTS jobs (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    dedupe_key TEXT NOT NULL UNIQUE,
    payload_json TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('pending','running','succeeded','failed','cancelled','waiting-auth')),
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 8,
    next_run_at INTEGER NOT NULL,
    lease_until INTEGER,
    heartbeat_at INTEGER,
    last_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_claim ON jobs(state, next_run_at, lease_until);

CREATE TABLE IF NOT EXISTS thumbnail_variants (
    id TEXT PRIMARY KEY,
    media_id TEXT NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    source_version TEXT NOT NULL,
    variant TEXT NOT NULL,
    cache_path TEXT NOT NULL,
    byte_size INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    error_message TEXT NOT NULL DEFAULT '',
    last_accessed_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE(media_id, source_version, variant)
);
CREATE INDEX IF NOT EXISTS idx_thumbnails_lru ON thumbnail_variants(last_accessed_at);

CREATE TABLE IF NOT EXISTS download_tickets (
    id TEXT PRIMARY KEY,
    token_hash BLOB NOT NULL UNIQUE,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    album_id TEXT REFERENCES albums(id) ON DELETE CASCADE,
    media_ids_json TEXT NOT NULL,
    filename TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    consumed_at INTEGER,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_download_tickets_expiry ON download_tickets(expires_at);

CREATE TABLE IF NOT EXISTS upload_requests (
    id TEXT PRIMARY KEY,
    album_id TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    idempotency_key TEXT NOT NULL,
    original_filename TEXT NOT NULL,
    target_path TEXT NOT NULL DEFAULT '',
    temp_path TEXT NOT NULL DEFAULT '',
    byte_size INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    media_id TEXT REFERENCES media_items(id),
    error_message TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(album_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_tags_normalized ON tags(normalized_name);
CREATE INDEX IF NOT EXISTS idx_album_tags_tag ON album_tags(tag_id, album_id);
