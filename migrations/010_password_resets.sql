CREATE TABLE password_resets (
    token_hash bytea PRIMARY KEY,
    user_id    bigint NOT NULL REFERENCES users ON DELETE CASCADE,
    expires_at timestamptz NOT NULL
);
