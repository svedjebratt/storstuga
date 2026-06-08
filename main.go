package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type config struct {
	BindAddr          string
	DBPath            string
	CheckInterval     time.Duration
	CheckTimeout      time.Duration
	CheckTargets      []string
	FailureThreshold  int
	RestartCooldown   time.Duration
	ShellyOffURL      string
	ShellyOnURL       string
	RestartDelay      time.Duration
	WebhookToken      string
	RetentionDays     int
	RetentionInterval time.Duration
}

type appState struct {
	mu               sync.RWMutex
	lastCheckUp      bool
	consecutiveFails int
	lastRestartAt    time.Time
	lastError        string
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	db, err := openDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("db error: %v", err)
	}
	defer db.Close()

	if err := initSchema(db); err != nil {
		log.Fatalf("schema error: %v", err)
	}

	state := &appState{}
	httpClient := &http.Client{Timeout: cfg.CheckTimeout}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go monitorLoop(ctx, cfg, state, httpClient)
	go retentionLoop(ctx, cfg, db)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		state.mu.RLock()
		resp := map[string]any{
			"ok":                true,
			"network_up":        state.lastCheckUp,
			"consecutive_fails": state.consecutiveFails,
			"last_restart_at":   state.lastRestartAt,
			"last_error":        state.lastError,
		}
		state.mu.RUnlock()
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/webhook/shelly", func(w http.ResponseWriter, r *http.Request) {
		handleShellyWebhook(w, r, cfg, db)
	})

	server := &http.Server{
		Addr:              cfg.BindAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("starting server on %s", cfg.BindAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

func loadConfig() (config, error) {
	cfg := config{
		BindAddr:          getenvDefault("BIND_ADDR", ":8080"),
		DBPath:            getenvDefault("DB_PATH", "./data.db"),
		CheckInterval:     mustDuration("CHECK_INTERVAL", "30s"),
		CheckTimeout:      mustDuration("CHECK_TIMEOUT", "4s"),
		CheckTargets:      splitCSV(getenvDefault("CHECK_TARGETS", "1.1.1.1:53,8.8.8.8:53")),
		FailureThreshold:  mustInt("FAILURE_THRESHOLD", 3),
		RestartCooldown:   mustDuration("RESTART_COOLDOWN", "30m"),
		RestartDelay:      mustDuration("RESTART_DELAY", "30s"),
		ShellyOffURL:      os.Getenv("SHELLY_OFF_URL"),
		ShellyOnURL:       os.Getenv("SHELLY_ON_URL"),
		WebhookToken:      os.Getenv("WEBHOOK_TOKEN"),
		RetentionDays:     mustInt("RETENTION_DAYS", 90),
		RetentionInterval: mustDuration("RETENTION_INTERVAL", "24h"),
	}

	if len(cfg.CheckTargets) == 0 {
		return cfg, fmt.Errorf("CHECK_TARGETS cannot be empty")
	}
	if cfg.FailureThreshold < 1 {
		return cfg, fmt.Errorf("FAILURE_THRESHOLD must be >= 1")
	}
	if cfg.ShellyOffURL == "" || cfg.ShellyOnURL == "" {
		return cfg, fmt.Errorf("SHELLY_OFF_URL and SHELLY_ON_URL are required")
	}
	return cfg, nil
}

func monitorLoop(ctx context.Context, cfg config, state *appState, client *http.Client) {
	ticker := time.NewTicker(cfg.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			up, err := checkNetwork(ctx, cfg.CheckTargets, cfg.CheckTimeout)
			state.mu.Lock()
			state.lastCheckUp = up
			if err != nil {
				state.lastError = err.Error()
			} else {
				state.lastError = ""
			}

			if up {
				state.consecutiveFails = 0
				state.mu.Unlock()
				continue
			}

			state.consecutiveFails++
			shouldRestart := state.consecutiveFails >= cfg.FailureThreshold && time.Since(state.lastRestartAt) >= cfg.RestartCooldown
			state.mu.Unlock()

			if shouldRestart {
				log.Printf("network down for %d checks, restarting via shelly", cfg.FailureThreshold)
				if err := restartViaShelly(ctx, client, cfg); err != nil {
					state.mu.Lock()
					state.lastError = "restart failed: " + err.Error()
					state.mu.Unlock()
					log.Printf("restart failed: %v", err)
					continue
				}
				state.mu.Lock()
				state.lastRestartAt = time.Now().UTC()
				state.consecutiveFails = 0
				state.mu.Unlock()
			}
		}
	}
}

func checkNetwork(ctx context.Context, targets []string, timeout time.Duration) (bool, error) {
	var errs []string
	for _, t := range targets {
		if strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://") {
			if err := checkHTTP(ctx, t, timeout); err == nil {
				return true, nil
			} else {
				errs = append(errs, fmt.Sprintf("%s: %v", t, err))
			}
			continue
		}
		if err := checkTCP(ctx, t, timeout); err == nil {
			return true, nil
		} else {
			errs = append(errs, fmt.Sprintf("%s: %v", t, err))
		}
	}
	return false, fmt.Errorf("all targets failed: %s", strings.Join(errs, "; "))
}

