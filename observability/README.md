# Flowe Observability Stack

Local LGTM-style stack (Loki, Grafana, Tempo, Prometheus + OTel Collector) for the Flowe Go server.

## What runs where

| Service        | Image                                           | Host port(s) | Purpose                                   |
| -------------- | ----------------------------------------------- | ------------ | ----------------------------------------- |
| otel-collector | otel/opentelemetry-collector-contrib:0.156.0    | 4317, 4318   | OTLP intake (gRPC/HTTP), fans out to LGT+P |
| prometheus     | prom/prometheus:v3.13.1                         | 9090         | Metrics (scrapes collector on :8889)      |
| loki           | grafana/loki:3.7.3                              | 3100         | Logs (native OTLP ingest at /otlp)        |
| tempo          | grafana/tempo:2.10.7                            | 3200         | Traces (fed via OTLP gRPC from collector) |
| grafana        | grafana/grafana:12.4.5                          | 3000         | UI, anonymous Admin, pre-provisioned      |

## Start

```sh
docker compose up -d otel-collector prometheus loki tempo grafana
```

Grafana: http://localhost:3000 (no login — anonymous Admin). Datasources (Prometheus, Loki, Tempo) and the three dashboards in the **Flowe** folder are provisioned automatically:

- **Flowe · Service Overview** (`flowe-overview`) — HTTP rate/errors/latency, status codes, runtime, DB pool/query p95, auth events, logs
- **Flowe · Workflow Engine** (`flowe-workflows`) — workflow runs, node executions, LLM & integration calls, approvals, run events, SSE streams, webhooks/schedules, emails, outbound HTTP, error logs
- **Flowe · Logs & Debugging** (`flowe-debug`) — log volume/tails (access, outbound, workflow lifecycle, slow DB), status classes, 4xx/5xx by route, rate limits, unauthorized events, panics/drops/misses stats

## How the app connects

The `app` service exports OTLP over HTTP to the collector inside the compose network:

- `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318`
- `OTEL_SERVICE_NAME=flowe-server`

Signal flow:

```
app ──OTLP http──▶ otel-collector ──┬─▶ tempo:4317        (traces)
                                    ├─▶ loki:3100/otlp    (logs)
                                    └─▶ :8889 ◀─scrape─ prometheus (metrics)
```

Prometheus scrapes the collector with `honor_labels: true`, so series keep `job="flowe-server"` (derived from `service.name`). Query logs in Loki with `{service_name="flowe-server"}`.

Config files live in this directory: `otel-collector.yaml`, `prometheus.yml`, `loki.yaml`, `tempo.yaml`, and `grafana/` (provisioning + dashboard JSON).
