"""Three small deterministic executors served by the official A2A SDK."""
import json
import asyncio

from a2a.server.agent_execution import AgentExecutor
from a2a.server.apps import A2AStarletteApplication
from a2a.server.request_handlers import DefaultRequestHandler
from a2a.server.tasks import InMemoryTaskStore
from a2a.types import AgentCapabilities, AgentCard, AgentSkill
from a2a.utils import new_agent_text_message
from starlette.responses import JSONResponse
from starlette.routing import Route

from agents.common import call_agent, call_mcp, context_headers, record, error_detail, mcp_session, llm_request


def create_app(name):
    events = []
    captured = {}
    release = asyncio.Event()

    class Executor(AgentExecutor):
        async def execute(self, context, event_queue):
            headers = context.call_context.state['headers']
            captured.clear()
            captured.update(dict(headers))
            record(events, headers, 'a2a.message/send', name)
            try:
                command = json.loads(context.get_user_input())
                operation = command.get('operation', 'ping')
                outbound = context_headers(headers, name)
                if operation == 'ping':
                    result = {'agent': name, 'context': context_headers(headers)}
                elif operation == 'hold':
                    release.clear()
                    await release.wait()
                    result = {'agent': name}
                elif operation == 'inspect':
                    async with mcp_session(outbound) as session:
                        tools = [t.name for t in (await session.list_tools()).tools]
                    models = await llm_request('models', outbound)
                    result = {'tools': tools, 'models': models['body'], 'context': context_headers(headers)}
                elif operation == 'llm_chat':
                    result = await llm_request('chat/completions', outbound,
                                               {'model': command.get('model', 'standard'),
                                                'messages': [{'role': 'user', 'content': 'Demo'}],
                                                'stream': command.get('stream', False)})
                elif name == 'agent-a':
                    destination = 'agent-b' if operation in ('get_invoice', 'approve_invoice', 'finance_report') else 'agent-c'
                    result = await call_agent(destination, command, outbound)
                elif operation == 'finance_report' and name == 'agent-b':
                    result = await call_agent('agent-c', {'operation': 'search_docs', 'arguments': {'query': 'invoice'}}, outbound)
                else:
                    result = await call_mcp(operation, command.get('arguments', {}), outbound)
                output = {'ok': True, 'result': result}
            except Exception as exc:
                output = {'ok': False, 'error': error_detail(exc)}
            await event_queue.enqueue_event(new_agent_text_message(json.dumps(output)))

        async def cancel(self, context, event_queue):
            raise NotImplementedError('This deterministic PoC only supports synchronous messages')

    card = AgentCard(name=name, description=f'Deterministic {name}',
                     url=f'http://{name}:8000/', version='1.0.0',
                     default_input_modes=['text'], default_output_modes=['text'],
                     capabilities=AgentCapabilities(streaming=False),
                     skills=[AgentSkill(id='commands', name='Commands', description='Static lab commands', tags=['poc'])])
    app = A2AStarletteApplication(card, DefaultRequestHandler(Executor(), InMemoryTaskStore())).build()

    async def health(request):
        return JSONResponse({'status': 'ok'})

    async def audit(request):
        if request.method == 'DELETE':
            events.clear()
            captured.clear()
            release.set()
        return JSONResponse({'events': events})

    async def probe(request):
        # Trusted loopback-only diagnostics: never return the raw reference/token.
        if request.method == 'GET':
            return JSONResponse({'context': context_headers(captured)})
        body = await request.json()
        if body.get('operation') == 'release':
            release.set()
            return JSONResponse({'status': 'released'})
        outbound = context_headers(captured, name)
        for key, value in body.get('headers', {}).items():
            if key.lower() in ('x-workflow-id', 'x-origin-context', 'x-origin-service', 'x-caller'):
                if value is None:
                    outbound.pop(key.lower(), None)
                else:
                    outbound[key.lower()] = value
        try:
            if body.get('operation') == 'models':
                return JSONResponse(await llm_request('models', outbound))
            if body.get('operation') == 'tools':
                async with mcp_session(outbound) as session:
                    return JSONResponse({'status': 200, 'tools': [t.name for t in (await session.list_tools()).tools]})
            if body.get('operation') == 'tool':
                result = await call_mcp(body['tool'], body.get('arguments', {}), outbound)
                return JSONResponse({'status': 200, 'result': result})
            if body.get('operation') == 'agent':
                result = await call_agent(body['agent'], {'operation': 'ping'}, outbound)
                return JSONResponse({'status': 200, 'result': result})
            return JSONResponse(await llm_request('chat/completions', outbound,
                                                  {'model': body.get('model', 'standard'),
                                                   'messages': [{'role': 'user', 'content': 'Demo'}]}))
        except Exception as exc:
            return JSONResponse({'status': 403, 'error': error_detail(exc)})

    app.routes.extend([Route('/health', health), Route('/_lab/events', audit, methods=['GET', 'DELETE']),
                       Route('/_lab/probe', probe, methods=['GET', 'POST'])])
    return app
