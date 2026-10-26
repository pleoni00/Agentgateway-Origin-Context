"""Local OpenAI-compatible provider fixture: deterministic output, no external spend."""
from uuid import uuid4

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

app = FastAPI()
events = []


@app.get('/health')
async def health():
    return {'status': 'ok'}


@app.api_route('/_lab/events', methods=['GET', 'DELETE'])
async def audit(request: Request):
    if request.method == 'DELETE':
        events.clear()
    return {'events': events}


@app.post('/v1/chat/completions')
async def completion(request: Request):
    if request.headers.get('authorization') != 'Bearer demo-provider-key':
        return JSONResponse({'error': 'invalid provider credential'}, status_code=401)
    body = await request.json()
    model = body.get('model')
    if model not in ('demo-standard', 'demo-advanced'):
        return JSONResponse({'error': 'unknown model'}, status_code=404)
    events.append({'model': model, 'workflow': request.headers.get('x-workflow-id'),
                   'origin': request.headers.get('x-origin-service'),
                   'caller': request.headers.get('x-caller'),
                   'has_origin_reference': bool(request.headers.get('x-origin-context'))})
    return {'id': 'chatcmpl-' + uuid4().hex, 'object': 'chat.completion', 'created': 1,
            'model': model, 'choices': [{'index': 0, 'message': {'role': 'assistant', 'content': 'Local demo response'},
                                        'finish_reason': 'stop'}],
            'usage': {'prompt_tokens': 5, 'completion_tokens': 3, 'total_tokens': 8}}
