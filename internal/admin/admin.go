package admin

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"

	"ratelimiter/internal/config"
	"ratelimiter/internal/counter"
	"ratelimiter/internal/metrics"
	"ratelimiter/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

const (
	pageSize = 25
	topN     = 25
)

type Server struct {
	cfg       *config.Config
	known     *counter.KnownMap
	unknown   *counter.UnknownMap
	store     *store.Store
	metrics   *metrics.Metrics
	logger    *slog.Logger
	startedAt time.Time
	version   string
	tpl       *template.Template
}

func New(
	cfg *config.Config,
	known *counter.KnownMap,
	unknown *counter.UnknownMap,
	s *store.Store,
	m *metrics.Metrics,
	logger *slog.Logger,
	startedAt time.Time,
	version string,
) (*Server, error) {
	tpl, err := template.New("").Funcs(template.FuncMap{
		"fmtTime": func(t time.Time) string {
			if t.IsZero() || t.Unix() == 0 {
				return "—"
			}
			return t.UTC().Format("2006-01-02 15:04:05")
		},
		"fmtDuration": func(d time.Duration) string {
			if d <= 0 {
				return "—"
			}
			return d.Truncate(time.Second).String()
		},
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		// topsortURL builds a query string that flips the top-25 sort
		// while preserving q and page so the bottom Redis-backed table
		// stays where the operator left it.
		"topsortURL": func(path, sortBy, q string, page int) string {
			v := url.Values{}
			v.Set("topsort", sortBy)
			if q != "" {
				v.Set("q", q)
			}
			if page > 1 {
				v.Set("page", strconv.Itoa(page))
			}
			return path + "?" + v.Encode()
		},
	}).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:       cfg,
		known:     known,
		unknown:   unknown,
		store:     s,
		metrics:   m,
		logger:    logger,
		startedAt: startedAt,
		version:   version,
		tpl:       tpl,
	}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/limits", s.handleLimits)
	mux.HandleFunc("/limits/delete", s.handleLimitsDelete)
	mux.HandleFunc("/limits/purge", s.handleLimitsPurge)
	mux.HandleFunc("/abuse/keys", s.handleAbuseKeys)
	mux.HandleFunc("/abuse/keys/delete", s.handleAbuseKeysDelete)
	mux.HandleFunc("/abuse/keys/purge", s.handleAbuseKeysPurge)
	mux.HandleFunc("/abuse/ips", s.handleAbuseIPs)
	mux.HandleFunc("/abuse/ips/delete", s.handleAbuseIPsDelete)
	mux.HandleFunc("/abuse/ips/purge", s.handleAbuseIPsPurge)
	return mux
}

// ----- index -----

type indexData struct {
	Title      string
	Version    string
	Uptime     string
	Now        string
	RedisOK    bool
	FlagRows   []flagRow
	MetricRows []metricRow
}

type flagRow struct {
	Name, Value string
}

type metricRow struct {
	Name, Type, Value, Help string
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	redisErr := s.store.Ping(ctx)

	data := indexData{
		Title:      "status",
		Version:    s.version,
		Uptime:     time.Since(s.startedAt).Truncate(time.Second).String(),
		Now:        time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		RedisOK:    redisErr == nil,
		FlagRows:   s.flagRows(),
		MetricRows: s.metricRows(),
	}
	s.render(w, "index.html", data)
}

func (s *Server) flagRows() []flagRow {
	c := s.cfg
	password := "***"
	if c.RedisPassword == "" {
		password = "(empty)"
	}
	return []flagRow{
		{"--listen", c.Listen},
		{"--socket-mode", c.SocketMode},
		{"--admin-listen", c.AdminListen},
		{"--metrics-listen", c.MetricsListen},
		{"--redis-addr", c.RedisAddr},
		{"--redis-password", password},
		{"--log-level", c.LogLevel},
		{"--log-format", c.LogFormat},
		{"--global-limit", strconv.Itoa(c.GlobalLimit)},
		{"--burst", strconv.Itoa(c.Burst)},
		{"--window", c.Window},
		{"--cleanup-interval", strconv.Itoa(c.CleanupInterval) + "m"},
		{"--abuse-ttl", strconv.Itoa(c.AbuseTTL) + "m"},
		{"--abuse-multiplier", strconv.Itoa(c.AbuseMultiplier)},
		{"--abuse-transfer-threshold", strconv.Itoa(c.AbuseTransferThreshold)},
	}
}

