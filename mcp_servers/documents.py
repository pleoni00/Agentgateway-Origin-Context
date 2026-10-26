from mcp.server.fastmcp import FastMCP, Context
from mcp.server.transport_security import TransportSecuritySettings
from mcp_servers.common import create_app, observe

mcp = FastMCP('documents', stateless_http=True, json_response=True,
              transport_security=TransportSecuritySettings(
                  allowed_hosts=['mcp-documents', 'mcp-documents:*', 'localhost:*', '127.0.0.1:*']))
events = []
documents = {'DOC-001': 'Invoice INV-001: amount 850.', 'DOC-002': 'Finance approval procedure.'}


@mcp.tool()
def search_docs(query: str, ctx: Context) -> dict:
    """Search static documents."""
    observe(events, ctx, 'search_docs')
    return {'documents': [{'document_id': key, 'content': value} for key, value in documents.items()
                          if query.lower() in value.lower()]}


@mcp.tool()
def read_doc(document_id: str, ctx: Context) -> dict:
    """Read a static document."""
    observe(events, ctx, 'read_doc')
    return {'document_id': document_id, 'content': documents[document_id]}


app = create_app(mcp, events)
