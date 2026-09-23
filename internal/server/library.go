package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type libraryInput struct {
	Name     string   `json:"name"`
	Roots    []string `json:"roots"`
	Interval int      `json:"intervalMinutes"`
}

func (s *Server) libraries(w http.ResponseWriter, r *http.Request) {
	rows, e := s.db.Query("SELECT id,name,roots,interval_minutes,last_scan FROM libraries ORDER BY name")
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id int64
		var n, roots string
		var interval, last int64
		_ = rows.Scan(&id, &n, &roots, &interval, &last)
		var a []string
		_ = json.Unmarshal([]byte(roots), &a)
		out = append(out, map[string]any{"id": id, "name": n, "roots": a, "intervalMinutes": interval, "lastScan": last})
	}
	jsonOut(w, out)
}
func (s *Server) saveLibrary(w http.ResponseWriter, r *http.Request) {
	var v libraryInput
	if !decode(w, r, &v) {
		return
	}
	v.Name = strings.TrimSpace(v.Name)
	if v.Name == "" || len(v.Name) > 100 || len(v.Roots) == 0 {
		fail(w, 400, "名称和路径不能为空")
		return
	}
	if v.Interval < 0 {
		v.Interval = 0
	}
	roots := []string{}
	seen := map[string]bool{}
	for _, p := range v.Roots {
		a, e := filepath.Abs(filepath.Clean(p))
		if e != nil {
			fail(w, 400, "路径无效")
			return
		}
		if seen[a] {
			continue
		}
		seen[a] = true
		if _, e = os.Stat(a); e != nil {
			fail(w, 400, "目录不存在："+a)
			return
		}
		roots = append(roots, a)
	}
	sort.Strings(roots)
	raw, _ := json.Marshal(roots)
	var id int64
	if r.Method == "PUT" {
		id, _ = strconv.ParseInt(r.PathValue("id"), 10, 64)
	}
	if id > 0 {
		_, e := s.db.Exec("UPDATE libraries SET name=?,roots=?,interval_minutes=? WHERE id=?", v.Name, string(raw), v.Interval, id)
		if e != nil {
			fail(w, 500, bad(e))
			return
		}
	} else {
		res, e := s.db.Exec("INSERT INTO libraries(name,roots,interval_minutes) VALUES(?,?,?)", v.Name, string(raw), v.Interval)
		if e != nil {
			fail(w, 500, bad(e))
			return
		}
		id, _ = res.LastInsertId()
	}
	jsonOut(w, map[string]any{"id": id})
}
func (s *Server) deleteLibrary(w http.ResponseWriter, r *http.Request) {
	_, e := s.db.Exec("DELETE FROM libraries WHERE id=?", id(r))
	if e != nil {
		fail(w, 500, bad(e))
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}
func (s *Server) directories(w http.ResponseWriter, r *http.Request) {
	roots := []string{}
	for _, p := range s.cfg.Roots {
		filepath.WalkDir(p, func(path string, d os.DirEntry, e error) error {
			if e == nil && d.IsDir() {
				roots = append(roots, path)
			}
			return nil
		})
	}
	jsonOut(w, roots)
}
func (s *Server) startScan(w http.ResponseWriter, r *http.Request) {
	s.scanMu.Lock()
	if s.scanning {
		s.scanMu.Unlock()
		jsonOut(w, map[string]any{"started": false, "message": "扫描已在进行"})
		return
	}
	s.scanning = true
	s.scanMessage = "准备扫描"
	s.scanMu.Unlock()
	go s.scanAll()
	jsonOut(w, map[string]bool{"started": true})
}
func (s *Server) scanAll() {
	defer func() { s.scanMu.Lock(); s.scanning = false; s.scanMu.Unlock() }()
	rows, e := s.db.Query("SELECT id,name,roots FROM libraries")
	if e != nil {
		s.log("scan", bad(e))
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name, raw string
		_ = rows.Scan(&id, &name, &raw)
		var roots []string
		_ = json.Unmarshal([]byte(raw), &roots)
		s.scanMessage = "扫描 " + name
		s.scanLibrary(id, roots)
		_, _ = s.db.Exec("UPDATE libraries SET last_scan=? WHERE id=?", time.Now().Unix(), id)
	}
	s.log("scan", "扫描完成")
}

func (s *Server) scheduleScan() {
	if s.scanning {
		return
	}
	now := time.Now().Unix()
	var due int
	if s.db.QueryRow("SELECT COUNT(*) FROM libraries WHERE interval_minutes>0 AND last_scan + interval_minutes*60 <= ?", now).Scan(&due) == nil && due > 0 {
		s.scanMu.Lock()
		if !s.scanning {
			s.scanning = true
			s.scanMessage = "定时扫描"
			go s.scanAll()
		}
		s.scanMu.Unlock()
	}
}
func (s *Server) scanLibrary(lib int64, roots []string) {
	seen := map[string]bool{}
	for _, root := range roots {
		trash := filepath.Join(root, "废弃")
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
			if e != nil {
				return nil
			}
			if d.IsDir() {
				if filepath.Clean(path) == filepath.Clean(trash) {
					return filepath.SkipDir
				}
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if !videoExt[ext] {
				return nil
			}
			seen[path] = true
			info, e := d.Info()
			if e != nil {
				return nil
			}
			var oldSize int64
			var oldMtime int64
			_ = s.db.QueryRow("SELECT size,mtime FROM observations WHERE path=?", path).Scan(&oldSize, &oldMtime)
			now := time.Now().Unix()
			_, _ = s.db.Exec("INSERT INTO observations(path,size,mtime,first_seen) VALUES(?,?,?,?) ON CONFLICT(path) DO UPDATE SET size=excluded.size,mtime=excluded.mtime", path, info.Size(), info.ModTime().Unix())
			if oldSize != 0 && (oldSize != info.Size() || oldMtime != info.ModTime().Unix()) {
				return nil
			}
			if oldSize == 0 {
				return nil
			}
			var exists int
			_ = s.db.QueryRow("SELECT 1 FROM videos WHERE library_id=? AND path=?", lib, path).Scan(&exists)
			if exists == 1 {
				_, _ = s.db.Exec("UPDATE videos SET missing=0,size=?,mtime=? WHERE library_id=? AND path=?", info.Size(), info.ModTime().Unix(), lib, path)
				return nil
			}
			meta := probe(s.cfg.FFprobe, path)
			title := meta.Title
			if title == "" {
				title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			}
			created := int64(0)
			if c, ok := creationTime(path); ok {
				created = c
			}
			finger := digest(path + ":" + strconv.FormatInt(info.Size(), 10) + ":" + strconv.FormatInt(info.ModTime().Unix(), 10))
			_, _ = s.db.Exec("INSERT INTO videos(library_id,root,path,title,size,mtime,created,added,duration,metadata,fingerprint) VALUES(?,?,?,?,?,?,?,?,?,?,?)", lib, root, path, title, info.Size(), info.ModTime().Unix(), created, now, meta.Duration, meta.JSON, finger)
			return nil
		})
	}
	_, _ = s.db.Exec("UPDATE videos SET missing=1 WHERE library_id=? AND path NOT IN (SELECT path FROM observations)", lib)
	for _, root := range roots {
		_, _ = s.db.Exec("UPDATE videos SET missing=1 WHERE library_id=? AND root=? AND path NOT IN (SELECT path FROM observations)", lib, root)
	}
}

var videoExt = map[string]bool{".mp4": true, ".mkv": true, ".webm": true, ".avi": true, ".mov": true, ".m4v": true, ".ts": true, ".m2ts": true, ".mts": true, ".wmv": true, ".flv": true, ".3gp": true, ".ogv": true}
