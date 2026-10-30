CREATE TABLE notes (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id bigint NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    folder_id    bigint,
    title        text NOT NULL DEFAULT '',
    content      jsonb,                     -- TipTap document
    content_text text NOT NULL DEFAULT '',  -- plain text, for listing and search
    author_id    bigint REFERENCES users ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, folder_id) REFERENCES folders (workspace_id, id) ON DELETE CASCADE
);

CREATE INDEX ON notes (workspace_id, updated_at DESC);
CREATE INDEX ON notes (folder_id);
