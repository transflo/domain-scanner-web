package main

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestAdminPasswordIsMandatory(t *testing.T) {
	for _, pw := range []string{"", "1234567", "short"} {
		_, err := LoadConfig(env(map[string]string{"ADMIN_PASSWORD": pw}))
		if err == nil || !strings.Contains(err.Error(), "ADMIN_PASSWORD") {
			t.Errorf("password %q: err = %v, want an error naming ADMIN_PASSWORD", pw, err)
		}
	}
	if _, err := LoadConfig(env(map[string]string{"ADMIN_PASSWORD": "12345678"})); err != nil {
		t.Fatalf("8 characters must be accepted: %v", err)
	}
}

func TestDefaults(t *testing.T) {
	c, err := LoadConfig(env(map[string]string{"ADMIN_PASSWORD": "long-enough-pw"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || c.DataDir != "/data" || c.WordlistDir != "/app/wordlists" ||
		c.MaxParallelJobs != 0 || c.TrustProxy {
		t.Fatalf("defaults = %+v", c)
	}
}

func TestOverrides(t *testing.T) {
	c, err := LoadConfig(env(map[string]string{
		"ADMIN_PASSWORD": "long-enough-pw", "LISTEN_ADDR": ":9000", "DATA_DIR": "/tmp/x", "WORDLIST_DIR": "/w",
		"MAX_PARALLEL_JOBS": "4", "TRUST_PROXY": "true", "TELEGRAM_BOT_TOKEN": "tok", "TELEGRAM_CHAT_ID": "7",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":9000" || c.DataDir != "/tmp/x" || c.WordlistDir != "/w" || c.MaxParallelJobs != 4 ||
		!c.TrustProxy || c.TelegramToken != "tok" || c.TelegramChatID != "7" {
		t.Fatalf("config = %+v", c)
	}
}

func TestInvalidNumbersAreErrors(t *testing.T) {
	for _, bad := range []string{"abc", "-3", "1000"} {
		_, err := LoadConfig(env(map[string]string{"ADMIN_PASSWORD": "long-enough-pw", "MAX_PARALLEL_JOBS": bad}))
		if err == nil || !strings.Contains(err.Error(), "MAX_PARALLEL_JOBS") {
			t.Errorf("MAX_PARALLEL_JOBS=%q: err = %v", bad, err)
		}
	}
	_, err := LoadConfig(env(map[string]string{"ADMIN_PASSWORD": "long-enough-pw", "TRUST_PROXY": "maybe"}))
	if err == nil || !strings.Contains(err.Error(), "TRUST_PROXY") {
		t.Errorf("TRUST_PROXY=maybe: err = %v", err)
	}
}

func TestRDAPServersParsing(t *testing.T) {
	base := map[string]string{"ADMIN_PASSWORD": "long-enough-pw"}
	with := func(v string) map[string]string {
		m := map[string]string{"RDAP_SERVERS": v}
		for k, x := range base {
			m[k] = x
		}
		return m
	}
	c, err := LoadConfig(env(with("de=https://rdap.example.de , .LI=http://x.test/rdap")))
	if err != nil {
		t.Fatal(err)
	}
	if c.RDAPServers["de"] != "https://rdap.example.de/" || c.RDAPServers["li"] != "http://x.test/rdap/" {
		t.Fatalf("parsed = %v (want lower-cased tld keys and trailing slashes)", c.RDAPServers)
	}
	for _, bad := range []string{"de", "de=", "=https://x/", "de=ftp://x/", "de=not a url"} {
		if _, err := LoadConfig(env(with(bad))); err == nil || !strings.Contains(err.Error(), "RDAP_SERVERS") {
			t.Errorf("RDAP_SERVERS=%q: err = %v, want an error naming RDAP_SERVERS", bad, err)
		}
	}
	if c, _ := LoadConfig(env(base)); len(c.RDAPServers) != 0 {
		t.Fatalf("unset RDAP_SERVERS should give no overrides, got %v", c.RDAPServers)
	}
}

func TestErrorNeverEchoesThePassword(t *testing.T) {
	_, err := LoadConfig(env(map[string]string{"ADMIN_PASSWORD": "abc"}))
	if err == nil || strings.Contains(err.Error(), "abc") {
		t.Fatalf("error must not echo the password: %v", err)
	}
}

func TestStorageDefaultsKeepTheDiskBounded(t *testing.T) {
	c, err := LoadConfig(env(map[string]string{"ADMIN_PASSWORD": "long-enough-pw"}))
	if err != nil {
		t.Fatal(err)
	}
	p := c.Storage
	if p.DebugDays != 3 || p.InfoDays != 14 || p.WarnDays != 90 || p.MaxDebugRows != 500_000 || p.MaxOtherRows != 500_000 ||
		p.MaxDBBytes != 1024<<20 || p.UnknownResultDays != 30 || p.MinFreeBytes != 1024<<20 {
		t.Fatalf("storage defaults = %+v", p)
	}
}

func TestStorageOverridesAndValidation(t *testing.T) {
	c, err := LoadConfig(env(map[string]string{"ADMIN_PASSWORD": "long-enough-pw", "LOG_RETENTION_DEBUG_DAYS": "1",
		"DB_MAX_MB": "256", "MIN_FREE_MB": "0", "UNKNOWN_RESULT_DAYS": "0"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Storage.DebugDays != 1 || c.Storage.MaxDBBytes != 256<<20 || c.Storage.MinFreeBytes != 0 || c.Storage.UnknownResultDays != 0 {
		t.Fatalf("overrides = %+v", c.Storage)
	}
	for _, key := range []string{"LOG_RETENTION_DEBUG_DAYS", "LOG_RETENTION_INFO_DAYS", "LOG_RETENTION_WARN_DAYS", "DB_MAX_MB", "MIN_FREE_MB", "UNKNOWN_RESULT_DAYS", "LOG_MAX_DEBUG_ROWS", "LOG_MAX_OTHER_ROWS"} {
		for _, bad := range []string{"abc", "-1"} {
			_, err := LoadConfig(env(map[string]string{"ADMIN_PASSWORD": "long-enough-pw", key: bad}))
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("%s=%q: err = %v", key, bad, err)
			}
		}
	}
}
