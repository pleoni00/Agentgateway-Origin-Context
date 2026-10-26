"""Create local control-plane secrets once, without printing or replacing them."""
import os
from pathlib import Path
import secrets

path = Path(__file__).resolve().parents[1] / '.env'
if not path.exists():
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as stream:
        for name in ('AUTHZ_ADMIN_TOKEN', 'AUTHZ_CHECK_TOKEN'):
            stream.write(f'{name}={secrets.token_urlsafe(32)}\n')
    print('Local control-plane secrets generated.')
else:
    values = dict(line.split('=', 1) for line in path.read_text().splitlines() if '=' in line and not line.startswith('#'))
    if any(not values.get(key) for key in ('AUTHZ_ADMIN_TOKEN', 'AUTHZ_CHECK_TOKEN')):
        raise SystemExit('Existing .env is incomplete: configure AUTHZ_ADMIN_TOKEN and AUTHZ_CHECK_TOKEN.')
    print('Existing control-plane secrets preserved.')
