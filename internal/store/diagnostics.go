package store

import (
	"context"
	"time"
)

// EgressDiag summarises the checks that went out through one egress.
type EgressDiag struct {
	Egress      string  `json:"egress"`
	Checks      int64   `json:"checks"`
	Available   int64   `json:"available"`
	Registered  int64   `json:"registered"`
	Unknown     int64   `json:"unknown"`
	RateLimited int64   `json:"rate_limited"`
	Timeouts    int64   `json:"timeouts"`
	NetworkErrs int64   `json:"network_errors"`
	Throttles   int64   `json:"throttles"` // RDAP 429 answers seen
	Storms      int64   `json:"storms"`    // error storms that triggered a backoff
	AvgMS       float64 `json:"avg_ms"`
	MaxMS       int64   `json:"max_ms"`
	SuccessRate float64 `json:"success_rate"` // known verdicts / checks
}

// TLDDiag is the verdict distribution for one suffix.
type TLDDiag struct {
	TLD        string `json:"tld"`
	Checks     int64  `json:"checks"`
	Available  int64  `json:"available"`
	Registered int64  `json:"registered"`
	Unknown    int64  `json:"unknown"`
}

// Diagnostics is what the logs say about the last period: how each egress behaved and how the
// verdicts are distributed. It reads the per-check "done" log lines, so its window is limited by
// the debug-log retention.
type Diagnostics struct {
	Since    time.Time    `json:"since"`
	Checks   int64        `json:"checks"`
	ByEgress []EgressDiag `json:"by_egress"`
	ByTLD    []TLDDiag    `json:"by_tld"`
}

func (s *Store) Diagnostics(ctx context.Context, since time.Time) (Diagnostics, error) {
	d := Diagnostics{Since: since, ByEgress: []EgressDiag{}, ByTLD: []TLDDiag{}}
	from := ms(since)

	rows, err := s.db.QueryContext(ctx, `SELECT egress,
			COUNT(*),
			SUM(json_extract(fields,'$.status')='available'),
			SUM(json_extract(fields,'$.status') IN ('registered','reserved')),
			SUM(json_extract(fields,'$.status')='unknown'),
			SUM(json_extract(fields,'$.err_kind')='rate_limited'),
			SUM(json_extract(fields,'$.err_kind')='timeout'),
			SUM(json_extract(fields,'$.err_kind')='network'),
			AVG(duration_ms), MAX(duration_ms)
		FROM logs WHERE component='check' AND event='done' AND time>=? GROUP BY egress ORDER BY egress`, from)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var e EgressDiag
		if err := rows.Scan(&e.Egress, &e.Checks, &e.Available, &e.Registered, &e.Unknown, &e.RateLimited, &e.Timeouts,
			&e.NetworkErrs, &e.AvgMS, &e.MaxMS); err != nil {
			rows.Close()
			return d, err
		}
		if e.Checks > 0 {
			e.SuccessRate = float64(e.Checks-e.Unknown) / float64(e.Checks)
		}
		d.Checks += e.Checks
		d.ByEgress = append(d.ByEgress, e)
	}
	rows.Close()

	for i := range d.ByEgress {
		e := &d.ByEgress[i]
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs WHERE component='check' AND event='step'
			AND egress=? AND time>=? AND json_extract(fields,'$.step')='rdap.throttle'`, e.Egress, from).Scan(&e.Throttles); err != nil {
			return d, err
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs WHERE component='egress' AND event='storm'
			AND time>=? AND json_extract(fields,'$.from')=?`, from, e.Egress).Scan(&e.Storms); err != nil {
			return d, err
		}
	}

	trows, err := s.db.QueryContext(ctx, `SELECT substr(domain, instr(domain,'.')+1) AS tld,
			COUNT(*),
			SUM(json_extract(fields,'$.status')='available'),
			SUM(json_extract(fields,'$.status') IN ('registered','reserved')),
			SUM(json_extract(fields,'$.status')='unknown')
		FROM logs WHERE component='check' AND event='done' AND domain<>'' AND time>=?
		GROUP BY tld ORDER BY COUNT(*) DESC, tld LIMIT 200`, from)
	if err != nil {
		return d, err
	}
	defer trows.Close()
	for trows.Next() {
		var x TLDDiag
		if err := trows.Scan(&x.TLD, &x.Checks, &x.Available, &x.Registered, &x.Unknown); err != nil {
			return d, err
		}
		d.ByTLD = append(d.ByTLD, x)
	}
	return d, trows.Err()
}
