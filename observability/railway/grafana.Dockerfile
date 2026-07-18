FROM grafana/grafana:12.4.5
# Railway-specific datasources (…railway.internal URLs); dashboards and the
# dashboards provider are shared with local dev.
COPY observability/railway/grafana-provisioning/datasources /etc/grafana/provisioning/datasources
COPY observability/grafana/provisioning/dashboards /etc/grafana/provisioning/dashboards
COPY observability/grafana/dashboards /var/lib/grafana/dashboards
# Root: Railway volume at /var/lib/grafana mounts root-owned; the grafana
# user (472) can't create grafana.db on it. COPYed dashboards stay readable.
USER root
