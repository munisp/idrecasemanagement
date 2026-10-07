FROM nginx:1.27-alpine
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
    sed -i "s/__BUILD_VERSION__/$v/g" /usr/share/nginx/html/app.html
EXPOSE 80
