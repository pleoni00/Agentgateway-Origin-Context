import asyncio
from contextlib import asynccontextmanager
from pathlib import Path
import sqlite3
from uuid import uuid4

import httpx
import pytest

from agents.auth import access_token, user_token
from agents.common import call_agent

BASE = 'http://localhost:8080'
BACKENDS = {'finance': 9101, 'documents': 9102, 'agent-a': 9201, 'agent-b': 9202, 'agent-c': 9203, 'llm': 9400}


def headers(principal='service-erp', workflow=None):
    token = user_token(principal) if principal in ('alice', 'observer', 'outsider') else access_token(principal)
    return {'authorization': 'Bearer ' + token, 'x-workflow-id': workflow or 'wf-' + uuid4().hex}


def sql(statement, params=()):
    path = Path(__file__).resolve().parents[1] / 'authorization/data/permissions.db'
    with sqlite3.connect(path, timeout=3) as db:
        db.row_factory = sqlite3.Row
        db.execute('PRAGMA foreign_keys=ON')
        return db.execute(statement, params).fetchall()


async def events(backend):
    async with httpx.AsyncClient(timeout=5) as client:
        response = await client.get(f'http://localhost:{BACKENDS[backend]}/_lab/events')
        response.raise_for_status()
        return response.json()['events']


@pytest.fixture(scope='session', autouse=True)
def ready_lab():
    with httpx.Client(timeout=5) as client:
        client.get(BASE + '/health').raise_for_status()
        client.get('http://localhost:8180/realms/lab/.well-known/openid-configuration').raise_for_status()


@pytest.fixture(autouse=True)
async def clean_backends():
    async with httpx.AsyncClient(timeout=5) as client:
        for port in BACKENDS.values():
            (await client.delete(f'http://localhost:{port}/_lab/events')).raise_for_status()


async def probe(caller, operation, **kwargs):
    async with httpx.AsyncClient(timeout=20) as client:
        response = await client.post(f'http://localhost:{BACKENDS[caller]}/_lab/probe',
                                     json={'operation': operation, **kwargs})
        response.raise_for_status()
        return response.json()


@asynccontextmanager
async def held(origin='service-erp', agent='agent-b'):
    h = headers(origin)
    task = asyncio.create_task(call_agent(agent, {'operation': 'hold'}, h))
    try:
        async with httpx.AsyncClient(timeout=5) as client:
            for _ in range(100):
                if task.done():
                    await task
                    raise AssertionError('Hold returned before release')
                response = await client.get(f'http://localhost:{BACKENDS[agent]}/_lab/probe')
                current = response.json().get('context', {})
                if current.get('x-workflow-id') == h['x-workflow-id']:
                    break
                await asyncio.sleep(.05)
            else:
                raise AssertionError('Agent did not receive the root request')
        yield current
    finally:
        await probe(agent, 'release')
        await asyncio.wait_for(task, 10)


def admin_headers():
    path = Path(__file__).resolve().parents[1] / '.env'
    values = dict(line.split('=', 1) for line in path.read_text().splitlines() if '=' in line and not line.startswith('#'))
    return {'authorization': 'Bearer ' + values['AUTHZ_ADMIN_TOKEN']}
