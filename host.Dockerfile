FROM python:3.12-alpine
RUN apk add --no-cache podman nodejs npm \
 && npm i -g @anthropic-ai/claude-code \
 && pip install --no-cache-dir textual
WORKDIR /app
COPY nc.py proxy.py tui.py ./
ENV BIND=0.0.0.0 NC_NETWORK=clawson-net PROXY_HOST=cs_host
ENTRYPOINT ["python3","nc.py"]
