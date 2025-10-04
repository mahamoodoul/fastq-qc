package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type QueueMessage struct {
	JobID       string `json:"job_id"`
	Path        string `json:"path"`
	Compression string `json:"compression"`
}

var (
	db     *sql.DB
	jobsProcessed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "qc_jobs_processed_total",
		Help: "Total number of processed QC jobs",
	})
	jobFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "qc_jobs_failed_total",
		Help: "Total number of failed QC jobs",
	})
	jobDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "qc_job_duration_ms",
		Help: "QC job duration in milliseconds",
		Buckets: prometheus.LinearBuckets(5, 20, 10),
	})
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	prometheus.MustRegister(jobsProcessed, jobFailures, jobDuration)

	// metrics server
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		log.Info().Msg("qc-worker metrics on :9090/metrics")
		http.ListenAndServe(":9090", nil)
	}()

	var err error
	db, err = sql.Open("pgx", env("DB_URL", "postgres://qcuser:qcpass@postgres:5432/qcdb"))
	must(err)
	must(db.Ping())
	must(initTables())

	amqpURL := env("AMQP_URL", "amqp://guest:guest@rabbitmq:5672/")
	conn, err := amqp.Dial(amqpURL)
	must(err)
	defer conn.Close()
	ch, err := conn.Channel()
	must(err)
	defer ch.Close()

	_, err = ch.QueueDeclare("qc.jobs", true, false, false, false, nil)
	must(err)

	msgs, err := ch.Consume("qc.jobs", "", false, false, false, false, nil)
	must(err)

	log.Info().Msg("qc-worker started, consuming from qc.jobs")

	for d := range msgs {
		start := time.Now()
		var msg QueueMessage
		if err := json.Unmarshal(d.Body, &msg); err != nil {
			log.Error().Err(err).Msg("bad message")
			d.Nack(false, false)
			jobFailures.Inc()
			continue
		}

		if err := setStatus(msg.JobID, "processing", nil); err != nil {
			log.Error().Err(err).Msg("db status error")
		}

		err := processFASTQ(msg.JobID, msg.Path)
		elapsed := time.Since(start)
		if err != nil {
			log.Error().Err(err).Msg("processing error")
			d.Nack(false, false) // send to DLQ if configured
			setStatus(msg.JobID, "error", &[]string{err.Error()}[0])
			jobFailures.Inc()
			continue
		}
		d.Ack(false)
		jobsProcessed.Inc()
		jobDuration.Observe(float64(elapsed.Milliseconds()))
		if err := setDone(msg.JobID); err != nil {
			log.Error().Err(err).Msg("db set done error")
		}
	}
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

func setStatus(jobID, status string, errMsg *string) error {
	_, err := db.Exec(`UPDATE jobs SET status=$2, error=$3 WHERE id=$1`, jobID, status, errMsg)
	return err
}

func setDone(jobID string) error {
	_, err := db.Exec(`UPDATE jobs SET status='done', completed_at=now() WHERE id=$1`, jobID)
	return err
}

func processFASTQ(jobID, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	start := time.Now()
	sc := bufio.NewScanner(f)
	// increase buffer for long FASTQ lines
	const maxCapacity = 1024 * 1024
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, maxCapacity)

	var totalReads int64
	var totalBases int64
	var gcCount int64
	var nCount int64

	lineIdx := 0
	for sc.Scan() {
		line := sc.Text()
		// FASTQ structure: every 4 lines = 1 read
		// 0: @header, 1: sequence, 2: +, 3: quality
		if lineIdx%4 == 1 {
			seq := strings.TrimSpace(line)
			l := int64(len(seq))
			totalReads++
			totalBases += l
			for i := 0; i < len(seq); i++ {
				switch seq[i] {
				case 'G', 'g', 'C', 'c':
					gcCount++
				case 'N', 'n':
					nCount++
				}
			}
		}
		lineIdx++
	}
	if err := sc.Err(); err != nil {
		return err
	}

	var avgLen float64
	if totalReads > 0 {
		avgLen = float64(totalBases) / float64(totalReads)
	}
	var gcFrac, nFrac float64
	if totalBases > 0 {
		gcFrac = float64(gcCount) / float64(totalBases)
		nFrac = float64(nCount) / float64(totalBases)
	}

	ms := int(time.Since(start).Milliseconds())
	_, err = db.Exec(`
INSERT INTO qc_results (job_id, reads, avg_read_length, gc_content, n_content, processing_ms)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (job_id) DO UPDATE SET
  reads=EXCLUDED.reads,
  avg_read_length=EXCLUDED.avg_read_length,
  gc_content=EXCLUDED.gc_content,
  n_content=EXCLUDED.n_content,
  processing_ms=EXCLUDED.processing_ms
`, jobID, totalReads, avgLen, gcFrac, nFrac, ms)
	return err
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
