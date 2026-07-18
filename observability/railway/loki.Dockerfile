FROM grafana/loki:3.7.3
# Local config works unchanged: Loki's default listen address is unspecified,
# which Go binds dual-stack, and the inmemory ring keeps instance_addr local.
COPY observability/loki.yaml /etc/loki/loki.yaml
# Root: Railway volume at /loki mounts root-owned; the loki user can't write it.
USER root
CMD ["-config.file=/etc/loki/loki.yaml"]
