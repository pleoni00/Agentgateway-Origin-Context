from mcp.server.fastmcp import FastMCP, Context
from mcp.server.transport_security import TransportSecuritySettings
from mcp_servers.common import create_app, observe

mcp = FastMCP('finance', stateless_http=True, json_response=True,
              transport_security=TransportSecuritySettings(
                  allowed_hosts=['mcp-finance', 'mcp-finance:*', 'localhost:*', '127.0.0.1:*']))
events = []
invoices = {}


def reset():
    invoices.clear()
    invoices.update({f'INV-{n:03}': {'invoice_id': f'INV-{n:03}', 'amount': amount, 'status': 'pending'}
                     for n, amount in enumerate((850, 120, 2400), 1)})


@mcp.tool()
def get_invoice(invoice_id: str, ctx: Context) -> dict:
    """Read an invoice."""
    observe(events, ctx, 'get_invoice')
    return dict(invoices[invoice_id])


@mcp.tool()
def approve_invoice(invoice_id: str, ctx: Context) -> dict:
    """Approve an invoice in memory."""
    observe(events, ctx, 'approve_invoice')
    invoices[invoice_id]['status'] = 'approved'
    return dict(invoices[invoice_id])


reset()
app = create_app(mcp, events, reset, lambda: invoices)
