-- Microsoft 365 / Outlook.com mailboxes sign in with OAuth (no passwords over IMAP): for them
-- the encrypted "password" is the refresh token, exchanged for an access token on each check.
ALTER TABLE imap_accounts ADD COLUMN auth text NOT NULL DEFAULT 'password' CHECK (auth IN ('password', 'microsoft'));
