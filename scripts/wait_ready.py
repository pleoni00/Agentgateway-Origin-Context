"""Wait for the public proxy and the demonstration identity provider."""
import time
import httpx

for _ in range(180):
    try:
        with httpx.Client(timeout=3) as client:
            client.get('http://localhost:8080/health').raise_for_status()
            client.get('http://localhost:8180/realms/lab/.well-known/openid-configuration').raise_for_status()
        print('Gateway and Keycloak are ready.')
        break
    except httpx.HTTPError:
        time.sleep(1)
else:
    raise SystemExit('Gateway or Keycloak failed to become ready.')
