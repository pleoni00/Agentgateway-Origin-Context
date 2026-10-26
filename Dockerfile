FROM python:3.12.12-slim
WORKDIR /app
COPY requirements.lock .
RUN pip install --no-cache-dir -r requirements.lock
COPY agents agents
COPY services services
COPY mcp_servers mcp_servers
COPY llm_server llm_server
ENV PYTHONUNBUFFERED=1
CMD ["uvicorn", "agents.agent_a:app", "--host", "0.0.0.0", "--port", "8000"]
