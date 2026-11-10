-- Mailboxes a user reads tasks from (task_sources of kind 'imap'); a user may have any number.
-- Only mail that arrives after a mailbox is added is read: last_uid starts at the newest message.
CREATE TABLE imap_accounts (
    source_id    bigint PRIMARY KEY REFERENCES task_sources ON DELETE CASCADE,
    host         text NOT NULL,
    port         int NOT NULL,
    security     text NOT NULL CHECK (security IN ('tls', 'starttls')),
    username     text NOT NULL,
    password     bytea NOT NULL, -- encrypted with SECRET_KEY
    folder       text NOT NULL DEFAULT 'INBOX',
    uid_validity bigint NOT NULL DEFAULT 0,
    last_uid     bigint NOT NULL DEFAULT 0,
    checked_at   timestamptz,
    last_error   text NOT NULL DEFAULT ''
);
