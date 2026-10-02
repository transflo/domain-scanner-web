package server

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"domain_scanner/internal/auth"
	"domain_scanner/internal/scheduler"
	"domain_scanner/internal/store"
)

func (a *api) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- auth ----

func (a *api) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, 4<<10, &body) {
		return
	}
	ip := a.clientIP(r)
	value, err := a.Auth.Login(ip, body.Password)
	switch {
	case errors.Is(err, auth.ErrLocked):
		a.Bus.Log("warn", 0, "登录被锁定(来源 %s):失败次数过多", ip)
		w.Header().Set("Retry-After", "300")
		writeErr(w, http.StatusTooManyRequests, "失败次数过多,请 5 分钟后再试")
	case errors.Is(err, auth.ErrBadPassword):
		a.Bus.Log("warn", 0, "登录失败(来源 %s)", ip)
		writeErr(w, http.StatusUnauthorized, "口令错误")
	case err != nil:
		a.fail(w, err)
	default:
		a.Auth.SetCookie(w, r, value)
		a.Bus.Log("info", 0, "登录成功(来源 %s)", ip)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func (a *api) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.CookieName); err == nil {
		a.Auth.Revoke(c.Value)
	}
	a.Auth.ClearCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *api) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": a.Auth.Authenticated(r)})
}

// ---- jobs ----

func (a *api) stats(w http.ResponseWriter, r *http.Request) {
	st, err := a.Store.Stats(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *api) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := a.Store.ListJobs(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": jobs})
}

func (a *api) createJob(w http.ResponseWriter, r *http.Request) {
	var p scheduler.Params
	if !decodeJSON(w, r, maxJSONBody, &p) {
		return
	}
	job, err := a.Sched.Create(r.Context(), p)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

func (a *api) getJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	job, err := a.Store.GetJob(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (a *api) jobAction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var err error
	switch r.PathValue("action") {
	case "pause":
		err = a.Sched.Pause(id)
	case "resume":
		err = a.Sched.Resume(id)
	case "cancel":
		err = a.Sched.Cancel(id)
	default:
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	job, err := a.Store.GetJob(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (a *api) deleteJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := a.Store.GetJob(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	if err := a.Sched.Delete(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- results ----

func resultFilter(r *http.Request) store.ResultFilter {
	return store.ResultFilter{
		JobID: int64Query(r, "job_id"), Status: r.URL.Query().Get("status"), Q: r.URL.Query().Get("q"),
		Limit: intQuery(r, "limit"), Offset: intQuery(r, "offset"),
	}
}

func (a *api) listResults(w http.ResponseWriter, r *http.Request) {
	items, total, err := a.Store.ListResults(r.Context(), resultFilter(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// csvSafe neutralises spreadsheet formula injection.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (a *api) exportResults(w http.ResponseWriter, r *http.Request) {
	f := resultFilter(r)
	f.Limit, f.Offset = 1000, 0
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="domains.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"domain", "status", "job_id", "signatures", "found_at"})
	for {
		items, total, err := a.Store.ListResults(r.Context(), f)
		if err != nil {
			a.Bus.Log("error", 0, "导出 CSV 失败:%v", err)
			break
		}
		for _, it := range items {
			_ = cw.Write([]string{csvSafe(it.Domain), it.Status, strconv.FormatInt(it.JobID, 10),
				csvSafe(it.Signatures), it.CreatedAt.UTC().Format(time.RFC3339)})
		}
		f.Offset += len(items)
		if len(items) == 0 || int64(f.Offset) >= total {
			break
		}
	}
	cw.Flush()
}

// ---- wordlists ----

func (a *api) listWordlists(w http.ResponseWriter, r *http.Request) {
	items, err := a.Words.List()
	if err != nil {
		a.fail(w, err)
		return
	}
	if items == nil {
		items = []wordlistsInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *api) uploadWordlist(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, a.MaxWordlistBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "wordlist too large")
		} else {
			writeErr(w, http.StatusBadRequest, "invalid multipart form")
		}
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field")
		return
	}
	defer file.Close()
	name := r.FormValue("name")
	if name == "" {
		name = hdr.Filename
	}
	info, err := a.Words.Save(name, file, a.MaxWordlistBytes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.Bus.Log("info", 0, "上传词库 %s(%d 个词)", info.ID, info.Count)
	writeJSON(w, http.StatusCreated, info)
}

// ---- logs ----

func logFilter(r *http.Request) store.LogFilter {
	q := r.URL.Query()
	return store.LogFilter{
		Level: q.Get("level"), JobID: int64Query(r, "job_id"), Component: q.Get("component"),
		Event: q.Get("event"), Domain: strings.ToLower(q.Get("domain")), Egress: q.Get("egress"),
		Q: q.Get("q"), Limit: intQuery(r, "limit"), BeforeID: int64Query(r, "before_id"),
	}
}

// matchLog applies a LogFilter to a live entry (the SSE stream is filtered server side).
func matchLog(f store.LogFilter, e store.LogEntry) bool {
	if f.Level != "" && store.LevelRank(e.Level) < store.LevelRank(f.Level) {
		return false
	}
	if f.JobID != 0 && e.JobID != f.JobID {
		return false
	}
	if (f.Component != "" && e.Component != f.Component) || (f.Event != "" && e.Event != f.Event) ||
		(f.Domain != "" && e.Domain != f.Domain) || (f.Egress != "" && e.Egress != f.Egress) {
		return false
	}
	if f.Q != "" {
		q := strings.ToLower(f.Q)
		if !strings.Contains(strings.ToLower(e.Message), q) && !strings.Contains(strings.ToLower(e.Domain), q) &&
			!strings.Contains(strings.ToLower(e.Event), q) {
			return false
		}
	}
	return true
}

func (a *api) exportLogs(w http.ResponseWriter, r *http.Request) {
	f := logFilter(r)
	f.Limit, f.BeforeID = 0, 0
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="domain-scanner-logs.jsonl"`)
	enc := json.NewEncoder(w)
	err := a.Store.EachLog(r.Context(), f, func(e store.LogEntry) error { return enc.Encode(e) })
	if err != nil {
		a.Bus.Log("error", 0, "导出日志失败:%v", err)
	}
}

func (a *api) logComponents(w http.ResponseWriter, r *http.Request) {
	items, err := a.Store.LogComponents(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *api) listLogs(w http.ResponseWriter, r *http.Request) {
	items, err := a.Store.ListLogs(r.Context(), logFilter(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 { // store returns newest first
		items[i], items[j] = items[j], items[i]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *api) streamLogs(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	if err := rc.Flush(); err != nil {
		return
	}

	filter := logFilter(r)
	ch, cancel := a.Bus.Subscribe()
	defer cancel()
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			fmt.Fprint(w, ": ping\n\n")
		case e, ok := <-ch:
			if !ok {
				return
			}
			if !matchLog(filter, e) {
				continue
			}
			data, _ := jsonMarshal(e)
			fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", e.ID, data)
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}
