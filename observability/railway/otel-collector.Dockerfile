# Built from the repo root (RAILWAY_DOCKERFILE_PATH selects this file).
FROM otel/opentelemetry-collector-contrib:0.156.0
COPY observability/railway/otel-collector.yaml /etc/otelcol-contrib/config.yaml
