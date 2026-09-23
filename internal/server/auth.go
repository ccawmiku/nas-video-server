package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
	"net"
	"net/http"
	"strings"
	"time"
)

type attempt struct {
	Count int
	Since time.Time
}

func token() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func digest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func (s *Server) authenticated(r *http.Request) bool {
	c, e := r.Cookie("nas_session")
	if e != nil {
		return false
	}
	var n int
	return s.db.QueryRow("SELECT 1 FROM sessions WHERE token=?", digest(c.Value)).Scan(&n) == nil
}
func (s *Server) protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticated(r) {
			fail(w, 401, "请先登录")
			return
		}
		next(w, r)
	}
}
func (s *Server) rate(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, a := range s.attempts {
		if now.Sub(a.Since) > 10*time.Minute {
			delete(s.attempts, k)
		}
	}
	a := s.attempts[host]
	if a.Count == 0 {
		a.Since = now
	}
	a.Count++
	s.attempts[host] = a
	return a.Count <= 15
}
func (s *Server) issue(w http.ResponseWriter, r *http.Request) error {
	v := token()
	_, e := s.db.Exec("INSERT INTO sessions(token,created,last_seen,agent) VALUES(?,?,?,?)", digest(v), time.Now().Unix(), time.Now().Unix(), r.UserAgent())
	if e != nil {
		return e
	}
	http.SetCookie(w, &http.Cookie{Name: "nas_session", Value: v, Path: "/", HttpOnly: true, Secure: s.cfg.Secure, SameSite: http.SameSiteStrictMode, MaxAge: 34560000})
	return nil
}
func (s *Server) authState(w http.ResponseWriter, r *http.Request) {
	ok := s.authenticated(r)
	if ok {
		c, _ := r.Cookie("nas_session")
		_, _ = s.db.Exec("UPDATE sessions SET last_seen=? WHERE token=?", time.Now().Unix(), digest(c.Value))
		http.SetCookie(w, &http.Cookie{Name: "nas_session", Value: c.Value, Path: "/", HttpOnly: true, Secure: s.cfg.Secure, SameSite: http.SameSiteStrictMode, MaxAge: 34560000})
	}
	jsonOut(w, map[string]any{"initialized": s.setting("password") != "", "authenticated": ok})
}

type credentials struct {
	Password string `json:"password"`
	Code     string `json:"code"`
	Secret   string `json:"secret"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if !s.rate(r) {
		fail(w, 429, "尝试过于频繁，请十分钟后重试")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setting("password") != "" {
		fail(w, 409, "已经初始化")
		return
	}
	var v credentials
	if !decode(w, r, &v) {
		return
	}
	if len(v.Password) < 10 || len(v.Password) > 72 {
		fail(w, 400, "密码需要 10～72 字节")
		return
	}
	if v.Code == "" {
		if s.setupSecret == "" || time.Now().After(s.setupExpires) {
			k, e := totp.Generate(totp.GenerateOpts{Issuer: "NAS Video", AccountName: "global"})
			if e != nil {
				fail(w, 500, "无法生成密钥")
				return
			}
			s.setupSecret = k.Secret()
			s.setupExpires = time.Now().Add(10 * time.Minute)
		}
		jsonOut(w, map[string]string{"secret": s.setupSecret, "url": "otpauth://totp/NAS%20Video:global?secret=" + s.setupSecret + "&issuer=NAS%20Video"})
		return
	}
	if s.setupSecret == "" || time.Now().After(s.setupExpires) || !totp.Validate(v.Code, s.setupSecret) {
		fail(w, 400, "动态验证码无效或初始化已超时")
		return
	}
	hash, e := bcrypt.GenerateFromPassword([]byte(v.Password), bcrypt.DefaultCost)
	if e != nil {
		fail(w, 500, "无法保存密码")
		return
	}
	tx, e := s.db.Begin()
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	defer tx.Rollback()
	_, e = tx.Exec("INSERT INTO settings(key,value) VALUES('password',?),('totp',?)", string(hash), s.setupSecret)
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	s.setupSecret = ""
	if e = s.issue(w, r); e != nil {
		fail(w, 500, e.Error())
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.rate(r) {
		fail(w, 429, "尝试过于频繁，请十分钟后重试")
		return
	}
	var v credentials
	if !decode(w, r, &v) {
		return
	}
	hash := s.setting("password")
	if hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), []byte(v.Password)) != nil || !totp.Validate(strings.TrimSpace(v.Code), s.setting("totp")) {
		fail(w, 401, "密码或动态验证码错误")
		return
	}
	if e := s.issue(w, r); e != nil {
		fail(w, 500, e.Error())
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie("nas_session")
	_, _ = s.db.Exec("DELETE FROM sessions WHERE token=?", digest(c.Value))
	http.SetCookie(w, &http.Cookie{Name: "nas_session", Path: "/", MaxAge: -1, HttpOnly: true})
	jsonOut(w, map[string]bool{"ok": true})
}
func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	rows, e := s.db.Query("SELECT token,created,last_seen,agent FROM sessions ORDER BY created DESC")
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	defer rows.Close()
	c, _ := r.Cookie("nas_session")
	out := []any{}
	for rows.Next() {
		var t, a string
		var created, last int64
		_ = rows.Scan(&t, &created, &last, &a)
		out = append(out, map[string]any{"id": t, "created": created, "lastSeen": last, "agent": a, "current": t == digest(c.Value)})
	}
	jsonOut(w, out)
}
func (s *Server) revokeSessions(w http.ResponseWriter, r *http.Request) {
	var v struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &v) {
		return
	}
	if v.ID == "all" {
		_, _ = s.db.Exec("DELETE FROM sessions")
	} else {
		_, _ = s.db.Exec("DELETE FROM sessions WHERE token=?", v.ID)
	}
	jsonOut(w, map[string]bool{"ok": true})
}
func (s *Server) rebind(w http.ResponseWriter, r *http.Request) {
	if !s.rate(r) {
		fail(w, 429, "尝试过于频繁")
		return
	}
	var v credentials
	if !decode(w, r, &v) {
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(s.setting("password")), []byte(v.Password)) != nil {
		fail(w, 401, "密码错误")
		return
	}
	if v.Secret == "" {
		k, e := totp.Generate(totp.GenerateOpts{Issuer: "NAS Video", AccountName: "global"})
		if e != nil {
			fail(w, 500, e.Error())
			return
		}
		jsonOut(w, map[string]string{"secret": k.Secret(), "url": k.URL()})
		return
	}
	if !totp.Validate(v.Code, v.Secret) {
		fail(w, 400, "动态验证码错误")
		return
	}
	_, e := s.db.Exec("UPDATE settings SET value=? WHERE key='totp'", v.Secret)
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	_, _ = s.db.Exec("DELETE FROM sessions")
	_ = s.issue(w, r)
	jsonOut(w, map[string]bool{"ok": true})
}
