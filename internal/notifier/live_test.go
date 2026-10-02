package notifier

import (
	"os"
	"testing"
	"time"

	"domain_scanner/internal/logbus"
)

// TestLiveTelegramKeyboardIsAccepted sends one real message through the production path, with a
// register button, and checks Telegram accepted it. It is skipped unless LIVE_TELEGRAM_TOKEN and
// LIVE_TELEGRAM_CHAT are set. The button points at a result id that does not exist, so tapping it
// only produces a refusal: nothing can be registered or charged from this message.
func TestLiveTelegramKeyboardIsAccepted(t *testing.T) {
	token, chatID := os.Getenv("LIVE_TELEGRAM_TOKEN"), os.Getenv("LIVE_TELEGRAM_CHAT")
	if token == "" || chatID == "" {
		t.Skip("LIVE_TELEGRAM_TOKEN / LIVE_TELEGRAM_CHAT not set")
	}
	bus := logbus.New(nil, 200)
	t.Cleanup(bus.Close)
	bus.SetMinLevel("debug")
	n := New(func() Config { return Config{Token: token, ChatID: chatID} }, bus, Options{FlushEvery: time.Hour})
	n.Start(t.Context())

	n.NotifyItems("界面与按钮自检(可忽略)", []Item{
		{ResultID: 2147483000, Domain: "button-check-confirmed.example", Status: StatusConfirmed, Price: "10.46", Currency: "USD"},
		{ResultID: 2147483001, Domain: "button-check-unconfirmed.li", Status: StatusUnconfirmed, Note: "Cloudflare 不支持 .li"},
	})
	n.Stop() // flushes what is pending

	var sent, failed []string
	for _, e := range bus.Recent(200, "debug", 0) {
		if e.Component != "notifier" {
			continue
		}
		if e.Level == "error" || e.Level == "warn" {
			failed = append(failed, e.Message)
		} else if e.Event == "part_sent" {
			sent = append(sent, e.Message)
		}
	}
	if len(failed) > 0 {
		t.Fatalf("Telegram rejected the message: %v", failed)
	}
	if len(sent) == 0 {
		t.Fatalf("no success event was logged: %v", bus.Recent(200, "debug", 0))
	}
	t.Logf("accepted by Telegram: %v", sent)
}
