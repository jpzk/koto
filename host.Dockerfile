FROM python:3.12-alpine
RUN apk add --no-cache podman nodejs npm \
 && npm i -g @anthropic-ai/claude-code
WORKDIR /app
COPY nc.py proxy.py stream_filter.js ./
ENV BIND=0.0.0.0 NC_NETWORK=clawson-net PROXY_HOST=cs_host
ENTRYPOINT ["python3","nc.py"]
