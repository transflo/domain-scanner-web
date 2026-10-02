package proxy

import (
	"strings"
	"testing"
)

// Real output of `xray run -test` (v26.3.27) for rejected configs.
const (
	realBogusNetwork = "Xray 26.3.27 (Xray, Penetrates Everything.) d2758a0 (go1.26.1 linux/amd64)\n" +
		"A unified platform for anti-censorship.\n" +
		"2026/10/02 18:14:10.498873 [Info] infra/conf/serial: Reading config: &{Name:/tmp/b.json Format:json}\n" +
		"Failed to start: main: failed to load config files: [/tmp/b.json] > infra/conf: failed to build outbound config with tag o > infra/conf: failed to build stream settings for outbound detour > infra/conf: Config: unknown transport protocol: bogus\n"
	realReality = "2026/10/02 18:14:10.511433 [Info] infra/conf/serial: Reading config: &{Name:/tmp/b.json Format:json}\n" +
		"Failed to start: main: failed to load config files: [/tmp/b.json] > infra/conf: failed to build outbound config with tag o > infra/conf: failed to build stream settings for outbound detour > infra/conf: Failed to build REALITY config. > infra/conf: empty \"password\"\n"
	realFingerprint = "2026/10/02 18:14:10.519545 [Warning] common/errors: The feature WebSocket transport (with ALPN http/1.1, etc.) is deprecated, not recommended for using and might be removed. Please migrate to XHTTP H2 & H3 as soon as possible.\n" +
		"Failed to start: main: failed to load config files: [/tmp/b.json] > infra/conf: failed to build outbound config with tag o > infra/conf: failed to build stream settings for outbound detour > infra/conf: Failed to build TLS config. > infra/conf: unknown \"fingerprint\": nonsense-fp\n"
)

func TestSummarizeExtractsTheRootCauseOfXraysErrorChain(t *testing.T) {
	cases := []struct{ in, want string }{
		{realBogusNetwork, "unknown transport protocol: bogus"},
		{realReality, `empty "password"`},
		{realFingerprint, `unknown "fingerprint": nonsense-fp`},
	}
	for _, c := range cases {
		got := summarize(c.in)
		if !strings.Contains(got, c.want) {
			t.Errorf("summarize = %q, want it to contain %q", got, c.want)
		}
		for _, noise := range []string{"/tmp/b.json", "failed to load config files", "Reading config", "deprecated", "d2758a0"} {
			if strings.Contains(got, noise) {
				t.Errorf("summarize = %q still contains noise %q", got, noise)
			}
		}
	}
	if got := summarize("Failed to build REALITY config. > infra/conf: empty \"password\""); !strings.Contains(got, "REALITY") {
		t.Errorf("the context of the failing component should survive: %q", got)
	}
}

func TestSummarizeHandlesEmptyAndUnknownOutput(t *testing.T) {
	if got := summarize(""); got == "" {
		t.Fatal("an empty output must still produce a message")
	}
	if got := summarize("something odd happened"); !strings.Contains(got, "something odd") {
		t.Fatalf("unrecognised output should be passed through: %q", got)
	}
	long := strings.Repeat("x", 1000)
	if got := summarize(long); len(got) > 320 {
		t.Fatalf("output must be capped, got %d bytes", len(got))
	}
}
