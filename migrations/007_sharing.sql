CREATE TABLE folder_members (
    folder_id bigint NOT NULL REFERENCES folders ON DELETE CASCADE,
    user_id   bigint NOT NULL REFERENCES users ON DELETE CASCADE,
    role      text NOT NULL CHECK (role IN ('editor', 'viewer')),
    PRIMARY KEY (folder_id, user_id)
);

CREATE INDEX ON folder_members (user_id);

-- The user's role inside a folder, or at the workspace root when fid is NULL: the strongest
-- of their workspace role and any share on the folder or its ancestors. NULL means no access.
CREATE FUNCTION effective_role(uid bigint, ws bigint, fid bigint) RETURNS text
LANGUAGE sql STABLE AS $$
    WITH RECURSIVE ancestors AS (
        SELECT id, parent_id FROM folders WHERE id = fid
        UNION ALL
        SELECT f.id, f.parent_id FROM folders f JOIN ancestors a ON f.id = a.parent_id
    ), roles AS (
        SELECT role FROM workspace_members WHERE workspace_id = ws AND user_id = uid
        UNION ALL
        SELECT role FROM folder_members WHERE user_id = uid AND folder_id IN (SELECT id FROM ancestors)
    )
    SELECT role FROM roles ORDER BY array_position(ARRAY['owner', 'editor', 'viewer'], role) LIMIT 1
$$;
