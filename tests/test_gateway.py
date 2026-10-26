import asyncio
from contextlib import closing
import sqlite3
from pathlib import Path

import httpx
import pytest

from agents.common import call_agent, message_request
from conftest import BASE, admin_headers, events, headers, held, probe, sql

SERVICE_TOOLS = {'service-portal': {'search_docs', 'read_doc', 'get_invoice'},
                 'service-erp': {'search_docs', 'read_doc', 'get_invoice', 'approve_invoice'}}
AGENT_TOOLS = {'agent-a': {'search_docs', 'read_doc'}, 'agent-b': SERVICE_TOOLS['service-erp'],
               'agent-c': {'search_docs', 'read_doc'}}


def args(tool):
    if tool == 'search_docs':
        return {'query': 'invoice'}
    return {'document_id': 'DOC-001'} if tool == 'read_doc' else {'invoice_id': 'INV-001'}


@pytest.mark.parametrize('origin', SERVICE_TOOLS)
@pytest.mark.parametrize('agent', AGENT_TOOLS)
async def test_tool_discovery_intersection(origin, agent):
    async with held(origin, agent):
        result = await probe(agent, 'tools')
        assert result['status'] == 200, result
        expected = SERVICE_TOOLS[origin] & AGENT_TOOLS[agent]
        assert set(result['tools']) == {('finance_' if t.endswith('invoice') else 'documents_') + t for t in expected}


@pytest.mark.parametrize('origin', SERVICE_TOOLS)
@pytest.mark.parametrize('agent', AGENT_TOOLS)
@pytest.mark.parametrize('tool', ['search_docs', 'read_doc', 'get_invoice', 'approve_invoice'])
async def test_tool_enforcement(origin, agent, tool):
    allowed = tool in SERVICE_TOOLS[origin] & AGENT_TOOLS[agent]
    async with held(origin, agent):
        result = await probe(agent, 'tool', tool=tool, arguments=args(tool))
        assert result['status'] == (200 if allowed else 403), result
    assert len(await events('finance' if tool.endswith('invoice') else 'documents')) == int(allowed)


@pytest.mark.parametrize('origin', SERVICE_TOOLS)
@pytest.mark.parametrize('agent', AGENT_TOOLS)
@pytest.mark.parametrize('model', ['standard', 'advanced'])
async def test_model_intersection_and_provider_not_called_on_denial(origin, agent, model):
    allowed = model == 'standard' or (origin == 'service-erp' and agent != 'agent-c')
    async with held(origin, agent):
        result = await probe(agent, 'chat', model=model)
        assert result['status'] == (200 if allowed else 403), result
        listed = await probe(agent, 'models')
        assert listed['status'] == 200, listed
        assert {m['id'] for m in listed['body']['data']} == ({'standard', 'advanced'} if origin == 'service-erp' and agent != 'agent-c' else {'standard'})
    actual = await events('llm')
    assert len(actual) == int(allowed)
    assert all(not e['has_origin_reference'] for e in actual)


@pytest.mark.parametrize('origin,allowed', [('service-erp', True), ('service-portal', False)])
async def test_approval_workflow(origin, allowed):
    if allowed:
        result = await call_agent('agent-a', {'operation': 'approve_invoice', 'arguments': {'invoice_id': 'INV-001'}}, headers(origin))
        assert result['status'] == 'approved'
    else:
        with pytest.raises(Exception, match='403|denied|Forbidden'):
            await call_agent('agent-a', {'operation': 'approve_invoice', 'arguments': {'invoice_id': 'INV-001'}}, headers(origin))
    assert len(await events('finance')) == int(allowed)


async def test_three_agent_workflow_and_revocation_before_response():
    h = headers()
    result = await call_agent('agent-a', {'operation': 'finance_report'}, h)
    assert result['documents'][0]['document_id'] == 'DOC-001'
    hashes = set()
    for backend, caller in [('agent-a', 'service-erp'), ('agent-b', 'agent-a'), ('agent-c', 'agent-b'), ('documents', 'agent-c')]:
        actual = await events(backend)
        assert len(actual) == 1
        assert actual[0]['x-caller'] == caller
        assert actual[0]['x-origin-service'] == 'service-erp'
        assert actual[0]['x-workflow-id'] == h['x-workflow-id']
        if backend != 'documents':
            hashes.add(actual[0]['context_hash'])
    assert len(hashes) == 1
    row = sql('SELECT revoked_at FROM delegation_contexts WHERE token_hash=?', (hashes.pop(),))[0]
    assert row['revoked_at'] is not None
    assert (await probe('agent-c', 'chat'))['status'] == 403


@pytest.mark.parametrize('changes', [{'x-workflow-id': 'another-workflow'}, {'x-origin-context': None}, {'x-origin-context': 'x' * 43}])
async def test_context_tampering(changes):
    async with held():
        result = await probe('agent-b', 'chat', headers=changes)
        assert result['status'] == 403, result
    assert await events('llm') == []


