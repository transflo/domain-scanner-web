package notifier

// Status says how sure we are that a domain can really be registered.
type Status string

const (
	// StatusConfirmed: Cloudflare's registrar confirmed the name is registrable (price known).
	// Only confirmed items get a "register" button.
	StatusConfirmed Status = "confirmed"
	// StatusUnconfirmed: found by RDAP/WHOIS but not confirmed by Cloudflare (unsupported
	// extension, check failed, or Cloudflare not configured). Announced with a warning label.
	StatusUnconfirmed Status = "unconfirmed"
)

// Item is one domain to announce.
type Item struct {
	ResultID int64 // database id, used by the button's callback data
	Domain   string
	Status   Status
	Price    string // first-year price, only for confirmed items
	Currency string
	Note     string // why it is unconfirmed (shown next to the domain)
}
