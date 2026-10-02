package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"domain_scanner/internal/logbus"
	"domain_scanner/internal/register"
)

const chat = "42"

// tgAPI is a fake Telegram Bot API covering the methods the notifier uses.
type tgAPI struct {
	srv *httptest.Server
	mu  sync.Mutex

	sent    []map[string]any // sendMessage payloads
	edits   []map[string]any // editMessageText payloads
	answers []map[string]any // answerCallbackQuery payloads
	offsets []int64          // offset of every getUpdates call

	updates []map[string]any // queued updates, delivered once
}

func newTGAPI(t *testing.T) *tgAPI {
	a := &tgAPI{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bot"+testToken+"/") {
			http.NotFound(w, r)
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/bot"+testToken+"/")
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		json.Unmarshal(body, &p)
		a.mu.Lock()
		defer a.mu.Unlock()
		switch method {
		case "sendMessage":
			a.sent = append(a.sent, p)
			fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, 100+len(a.sent))
		case "editMessageText":
			a.edits = append(a.edits, p)
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		case "answerCallbackQuery":
			a.answers = append(a.answers, p)
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		case "getUpdates":
			off, _ := p["offset"].(float64)
			a.offsets = append(a.offsets, int64(off))
			out := []map[string]any{}
			for _, u := range a.updates {
				if int64(u["update_id"].(int)) >= int64(off) {
					out = append(out, u)
				}
			}
			a.updates = nil
			b, _ := json.Marshal(map[string]any{"ok": true, "result": out})
			if len(out) == 0 {
				a.mu.Unlock()
				time.Sleep(20 * time.Millisecond) // behave like a short long-poll
				a.mu.Lock()
			}
			w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *tgAPI) queueCallback(updateID int, data string, chatID, fromID int64, msgDate time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updates = append(a.updates, map[string]any{
		"update_id": updateID,
		"callback_query": map[string]any{
			"id": fmt.Sprintf("cb%d", updateID), "data": data, "from": map[string]any{"id": fromID},
			"message": map[string]any{"message_id": 77, "date": msgDate.Unix(), "chat": map[string]any{"id": chatID}},
		},
	})
}

func (a *tgAPI) counts() (sent, edits, answers int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sent), len(a.edits), len(a.answers)
}

func (a *tgAPI) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type fakeReg struct {
	mu        sync.Mutex
	previews  []int64
	registers []int64
	preview   register.Preview
	previewEr error
	outcome   register.Outcome
	regErr    error
	delay     time.Duration
}

func (f *fakeReg) Preview(_ context.Context, id int64) (register.Preview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.previews = append(f.previews, id)
	p := f.preview
	p.ResultID = id
	return p, f.previewEr
}

func (f *fakeReg) Register(_ context.Context, id int64) (register.Outcome, error) {
	f.mu.Lock()
	f.registers = append(f.registers, id)
	out, err, d := f.outcome, f.regErr, f.delay
	f.mu.Unlock()
	time.Sleep(d)
	return out, err
}

func (f *fakeReg) counts() (previews, registers int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.previews), len(f.registers)
}

type btnEnv struct {
	n     *Notifier
	api   *tgAPI
	reg   *fakeReg
	saved atomic.Int64
	logs  *logs
}

func newBtnEnv(t *testing.T, cfg Config, reg *fakeReg) *btnEnv {
	t.Helper()
	api := newTGAPI(t)
	l := &logs{}
	bus := logbus.New(l.sink, 100)
	e := &btnEnv{api: api, reg: reg, logs: l}
	n := New(func() Config { return cfg }, bus, Options{BaseURL: api.srv.URL, FlushEvery: time.Hour, MaxBatch: 100,
		Backoff: time.Millisecond, PollTimeout: time.Second,
		LoadOffset: func() int64 { return e.saved.Load() }, SaveOffset: func(v int64) { e.saved.Store(v) }})
	e.n = n
	n.Start(context.Background())
	t.Cleanup(func() { n.Stop(); bus.Close() })
	return e
}

func (e *btnEnv) startCallbacks(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.n.StartCallbacks(ctx, e.reg)
}

func keyboardOf(t *testing.T, msg map[string]any) [][]map[string]any {
	t.Helper()
	rm, ok := msg["reply_markup"].(map[string]any)
	if !ok {
		return nil
	}
	var out [][]map[string]any
	for _, row := range rm["inline_keyboard"].([]any) {
		var r []map[string]any
		for _, b := range row.([]any) {
			r = append(r, b.(map[string]any))
		}
		out = append(out, r)
	}
	return out
}

