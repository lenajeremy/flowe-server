FROM grafana/tempo:2.10.7
COPY observability/railway/tempo.yaml /etc/tempo/tempo.yaml
# Root: Railway volume at /var/tempo mounts root-owned.
USER root
CMD ["-config.file=/etc/tempo/tempo.yaml"]
