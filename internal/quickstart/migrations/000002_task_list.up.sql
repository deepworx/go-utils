-- Version 1 of the task_list Projection. Readers query the view; a later
-- version bump points it at task_list_v2.
CREATE TABLE task_list_v1 (
    tenant_id TEXT        NOT NULL,
    id        UUID        NOT NULL,
    title     TEXT        NOT NULL,
    assignee  TEXT        NOT NULL,
    done      BOOLEAN     NOT NULL DEFAULT false,
    notified  BOOLEAN     NOT NULL DEFAULT false,
    opened_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, id)
);

CREATE VIEW task_list AS
    SELECT tenant_id, id, title, assignee, done, notified, opened_at FROM task_list_v1;
