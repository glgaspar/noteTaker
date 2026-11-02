CREATE TABLE attachments (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    note_id    bigint NOT NULL REFERENCES notes ON DELETE CASCADE,
    name       text NOT NULL,
    mime       text NOT NULL,
    size       bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ON attachments (note_id);

-- HTML of an imported note, used once to seed its editor document, then cleared.
ALTER TABLE notes ADD COLUMN import_html text;
