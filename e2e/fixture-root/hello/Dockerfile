FROM nginx:1.27-alpine

COPY index.html /usr/share/nginx/html/index.html

HEALTHCHECK --interval=2s --timeout=2s --retries=10 \
  CMD wget -q -O /dev/null http://127.0.0.1/ || exit 1
