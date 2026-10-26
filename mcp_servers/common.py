"""Only observation/reset endpoints; backend tools deliberately have no policy checks."""
from starlette.responses import JSONResponse
from starlette.routing import Route

from agents.common import record


def observe(events, ctx, tool):
    request = ctx.request_context.request
    record(events, request.headers if request else {}, 'mcp.tools/call', tool)


def create_app(mcp, events, reset=lambda: None, state=lambda: {}):
    app = mcp.streamable_http_app()

    async def health(request):
        return JSONResponse({'status': 'ok'})

    async def audit(request):
        if request.method == 'DELETE':
            events.clear()
            reset()
        return JSONResponse({'events': events, 'state': state()})

    app.routes.extend([Route('/health', health), Route('/_lab/events', audit, methods=['GET', 'DELETE'])])
    return app
