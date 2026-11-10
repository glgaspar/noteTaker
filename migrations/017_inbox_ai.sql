-- A language model reads new inbox items in the background (parse_state 'pending'); until it
-- answers, or if it fails, the item keeps the title and details guessed on arrival.
ALTER TABLE inbox_items
    ADD COLUMN tags        text[] NOT NULL DEFAULT '{}',
    ADD COLUMN subtasks    text[] NOT NULL DEFAULT '{}',
    ADD COLUMN parse_state text NOT NULL DEFAULT 'done' CHECK (parse_state IN ('pending', 'done', 'failed')),
    ADD COLUMN parse_error text NOT NULL DEFAULT '';

CREATE INDEX ON inbox_items (id) WHERE parse_state = 'pending';

-- Each user's choice of model; empty fields use the server's default (LLM_* variables).
CREATE TABLE llm_settings (
    user_id bigint PRIMARY KEY REFERENCES users ON DELETE CASCADE,
    enabled boolean NOT NULL DEFAULT true,
    api_url text NOT NULL DEFAULT '',
    model   text NOT NULL DEFAULT '',
    api_key bytea -- encrypted with SECRET_KEY (see secret.go)
);
