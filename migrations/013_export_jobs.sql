CREATE TABLE export_jobs (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    bigint NOT NULL REFERENCES users ON DELETE CASCADE,
    name       text NOT NULL,   -- what was exported, for the download name
    format     text NOT NULL CHECK (format IN ('md', 'html')),
    status     text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'done', 'failed')),
    notes      int NOT NULL DEFAULT 0,
    files      int NOT NULL DEFAULT 0,
    error      text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL DEFAULT now() + interval '1 day'
);

CREATE INDEX ON export_jobs (user_id);
