package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/gorilla/mux"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/google/uuid"
)

type Job struct {
	ID          string    `json:"id"`
	Filename    string    `json:"filename"`
	Status      string    `json:"status"`
	Error       *string   `json:"error"`
	SubmittedAt time.Time `json:"submitted_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

type QueueMessage struct {
	JobID       string `json:"job_id"`
	Path        string `json:"path"`
	Compression string `json:"compression"`
}

var db *sql.DB
var amqpCh *amqp.Channel
var uploadDir string

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	uploadDir = env("UPLOAD_DIR", "/data/uploads")

	// DB
	var err error
	db, err = sql.Open("pgx", env("DB_URL", "postgres://qcuser:qcpass@postgres:5432/qcdb"))
	must(err)
	must(db.Ping())
	must(initTables())

	// AMQP
	amqpURL := env("AMQP_URL", "amqp://guest:guest@rabbitmq:5672/")
	conn, err := amqp.Dial(amqpURL)
	must(err)
	defer conn.Close()
	amqpCh, err = conn.Channel()
	must(err)
	defer amqpCh.Close()

	// declare queue
	_, err = amqpCh.QueueDeclare("qc.jobs", true, false, false, false, nil)
	must(err)

	// HTTP
	r := mux.NewRouter()
	r.HandleFunc("/submit", handleSubmit).Methods("POST")
	r.Handle("/metrics", promhttp.Handler()).Methods("GET")
	addr := env("SERVICE_ADDR", ":8080")
	log.Info().Msgf("ingress-api listening on %s", addr)
	must(http.ListenAndServe(addr, r))
}

func initTables() error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS jobs (
  id UUID PRIMARY KEY,
  filename TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('queued','processing','done','error')),
  error TEXT,
  submitted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  completed_at TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS qc_results (
  job_id UUID PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
  reads BIGINT NOT NULL,
  avg_read_length DOUBLE PRECISION NOT NULL,
  gc_content DOUBLE PRECISION NOT NULL,
  n_content DOUBLE PRECISION NOT NULL,
  processing_ms INTEGER NOT NULL
);
`)
	return err
}

func handleSubmit(w http.ResponseWriter, r *http.Request) {
	err := r.ParseMultipartForm(50 << 20) // 50MB
	if err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file field is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		http.Error(w, "server storage error", http.StatusInternalServerError)
		return
	}

	filename := filepath.Base(header.Filename)
	jobID := uuid.New().String()
	dstPath := filepath.Join(uploadDir, fmt.Sprintf("%s_%s", jobID, filename))

	out, err := os.Create(dstPath)
	if err != nil {
		http.Error(w, "failed to save file", http.StatusInternalServerError)
		return
	}
	defer out.Close()
	if _, err := out.ReadFrom(file); err != nil {
		http.Error(w, "failed to write file", http.StatusInternalServerError)
		return
	}

	// record job
	_, err = db.Exec(`INSERT INTO jobs (id, filename, status) VALUES ($1,$2,'queued')`, jobID, filename)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	// publish message
	msg := QueueMessage{JobID: jobID, Path: dstPath, Compression: "none"}
	body, _ := json.Marshal(msg)
	err = amqpCh.PublishWithContext(r.Context(), "", "qc.jobs", false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        body,
		DeliveryMode: amqp.Persistent,
	})
	if err != nil {
		http.Error(w, "queue error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf(`{"job_id":"%s"}`, jobID)))
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func must(err error) {
	if err != nil {
		log.Fatal().Err(err).Msg("fatal")
	}
}
