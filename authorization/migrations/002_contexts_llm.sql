CREATE TABLE delegation_contexts (
    token_hash TEXT PRIMARY KEY,
    origin_id TEXT NOT NULL REFERENCES principals(id),
    workflow_id TEXT NOT NULL,
    owner TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    revoked_at INTEGER
);
CREATE INDEX delegation_workflow ON delegation_contexts(workflow_id);
CREATE TABLE llm_models (
    id TEXT PRIMARY KEY,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1))
);
CREATE TABLE llm_groups (id TEXT PRIMARY KEY);
CREATE TABLE llm_group_members (
    group_id TEXT NOT NULL REFERENCES llm_groups(id) ON DELETE CASCADE,
    model_id TEXT NOT NULL REFERENCES llm_models(id),
    PRIMARY KEY(group_id,model_id)
);
CREATE TABLE llm_grants (
    principal_id TEXT NOT NULL REFERENCES principals(id),
    group_id TEXT NOT NULL REFERENCES llm_groups(id) ON DELETE CASCADE,
    PRIMARY KEY(principal_id,group_id)
);
