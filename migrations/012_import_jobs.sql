CREATE TABLE import_jobs (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id bigint NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    user_id      bigint NOT NULL REFERENCES users ON DELETE CASCADE,
    running      boolean NOT NULL DEFAULT true,
    stats        jsonb NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now()
);
