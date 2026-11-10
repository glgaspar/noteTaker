-- 'skipped': an item an API already listed when it was added, recorded only so it isn't
-- imported later.
ALTER TABLE inbox_items DROP CONSTRAINT inbox_items_status_check,
    ADD CONSTRAINT inbox_items_status_check CHECK (status IN ('new', 'accepted', 'discarded', 'skipped'));

-- JSON APIs a user polls for tasks (task_sources of kind 'api'); a user may have any number.
CREATE TABLE api_sources (
    source_id     bigint PRIMARY KEY REFERENCES task_sources ON DELETE CASCADE,
    url           text NOT NULL,
    headers       bytea, -- JSON object of request headers, encrypted with SECRET_KEY
    items_path    text NOT NULL DEFAULT '', -- where the list is, e.g. "data.items"; empty = the whole answer
    id_field      text NOT NULL DEFAULT 'id',
    every_minutes int NOT NULL DEFAULT 15 CHECK (every_minutes >= 5),
    checked_at    timestamptz,
    last_error    text NOT NULL DEFAULT ''
);
