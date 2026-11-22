FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    FASTEMBED_CACHE_PATH=/var/lib/zoe/memory/models \
    MEM0_TELEMETRY=false

WORKDIR /app
COPY services/memory/pyproject.toml /app/services/memory/pyproject.toml
RUN pip install --no-cache-dir \
    fastembed==0.9.0 \
    mem0ai==2.2.1 \
    qdrant-client==1.19.1
COPY services /app/services
COPY internal/limits/schema.json /app/internal/limits/schema.json

CMD ["python", "-m", "services.memory.worker"]
