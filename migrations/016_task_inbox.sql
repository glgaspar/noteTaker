-- Places a user's tasks come from: webhooks now, mailboxes and APIs later.
CREATE TABLE task_sources (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    bigint NOT NULL REFERENCES users ON DELETE CASCADE,
    kind       text NOT NULL CHECK (kind IN ('webhook', 'imap', 'api')),
    name       text NOT NULL,
    token_hash bytea UNIQUE, -- webhooks: sha256 of the secret in their URL
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, id)
);

-- What the sources received, waiting for the user to accept it as a task or discard it.
-- Handled items stay (without their raw text) so a resent external_id is not added again.
CREATE TABLE inbox_items (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id     bigint NOT NULL,
    source_id   bigint NOT NULL,
    external_id text,
    raw         text NOT NULL,
    title       text NOT NULL,
    details     text NOT NULL DEFAULT '',
    due_on      date,
    status      text NOT NULL DEFAULT 'new' CHECK (status IN ('new', 'accepted', 'discarded')),
    received_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (user_id, source_id) REFERENCES task_sources (user_id, id) ON DELETE CASCADE,
    UNIQUE (source_id, external_id)
);

CREATE INDEX ON inbox_items (user_id, status);
