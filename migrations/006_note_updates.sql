-- Yjs update log per note; replayed in order to rebuild the document.
CREATE TABLE note_updates (
    seq     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    note_id bigint NOT NULL REFERENCES notes ON DELETE CASCADE,
    data    bytea NOT NULL
);

CREATE INDEX ON note_updates (note_id, seq);
