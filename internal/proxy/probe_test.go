package proxy

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestProbeThroughAWorkingProxy(t *testing.T) {
	site := testSite(t, 204, 0)
	proxy := startSocks(t, "127.0.0.1:0")
	r := Probe(context.Background(), proxy.addr(), ProbeOptions{TestURL: site.URL + "/gen204", TraceURL: site.URL + "/trace", Timeout: 3 * time.Second})
	if !r.OK || r.Status != 204 || r.Error != "" {
		t.Fatalf("result = %+v", r)
	}
	if r.IP != "203.0.113.7" || r.Country != "HK" {
		t.Fatalf("trace not parsed: ip=%q country=%q", r.IP, r.Country)
	}
	if r.DelayMS < 0 || r.ColdMS < 0 {
		t.Fatalf("timings: %+v", r)
	}
	if proxy.conns.Load() == 0 {
		t.Fatal("the probe did not go through the proxy")
	}
}

func TestProbeReportsAnUnreachableProxy(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	r := Probe(context.Background(), addr, ProbeOptions{TestURL: "http://example.invalid/", Timeout: time.Second})
	if r.OK || r.Error == "" {
		t.Fatalf("result = %+v, want a failure with a reason", r)
	}
}

func TestProbeTreatsServerErrorsAsFailure(t *testing.T) {
	site := testSite(t, 503, 0)
	proxy := startSocks(t, "127.0.0.1:0")
	r := Probe(context.Background(), proxy.addr(), ProbeOptions{TestURL: site.URL + "/x", Timeout: 2 * time.Second})
	if r.OK || r.Status != 503 || !strings.Contains(r.Error, "503") {
		t.Fatalf("result = %+v", r)
	}
}

func TestProbeTimesOut(t *testing.T) {
	site := testSite(t, 204, 2*time.Second)
	proxy := startSocks(t, "127.0.0.1:0")
	start := time.Now()
	r := Probe(context.Background(), proxy.addr(), ProbeOptions{TestURL: site.URL + "/slow", Timeout: 300 * time.Millisecond})
	if r.OK || r.Error == "" {
		t.Fatalf("result = %+v, want a timeout failure", r)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("probe took %v despite a 300ms timeout", time.Since(start))
	}
}

func TestProbeHonoursContext(t *testing.T) {
	site := testSite(t, 204, time.Second)
	proxy := startSocks(t, "127.0.0.1:0")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := Probe(ctx, proxy.addr(), ProbeOptions{TestURL: site.URL + "/x", Timeout: 5 * time.Second})
	if r.OK || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("result = %+v after %v", r, time.Since(start))
	}
}

func TestProbeTraceFailureDoesNotFailTheProbe(t *testing.T) {
	site := testSite(t, 204, 0)
	proxy := startSocks(t, "127.0.0.1:0")
	r := Probe(context.Background(), proxy.addr(), ProbeOptions{TestURL: site.URL + "/x", TraceURL: "http://127.0.0.1:1/none", Timeout: time.Second})
	if !r.OK || r.IP != "" {
		t.Fatalf("result = %+v; a missing trace only means no IP is shown", r)
	}
}
