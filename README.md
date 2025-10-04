# FASTQ QC Microservice (Go + RabbitMQ + Postgres + Docker)

A tiny project to demonstrate **Go**, **Linux/containers**, **message queuing**, and a **life-science** context.

This repo provides:
- `ingress-api` — REST API to upload a FASTQ file and queue a QC job
- `qc-worker` — worker that consumes queue messages and computes FASTQ QC metrics
- `results-api` — REST API to fetch job status and QC results
- `docker-compose.yml` to run Postgres, RabbitMQ and all services locally
- A small `samples/tiny.fastq` file to demo quickly
- Prometheus `/metrics` endpoints on every service (ready to scrape)


---

## 1) Prerequisites

1. **Docker Desktop** (recommended)  
   - Install: https://www.docker.com/products/docker-desktop/
   - After install, open Docker Desktop and ensure it is running.

2. **Go (optional for local dev)**  
   - Install via Homebrew: `brew install go`
   - Version: Go 1.21+ recommended

3. **curl** and **jq** (for testing)  
   - `brew install curl jq`

> You don't need to install RabbitMQ/Postgres natively—Compose provides them.

---

## 2) Quick Start (one command)

```bash
# from the repo root
docker compose up -d --build
```

This will start:
- Postgres on `localhost:5432` (user `qcuser`, pass `qcpass`, db `qcdb`)
- RabbitMQ on `localhost:5672` (UI: http://localhost:15672  user/pass: guest/guest)
- ingress-api on `http://localhost:8080`
- results-api on `http://localhost:8081`
- qc-worker (no port; logs in `docker compose logs -f qc-worker`)

> First run takes longer while images are built/pulled.

---

## 3) Demo

### 3.1 Submit a job (upload FASTQ)
```bash
curl -F "file=@samples/tiny.fastq" http://localhost:8080/submit
# => {"job_id":"<UUID>"}
```

### 3.2 Poll for status/result
```bash
JOB_ID="<paste id here>"
curl http://localhost:8081/job/$JOB_ID | jq
```

**Example successful output:**
```json
{
  "job": {
    "id": "12f3b1d0-5a1f-4b1f-9a68-123456789abc",
    "filename": "tiny.fastq",
    "status": "done",
    "submitted_at": "2025-10-04T19:00:00Z",
    "completed_at": "2025-10-04T19:00:01Z",
    "error": null
  },
  "qc": {
    "reads": 2,
    "avg_read_length": 20,
    "gc_content": 0.45,
    "n_content": 0.00,
    "processing_ms": 22
  }
}
```

### 3.3 Metrics (Prometheus format)
```bash
# ingress-api
curl http://localhost:8080/metrics | head

# results-api
curl http://localhost:8081/metrics | head

# (worker has a /metrics endpoint too, but it's not exposed via ports in compose;
# you can 'docker exec' into the container or expose a port in docker-compose.yml)
```

---

## 4) Project Structure

```
fastq-qc/
  docker-compose.yml
  .env.example
  README.md
  samples/
    tiny.fastq
  data/uploads/            # bind-mounted for file storage
  db/
    migrations/            # (not used by default; tables auto-create on startup)
  services/
    ingress-api/
      Dockerfile
      go.mod
      main.go
    qc-worker/
      Dockerfile
      go.mod
      main.go
      fastq.go
    results-api/
      Dockerfile
      go.mod
      main.go
```

---

## 5) How it Works

1. **Upload**: `POST /submit` (ingress-api) saves the file under `data/uploads/` and records a `job` row in Postgres with status `queued`. It publishes a message to RabbitMQ (`qc.jobs` queue) containing the file path and job ID.

2. **Process**: `qc-worker` consumes messages, parses the FASTQ stream in Go, computes QC metrics (reads, average read length, GC%, N%) and writes a `qc_results` row; the job is marked `done` or `error`.

3. **Query**: `GET /job/{id}` (results-api) reads the DB and returns job status and QC result (if available).

4. **Metrics**: Each service exposes `/metrics` (Prometheus).

---

## 6) Environment Variables

See **.env.example**:
```
DB_URL=postgres://qcuser:qcpass@postgres:5432/qcdb
AMQP_URL=amqp://guest:guest@rabbitmq:5672/
UPLOAD_DIR=/data/uploads
SERVICE_ADDR=:8080
```

Create your own `.env` or pass variables via Compose.

---

## 7) Common Commands

```bash
# Start/stop
docker compose up -d --build
docker compose logs -f
docker compose down -v

# Tail a specific service
docker compose logs -f qc-worker

# Rebuild one service
docker compose build ingress-api && docker compose up -d ingress-api
```

---

## 8) Notes & Next Steps

- **Gzip**: You can extend the worker to detect `.gz` and stream-decompress.
- **DLQ/Retry**: RabbitMQ policy can route failed messages to a DLQ; add retry logic in the worker.
- **MinIO**: Replace local uploads with signed URLs and bucket notifications.
- **Auth**: Add a simple bearer token for `/submit` if you want access control.
- **Grafana**: Import a dashboard and scrape with Prometheus for pretty charts.

Happy hacking! 🚀
