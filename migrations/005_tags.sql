CREATE TABLE tags (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id bigint NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    name         text NOT NULL,
    UNIQUE (workspace_id, name)
);

CREATE TABLE note_tags (
    note_id bigint NOT NULL REFERENCES notes ON DELETE CASCADE,
    tag_id  bigint NOT NULL REFERENCES tags ON DELETE CASCADE,
    PRIMARY KEY (note_id, tag_id)
);

CREATE INDEX ON note_tags (tag_id);
