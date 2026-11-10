-- A task is in Today while today_on is the user's local date. Done tasks whose today_on has
-- passed are the Completed history; unfinished ones fall back to the main list.
ALTER TABLE tasks ADD COLUMN today_on date, ADD COLUMN done_at timestamptz;

CREATE INDEX ON tasks (user_id, today_on);
