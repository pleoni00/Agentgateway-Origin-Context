# Future work

## User/tool-level policies

Allow specific authorization rules to be associated with a user and a target tool, alongside default policies.

This capability is planned and is not implemented in the current version.

## Workflow call tracing

Track individual calls between services, agents, MCP tools and LLM providers, correlating them through `workflow_id`. Record caller, destination, operation, timestamps, outcome and parent call so the execution chain can be reconstructed.

This capability is planned and is not implemented in the current version. The existing workflow ID supports correlation but does not provide a complete call history.
