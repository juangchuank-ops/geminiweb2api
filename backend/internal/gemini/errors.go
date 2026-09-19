package gemini

import "errors"

// Errors the pool and gateway branch on.
//
// The distinction that matters is between "this account is dead" and "this
// request failed". Getting it wrong is not cosmetic: the pool retires an
// account on ErrInvalidCredential, so mislabelling a transient upstream fault
// as a credential problem removes a healthy account from rotation, while
// mislabelling a dead cookie as transient burns every request on it forever.
var (
	// ErrInvalidCredential means the cookie was rejected or is no longer a
	// session. The account is retired rather than cooled down.
	ErrInvalidCredential = errors.New("credential rejected")

	// ErrRateLimited means Google throttled this IP, not this account. It is
	// deliberately not a credential problem: the same cookie works again from
	// another egress, and retiring it would throw away a good account.
	ErrRateLimited = errors.New("upstream rate limited")

	// ErrUsageLimit means the account's quota for the day is spent. The
	// account is healthy and will work again after the quota resets.
	ErrUsageLimit = errors.New("account usage limit reached")

	// ErrLocationRejected means Gemini is not offered in the egress country.
	// Nothing about the account is wrong; the proxy is.
	ErrLocationRejected = errors.New("gemini unavailable in this region")

	// ErrModelUnavailable means the account is not entitled to the requested
	// model tier. Retrying the same account is pointless, but the account
	// itself is fine.
	ErrModelUnavailable = errors.New("model not available for this account")

	// ErrUpstreamUnavailable is any other upstream failure worth retrying on a
	// different account.
	ErrUpstreamUnavailable = errors.New("upstream unavailable")
)

// UpstreamError carries the numeric code the upstream returned alongside a
// classified error, so the console can show the raw value.
type UpstreamError struct {
	Code int
	Kind error
	Msg  string
}

func (e *UpstreamError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return e.Kind.Error()
}

func (e *UpstreamError) Unwrap() error { return e.Kind }

// Known BardErrorInfo codes.
//
// These arrive as `BardErrorInfo [1037]` embedded in an otherwise successful
// HTTP 200 response, which is why they have to be scraped out of the body: the
// status code says the request was fine and the body says it was not.
const (
	codeTemporaryError     = 1013 // transient, retry
	codeUnauthenticated    = 1016 // cookie no longer a session
	codeUsageLimit         = 1037 // daily quota spent
	codeModelInconsistent  = 1050 // the model number and header disagree
	codeModelHeaderInvalid = 1052 // the account is not entitled to this tier
	codeLocationRejected   = 1060 // region not supported
)

// classifyCode maps a BardErrorInfo code onto a sentinel error.
func classifyCode(code int) error {
	switch code {
	case codeUnauthenticated:
		return ErrInvalidCredential
	case codeUsageLimit:
		return ErrUsageLimit
	case codeModelHeaderInvalid, codeModelInconsistent:
		return ErrModelUnavailable
	case codeLocationRejected:
		return ErrLocationRejected
	case codeTemporaryError:
		return ErrUpstreamUnavailable
	default:
		return ErrUpstreamUnavailable
	}
}

// classifyStatus maps an HTTP status onto a sentinel error.
func classifyStatus(status int) error {
	switch {
	case status == 401 || status == 403:
		return ErrInvalidCredential
	case status == 429:
		return ErrRateLimited
	default:
		return ErrUpstreamUnavailable
	}
}
