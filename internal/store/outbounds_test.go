package store

import (
	"context"
	"encoding/json"
	"testing"
)

func sampleOutbound(name string) *Outbound {
	return &Outbound{Name: name, Protocol: "vless", Address: "a.example", Port: 443, Enabled: true,
		Config: json.RawMessage(`{"protocol":"vless","settings":{"address":"a.example","port":443,"id":"u"}}`)}
}

func TestOutboundCRUD(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	id, err := s.CreateOutbound(ctx, sampleOutbound("hk"))
	if err != nil || id == 0 {
		t.Fatalf("create: %d %v", id, err)
	}
	got, err := s.GetOutbound(ctx, id)
	if err != nil || got.Name != "hk" || got.Protocol != "vless" || got.Port != 443 || !got.Enabled ||
		string(got.Config) != string(sampleOutbound("").Config) || got.LastTestAt != nil {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	if _, err := s.GetOutbound(ctx, 999); err != ErrNotFound {
		t.Fatalf("missing: err = %v", err)
	}

	got.Name, got.Address, got.Port, got.Enabled = "renamed", "b.example", 8443, false
	got.Config = json.RawMessage(`{"protocol":"vless","settings":{"address":"b.example","port":8443,"id":"u"}}`)
	if err := s.UpdateOutbound(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, _ := s.GetOutbound(ctx, id)
	if again.Name != "renamed" || again.Address != "b.example" || again.Port != 8443 || again.Enabled {
		t.Fatalf("update lost data: %+v", again)
	}

	s.CreateOutbound(ctx, sampleOutbound("second"))
	list, _ := s.ListOutbounds(ctx)
	if len(list) != 2 || list[0].ID >= list[1].ID {
		t.Fatalf("list = %+v, want ascending ids", list)
	}
	if err := s.DeleteOutbound(ctx, id); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.ListOutbounds(ctx); len(list) != 1 {
		t.Fatalf("after delete: %d", len(list))
	}
	if err := s.DeleteOutbound(ctx, id); err != ErrNotFound {
		t.Fatalf("deleting twice: err = %v, want ErrNotFound", err)
	}
}

func TestOutboundTestResultsAreStoredAndResetByConfigChange(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	id, _ := s.CreateOutbound(ctx, sampleOutbound("n"))
	if err := s.SaveOutboundTest(ctx, id, true, 183, "", "203.0.113.9", "HK"); err != nil {
		t.Fatal(err)
	}
	o, _ := s.GetOutbound(ctx, id)
	if o.LastTestAt == nil || !o.LastOK || o.LastDelayMS != 183 || o.LastIP != "203.0.113.9" || o.LastCountry != "HK" {
		t.Fatalf("saved test = %+v", o)
	}
	if err := s.SaveOutboundTest(ctx, id, false, 0, "connection refused", "", ""); err != nil {
		t.Fatal(err)
	}
	o, _ = s.GetOutbound(ctx, id)
	if o.LastOK || o.LastError != "connection refused" || o.LastIP != "" {
		t.Fatalf("failed test = %+v", o)
	}

	s.SaveOutboundTest(ctx, id, true, 50, "", "1.1.1.1", "US")
	o, _ = s.GetOutbound(ctx, id)
	o.Config = json.RawMessage(`{"protocol":"vless","settings":{"address":"changed.example","port":443,"id":"u"}}`)
	if err := s.UpdateOutbound(ctx, o); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetOutbound(ctx, id)
	if after.LastTestAt != nil || after.LastOK {
		t.Fatalf("a changed config must clear the stale test result: %+v", after)
	}
}