func TestConfirmedItemsGetPriceAndRegisterButtons(t *testing.T) {
	e := newBtnEnv(t, Config{testToken, chat}, &fakeReg{})
	e.n.NotifyItems("job", []Item{
		{ResultID: 7, Domain: "free.com", Status: StatusConfirmed, Price: "10.46", Currency: "USD"},
		{ResultID: 8, Domain: "x.li", Status: StatusUnconfirmed, Note: "Cloudflare 不支持该后缀,无法确认"},
	})
	e.n.Stop() // flush
	e.api.waitFor(t, "a message", func() bool { s, _, _ := e.api.counts(); return s == 1 })
	msg := e.api.sent[0]
	text := msg["text"].(string)
	for _, want := range []string{"free.com", "10.46", "USD", "已确认", "x.li", "未经 Cloudflare 确认", "不支持该后缀"} {
		if !strings.Contains(text, want) {
			t.Errorf("message lacks %q:\n%s", want, text)
		}
	}
	kb := keyboardOf(t, msg)
	if len(kb) != 1 || len(kb[0]) != 1 || kb[0][0]["callback_data"] != "reg:7" || !strings.Contains(kb[0][0]["text"].(string), "free.com") {
		t.Fatalf("keyboard = %v; exactly one button, for the confirmed domain only", kb)
	}
}

func TestMoreThanEightButtonsSplitIntoSeveralMessages(t *testing.T) {
	e := newBtnEnv(t, Config{testToken, chat}, &fakeReg{})
	var items []Item
	for i := 1; i <= 20; i++ {
		items = append(items, Item{ResultID: int64(i), Domain: fmt.Sprintf("d%02d.com", i), Status: StatusConfirmed, Price: "9", Currency: "USD"})
	}
	e.n.NotifyItems("job", items)
	e.n.Stop()
	e.api.waitFor(t, "3 messages", func() bool { s, _, _ := e.api.counts(); return s == 3 })
	total := 0
	for _, m := range e.api.sent {
		kb := keyboardOf(t, m)
		if len(kb) > 8 {
			t.Fatalf("a message carries %d buttons, want at most 8", len(kb))
		}
		total += len(kb)
	}
	if total != 20 {
		t.Fatalf("buttons in total = %d, want 20", total)
	}
}

func TestNoButtonsForGroupsChannelsOrWithoutResultID(t *testing.T) {
	for _, chatID := range []string{"-100123456", "@mychannel"} {
		e := newBtnEnv(t, Config{testToken, chatID}, &fakeReg{})
		e.n.NotifyItems("job", []Item{{ResultID: 7, Domain: "free.com", Status: StatusConfirmed, Price: "10", Currency: "USD"}})
		e.n.Stop()
		e.api.waitFor(t, "message", func() bool { s, _, _ := e.api.counts(); return s == 1 })
		if kb := keyboardOf(t, e.api.sent[0]); len(kb) != 0 {
			t.Errorf("chat %s: a registration button in a shared chat would let anyone there spend money: %v", chatID, kb)
		}
		if !strings.Contains(e.api.sent[0]["text"].(string), "free.com") {
			t.Errorf("chat %s: the domain must still be announced", chatID)
		}
	}
	e := newBtnEnv(t, Config{testToken, chat}, &fakeReg{})
	e.n.NotifyItems("job", []Item{{Domain: "legacy.com", Status: StatusConfirmed, Price: "10"}}) // no ResultID
	e.n.Stop()
	e.api.waitFor(t, "message", func() bool { s, _, _ := e.api.counts(); return s == 1 })
	if kb := keyboardOf(t, e.api.sent[0]); len(kb) != 0 {
		t.Errorf("an item without a result id cannot be registered: %v", kb)
	}
}

func TestRegisterButtonAsksForConfirmation(t *testing.T) {
	reg := &fakeReg{preview: register.Preview{Domain: "free.com", Price: "10.46", Currency: "USD", Confirm: true}}
	e := newBtnEnv(t, Config{testToken, chat}, reg)
	e.api.queueCallback(1, "reg:7", 42, 42, time.Now())
	e.startCallbacks(t)
	e.api.waitFor(t, "the confirmation message", func() bool { s, _, a := e.api.counts(); return s == 1 && a >= 1 })

	msg := e.api.sent[0]
	if !strings.Contains(msg["text"].(string), "free.com") || !strings.Contains(msg["text"].(string), "10.46") || !strings.Contains(msg["text"].(string), "不可退款") {
		t.Fatalf("confirmation text = %q", msg["text"])
	}
	kb := keyboardOf(t, msg)
	if len(kb) != 1 || len(kb[0]) != 2 || kb[0][0]["callback_data"] != "ok:7" || kb[0][1]["callback_data"] != "no:7" {
		t.Fatalf("confirmation keyboard = %v", kb)
	}
	if _, regs := reg.counts(); regs != 0 {
		t.Fatal("tapping Register must not charge anything yet")
	}
}

