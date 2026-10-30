CREATE TABLE folders (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id bigint NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    parent_id    bigint,
    name         text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, id),
    -- the parent must belong to the same workspace
    FOREIGN KEY (workspace_id, parent_id) REFERENCES folders (workspace_id, id) ON DELETE CASCADE
);
