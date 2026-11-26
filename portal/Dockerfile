# Not root (2026-11-26, Trivy DS-0002): nginx-unprivileged runs as user 101 and listens on 8080; the k8s Service
# reaches it through the named port "http", so the switch from port 80 needs no Service change.
FROM nginxinc/nginx-unprivileged:1.27-alpine
LABEL org.opencontainers.image.authors="Patrick Munis at newwave"
USER root
COPY . /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
# sw.js's cache name AND app.html's ?v= query params get the same real
# version per build instead of a hand-maintained "v10" nobody remembers to
# bump -- that's what let Cloudflare's edge cache (which caches .js/.css
# by extension regardless of origin Cache-Control) keep serving a
# pre-deploy bundle under the exact same cached URL for hours after a
# redeploy, confirmed live more than once. Baked in at build time since
# this is a static nginx image with no templating/entrypoint substitution
# step.
RUN v=$(date -u +%Y%m%d%H%M%S) && \
    sed -i "s/__BUILD_VERSION__/$v/" /usr/share/nginx/html/sw.js && \
    sed -i "s/__BUILD_VERSION__/$v/g" /usr/share/nginx/html/app.html /usr/share/nginx/html/dispute.html
USER 101
EXPOSE 8080
