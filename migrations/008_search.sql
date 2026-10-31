-- Language-neutral, accent-insensitive full-text search ("acao" matches "ação").
CREATE EXTENSION IF NOT EXISTS unaccent;
CREATE TEXT SEARCH CONFIGURATION notes (COPY = simple);
ALTER TEXT SEARCH CONFIGURATION notes ALTER MAPPING FOR hword, hword_part, word WITH unaccent, simple;

-- Punctuation becomes spaces so code like fmt.Println is indexed as separate words, matching
-- how the app splits search input.
ALTER TABLE notes ADD COLUMN search tsvector GENERATED ALWAYS AS (
    setweight(to_tsvector('notes', regexp_replace(title, '[[:punct:]]+', ' ', 'g')), 'A') ||
    setweight(to_tsvector('notes', regexp_replace(content_text, '[[:punct:]]+', ' ', 'g')), 'B')
) STORED;

CREATE INDEX ON notes USING gin (search);