async def test_spoofed_identity_labels_do_not_change_permissions():
    async with held('service-portal'):
        result = await probe('agent-b', 'chat', model='advanced', headers={'x-origin-service': 'service-erp', 'x-caller': 'agent-a'})
        assert result['status'] == 403
    assert await events('llm') == []


async def test_revoking_origin_access_to_calling_agent_blocks_tools_and_models():
    grants = sql("SELECT group_id FROM agent_grants WHERE principal_id='service-erp'")
    async with held():
        try:
            sql("DELETE FROM agent_grants WHERE principal_id='service-erp'")
            assert (await probe('agent-b', 'chat'))['status'] == 403
            assert (await probe('agent-b', 'tools'))['status'] == 403
        finally:
            for grant in grants:
                sql('INSERT INTO agent_grants VALUES(?,?)', ('service-erp', grant['group_id']))
    assert await events('llm') == []
    assert await events('finance') == []


async def test_tool_grant_changes_apply_to_existing_context():
    groups = sql("SELECT group_id FROM tool_grants WHERE principal_id='service-erp'")
    async with held():
        try:
            assert 'finance_approve_invoice' in (await probe('agent-b', 'tools'))['tools']
            sql("DELETE FROM tool_grants WHERE principal_id='service-erp'")
            sql("INSERT INTO tool_grants VALUES('service-erp','finance-reader')")
            assert 'finance_approve_invoice' not in (await probe('agent-b', 'tools'))['tools']
            assert (await probe('agent-b', 'tool', tool='approve_invoice', arguments=args('approve_invoice')))['status'] == 403
        finally:
            sql("DELETE FROM tool_grants WHERE principal_id='service-erp'")
            for group in groups:
                sql('INSERT INTO tool_grants VALUES(?,?)', ('service-erp', group['group_id']))
    assert await events('finance') == []


async def test_llm_groups_admin_changes_are_immediate():
    async with held('service-portal'):
        async with httpx.AsyncClient(timeout=10) as client:
            assert (await client.get(BASE + '/admin/llm')).status_code == 401
            try:
                response = await client.put(BASE + '/admin/llm/groups/integration-test', headers=admin_headers(),
                                             json={'models': ['advanced'], 'principals': ['service-portal', 'agent-b']})
                assert response.status_code == 200, response.text
                assert (await probe('agent-b', 'chat', model='advanced'))['status'] == 200
            finally:
                (await client.delete(BASE + '/admin/llm/groups/integration-test', headers=admin_headers())).raise_for_status()
            assert (await probe('agent-b', 'chat', model='advanced'))['status'] == 403


@pytest.mark.parametrize('user,allowed', [('alice', True), ('observer', False), ('outsider', False)])
async def test_users_authenticate_with_keycloak_and_native_db_permissions(user, allowed):
    if allowed:
        result = await call_agent('agent-a', {'operation': 'inspect'}, headers(user))
        assert set(result['tools']) == {'documents_search_docs', 'documents_read_doc'}
        assert {m['id'] for m in result['models']['data']} == {'standard'}
    else:
        with pytest.raises(httpx.HTTPStatusError) as error:
            await call_agent('agent-a', {'operation': 'ping'}, headers(user))
        assert error.value.response.status_code == 403
        assert await events('agent-a') == []


async def test_user_origin_needs_direct_access_to_downstream_agent():
    with pytest.raises(Exception, match='403|Forbidden'):
        await call_agent('agent-a', {'operation': 'search_docs'}, headers('alice'))
    assert await events('agent-c') == []
    assert await events('documents') == []


@pytest.mark.parametrize('principal', ['service-portal', 'service-erp'])
@pytest.mark.parametrize('agent', ['agent-a', 'agent-b', 'agent-c'])
async def test_services_have_direct_permissions_on_all_three_agents(principal, agent):
    result = await call_agent(agent, {'operation': 'ping'}, headers(principal))
    assert result['agent'] == agent
    assert 'x-origin-context' not in result['context']
    assert (await probe(agent, 'chat'))['status'] == 403


async def test_agent_cannot_invoke_gateway_without_context():
    async with httpx.AsyncClient() as client:
        response = await client.post(BASE + '/llm/v1/chat/completions', headers=headers('agent-b'), json={'model': 'standard'})
    assert response.status_code == 403
    assert await events('llm') == []


async def test_service_cannot_inject_reference():
    h = headers()
    h['x-origin-context'] = 'x' * 43
    async with httpx.AsyncClient() as client:
        response = await client.post(BASE + '/a2a/agent-a/jsonrpc', headers=h, json=message_request({'operation': 'ping'}))
    assert response.status_code == 403
    assert await events('agent-a') == []


@pytest.mark.parametrize('authorization', ['', 'Bearer invalid'])
async def test_missing_or_invalid_token_is_denied(authorization):
    async with httpx.AsyncClient() as client:
        response = await client.post(BASE + '/a2a/agent-a/jsonrpc', headers={'authorization': authorization}, json=message_request({'operation': 'ping'}))
    assert response.status_code == 401
    assert await events('agent-a') == []


async def test_provider_denies_streaming_until_lifecycle_is_supported():
    result = await call_agent('agent-b', {'operation': 'llm_chat', 'stream': True}, headers())
    assert result['status'] == 403
    assert await events('llm') == []