func TestConfirmingRegistersOnceAndReportsTheResult(t *testing.T) {
	reg := &fakeReg{outcome: register.Outcome{Status: "succeeded", Message: "注册成功", Domain: "free.com"}}
	e := newBtnEnv(t, Config{testToken, chat}, reg)
	e.api.queueCallback(1, "ok:7", 42, 42, time.Now())
	e.startCallbacks(t)
	e.api.waitFor(t, "the result edit", func() bool { _, ed, _ := e.api.counts(); return ed >= 2 })
	if _, regs := reg.counts(); regs != 1 {
		t.Fatalf("registrations = %d, want exactly 1", regs)
	}
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	first, last := e.api.edits[0]["text"].(string), e.api.edits[len(e.api.edits)-1]["text"].(string)
	if !strings.Contains(first, "正在注册") || !strings.Contains(last, "注册成功") || !strings.Contains(last, "free.com") {
		t.Fatalf("edits: %q then %q", first, last)
	}
	if kb := keyboardOf(t, e.api.edits[0]); len(kb) != 0 {
		t.Fatalf("buttons must disappear once registration starts: %v", kb)
	}
}

func TestFailedAndPendingOutcomesAreExplained(t *testing.T) {
	for _, tc := range []struct {
		out  register.Outcome
		want string
	}{
		{register.Outcome{Status: "failed", Message: "no default payment method", Domain: "free.com"}, "no default payment method"},
		{register.Outcome{Status: "pending", Message: "Cloudflare 仍在处理", Domain: "free.com"}, "仍在处理"},
	} {
		e := newBtnEnv(t, Config{testToken, chat}, &fakeReg{outcome: tc.out})
		e.api.queueCallback(1, "ok:7", 42, 42, time.Now())
		e.startCallbacks(t)
		e.api.waitFor(t, "final edit", func() bool { _, ed, _ := e.api.counts(); return ed >= 2 })
		e.api.mu.Lock()
		last := e.api.edits[len(e.api.edits)-1]["text"].(string)
		e.api.mu.Unlock()
		if !strings.Contains(last, tc.want) {
			t.Errorf("final text %q lacks %q", last, tc.want)
		}
	}
}

func TestCancelDoesNothingButSaysSo(t *testing.T) {
	reg := &fakeReg{}
	e := newBtnEnv(t, Config{testToken, chat}, reg)
	e.api.queueCallback(1, "no:7", 42, 42, time.Now())
	e.startCallbacks(t)
	e.api.waitFor(t, "edit", func() bool { _, ed, _ := e.api.counts(); return ed == 1 })
	if !strings.Contains(e.api.edits[0]["text"].(string), "已取消") {
		t.Fatalf("text = %q", e.api.edits[0]["text"])
	}
	if p, r := reg.counts(); p != 0 || r != 0 {
		t.Fatalf("cancel touched the registrar: %d %d", p, r)
	}
}

func TestOnlyTheConfiguredChatMayTapButtons(t *testing.T) {
	reg := &fakeReg{preview: register.Preview{Domain: "free.com", Confirm: true}}
	e := newBtnEnv(t, Config{testToken, chat}, reg)
	e.api.queueCallback(1, "ok:7", 999, 999, time.Now())  // someone else's chat
	e.api.queueCallback(2, "ok:7", 42, 999, time.Now())   // right chat, wrong person
	e.api.queueCallback(3, "reg:7", 999, 999, time.Now()) // someone else asking for a preview
	e.startCallbacks(t)
	e.api.waitFor(t, "three answers", func() bool { _, _, a := e.api.counts(); return a == 3 })
	if p, r := reg.counts(); p != 0 || r != 0 {
		t.Fatalf("an unauthorised tap reached the registrar: previews=%d registers=%d", p, r)
	}
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	for _, a := range e.api.answers {
		if !strings.Contains(a["text"].(string), "无权") {
			t.Errorf("answer = %v", a)
		}
	}
	if len(e.api.sent) != 0 || len(e.api.edits) != 0 {
		t.Fatal("nothing may be sent or edited for an unauthorised tap")
	}
}

func TestStaleConfirmationsAreIgnored(t *testing.T) {
	reg := &fakeReg{}
	e := newBtnEnv(t, Config{testToken, chat}, reg)
	e.api.queueCallback(1, "ok:7", 42, 42, time.Now().Add(-time.Hour)) // an hour-old confirmation message
	e.startCallbacks(t)
	e.api.waitFor(t, "answer", func() bool { _, _, a := e.api.counts(); return a == 1 })
	if _, r := reg.counts(); r != 0 {
		t.Fatal("a stale confirmation must not register anything")
	}
	if !strings.Contains(e.api.answers[0]["text"].(string), "过期") {
		t.Fatalf("answer = %v", e.api.answers[0])
	}
}

