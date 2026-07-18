FROM grafana/grafana:12.4.5
# Railway-specific provisioning (…railway.internal URLs; dashboards under
# /etc because the Railway volume mounts over /var/lib/grafana and would
# shadow anything baked there). Dashboard JSONs are shared with local dev.
COPY observability/railway/grafana-provisioning/datasources /etc/grafana/provisioning/datasources
COPY observability/railway/grafana-provisioning/dashboards /etc/grafana/provisioning/dashboards
COPY observability/grafana/dashboards /etc/grafana/dashboards
# Root: Railway volume at /var/lib/grafana mounts root-owned; the grafana
# user (472) can't create grafana.db on it. COPYed dashboards stay readable.
USER root
