CREATE TABLE principal_actions (
    principal_id TEXT NOT NULL REFERENCES principals(id),
    action TEXT NOT NULL CHECK(action IN ('a2a.invoke','tools.read','tools.execute','llm.read','llm.invoke')),
    PRIMARY KEY(principal_id,action)
);
