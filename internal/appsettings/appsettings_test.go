package appsettings

import (
	"context"
	"path/filepath"
	"testing"

	"domain_scanner/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCloudflareFallsBackToEnvironmentAndSettingsWin(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	get := CloudflareFunc(st, Cloudflare{AccountID: "envacct", Token: "envtoken"})
	if c := get(); c.AccountID != "envacct" || c.Token != "envtoken" || !c.Configured() {
		t.Fatalf("env fallback = %+v", c)
	}
	st.SetSetting(ctx, KeyCFAccountID, "savedacct")
	st.SetSetting(ctx, KeyCFToken, "savedtoken")
	if c := get(); c.AccountID != "savedacct" || c.Token != "savedtoken" {
		t.Fatalf("saved settings must win: %+v", c)
	}
	empty := CloudflareFunc(openStore(t), Cloudflare{})
	if empty().Configured() {
		t.Fatal("nothing configured must not report Configured")
	}
}

func TestRegisterPolicyDefaultsAndOverrides(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	get := RegisterPolicyFunc(st)
	p := get()
	if !p.Confirm || p.MaxPrice != 30 || p.DailyCap != 5 || !p.PushUnconfirmed {
		t.Fatalf("defaults = %+v (want confirm on, 30 USD, 5/day, push unconfirmed)", p)
	}
	st.SetSetting(ctx, KeyRegisterConfirm, "false")
	st.SetSetting(ctx, KeyRegisterMaxPrice, "12.5")
	st.SetSetting(ctx, KeyRegisterDailyCap, "0")
	st.SetSetting(ctx, KeyPushUnconfirmed, "false")
	p = get()
	if p.Confirm || p.MaxPrice != 12.5 || p.DailyCap != 0 || p.PushUnconfirmed {
		t.Fatalf("overrides = %+v", p)
	}
	st.SetSetting(ctx, KeyRegisterMaxPrice, "garbage")
	if p = get(); p.MaxPrice != 30 {
		t.Fatalf("an unparsable price must fall back to the default, got %v", p.MaxPrice)
	}
}

func TestLogLevelAndProxyTestURL(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if LogLevel(st) != "debug" || ProxyTestURL(st) != DefaultProxyTestURL {
		t.Fatalf("defaults: %q %q", LogLevel(st), ProxyTestURL(st))
	}
	st.SetSetting(ctx, KeyLogLevel, "warn")
	st.SetSetting(ctx, KeyProxyTestURL, "https://example.com/x")
	if LogLevel(st) != "warn" || ProxyTestURL(st) != "https://example.com/x" {
		t.Fatalf("overrides: %q %q", LogLevel(st), ProxyTestURL(st))
	}
}