func checkTCP(ctx context.Context, target string, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return err
	}
	_ = c.Close()
	return nil
}

func checkHTTP(ctx context.Context, target string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func restartViaShelly(ctx context.Context, client *http.Client, cfg config) error {
	log.Printf("power off router via Shelly: %s", cfg.ShellyOffURL)
	if err := doRequest(ctx, client, cfg.ShellyOffURL); err != nil {
		return fmt.Errorf("off request: %w", err)
	}
	t := time.NewTimer(cfg.RestartDelay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
	}
	log.Printf("power on router via Shelly: %s", cfg.ShellyOnURL)
	if err := doRequest(ctx, client, cfg.ShellyOnURL); err != nil {
		return fmt.Errorf("on request: %w", err)
	}
	return nil
}

func doRequest(ctx context.Context, client *http.Client, rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status=%d body=%q", resp.StatusCode, string(body))
	}
	return nil
}

func handleShellyWebhook(w http.ResponseWriter, r *http.Request, cfg config, db *sql.DB) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if cfg.WebhookToken != "" {
		tok := r.Header.Get("X-Webhook-Token")
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		if tok != cfg.WebhookToken {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}

	parsed := parseMeasurement(body)
	if parsed.Timestamp.IsZero() {
		parsed.Timestamp = time.Now().UTC()
	}

	_, err = db.Exec(`
		INSERT INTO temperature_events (device_id, ts, temp_c, humidity, battery, raw_json)
		VALUES (?, ?, ?, ?, ?, ?)
	`, parsed.DeviceID, parsed.Timestamp, parsed.TempC, parsed.Humidity, parsed.Battery, string(bytes.TrimSpace(body)))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db insert failed"})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

type measurement struct {
	DeviceID  string
	Timestamp time.Time
	TempC     *float64
	Humidity  *float64
	Battery   *float64
}

func parseMeasurement(body []byte) measurement {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return measurement{DeviceID: "unknown", Timestamp: time.Now().UTC()}
	}

	m := measurement{DeviceID: getString(payload, "device_id", "id", "mac", "src")}
	if m.DeviceID == "" {
		m.DeviceID = "unknown"
	}
	m.TempC = getNumber(payload, "temp", "temperature", "tC", "temp_c")
	m.Humidity = getNumber(payload, "humidity", "hum")
	m.Battery = getNumber(payload, "battery", "bat", "battery_percent")

	if ts := getNumber(payload, "ts", "timestamp", "time"); ts != nil {
		secs := int64(*ts)
		m.Timestamp = time.Unix(secs, 0).UTC()
	} else {
		m.Timestamp = time.Now().UTC()
	}

	return m
}

func getString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch vv := v.(type) {
			case string:
				return vv
			}
		}
	}
	return ""
}

func getNumber(m map[string]any, keys ...string) *float64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch vv := v.(type) {
		case float64:
			if !math.IsNaN(vv) && !math.IsInf(vv, 0) {
				return &vv
			}
		case int:
			f := float64(vv)
			return &f
		case json.Number:
			f, err := vv.Float64()
			if err == nil {
				return &f
			}
		case string:
			f, err := strconv.ParseFloat(vv, 64)
			if err == nil {
				return &f
			}
		case map[string]any:
			if nested := getNumber(vv, "value", "tC", "temp"); nested != nil {
				return nested
			}
		}
	}

	if ev, ok := m["event"].(map[string]any); ok {
		if data, ok := ev["data"].(map[string]any); ok {
			return getNumber(data, keys...)
		}
	}
	return nil
}

func retentionLoop(ctx context.Context, cfg config, db *sql.DB) {
	ticker := time.NewTicker(cfg.RetentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().AddDate(0, 0, -cfg.RetentionDays)
			_, err := db.Exec(`DELETE FROM temperature_events WHERE ts < ?`, cutoff.UTC())
			if err != nil {
				log.Printf("retention prune failed: %v", err)
			}
		}
	}
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		return nil, err
	}
	return db, nil
}

func initSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS temperature_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			device_id TEXT NOT NULL,
			ts TIMESTAMP NOT NULL,
			temp_c REAL,
			humidity REAL,
			battery REAL,
			raw_json TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_temperature_events_device_ts ON temperature_events(device_id, ts);
		CREATE INDEX IF NOT EXISTS idx_temperature_events_ts ON temperature_events(ts);
	`)
	return err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func getenvDefault(key, fallback string) string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v
}

func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func mustDuration(key, fallback string) time.Duration {
	v := getenvDefault(key, fallback)
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("invalid duration for %s: %s", key, v)
	}
	return d
}

func mustInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("invalid int for %s: %s", key, v)
	}
	return i
}
