-- Each user's private to-do list. Every row carries user_id, and the composite foreign keys
-- make a subtask or tag link point only at rows of the same user.
CREATE TABLE tasks (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    bigint NOT NULL REFERENCES users ON DELETE CASCADE,
    parent_id  bigint, -- set on subtasks, which are one level deep (enforced on insert)
    title      text NOT NULL,
    details    text NOT NULL DEFAULT '',
    due_on     date,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, id),
    FOREIGN KEY (user_id, parent_id) REFERENCES tasks (user_id, id) ON DELETE CASCADE
);

CREATE INDEX ON tasks (user_id, parent_id);

CREATE TABLE task_tags (
    id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users ON DELETE CASCADE,
    name    text NOT NULL,
    UNIQUE (user_id, name),
    UNIQUE (user_id, id)
);

CREATE TABLE task_tag_links (
    user_id bigint NOT NULL,
    task_id bigint NOT NULL,
    tag_id  bigint NOT NULL,
    PRIMARY KEY (task_id, tag_id),
    FOREIGN KEY (user_id, task_id) REFERENCES tasks (user_id, id) ON DELETE CASCADE,
    FOREIGN KEY (user_id, tag_id) REFERENCES task_tags (user_id, id) ON DELETE CASCADE
);

CREATE INDEX ON task_tag_links (tag_id);
