package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Outbound is a stored proxy: an Xray outbound object plus the result of its last liveness test.
type Outbound struct {
	ID       int64           `json:"id"`
	Name     string          `json:"name"`
	Protocol string          `json:"protocol"`
	Address  string          `json:"address"`
	Port     int             `json:"port"`
	Config   json.RawMessage `json:"config"`
	Enabled  bool            `json:"enabled"`

	LastTestAt  *time.Time `json:"last_test_at,omitempty"`
	LastOK      bool       `json:"last_ok"`
	LastDelayMS int64      `json:"last_delay_ms"`
	LastError   string     `json:"last_error"`
	LastIP      string     `json:"last_ip"`
	LastCountry string     `json:"last_country"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const outboundCols = `id,name,protocol,address,port,config,enabled,last_test_at,last_ok,last_delay_ms,last_error,last_ip,last_country,created_at,updated_at`

func scanOutbound(sc interface{ Scan(...any) error }) (*Outbound, error) {
	var o Outbound
	var cfg string
	var enabled, ok int
	var tested, created, updated int64
	err := sc.Scan(&o.ID, &o.Name, &o.Protocol, &o.Address, &o.Port, &cfg, &enabled, &tested, &ok, &o.LastDelayMS,
		&o.LastError, &o.LastIP, &o.LastCountry, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	o.Config = json.RawMessage(cfg)
	o.Enabled, o.LastOK = enabled != 0, ok != 0
	if tested > 0 {
		t := fromMS(tested)
		o.LastTestAt = &t
	}
	o.CreatedAt, o.UpdatedAt = fromMS(created), fromMS(updated)
	return &o, nil
}

func (s *Store) CreateOutbound(ctx context.Context, o *Outbound) (int64, error) {
	now := ms(time.Now())
	res, err := s.db.ExecContext(ctx, `INSERT INTO outbounds
		(name,protocol,address,port,config,enabled,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		o.Name, o.Protocol, o.Address, o.Port, string(o.Config), b2i(o.Enabled), now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetOutbound(ctx context.Context, id int64) (*Outbound, error) {
	return scanOutbound(s.db.QueryRowContext(ctx, `SELECT `+outboundCols+` FROM outbounds WHERE id=?`, id))
}

func (s *Store) ListOutbounds(ctx context.Context) ([]Outbound, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+outboundCols+` FROM outbounds ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Outbound{}
	for rows.Next() {
		o, err := scanOutbound(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// UpdateOutbound rewrites an outbound. If its config changed, the previous test result no
// longer describes it and is cleared.
func (s *Store) UpdateOutbound(ctx context.Context, o *Outbound) error {
	cur, err := s.GetOutbound(ctx, o.ID)
	if err != nil {
		return err
	}
	reset := string(cur.Config) != string(o.Config)
	q := `UPDATE outbounds SET name=?,protocol=?,address=?,port=?,config=?,enabled=?,updated_at=?`
	if reset {
		q += `,last_test_at=0,last_ok=0,last_delay_ms=0,last_error='',last_ip='',last_country=''`
	}
	_, err = s.db.ExecContext(ctx, q+` WHERE id=?`, o.Name, o.Protocol, o.Address, o.Port, string(o.Config),
		b2i(o.Enabled), ms(time.Now()), o.ID)
	return err
}

func (s *Store) DeleteOutbound(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM outbounds WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SaveOutboundTest records the outcome of a liveness test.
func (s *Store) SaveOutboundTest(ctx context.Context, id int64, ok bool, delayMS int64, errMsg, ip, country string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbounds SET last_test_at=?,last_ok=?,last_delay_ms=?,last_error=?,last_ip=?,last_country=? WHERE id=?`,
		ms(time.Now()), b2i(ok), delayMS, errMsg, ip, country, id)
	return err
}