func (s *Server) metricRows() []metricRow {
	mfs, err := s.metrics.Registry.Gather()
	if err != nil {
		return nil
	}
	var out []metricRow
	for _, mf := range mfs {
		name := mf.GetName()
		help := mf.GetHelp()
		typ := mf.GetType().String()
		for _, mtr := range mf.GetMetric() {
			label := name
			if len(mtr.Label) > 0 {
				parts := make([]string, 0, len(mtr.Label))
				for _, l := range mtr.Label {
					parts = append(parts, fmt.Sprintf(`%s=%q`, l.GetName(), l.GetValue()))
				}
				label = name + "{" + strings.Join(parts, ",") + "}"
			}
			out = append(out, metricRow{
				Name:  label,
				Type:  typ,
				Value: formatMetricValue(mtr),
				Help:  help,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func formatMetricValue(m *dto.Metric) string {
	switch {
	case m.Counter != nil:
		return strconv.FormatFloat(m.Counter.GetValue(), 'f', -1, 64)
	case m.Gauge != nil:
		return strconv.FormatFloat(m.Gauge.GetValue(), 'f', -1, 64)
	case m.Histogram != nil:
		h := m.Histogram
		count := h.GetSampleCount()
		sum := h.GetSampleSum()
		return fmt.Sprintf("count=%d sum=%.6fs", count, sum)
	}
	return ""
}

// ----- /limits -----

// ───── top-25 from in-memory counters ─────────────────────────────────

type knownTopRow struct {
	Key           string
	Total         int64
	BurstHits     int64
	ViolationHits int64
}

type unknownTopRow struct {
	Key       string
	Total     int64
	BurstHits int64
	AbuseHits int64
}

func resolveKnownSort(sort string) string {
	switch sort {
	case "total", "burst":
		return sort
	}
	return "violations"
}

func resolveUnknownSort(sort string) string {
	switch sort {
	case "total", "burst":
		return sort
	}
	return "abuse"
}

func (s *Server) topKnown(sortBy string) []knownTopRow {
	snap := s.known.Snapshot()
	rows := make([]knownTopRow, 0, len(snap))
	for _, c := range snap {
		rows = append(rows, knownTopRow{
			Key:           c.Key,
			Total:         c.Total,
			BurstHits:     c.BurstHits,
			ViolationHits: c.ViolationHits,
		})
	}
	switch sortBy {
	case "total":
		sort.Slice(rows, func(i, j int) bool { return rows[i].Total > rows[j].Total })
	case "burst":
		sort.Slice(rows, func(i, j int) bool { return rows[i].BurstHits > rows[j].BurstHits })
	default:
		sort.Slice(rows, func(i, j int) bool { return rows[i].ViolationHits > rows[j].ViolationHits })
	}
	if len(rows) > topN {
		rows = rows[:topN]
	}
	return rows
}

func (s *Server) topUnknown(prefix, sortBy string) []unknownTopRow {
	snap := s.unknown.Snapshot()
	rows := make([]unknownTopRow, 0, len(snap))
	for _, c := range snap {
		if !strings.HasPrefix(c.Key, prefix) {
			continue
		}
		rows = append(rows, unknownTopRow{
			Key:       strings.TrimPrefix(c.Key, prefix),
			Total:     c.Total,
			BurstHits: c.BurstHits,
			AbuseHits: c.AbuseHits,
		})
	}
	switch sortBy {
	case "total":
		sort.Slice(rows, func(i, j int) bool { return rows[i].Total > rows[j].Total })
	case "burst":
		sort.Slice(rows, func(i, j int) bool { return rows[i].BurstHits > rows[j].BurstHits })
	default:
		sort.Slice(rows, func(i, j int) bool { return rows[i].AbuseHits > rows[j].AbuseHits })
	}
	if len(rows) > topN {
		rows = rows[:topN]
	}
	return rows
}

// ───── /limits ─────────────────────────────────────────────────────────

type limitsRow struct {
	APIKey        string
	Limit         int64
	CreatedAt     time.Time
	Total         int64
	WindowsAbove  int64
	BurstRequests int64
}

type limitsPage struct {
	Title        string
	BasePath     string
	Rows         []limitsRow
	Page         int
	HasPrev      bool
	HasNext      bool
	Q            string
	Total        int
	DeleteAction string
	PurgeAction  string
	TopRows      []knownTopRow
	TopSort      string
}

func (s *Server) handleLimits(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	topSort := resolveKnownSort(r.URL.Query().Get("topsort"))

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	limits, err := s.store.ScanLimits(ctx)
	if err != nil {
		http.Error(w, "redis scan failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	rows := make([]limitsRow, 0, len(limits))
	for _, l := range limits {
		if q != "" && !strings.Contains(l.APIKey, q) {
			continue
		}
		row := limitsRow{
			APIKey:    l.APIKey,
			Limit:     l.Limit,
			CreatedAt: l.CreatedAt,
		}
		if c, ok := s.known.Get(l.APIKey); ok {
			row.Total = c.Total
			row.WindowsAbove = c.ViolationHits
			row.BurstRequests = c.BurstHits
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].APIKey < rows[j].APIKey })

	total := len(rows)
	from := (page - 1) * pageSize
	to := from + pageSize
	if from > total {
		from = total
	}
	if to > total {
		to = total
	}
	s.render(w, "limits.html", limitsPage{
		Title:        "redisDB1 — limits",
		BasePath:     "/limits",
		Rows:         rows[from:to],
		Page:         page,
		HasPrev:      page > 1,
		HasNext:      to < total,
		Q:            q,
		Total:        total,
		DeleteAction: "/limits/delete",
		PurgeAction:  "/limits/purge",
		TopRows:      s.topKnown(topSort),
		TopSort:      topSort,
	})
}

// ----- /abuse/keys, /abuse/ips -----

type abuseRow struct {
	Key        string
	FirstSeen  time.Time
	LastSeen   time.Time
	Total      int64
	BurstHits  int64
	AbuseHits  int64
	TTL        time.Duration
}

func (s *Server) handleAbuseKeys(w http.ResponseWriter, r *http.Request) {
	s.handleAbuse(w, r, abuseConfig{
		template:      "abuse_keys.html",
		title:         "redisDB2 — abusive api keys",
		scan:          s.store.ScanAbuseKeys,
		deleteAction:  "/abuse/keys/delete",
		purgeAction:   "/abuse/keys/purge",
		basePath:      "/abuse/keys",
		counterPrefix: counter.UnknownKeyPrefix,
		keyHeader:     "api_key",
	})
}
func (s *Server) handleAbuseIPs(w http.ResponseWriter, r *http.Request) {
	s.handleAbuse(w, r, abuseConfig{
		template:      "abuse_ips.html",
		title:         "redisDB3 — abusive IPs",
		scan:          s.store.ScanAbuseIPs,
		deleteAction:  "/abuse/ips/delete",
		purgeAction:   "/abuse/ips/purge",
		basePath:      "/abuse/ips",
		counterPrefix: counter.UnknownIPPrefix,
		keyHeader:     "IP",
	})
}

type abuseConfig struct {
	template      string
	title         string
	scan          func(context.Context) ([]store.AbuseEntry, error)
	deleteAction  string
	purgeAction   string
	basePath      string
	counterPrefix string
	keyHeader     string
}

func (s *Server) handleAbuse(w http.ResponseWriter, r *http.Request, cfg abuseConfig) {
	q := r.URL.Query().Get("q")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	topSort := resolveUnknownSort(r.URL.Query().Get("topsort"))

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	entries, err := cfg.scan(ctx)
	if err != nil {
		http.Error(w, "redis scan failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	rows := make([]abuseRow, 0, len(entries))
	for _, e := range entries {
		if q != "" && !strings.Contains(e.Key, q) {
			continue
		}
		rows = append(rows, abuseRow{
			Key:       e.Key,
			FirstSeen: e.FirstSeen,
			LastSeen:  e.LastSeen,
			Total:     e.TotalRequests,
			BurstHits: e.BurstHits,
			AbuseHits: e.AbuseHits,
			TTL:       e.TTL,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].LastSeen.After(rows[j].LastSeen) })

	total := len(rows)
	from := (page - 1) * pageSize
	to := from + pageSize
	if from > total {
		from = total
	}
	if to > total {
		to = total
	}

	pageData := struct {
		Title        string
		BasePath     string
		Rows         []abuseRow
		Page         int
		HasPrev      bool
		HasNext      bool
		Q            string
		Total        int
		DeleteAction string
		PurgeAction  string
		TopRows      []unknownTopRow
		TopSort      string
		KeyHeader    string
	}{
		Title:        cfg.title,
		BasePath:     cfg.basePath,
		Rows:         rows[from:to],
		Page:         page,
		HasPrev:      page > 1,
		HasNext:      to < total,
		Q:            q,
		Total:        total,
		DeleteAction: cfg.deleteAction,
		PurgeAction:  cfg.purgeAction,
		TopRows:      s.topUnknown(cfg.counterPrefix, topSort),
		TopSort:      topSort,
		KeyHeader:    cfg.keyHeader,
	}
	s.render(w, cfg.template, pageData)
}

// ───── delete / purge handlers ─────────────────────────────────────────

type confirmData struct {
	Title        string
	Message      string
	Action       string
	BackTo       string
	ConfirmLabel string
}

type deleteFn func(ctx context.Context, ids []string) (int64, error)
type purgeFn func(ctx context.Context) error

func (s *Server) handleLimitsDelete(w http.ResponseWriter, r *http.Request) {
	s.handleDelete(w, r, "redisDB1 limit", "/limits", s.store.DeleteLimits)
}
func (s *Server) handleLimitsPurge(w http.ResponseWriter, r *http.Request) {
	s.handlePurge(w, r, "redisDB1", "/limits", "/limits/purge", s.store.PurgeLimits)
}
func (s *Server) handleAbuseKeysDelete(w http.ResponseWriter, r *http.Request) {
	s.handleDelete(w, r, "redisDB2 abuse-key", "/abuse/keys", s.store.DeleteAbuseKeys)
}
func (s *Server) handleAbuseKeysPurge(w http.ResponseWriter, r *http.Request) {
	s.handlePurge(w, r, "redisDB2", "/abuse/keys", "/abuse/keys/purge", s.store.PurgeAbuseKeys)
}
func (s *Server) handleAbuseIPsDelete(w http.ResponseWriter, r *http.Request) {
	s.handleDelete(w, r, "redisDB3 abuse-ip", "/abuse/ips", s.store.DeleteAbuseIPs)
}
func (s *Server) handleAbuseIPsPurge(w http.ResponseWriter, r *http.Request) {
	s.handlePurge(w, r, "redisDB3", "/abuse/ips", "/abuse/ips/purge", s.store.PurgeAbuseIPs)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, label, redirectTo string, fn deleteFn) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	keys := r.PostForm["keys"]
	if len(keys) == 0 {
		http.Redirect(w, r, redirectTo, http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	deleted, err := fn(ctx, keys)
	if err != nil {
		s.logger.Error("admin delete failed", "label", label, "err", err)
		http.Error(w, "delete failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.logger.Info("admin delete", "label", label, "requested", len(keys), "deleted", deleted)
	http.Redirect(w, r, redirectTo, http.StatusSeeOther)
}

func (s *Server) handlePurge(w http.ResponseWriter, r *http.Request, label, redirectTo, action string, fn purgeFn) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Two-step confirmation: first POST renders a page, second POST with
	// confirm=yes actually flushes. Avoids accidental clicks without JS.
	if r.PostFormValue("confirm") != "yes" {
		s.render(w, "confirm.html", confirmData{
			Title:        "Purge " + label + "?",
			Message:      "Permanently delete ALL entries from " + label + ". This cannot be undone.",
			Action:       action,
			BackTo:       redirectTo,
			ConfirmLabel: "Yes, purge " + label,
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := fn(ctx); err != nil {
		s.logger.Error("admin purge failed", "label", label, "err", err)
		http.Error(w, "purge failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.logger.Info("admin purge", "label", label)
	http.Redirect(w, r, redirectTo, http.StatusSeeOther)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("template render failed", "name", name, "err", err)
	}
}
