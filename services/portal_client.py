import asyncio
import json
import sys
from uuid import uuid4
from agents.common import call_agent
from agents.auth import access_token


async def main(service='service-portal'):
    operation = sys.argv[1] if len(sys.argv) > 1 else 'get_invoice'
    arguments = json.loads(sys.argv[2]) if len(sys.argv) > 2 else {'invoice_id': 'INV-001'}
    headers = {'authorization': 'Bearer ' + access_token(service), 'x-workflow-id': 'wf-' + uuid4().hex}
    try:
        result = await call_agent('agent-a', {'operation': operation, 'arguments': arguments}, headers)
        print(json.dumps(result, indent=2))
    except Exception as exc:
        print(f'Workflow failed: {exc}', file=sys.stderr)
        raise SystemExit(1)


if __name__ == '__main__':
    asyncio.run(main())
