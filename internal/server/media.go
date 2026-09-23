package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type MediaMeta struct {
	Title    string
	Duration float64
	JSON     string
}

func probe(bin, path string) MediaMeta {
	c, e := exec.Command(bin, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path).Output()
	if e != nil {
		return MediaMeta{JSON: "{}"}
	}
	var v struct {
		Format struct {
			Duration string            `json:"duration"`
			Tags     map[string]string `json:"tags"`
		} `json:"format"`
	}
	_ = json.Unmarshal(c, &v)
	return MediaMeta{Title: v.Format.Tags["title"], Duration: num(v.Format.Duration), JSON: string(c)}
}

type Job struct {
	ID       string
	Video    int64
	Dir      string
	cancel   context.CancelFunc
	mu       sync.RWMutex
	Progress float64
	Speed    string
	Mode     string
	Message  string
	Started  time.Time
	Done     bool
	Err      string
	Timer    *time.Timer
}

func (j *Job) view() map[string]any {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return map[string]any{"id": j.ID, "videoId": j.Video, "progress": j.Progress, "speed": j.Speed, "mode": j.Mode, "message": j.Message, "done": j.Done, "error": j.Err}
}
func (s *Server) videos(w http.ResponseWriter, r *http.Request) {
	lib, _ := strconv.ParseInt(r.URL.Query().Get("library"), 10, 64)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	sort := r.URL.Query().Get("sort")
	if sort == "" {
		sort = "title"
	}
	allowed := map[string]string{"title": "title COLLATE NOCASE", "size": "size", "duration": "duration", "created": "created", "modified": "mtime", "favorite": "favorite", "progress": "last_played"}
	order, ok := allowed[sort]
	if !ok {
		order = allowed["title"]
	}
	dir := "ASC"
	if strings.ToLower(r.URL.Query().Get("direction")) == "desc" {
		dir = "DESC"
	}
	where := []string{"missing=0"}
	args := []any{}
	if lib > 0 {
		where = append(where, "library_id=?")
		args = append(args, lib)
	}
	if q != "" {
		where = append(where, "(title LIKE ? OR path LIKE ?)")
		args = append(args, "%"+q+"%", "%"+q+"%")
	}
	if r.URL.Query().Get("favorite") == "1" {
		where = append(where, "favorite=1")
	}
	rows, e := s.db.Query("SELECT id,library_id,title,path,size,mtime,created,duration,position,favorite,last_played,metadata FROM videos WHERE "+strings.Join(where, " AND ")+" ORDER BY "+order+" "+dir+",id DESC LIMIT 1000", args...)
	if e != nil {
		fail(w, 500, bad(e))
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id, l int64
		var title, path string
		var size, mtime, created, last int64
		var duration, pos float64
		var fav int
		var raw string
		_ = rows.Scan(&id, &l, &title, &path, &size, &mtime, &created, &duration, &pos, &fav, &last, &raw)
		out = append(out, map[string]any{"id": id, "libraryId": l, "title": title, "path": path, "size": size, "mtime": mtime, "created": created, "duration": duration, "position": pos, "favorite": fav == 1, "lastPlayed": last, "metadata": json.RawMessage(raw)})
	}
	jsonOut(w, map[string]any{"items": out, "count": len(out)})
}
func (s *Server) videoDetail(w http.ResponseWriter, r *http.Request) {
	var vid, l int64
	var title, path string
	var size, mtime, created, last int64
	var duration, pos float64
	var fav int
	var raw string
	e := s.db.QueryRow("SELECT id,library_id,title,path,size,mtime,created,duration,position,favorite,last_played,metadata FROM videos WHERE id=? AND missing=0", id(r)).Scan(&vid, &l, &title, &path, &size, &mtime, &created, &duration, &pos, &fav, &last, &raw)
	if e != nil {
		fail(w, 404, "视频不存在")
		return
	}
	jsonOut(w, map[string]any{"id": vid, "libraryId": l, "title": title, "path": path, "size": size, "mtime": mtime, "created": created, "duration": duration, "position": pos, "favorite": fav == 1, "lastPlayed": last, "metadata": json.RawMessage(raw), "streams": probe(s.cfg.FFprobe, path)})
}
func (s *Server) progress(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Position float64 `json:"position"`
	}
	if !decode(w, r, &v) {
		return
	}
	if v.Position < 0 {
		v.Position = 0
	}
	_, e := s.db.Exec("UPDATE videos SET position=?,last_played=? WHERE id=?", v.Position, time.Now().Unix(), id(r))
	if e != nil {
		fail(w, 500, bad(e))
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}
func (s *Server) favorite(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Favorite bool `json:"favorite"`
	}
	if !decode(w, r, &v) {
		return
	}
	_, e := s.db.Exec("UPDATE videos SET favorite=? WHERE id=?", boolInt(v.Favorite), id(r))
	if e != nil {
		fail(w, 500, bad(e))
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func (s *Server) trash(w http.ResponseWriter, r *http.Request) {
	var root, path string
	var title string
	e := s.db.QueryRow("SELECT root,path,title FROM videos WHERE id=?", id(r)).Scan(&root, &path, &title)
	if e != nil {
		fail(w, 404, "视频不存在")
		return
	}
	dest := filepath.Join(root, "废弃")
	if err := os.MkdirAll(dest, 0750); err != nil {
		fail(w, 500, bad(err))
		return
	}
	target := filepath.Join(dest, filepath.Base(path))
	if filepath.Clean(target) == filepath.Clean(path) {
		fail(w, 400, "目标路径无效")
		return
	}
	if _, e = os.Stat(target); e == nil {
		target = filepath.Join(dest, fmt.Sprintf("%s-%d%s", strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), time.Now().UnixNano(), filepath.Ext(path)))
	}
	if e = os.Rename(path, target); e != nil {
		fail(w, 500, bad(e))
		return
	}
	_, _ = s.db.Exec("DELETE FROM videos WHERE id=?", id(r))
	jsonOut(w, map[string]string{"movedTo": target})
}
func (s *Server) mediaFile(w http.ResponseWriter, r *http.Request) {
	var path string
	if e := s.db.QueryRow("SELECT path FROM videos WHERE id=? AND missing=0", id(r)).Scan(&path); e != nil {
		fail(w, 404, "视频不存在")
		return
	}
	http.ServeFile(w, r, path)
}
func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	var path string
	var duration float64
	if e := s.db.QueryRow("SELECT path,duration FROM videos WHERE id=?", id(r)).Scan(&path, &duration); e != nil {
		fail(w, 404, "视频不存在")
		return
	}
	sec := num(r.URL.Query().Get("t"))
	if sec < 0 {
		sec = 0
	}
	if duration > 0 && sec > duration {
		sec = duration
	}
	key := digest(path + ":" + strconv.FormatInt(int64(sec/10), 10))
	out := filepath.Join(s.cfg.Cache, "previews", key+".jpg")
	if _, e := os.Stat(out); e != nil {
		select {
		case s.previewSlots <- struct{}{}:
			defer func() { <-s.previewSlots }()
		default:
			fail(w, 429, "预览正在生成")
			return
		}
		_ = os.MkdirAll(filepath.Dir(out), 0750)
		e = exec.Command(s.cfg.FFmpeg, "-hide_banner", "-loglevel", "error", "-ss", fmt.Sprintf("%.2f", sec), "-i", path, "-frames:v", "1", "-vf", "scale=320:-2", "-q:v", "8", "-y", out).Run()
		if e != nil {
			fail(w, 500, "无法生成预览")
			return
		}
	}
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeFile(w, r, out)
}
func (s *Server) subtitle(w http.ResponseWriter, r *http.Request) {
	var path string
	if e := s.db.QueryRow("SELECT path FROM videos WHERE id=?", id(r)).Scan(&path); e != nil {
		fail(w, 404, "视频不存在")
		return
	}
	data, e := os.ReadFile(path)
	if e != nil {
		fail(w, 500, bad(e))
		return
	}
	_ = data
	fail(w, 501, "字幕轨道将在播放会话中输出")
}
func (s *Server) randomVideo(w http.ResponseWriter, r *http.Request) {
	lib, _ := strconv.ParseInt(r.URL.Query().Get("library"), 10, 64)
	var id int64
	var e error
	if lib > 0 {
		e = s.db.QueryRow("SELECT id FROM videos WHERE library_id=? AND missing=0 ORDER BY random() LIMIT 1", lib).Scan(&id)
	} else {
		e = s.db.QueryRow("SELECT id FROM videos WHERE missing=0 ORDER BY random() LIMIT 1").Scan(&id)
	}
	if e != nil {
		fail(w, 404, "没有可播放视频")
		return
	}
	jsonOut(w, map[string]int64{"id": id})
}
func (s *Server) play(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Video     int64  `json:"videoId"`
		Mode      string `json:"mode"`
		Audio     int    `json:"audio"`
		Subtitle  int    `json:"subtitle"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		StopAfter int    `json:"stopAfter"`
	}
	if !decode(w, r, &v) {
		return
	}
	if v.Mode == "" {
		v.Mode = "auto"
	}
	var path string
	if e := s.db.QueryRow("SELECT path FROM videos WHERE id=? AND missing=0", v.Video).Scan(&path); e != nil {
		fail(w, 404, "视频不存在")
		return
	}
	if v.Mode == "direct" {
		jsonOut(w, map[string]any{"direct": true, "url": "/api/videos/" + strconv.FormatInt(v.Video, 10) + "/file"})
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	j := &Job{ID: token()[:16], Video: v.Video, Started: time.Now(), Mode: "cpu", cancel: cancel}
	s.mu.Lock()
	if len(s.jobs) >= s.cfg.MaxJobs {
		s.mu.Unlock()
		cancel()
		fail(w, 429, "转码任务已满，请稍后重试")
		return
	}
	s.jobs[j.ID] = j
	s.mu.Unlock()
	go s.transcode(ctx, j, path, v)
	jsonOut(w, map[string]any{"direct": false, "jobId": j.ID, "url": "/api/streams/" + j.ID + "/index.m3u8"})
}
func (s *Server) transcode(ctx context.Context, j *Job, path string, v struct {
	Video     int64  `json:"videoId"`
	Mode      string `json:"mode"`
	Audio     int    `json:"audio"`
	Subtitle  int    `json:"subtitle"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	StopAfter int    `json:"stopAfter"`
}) {
	defer func() { s.mu.Lock(); delete(s.jobs, j.ID); s.mu.Unlock() }()
	dir := filepath.Join(s.cfg.Cache, "streams", j.ID)
	_ = os.MkdirAll(dir, 0750)
	out := filepath.Join(dir, "index.m3u8")
	cpuArgs := []string{"-hide_banner", "-loglevel", "error", "-i", path, "-map", "0:v:0", "-map", "0:a:0?", "-c:v", "libx264", "-preset", "veryfast", "-c:a", "aac", "-b:a", "128k", "-f", "hls", "-hls_time", "4", "-hls_list_size", "8", "-hls_flags", "delete_segments+append_list", out}
	args := cpuArgs
	if _, e := os.Stat(s.cfg.Device); e == nil {
		j.mu.Lock()
		j.Mode = "vaapi"
		j.mu.Unlock()
		args = []string{"-hide_banner", "-loglevel", "error", "-vaapi_device", s.cfg.Device, "-i", path, "-map", "0:v:0", "-map", "0:a:0?", "-vf", "format=nv12,hwupload", "-c:v", "h264_vaapi", "-b:v", "4M", "-c:a", "aac", "-b:a", "128k", "-f", "hls", "-hls_time", "4", "-hls_list_size", "8", "-hls_flags", "delete_segments+append_list", out}
	}
	cmd := exec.CommandContext(ctx, s.cfg.FFmpeg, args...)
	err := cmd.Run()
	if err != nil && j.Mode == "vaapi" {
		j.mu.Lock()
		j.Mode = "cpu"
		j.Message = "VAAPI 失败，已回退 CPU"
		j.mu.Unlock()
		_ = os.RemoveAll(dir)
		_ = os.MkdirAll(dir, 0750)
		cmd = exec.CommandContext(ctx, s.cfg.FFmpeg, cpuArgs...)
		err = cmd.Run()
	}
	j.mu.Lock()
	j.Done = true
	if err != nil && ctx.Err() == nil {
		j.Err = err.Error()
	}
	j.mu.Unlock()
	if v.StopAfter > 0 {
		time.AfterFunc(time.Duration(v.StopAfter)*time.Minute, func() { j.cancel() })
	}
}
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	job := r.PathValue("id")
	file := filepath.Base(r.PathValue("file"))
	s.mu.Lock()
	j := s.jobs[job]
	s.mu.Unlock()
	if j == nil {
		fail(w, 404, "转码任务不存在")
		return
	}
	path := filepath.Join(s.cfg.Cache, "streams", job, file)
	if filepath.Ext(path) == ".m3u8" {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	} else {
		w.Header().Set("Content-Type", mime.TypeByExtension(filepath.Ext(path)))
	}
	http.ServeFile(w, r, path)
}
func (s *Server) jobStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	s.mu.Unlock()
	if j == nil {
		fail(w, 404, "转码任务不存在")
		return
	}
	jsonOut(w, j.view())
}
func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, map[string]bool{"ok": true})
}
func (s *Server) stopJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	s.mu.Unlock()
	if j != nil {
		j.cancel()
	}
	jsonOut(w, map[string]bool{"ok": true})
}
func (s *Server) reapJobs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, j := range s.jobs {
		j.mu.RLock()
		done := j.Done
		j.mu.RUnlock()
		if done {
			delete(s.jobs, id)
			_ = os.RemoveAll(filepath.Join(s.cfg.Cache, "streams", id))
		}
	}
}
func (s *Server) pruneCache() {
	root := filepath.Join(s.cfg.Cache, "previews")
	entries, _ := os.ReadDir(root)
	if len(entries) > 5000 {
		for _, e := range entries[:len(entries)-5000] {
			_ = os.Remove(filepath.Join(root, e.Name()))
		}
	}
}
func (s *Server) clearCache(w http.ResponseWriter, r *http.Request) {
	_ = os.RemoveAll(filepath.Join(s.cfg.Cache, "previews"))
	_ = os.MkdirAll(filepath.Join(s.cfg.Cache, "previews"), 0750)
	jsonOut(w, map[string]bool{"ok": true})
}
func (s *Server) backup(w http.ResponseWriter, r *http.Request) {
	f, e := os.Open(filepath.Join(s.cfg.Data, "library.db"))
	if e != nil {
		fail(w, 500, bad(e))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=nas-video-backup.db")
	_, _ = io.Copy(w, f)
}
func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	fail(w, 501, "恢复功能将在下一阶段加入")
}
