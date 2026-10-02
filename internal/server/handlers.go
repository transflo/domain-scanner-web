package server

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		a.Bus.Logger("auth").Warn("locked", 0, fmt.Sprintf("登录被锁定(来源 %s):失败次数过多", ip), map[string]any{"ip": ip})
		w.Header().Set("Retry-After", "300")
		writeErr(w, http.StatusTooManyRequests, "失败次数过多,请 5 分钟后再试")
	case errors.Is(err, auth.ErrBadPassword):
		a.Bus.Logger("auth").Warn("login_failed", 0, fmt.Sprintf("登录失败(来源 %s)", ip), map[string]any{"ip": ip})
		writeErr(w, http.StatusUnauthorized, "口令错误")
	case err != nil:
		a.fail(w, err)
	default:
		a.Auth.SetCookie(w, r, value)
		a.Bus.Logger("auth").Info("login_ok", 0, fmt.Sprintf("登录成功(来源 %s)", ip), map[string]any{"ip": ip})
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
		CFStatus: r.URL.Query().Get("cf_status"),
		Limit:    intQuery(r, "limit"), Offset: intQuery(r, "offset"),
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
	_ = cw.Write([]string{"domain", "status", "job_id", "signatures", "found_at", "cloudflare", "price", "currency", "registration"})
	for {
		items, total, err := a.Store.ListResults(r.Context(), f)
		if err != nil {
			a.Bus.Logger("http").Error("export_failed", 0, fmt.Sprintf("导出 CSV 失败:%v", err), nil)
			break
		}
		for _, it := range items {
			cf := it.CFStatus
			if it.CFReason != "" {
				cf += ":" + it.CFReason
			}
			_ = cw.Write([]string{csvSafe(it.Domain), it.Status, strconv.FormatInt(it.JobID, 10),
				csvSafe(it.Signatures), it.CreatedAt.UTC().Format(time.RFC3339), csvSafe(cf), it.CFPrice, it.CFCurrency,
				csvSafe(it.RegisterStatus)})
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
	a.Bus.Logger("wordlist").Info("uploaded", 0, fmt.Sprintf("上传词库 %s(%d 个词)", info.ID, info.Count), map[string]any{"id": info.ID, "words": info.Count})
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

// exportLogs streams every stored log row matching the filter (all of them when none is given),
// oldest first, as JSON lines or as plain text. It reads the database, not the browser's buffer.
func (a *api) exportLogs(w http.ResponseWriter, r *http.Request) {
	f := logFilter(r)
	f.Limit, f.BeforeID = 0, 0
	a.Bus.Flush() // lines still waiting in memory belong in the export too
	stamp := time.Now().UTC().Format("2006-01-02-15-04-05")
	text := r.URL.Query().Get("format") == "text"
	var write func(store.LogEntry) error
	if text {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="domain-scanner-`+stamp+`.log"`)
		write = func(e store.LogEntry) error { _, err := io.WriteString(w, formatLogLine(e)+"\n"); return err }
	} else {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="domain-scanner-`+stamp+`.jsonl"`)
		enc := json.NewEncoder(w)
		write = func(e store.LogEntry) error { return enc.Encode(e) }
	}
	if err := a.Store.EachLog(r.Context(), f, write); err != nil {
		a.Bus.Logger("http").Error("export_failed", 0, fmt.Sprintf("导出日志失败:%v", err), nil)
	}
}

// formatLogLine renders one entry as a single readable line.
func formatLogLine(e store.LogEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-5s", e.Time.UTC().Format("2006-01-02T15:04:05.000Z"), strings.ToUpper(e.Level))
	if e.JobID > 0 {
		fmt.Fprintf(&b, " [job %d]", e.JobID)
	}
	if e.Component != "" || e.Event != "" {
		fmt.Fprintf(&b, " %s/%s", e.Component, e.Event)
	}
	b.WriteString(" ")
	b.WriteString(strings.ReplaceAll(e.Message, "\n", " ⏎ "))
	var extra []string
	if e.Domain != "" {
		extra = append(extra, "domain="+e.Domain)
	}
	if e.Egress != "" {
		extra = append(extra, "egress="+e.Egress)
	}
	if e.DurationMS > 0 {
		extra = append(extra, fmt.Sprintf("duration_ms=%d", e.DurationMS))
	}
	if len(e.Fields) > 0 {
		if j, err := json.Marshal(e.Fields); err == nil {
			extra = append(extra, "fields="+string(j))
		}
	}
	if len(extra) > 0 {
		b.WriteString(" | " + strings.Join(extra, " "))
	}
	return b.String()
}

func (a *api) storage(w http.ResponseWriter, r *http.Request) {
	if a.Keeper == nil {
		writeErr(w, http.StatusServiceUnavailable, "存储管理未启用")
		return
	}
	writeJSON(w, http.StatusOK, a.Keeper.Last())
}

func (a *api) storageCleanup(w http.ResponseWriter, r *http.Request) {
	if a.Keeper == nil {
		writeErr(w, http.StatusServiceUnavailable, "存储管理未启用")
		return
	}
	writeJSON(w, http.StatusOK, a.Keeper.Run(r.Context()))
}

func (a *api) diagnostics(w http.ResponseWriter, r *http.Request) {
	hours := intQuery(r, "hours")
	if hours < 1 || hours > 720 {
		hours = 24
	}
	d, err := a.Store.Diagnostics(r.Context(), time.Now().Add(-time.Duration(hours)*time.Hour))
	if err != nil {
		a.fail(w, err)
		return
	}
	count, oldest, newest, err := a.Store.LogRange(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hours":       hours,
		"diagnostics": d,
		"logs":        map[string]any{"count": count, "oldest": oldest, "newest": newest},
		"egresses":    a.Proxy.Status().Egresses,
	})
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