async def test_database_unavailable_fails_closed():
    path = Path(__file__).resolve().parents[1] / 'authorization/data/permissions.db'
    with closing(sqlite3.connect(path, timeout=3)) as db:
        db.execute('BEGIN EXCLUSIVE')
        try:
            async with httpx.AsyncClient(timeout=10) as client:
                response = await client.post(BASE + '/a2a/agent-a/jsonrpc', headers=headers(), json=message_request({'operation': 'ping'}))
            assert response.status_code == 503, response.text
        finally:
            db.rollback()
    assert await events('agent-a') == []

async def test_observer_can_discover_but_cannot_execute_tools():
    from agents.common import mcp_session, error_detail
    h = headers('observer')
    with pytest.raises(Exception) as error:
        async with mcp_session(h) as session:
            names = {t.name for t in (await session.list_tools()).tools}
            assert names == {'documents_search_docs', 'documents_read_doc'}
            await session.call_tool('documents_search_docs', {'query': 'invoice'})
    assert '403' in error_detail(error.value)
    assert await events('documents') == []


async def test_agent_destination_permissions_are_checked_for_current_actor():
    async with held('service-erp', 'agent-c'):
        assert (await probe('agent-c', 'agent', agent='agent-b'))['status'] == 403
    assert await events('agent-b') == []


async def test_model_disable_is_immediate_for_existing_context():
    async with held():
        async with httpx.AsyncClient() as client:
            try:
                response = await client.patch(BASE + '/admin/llm/models/advanced', headers=admin_headers(), json={'enabled': False})
                assert response.status_code == 200
                assert (await probe('agent-b', 'chat', model='advanced'))['status'] == 403
                assert {m['id'] for m in (await probe('agent-b', 'models'))['body']['data']} == {'standard'}
            finally:
                (await client.patch(BASE + '/admin/llm/models/advanced', headers=admin_headers(), json={'enabled': True})).raise_for_status()
    assert await events('llm') == []


async def test_unknown_model_is_denied_without_provider_execution():
    async with held():
        assert (await probe('agent-b', 'chat', model='unknown'))['status'] == 403
    assert await events('llm') == []


async def test_origin_action_permission_is_checked_for_delegated_calls():
    async with held():
        try:
            sql("DELETE FROM principal_actions WHERE principal_id='service-erp' AND action='llm.invoke'")
            assert (await probe('agent-b', 'chat'))['status'] == 403
        finally:
            sql("INSERT INTO principal_actions VALUES('service-erp','llm.invoke')")
    assert await events('llm') == []


async def test_registry_configures_gateway_without_restart():
    """Discover a new upstream, grant tools, invoke through the reloaded gateway."""
    from agents.common import mcp_session

    async def tool_names():
        async with mcp_session(headers('service-erp')) as session:
            return {tool.name for tool in (await session.list_tools()).tools}

    async with httpx.AsyncClient(timeout=25) as client:
        assert (await client.get(BASE + '/admin/mcp')).status_code == 401
        endpoint = BASE + '/admin/mcp/servers/registry-test'
        group = BASE + '/admin/mcp/groups/registry-test'
        try:
            registered = await client.put(endpoint, headers=admin_headers(),
                                          json={'endpoint': 'http://mcp-documents:8000/mcp'})
            assert registered.status_code == 200, registered.text
            synced = await client.post(endpoint + '/sync', headers=admin_headers())
            assert synced.status_code == 200, synced.text
            catalog = (await client.get(BASE + '/admin/mcp', headers=admin_headers())).json()
            discovered = [t for t in catalog['tools'] if t['server_id'] == 'registry-test']
            assert {t['name'] for t in discovered} == {'search_docs', 'read_doc'}
            assert all(t['available'] == 1 and 'inputSchema' in t['definition'] for t in discovered)
            assert not any(name.startswith('registry-test_') for name in await tool_names())
            grant = await client.put(group, headers=admin_headers(),
                                     json={'tools': [t['id'] for t in discovered], 'principals': ['service-erp']})
            grant.raise_for_status()
            for _ in range(100):
                if 'registry-test_search_docs' in await tool_names():
                    break
                await asyncio.sleep(.1)
            else:
                raise AssertionError('agentgateway did not reload registry destinations')
            async with mcp_session(headers('service-erp')) as session:
                result = await session.call_tool('registry-test_search_docs', {'query': 'invoice'})
                assert not result.isError
            # The same grant is insufficient for an agent that has no membership.
            async with held('service-erp', 'agent-b'):
                result = await probe('agent-b', 'tools')
                assert not any(name.startswith('registry-test_') for name in result['tools'])
            disabled = await client.put(endpoint, headers=admin_headers(),
                                        json={'endpoint': 'http://mcp-documents:8000/mcp', 'enabled': False})
            disabled.raise_for_status()
            assert not any(name.startswith('registry-test_') for name in await tool_names())
        finally:
            (await client.delete(group, headers=admin_headers())).raise_for_status()
            (await client.delete(endpoint, headers=admin_headers())).raise_for_status()