func TestBlockedPreviewIsShownToTheUser(t *testing.T) {
	reg := &fakeReg{previewEr: &register.BlockedError{Reason: "价格 40 USD 超过单价上限 30.00"}}
	e := newBtnEnv(t, Config{testToken, chat}, reg)
	e.api.queueCallback(1, "reg:7", 42, 42, time.Now())
	e.startCallbacks(t)
	e.api.waitFor(t, "answer", func() bool { _, _, a := e.api.counts(); return a == 1 })
	if !strings.Contains(e.api.answers[0]["text"].(string), "超过单价上限") || e.api.answers[0]["show_alert"] != true {
		t.Fatalf("answer = %v", e.api.answers[0])
	}
	if s, _, _ := e.api.counts(); s != 0 {
		t.Fatal("no confirmation prompt for a blocked registration")
	}
}

func TestWithoutConfirmationTheButtonRegistersDirectly(t *testing.T) {
	reg := &fakeReg{preview: register.Preview{Domain: "free.com", Price: "9", Currency: "USD", Confirm: false},
		outcome: register.Outcome{Status: "succeeded", Message: "注册成功", Domain: "free.com"}}
	e := newBtnEnv(t, Config{testToken, chat}, reg)
	e.api.queueCallback(1, "reg:7", 42, 42, time.Now())
	e.startCallbacks(t)
	e.api.waitFor(t, "registration", func() bool { _, r := reg.counts(); return r == 1 })
	e.api.waitFor(t, "result message", func() bool { s, _, _ := e.api.counts(); return s >= 1 })
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	if !strings.Contains(e.api.sent[len(e.api.sent)-1]["text"].(string), "注册成功") {
		t.Fatalf("sent = %v", e.api.sent)
	}
}

func TestRegistrarErrorsAreAnsweredNotSwallowed(t *testing.T) {
	reg := &fakeReg{regErr: register.ErrInProgress}
	e := newBtnEnv(t, Config{testToken, chat}, reg)
	e.api.queueCallback(1, "ok:7", 42, 42, time.Now())
	e.startCallbacks(t)
	e.api.waitFor(t, "edit with the error", func() bool { _, ed, _ := e.api.counts(); return ed >= 2 })
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	if last := e.api.edits[len(e.api.edits)-1]["text"].(string); !strings.Contains(last, "正在注册中") {
		t.Fatalf("final text = %q", last)
	}
}

func TestUpdateOffsetIsPersistedAndUsed(t *testing.T) {
	reg := &fakeReg{}
	e := newBtnEnv(t, Config{testToken, chat}, reg)
	e.saved.Store(50) // pretend an earlier run already handled everything up to 49
	e.api.queueCallback(60, "no:7", 42, 42, time.Now())
	e.startCallbacks(t)
	e.api.waitFor(t, "handled", func() bool { _, ed, _ := e.api.counts(); return ed == 1 })
	e.api.waitFor(t, "offset saved", func() bool { return e.saved.Load() == 61 })
	e.api.mu.Lock()
	first := e.api.offsets[0]
	e.api.mu.Unlock()
	if first != 50 {
		t.Fatalf("first getUpdates used offset %d, want the persisted 50", first)
	}
	e.api.waitFor(t, "next poll uses the new offset", func() bool {
		e.api.mu.Lock()
		defer e.api.mu.Unlock()
		return len(e.api.offsets) >= 2 && e.api.offsets[len(e.api.offsets)-1] == 61
	})
}

func TestNoPollingWhileUnconfigured(t *testing.T) {
	e := newBtnEnv(t, Config{}, &fakeReg{})
	e.startCallbacks(t)
	time.Sleep(150 * time.Millisecond)
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	if len(e.api.offsets) != 0 {
		t.Fatalf("polled Telegram %d times without credentials", len(e.api.offsets))
	}
}

func TestTokenNeverAppearsInCallbackLogs(t *testing.T) {
	reg := &fakeReg{previewEr: errors.New("boom " + testToken)}
	e := newBtnEnv(t, Config{testToken, chat}, reg)
	e.api.queueCallback(1, "reg:7", 42, 42, time.Now())
	e.startCallbacks(t)
	e.api.waitFor(t, "answer", func() bool { _, _, a := e.api.counts(); return a == 1 })
	e.n.Stop()
	time.Sleep(400 * time.Millisecond)
	if strings.Contains(e.logs.text(), "AAHdummy") {
		t.Fatalf("token leaked into logs:\n%s", e.logs.text())
	}
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	if strings.Contains(fmt.Sprint(e.api.answers), "AAHdummy") {
		t.Fatalf("token leaked into a Telegram answer: %v", e.api.answers)
	}
}
