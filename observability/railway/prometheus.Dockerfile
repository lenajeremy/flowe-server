FROM prom/prometheus:v3.13.1
COPY observability/railway/prometheus.yml /etc/prometheus/prometheus.yml
# Root: Railway volumes mount root-owned; the stock nobody user can't write
# /prometheus. Single-tenant internal service, acceptable trade.
USER root
# Overriding CMD drops the image defaults, so restate config/storage paths.
# [::] listen: reachable over Railway's IPv6 private network (Grafana).
CMD ["--config.file=/etc/prometheus/prometheus.yml", \
     "--storage.tsdb.path=/prometheus", \
     "--storage.tsdb.retention.time=15d", \
     "--web.listen-address=[::]:9090"]
