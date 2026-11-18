-- The model also sorts what arrives: noise (newsletters, notifications, FYIs) is filtered out of
-- the inbox, and an item that belongs to an open task or to another inbox item is attached to it.
ALTER TABLE inbox_items
    ADD UNIQUE (user_id, id),
    ADD COLUMN verdict        text NOT NULL DEFAULT 'task' CHECK (verdict IN ('task', 'noise')),
    ADD COLUMN reason         text NOT NULL DEFAULT '', -- why it was filtered out
    ADD COLUMN parent_item_id bigint,
    ADD COLUMN parent_task_id bigint,
    ADD FOREIGN KEY (user_id, parent_item_id) REFERENCES inbox_items (user_id, id) ON DELETE SET NULL (parent_item_id),
    ADD FOREIGN KEY (user_id, parent_task_id) REFERENCES tasks (user_id, id) ON DELETE SET NULL (parent_task_id);

CREATE INDEX ON inbox_items (parent_item_id) WHERE parent_item_id IS NOT NULL;

-- What the user wants the model to know when sorting (who matters, what to ignore).
ALTER TABLE llm_settings ADD COLUMN notes text NOT NULL DEFAULT '';

-- Sort the items already waiting.
UPDATE inbox_items SET parse_state = 'pending', parse_error = '' WHERE status = 'new';
