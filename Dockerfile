FROM node:22-alpine
RUN apk add --no-cache coreutils bash && npm i -g @anthropic-ai/claude-code
WORKDIR /workspace
COPY entrypoint.sh /e.sh
USER node
ENTRYPOINT ["/bin/sh","/e.sh"]
