"""Own-identity OAuth clients; origin references are propagated separately."""
import os
import threading
import time

import httpx

_lock = threading.Lock()
_tokens = {}


def access_token(principal=None):
    principal = principal or os.environ['OAUTH_CLIENT_ID']
    with _lock:
        cached = _tokens.get(principal)
        if cached and cached[1] > time.time() + 10:
            return cached[0]
        issuer = os.getenv('KEYCLOAK_URL', 'http://localhost:8180/realms/lab')
        secret = (os.environ['OAUTH_CLIENT_SECRET'] if principal == os.getenv('OAUTH_CLIENT_ID')
                  else f'agentgateway-lab-{principal}-secret')
        response = httpx.post(issuer + '/protocol/openid-connect/token',
                              data={'grant_type': 'client_credentials', 'client_id': principal,
                                    'client_secret': secret}, timeout=10)
        response.raise_for_status()
        body = response.json()
        _tokens[principal] = (body['access_token'], time.time() + body['expires_in'])
        return body['access_token']


def user_token(user):
    """Local integration fixture; browser clients should use Authorization Code + PKCE."""
    issuer = os.getenv('KEYCLOAK_URL', 'http://localhost:8180/realms/lab')
    response = httpx.post(issuer + '/protocol/openid-connect/token',
                          data={'grant_type': 'password', 'client_id': 'lab-test-password',
                                'username': user, 'password': 'Agentgateway-Lab-User-2026!'}, timeout=10)
    response.raise_for_status()
    return response.json()['access_token']
