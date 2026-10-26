"""Generate the standalone demonstration realm; never synchronize a live realm."""
import json
from pathlib import Path


def mapper(name, value=None, attribute=None):
    config = {'claim.name': name, 'jsonType.label': 'String',
              'access.token.claim': 'true', 'id.token.claim': 'false'}
    if attribute:
        config['user.attribute'] = attribute
    else:
        config['claim.value'] = value
    return {'name': name, 'protocol': 'openid-connect',
            'protocolMapper': 'oidc-usermodel-property-mapper' if attribute else 'oidc-hardcoded-claim-mapper',
            'config': config}


clients = []
for principal in ('service-portal', 'service-erp', 'agent-a', 'agent-b', 'agent-c'):
    clients.append({'clientId': principal, 'enabled': True, 'protocol': 'openid-connect',
                    'publicClient': False, 'secret': f'agentgateway-lab-{principal}-secret',
                    'serviceAccountsEnabled': True, 'standardFlowEnabled': False,
                    'directAccessGrantsEnabled': False,
                    'protocolMappers': [mapper('aud', 'agentgateway-lab'),
                                        mapper('principal_name', principal),
                                        mapper('principal_kind', 'agent' if principal.startswith('agent-') else 'service')]})
# Password grant is confined to an explicitly named local test client.
for client_id, password_grant in [('lab-browser', False), ('lab-test-password', True)]:
    clients.append({'clientId': client_id, 'enabled': True, 'protocol': 'openid-connect',
                    'publicClient': True, 'standardFlowEnabled': not password_grant,
                    'directAccessGrantsEnabled': password_grant,
                    'redirectUris': ['http://localhost:18080/callback'],
                    'attributes': {'pkce.code.challenge.method': 'S256'},
                    'protocolMappers': [mapper('aud', 'agentgateway-lab'),
                                        mapper('principal_name', attribute='username'),
                                        mapper('principal_kind', 'user')]})
realm = {'realm': 'lab', 'enabled': True, 'sslRequired': 'none',
         'registrationAllowed': False, 'accessTokenLifespan': 300, 'clients': clients,
         'users': [{'username': user, 'enabled': True, 'emailVerified': True,
                    'firstName': user.title(), 'lastName': 'Example',
                    'email': f'{user}@lab.example',
                    'credentials': [{'type': 'password', 'value': 'Agentgateway-Lab-User-2026!', 'temporary': False}]}
                   for user in ('alice', 'observer', 'outsider')]}
Path(__file__).resolve().parents[1].joinpath('config/realm.json').write_text(json.dumps(realm, indent=2) + '\n')
