-- Tags every item from a source starts with; the model's own tags are added after them.
ALTER TABLE task_sources ADD COLUMN tags text[] NOT NULL DEFAULT '{}';
