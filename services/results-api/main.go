package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type Job struct {
	ID          string   `json:"id"`
	Filename    string   `json:"filename"`
	Status      string   `json:"status"`
	Error       *string  `json:"error"`
	SubmittedAt string   `json:"submitted_at"`
	CompletedAt *string  `json:"completed_at"`
}

type QC struct {
	Reads          int64   `json:"reads"`
	AvgReadLength  float64 `json:"avg_read_length"`
	GCContent      float64 `json:"gc_content"`
	NContent       float64 `json:"n_content"`
	ProcessingMS   int     `json:"processing_ms"`
}

type Resp struct {
	Job *Job `json:"job"`
	QC  *QC  `json:"qc,omitempty"`
}

var db *sql.DB

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	var err error
	db, err = sql.Open("pgx", env("DB_URL", "postgres://qcuser:qcpass@postgres:5432/qcdb"))
	must(err)
	must(db.Ping())

	r := mux.NewRouter()
	r.HandleFunc("/job/{id}", handleGetJob).Methods("GET")
	r.Handle("/metrics", promhttp.Handler()).Methods("GET")

	addr := env("SERVICE_ADDR", ":8080")
	log.Info().Msgf("results-api listening on %s", addr)
	must(http.ListenAndServe(addr, r))
}

func handleGetJob(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]

	job := &Job{}
	err := db.QueryRow(`
SELECT id, filename, status, error,
       to_char(submitted_at, 'YYYY-MM-DD\"T\"HH24:MI:SSZ'),
       CASE WHEN completed_at IS NULL THEN NULL ELSE to_char(completed_at, 'YYYY-MM-DD\"T\"HH24:MI:SSZ') END
FROM jobs WHERE id=$1`, id).Scan(&job.ID, &job.Filename, &job.Status, &job.Error, &job.SubmittedAt, &job.CompletedAt)
	if err != nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}

	var qc *QC = nil
	row := db.QueryRow(`SELECT reads, avg_read_length, gc_content, n_content, processing_ms FROM qc_results WHERE job_id=$1`, id)
	tmp := QC{}
	if err := row.Scan(&tmp.Reads, &tmp.AvgReadLength, &tmp.GCContent, &tmp.NContent, &tmp.ProcessingMS); err == nil {
		qc = &tmp
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Resp{Job: job, QC: qc})
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
