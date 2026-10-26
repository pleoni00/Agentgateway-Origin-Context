FROM busybox:1.37.0-musl AS healthcheck
FROM ghcr.io/agentgateway/agentgateway:v1.6.0@sha256:9d3e6044ddcdc0878b1787f77bd401252b95e22684203fb5e874c4c42d2ed90c
COPY --from=healthcheck /bin/busybox /busybox
HEALTHCHECK --interval=5s --timeout=3s --retries=30 CMD ["/busybox", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/health"]
