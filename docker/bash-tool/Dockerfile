FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONUNBUFFERED=1

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        ca-certificates \
        curl \
        git \
        jq \
        procps \
        python3 \
        python3-venv \
        wget \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace
COPY server.py /opt/bash-tool/server.py
EXPOSE 8080
CMD ["python3", "/opt/bash-tool/server.py"]
