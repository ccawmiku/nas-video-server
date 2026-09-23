package server

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	_ "modernc.org/sqlite"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed all:static
var frontend embed.FS

type Config struct {
	Data, Cache, Address, FFmpeg, FFprobe, Device string
	Roots                                         []string
	MaxJobs, Threads                              int
	Stable                                        time.Duration
	Secure                                        bool
}
type Server struct {
	cfg          Config
	db           *sql.DB
	ctx          context.Context
	mu           sync.Mutex
	scanning     bool
	scanMessage  string
	jobs         map[string]*Job
	setupSecret  string
	setupExpires time.Time
	attempts     map[string]attempt
	previewSlots chan struct{}
	scanMu       sync.Mutex
	fileMu       sync.RWMutex
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
func envInt(k string, fallback int) int {
	n, e := strconv.Atoi(os.Getenv(k))
	if e != nil || n < 1 {
		return fallback
	}
	return n
}
func Run(ctx context.Context) error {
	cfg := Config{Data: env("DATA_DIR", "./.local/data"), Cache: env("CACHE_DIR", "./.local/cache"), Address: env("LISTEN_ADDR", ":8096"), FFmpeg: env("FFMPEG", "ffmpeg"), FFprobe: env("FFPROBE", "ffprobe"), Device: env("VAAPI_DEVICE", "/dev/dri/renderD128"), Roots: filepath.SplitList(env("MEDIA_ROOTS", "/media")), MaxJobs: envInt("MAX_TRANSCODES", 2), Threads: envInt("CPU_THREADS", 2), Stable: time.Duration(envInt("STABLE_SECONDS", 60)) * time.Second, Secure: env("COOKIE_SECURE", "false") == "true"}
	s, err := New(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	srv := &http.Server{Addr: cfg.Address, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	}()
	go s.maintenance()
	log.Printf("NAS Video listening on %s", cfg.Address)
	err = srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func New(ctx context.Context, cfg Config) (*Server, error) {
	for _, p := range []string{cfg.Data, cfg.Cache} {
		if err := os.MkdirAll(p, 0700); err != nil {
			return nil, err
		}
	}
	for i, p := range cfg.Roots {
		a, e := filepath.Abs(p)
		if e != nil {
			return nil, e
		}
		cfg.Roots[i] = filepath.Clean(a)
	}
	db, e := sql.Open("sqlite", filepath.Join(cfg.Data, "library.db"))
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
 CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS sessions(token TEXT PRIMARY KEY,created INTEGER NOT NULL,last_seen INTEGER NOT NULL,agent TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS libraries(id INTEGER PRIMARY KEY,name TEXT NOT NULL,roots TEXT NOT NULL,interval_minutes INTEGER NOT NULL DEFAULT 360,last_scan INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS videos(id INTEGER PRIMARY KEY,library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,root TEXT NOT NULL,path TEXT NOT NULL,title TEXT NOT NULL,size INTEGER NOT NULL,mtime INTEGER NOT NULL,created INTEGER,added INTEGER NOT NULL,duration REAL NOT NULL,metadata TEXT NOT NULL,fingerprint TEXT NOT NULL,missing INTEGER NOT NULL DEFAULT 0,position REAL NOT NULL DEFAULT 0,favorite INTEGER NOT NULL DEFAULT 0,last_played INTEGER NOT NULL DEFAULT 0,UNIQUE(library_id,path));
 CREATE INDEX IF NOT EXISTS videos_library ON videos(library_id,added);
 CREATE INDEX IF NOT EXISTS videos_fingerprint ON videos(library_id,fingerprint);
 CREATE TABLE IF NOT EXISTS observations(path TEXT PRIMARY KEY,size INTEGER NOT NULL,mtime INTEGER NOT NULL,first_seen INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS logs(id INTEGER PRIMARY KEY,time INTEGER NOT NULL,kind TEXT NOT NULL,message TEXT NOT NULL);`)
	if e != nil {
		db.Close()
		return nil, e
	}
	return &Server{cfg: cfg, db: db, ctx: ctx, jobs: map[string]*Job{}, attempts: map[string]attempt{}, previewSlots: make(chan struct{}, 1)}, nil
}
func (s *Server) Close() {
	s.mu.Lock()
	for _, j := range s.jobs {
		j.cancel()
	}
	s.mu.Unlock()
	s.db.Close()
}
func (s *Server) setting(key string) string {
	var v string
	_ = s.db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&v)
	return v
}
func (s *Server) log(kind, msg string) {
	log.Printf("%s: %s", kind, msg)
	_, _ = s.db.Exec("INSERT INTO logs(time,kind,message) VALUES(?,?,?)", time.Now().Unix(), kind, msg)
	_, _ = s.db.Exec("DELETE FROM logs WHERE id < (SELECT COALESCE(MAX(id),0)-1000 FROM logs)")
}
func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		fail(w, 400, "请求格式错误")
		return false
	}
	return true
}
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, map[string]string{"status": "ok"}) })
	m.HandleFunc("GET /api/auth", s.authState)
	m.HandleFunc("POST /api/setup", s.setup)
	m.HandleFunc("POST /api/login", s.login)
	m.HandleFunc("POST /api/logout", s.protect(s.logout))
	m.HandleFunc("GET /api/sessions", s.protect(s.sessions))
	m.HandleFunc("POST /api/sessions/revoke", s.protect(s.revokeSessions))
	m.HandleFunc("POST /api/auth/rebind", s.protect(s.rebind))
	m.HandleFunc("GET /api/libraries", s.protect(s.libraries))
	m.HandleFunc("POST /api/libraries", s.protect(s.saveLibrary))
	m.HandleFunc("PUT /api/libraries/{id}", s.protect(s.saveLibrary))
	m.HandleFunc("DELETE /api/libraries/{id}", s.protect(s.deleteLibrary))
	m.HandleFunc("GET /api/directories", s.protect(s.directories))
	m.HandleFunc("POST /api/scan", s.protect(s.startScan))
	m.HandleFunc("GET /api/videos", s.protect(s.videos))
	m.HandleFunc("GET /api/videos/{id}", s.protect(s.videoDetail))
	m.HandleFunc("POST /api/videos/{id}/progress", s.protect(s.progress))
	m.HandleFunc("POST /api/videos/{id}/favorite", s.protect(s.favorite))
	m.HandleFunc("POST /api/videos/{id}/trash", s.protect(s.trash))
	m.HandleFunc("GET /api/videos/{id}/file", s.protect(s.mediaFile))
	m.HandleFunc("GET /api/videos/{id}/preview", s.protect(s.preview))
	m.HandleFunc("GET /api/videos/{id}/subtitle", s.protect(s.subtitle))
	m.HandleFunc("GET /api/random", s.protect(s.randomVideo))
	m.HandleFunc("POST /api/play", s.protect(s.play))
	m.HandleFunc("GET /api/jobs/{id}", s.protect(s.jobStatus))
	m.HandleFunc("POST /api/jobs/{id}/heartbeat", s.protect(s.heartbeat))
	m.HandleFunc("DELETE /api/jobs/{id}", s.protect(s.stopJob))
	m.HandleFunc("GET /api/streams/{id}/{file}", s.protect(s.stream))
	m.HandleFunc("GET /api/status", s.protect(s.status))
	m.HandleFunc("POST /api/cache/clear", s.protect(s.clearCache))
	m.HandleFunc("GET /api/backup", s.protect(s.backup))
	m.HandleFunc("POST /api/restore", s.protect(s.restore))
	content, _ := fs.Sub(frontend, "static")
	files := http.FileServer(http.FS(content))
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			fail(w, 404, "接口不存在")
			return
		}
		if r.URL.Path == "/" {
			if _, e := fs.Stat(content, "index.html"); e != nil {
				http.Error(w, "Build the web app with npm run build", 503)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' blob:; worker-src 'self' blob:; connect-src 'self'; frame-ancestors 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if r.Header.Get("X-Requested-With") != "nas-video" {
				fail(w, 403, "缺少请求校验头")
				return
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				fail(w, 403, "禁止跨站请求")
				return
			}
		}
		m.ServeHTTP(w, r)
	})
}
func (s *Server) maintenance() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.reapJobs()
			s.scheduleScan()
			s.pruneCache()
		}
	}
}
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	scan, msg := s.scanning, s.scanMessage
	jobs := []any{}
	for _, j := range s.jobs {
		jobs = append(jobs, j.view())
	}
	s.mu.Unlock()
	rows, e := s.db.Query("SELECT time,kind,message FROM logs ORDER BY id DESC LIMIT 100")
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	defer rows.Close()
	logs := []any{}
	for rows.Next() {
		var t int64
		var k, m string
		_ = rows.Scan(&t, &k, &m)
		logs = append(logs, map[string]any{"time": t, "kind": k, "message": m})
	}
	_, ve := os.Stat(s.cfg.Device)
	jsonOut(w, map[string]any{"scanning": scan, "scanMessage": msg, "jobs": jobs, "logs": logs, "vaapiDevice": s.cfg.Device, "vaapiPresent": ve == nil, "maxTranscodes": s.cfg.MaxJobs, "cpuThreads": s.cfg.Threads})
}
func id(r *http.Request) int64 { n, _ := strconv.ParseInt(r.PathValue("id"), 10, 64); return n }
func num(s string) float64     { n, _ := strconv.ParseFloat(s, 64); return n }
func bad(err error) string     { return fmt.Sprintf("操作失败：%v", err) }
