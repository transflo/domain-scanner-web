package store

import (
	"context"
	"time"
)

// CFUpdate is the outcome of a Cloudflare check for one result. Status "pending" means the
// check has been queued but has not run, so no check time is recorded.
type CFUpdate struct {
	Status   string // pending, confirmed, rejected, unsupported, error
	Reason   string
	Price    string
	Currency string
}

// UpdateResultCF stores a Cloudflare verdict, replacing any earlier one.
func (s *Store) UpdateResultCF(ctx context.Context, id int64, u CFUpdate) error {
	var checked int64
	if u.Status != "pending" {
		checked = ms(time.Now())
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE results SET cf_status=?,cf_reason=?,cf_price=?,cf_currency=?,cf_checked_at=? WHERE id=?`,
		u.Status, u.Reason, u.Price, u.Currency, checked, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClaimRegistration atomically moves a Cloudflare-confirmed, not yet registered (or previously
// failed) result into "registering". Exactly one caller can win, which is what stops a double
// tap on the Telegram button from spending the money twice.
func (s *Store) ClaimRegistration(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE results SET register_status='registering',register_note='',register_at=?
		WHERE id=? AND cf_status='confirmed' AND register_status IN ('','failed')`, ms(time.Now()), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SetRegisterOutcome records how a registration ended (succeeded, failed) or that it is still
// being processed by Cloudflare (registering).
func (s *Store) SetRegisterOutcome(ctx context.Context, id int64, status, note string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE results SET register_status=?,register_note=? WHERE id=?`, status, note, id)
	return err
}

// CountRegistrationsSince counts registrations started since t that are in flight or succeeded
// (failed attempts cost nothing and do not count against the daily cap).
func (s *Store) CountRegistrationsSince(ctx context.Context, t time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM results
		WHERE register_status IN ('registering','succeeded') AND register_at>=?`, ms(t)).Scan(&n)
	return n, err
}
