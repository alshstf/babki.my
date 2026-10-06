package background

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/jobs"
)

// figureSources are the sources whose staleness leaves the figures on screen
// at old prices or rates, the ones the screen warns about (web
// components/data-sources.tsx, PRICE_SOURCES). A broker's sync is not among
// them: an account whose connection was removed stays «stale» for ever.
var figureSources = []string{
	marketdata.RefreshQuotesArgs{}.Kind(),
	marketdata.RefreshFxArgs{}.Kind(),
	tinvest.RefreshQuotesArgs{}.Kind(),
	marketdata.RefreshCryptoPricesArgs{}.Kind(),
}

// DataHealthHandler answers GET /api/healthz/data for an outside watcher
// (Uptime Kuma and the like, decision Р-22): 200 «ok», or 503 «stale» with the
// sources that are. It needs no sign-in — a watcher has none — and says
// nothing else: no amounts, no papers, no names.
type DataHealthHandler struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewDataHealthHandler(pool *pgxpool.Pool) *DataHealthHandler {
	return &DataHealthHandler{pool: pool, now: time.Now}
}

func (h *DataHealthHandler) Mount(srv *httpserver.Server) {
	srv.Mount("GET /api/healthz/data", http.HandlerFunc(h.handle))
}

func (h *DataHealthHandler) handle(w http.ResponseWriter, r *http.Request) {
	stale, err := staleFigureSources(r.Context(), h.pool, h.now())
	if err != nil {
		httpjson.Error(w, http.StatusServiceUnavailable, "the sources could not be read")
		return
	}
	if len(stale) > 0 {
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]any{"status": "stale", "stale": stale})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"status": "ok", "stale": []string{}})
}

// staleFigureSources lists the figure sources that are stale, in their order.
func staleFigureSources(ctx context.Context, pool *pgxpool.Pool, now time.Time) ([]string, error) {
	list, err := Sources(ctx, pool)
	if err != nil {
		return nil, err
	}
	stale := []string{}
	for _, s := range list {
		if slices.Contains(figureSources, s.Kind) && s.Stale(now) {
			stale = append(stale, s.Kind)
		}
	}
	return stale, nil
}

// SourcesCollector publishes each source's last success and failure and
// whether it is stale, read when Prometheus asks.
type SourcesCollector struct {
	pool                    *pgxpool.Pool
	success, failure, stale *prometheus.Desc
}

func NewSourcesCollector(pool *pgxpool.Pool) *SourcesCollector {
	return &SourcesCollector{
		pool: pool,
		success: prometheus.NewDesc("babki_source_last_success_timestamp_seconds",
			"When the source's job last succeeded, Unix seconds; absent if it never has.", []string{"kind"}, nil),
		failure: prometheus.NewDesc("babki_source_last_failure_timestamp_seconds",
			"When the source's job last failed, Unix seconds; absent if it never has.", []string{"kind"}, nil),
		stale: prometheus.NewDesc("babki_source_stale",
			"1 when the source has not succeeded for three of its intervals, or never has while it has failed.", []string{"kind"}, nil),
	}
}

func (c *SourcesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.success
	ch <- c.failure
	ch <- c.stale
}

func (c *SourcesCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	list, err := Sources(ctx, c.pool)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.stale, err)
		return
	}
	now := time.Now()
	for _, s := range list {
		collectSource(ch, c, s, now)
	}
}

func collectSource(ch chan<- prometheus.Metric, c *SourcesCollector, s jobs.Source, now time.Time) {
	if s.LastSuccessAt != nil {
		ch <- prometheus.MustNewConstMetric(c.success, prometheus.GaugeValue, float64(s.LastSuccessAt.Unix()), s.Kind)
	}
	if s.LastFailureAt != nil {
		ch <- prometheus.MustNewConstMetric(c.failure, prometheus.GaugeValue, float64(s.LastFailureAt.Unix()), s.Kind)
	}
	stale := 0.0
	if s.Stale(now) {
		stale = 1
	}
	ch <- prometheus.MustNewConstMetric(c.stale, prometheus.GaugeValue, stale, s.Kind)
}
