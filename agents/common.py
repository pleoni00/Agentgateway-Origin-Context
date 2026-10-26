"""Wire clients and explicit context propagation; no authorization here."""
import json
import hashlib

from agents.auth import access_token
import logging
import os
from contextlib import asynccontextmanager
from uuid import uuid4

import httpx
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client

HEADERS = ('x-origin-service', 'x-caller', 'x-workflow-id')
PROPAGATED = ('x-origin-context', 'x-workflow-id')
logging.basicConfig(level=logging.INFO, format='%(message)s')
log = logging.getLogger('lab')


def context_headers(headers, caller=None):
    if caller:
        result = {k: headers[k] for k in PROPAGATED if k in headers}
        result['authorization'] = 'Bearer ' + access_token()
        return result
    # Audit and responses never expose the bearer or the opaque reference.
    result = {k: headers[k] for k in HEADERS if k in headers}
    if headers.get('x-origin-context'):
        result['context_hash'] = hashlib.sha256(headers['x-origin-context'].encode()).hexdigest()
    return result


def record(events, headers, operation, target):
    event = dict(context_headers(headers), operation=operation, target=target)
    events.append(event)
    log.info('workflow=%s origin=%s caller=%s operation=%s target=%s decision=EXECUTED',
             headers.get(HEADERS[2]), headers.get(HEADERS[0]),
             headers.get(HEADERS[1]), operation, target)


def gateway_url():
    return os.getenv('GATEWAY_URL', 'http://localhost:8080').rstrip('/')


def agent_url(agent):
    path = f'/a2a/{agent}/jsonrpc'
    return gateway_url() + path


def message_request(command):
    return {'jsonrpc': '2.0', 'id': uuid4().hex, 'method': 'message/send',
            'params': {'message': {'kind': 'message', 'messageId': uuid4().hex,
                                  'role': 'user', 'parts': [
                                      {'kind': 'text', 'text': json.dumps(command)}]}}}


async def call_agent(agent, command, headers):
    async with httpx.AsyncClient(timeout=40) as client:
        response = await client.post(agent_url(agent), json=message_request(command), headers=headers)
    response.raise_for_status()
    body = response.json()
    if 'error' in body:
        raise RuntimeError(json.dumps(body['error']))
    result = body['result']
    for part in result.get('parts', []):
        if part.get('kind') == 'text':
            output = json.loads(part['text'])
            if not output.get('ok'):
                raise RuntimeError(output.get('error', 'downstream failure'))
            return output['result']
    raise RuntimeError(f'Unexpected A2A result: {result}')


async def read_error_response(response):
    if response.is_error:
        await response.aread()


@asynccontextmanager
async def mcp_session(headers):
    async with httpx.AsyncClient(headers=headers, timeout=40, follow_redirects=True,
                                 event_hooks={'response': [read_error_response]}) as client:
        async with streamable_http_client(gateway_url() + '/mcp/', http_client=client) as (read, write, _):
            async with ClientSession(read, write) as session:
                await session.initialize()
                yield session


def error_detail(exc):
    if isinstance(exc, BaseExceptionGroup):
        return '; '.join(error_detail(e) for e in exc.exceptions)
    if isinstance(exc, httpx.HTTPStatusError):
        return f'{exc}: {exc.response.text}'
    return str(exc)


def matches_tool(name, tool):
    normalized = name.replace('-', '_')
    return normalized == tool or normalized.endswith('_' + tool)


async def call_mcp(tool, arguments, headers):
    async with mcp_session(headers) as session:
        tools = await session.list_tools()
        # Resolve the federated name from discovery.
        names = [t.name for t in tools.tools if matches_tool(t.name, tool)]
        name = names[0] if names else tool
        result = await session.call_tool(name, arguments)
        if result.isError:
            raise RuntimeError(str(result.content))
        if result.structuredContent:
            return result.structuredContent
        return json.loads(next(c.text for c in result.content if c.type == 'text'))


async def llm_request(path, headers, body=None):
    async with httpx.AsyncClient(timeout=40) as client:
        response = await client.request('POST' if body is not None else 'GET',
                                         gateway_url() + '/llm/v1/' + path,
                                         headers=headers, json=body)
    return {'status': response.status_code, 'body': response.json()}
