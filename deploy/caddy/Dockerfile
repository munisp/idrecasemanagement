# IDRE edge image.
#
# Default build is stock Caddy (config mounted at runtime):
#
#   docker build -t idre-edge .
#
# The open-appsec WAF variant is a separate build target — open-appsec ships
# as a native Caddy module, which is precisely why Caddy (not nginx, not
# APISIX) is the WAF attachment point for north-south traffic:
#
#   docker build --target appsec -t idre-edge:appsec .
#
# The appsec image embeds the open-appsec agent; attach it to your deployment
# at https://my.openappsec.io (SaaS management) or mount a declarative local
# policy under /etc/caddy/appsec before exposing ports 80/443.

FROM caddy:2.8-builder AS appsec-build
LABEL org.opencontainers.image.authors="Patrick Munis at newwave"
RUN xcaddy build \
	--with github.com/openappsec/attachment

FROM caddy:2.8-alpine AS appsec
COPY --from=appsec-build /usr/bin/caddy /usr/bin/caddy
# Not root (2026-11-26, Trivy DS-0002): Caddy keeps ports 80/443 through a file capability instead.
RUN apk add --no-cache libcap && setcap cap_net_bind_service=+ep /usr/bin/caddy \
	&& adduser -S -D -H -u 10001 caddy && mkdir -p /data /config && chown -R 10001 /data /config
USER 10001

# Default target: stock Caddy. Must stay the LAST stage in this file.
FROM caddy:2.8-alpine AS stock
# Not root (2026-11-26, Trivy DS-0002): Caddy keeps ports 80/443 through a file capability instead.
RUN apk add --no-cache libcap && setcap cap_net_bind_service=+ep /usr/bin/caddy \
	&& adduser -S -D -H -u 10001 caddy && mkdir -p /data /config && chown -R 10001 /data /config
USER 10001
