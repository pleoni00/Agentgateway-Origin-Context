PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS principals (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('user', 'service', 'agent')),
    role TEXT NOT NULL,
    domain TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1))
);
CREATE TABLE IF NOT EXISTS agents (
    id TEXT PRIMARY KEY REFERENCES principals(id),
    endpoint TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS mcp_servers (
    id TEXT PRIMARY KEY,
    endpoint TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tools (
    id INTEGER PRIMARY KEY,
    server_id TEXT NOT NULL REFERENCES mcp_servers(id),
    name TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    UNIQUE (server_id, name)
);
CREATE TABLE IF NOT EXISTS agent_groups (id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS tool_groups (id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS agent_group_members (
    group_id TEXT REFERENCES agent_groups(id),
    agent_id TEXT REFERENCES agents(id),
    PRIMARY KEY (group_id, agent_id)
);
CREATE TABLE IF NOT EXISTS tool_group_members (
    group_id TEXT REFERENCES tool_groups(id),
    tool_id INTEGER REFERENCES tools(id),
    PRIMARY KEY (group_id, tool_id)
);
CREATE TABLE IF NOT EXISTS agent_grants (
    principal_id TEXT REFERENCES principals(id),
    group_id TEXT REFERENCES agent_groups(id),
    PRIMARY KEY (principal_id, group_id)
);
CREATE TABLE IF NOT EXISTS tool_grants (
    principal_id TEXT REFERENCES principals(id),
    group_id TEXT REFERENCES tool_groups(id),
    PRIMARY KEY (principal_id, group_id)
);
