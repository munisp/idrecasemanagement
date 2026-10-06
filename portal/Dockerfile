FROM nginx:1.27-alpine
COPY . /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
# sw.js's cache name gets a real version per build instead of a hand-
# maintained "v1" nobody remembers to bump -- that's what let browsers get
# permanently stuck serving a stale cached config.js (service workers
# ignore Cache-Control entirely; only a changed cache name purges the old
# entry). Baked in at build time since this is a static nginx image with
# no templating/entrypoint substitution step.
RUN sed -i "s/__BUILD_VERSION__/$(date -u +%Y%m%d%H%M%S)/" /usr/share/nginx/html/sw.js
EXPOSE 80
