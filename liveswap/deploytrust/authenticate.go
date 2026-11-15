package deploytrust

import (
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// Refusal is what the preamble answers a refused request with, for the
// caller to write (Write) through its own response filter: the flat
// 401 and its fixed body, the same whatever the reason and however
// often the address has failed, so a caller learns neither which apps
// exist, nor what a source pins, nor what the throttle's budgets say.
// Nothing in it comes from the request or from the reason; the reason
// went to the journal, if the budgets let it.
type Refusal struct {
	Status  int
	Message string
}

// Write answers the refused request with the fixed body through write
// — the caller's filtered writer, so every body a webhook sends passes
// its filter.
func (ref *Refusal) Write(write func(code int, body any) error) error {
	return write(ref.Status, wire.Error{Error: ref.Message})
}

// unauthorizedMessage is the flat 401: the same sentence whatever the
// reason, so a caller learns neither which apps exist nor what a
// source pins.
const unauthorizedMessage = "invalid or missing deploy token (Authorization: Bearer <jwt>)"

// bearerToken extracts the deploy JWT from `Authorization: Bearer
// <jwt>`. Bearer is the only accepted transport: Caddy redacts the
// Authorization header from access logs automatically (the retired
// X-Liveswap-Secret custom header did not, which leaked it).
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	if auth := r.Header.Get("Authorization"); len(auth) > len(prefix) &&
		strings.EqualFold(auth[:len(prefix)], prefix) {
		return auth[len(prefix):]
	}
	return ""
}

// scopeField appends the caller's one journal field for the preamble's
// lines — liveswap's app name; whatever the box webhook names its
// request by — when there is one. The value is bounded as a refusal is
// (boundRefusal: one line of at most MaxRefusalLen bytes) whatever the
// caller put in it: the limiter bounds how many lines a caller can
// write, not how long each is, so the length is bounded here, at the
// mechanism, rather than asked of each caller. The key is the caller's
// own constant.
func scopeField(fields []zap.Field, key, value string) []zap.Field {
	if key == "" {
		return fields
	}
	return append(fields, zap.String(key, boundRefusal(value)))
}

// Authenticate is the preamble every webhook request passes before
// anything else is revealed, and the one place a deploy token is
// judged: the bearer against verifiers (authorize), then what a
// refusal costs this limiter and the journal. An accepted token is who
// it is (Identity), and the address's failures are forgotten. A
// refused one is answered by the caller with the Refusal returned —
// the flat 401, whatever the reason and whatever the budgets say —
// through the caller's own response filter, so every body a webhook
// sends passes one. logger is the caller's; scopeKey and
// scopeValue are the one field its lines carry besides the address
// and the reason — liveswap names the app — bounded where a line is
// written (scopeField); an empty key is no field. liveswap's webhook
// and the box's both call this on the limiter Shared returns, so one
// address has one budget whichever webhook a flood aims at.
func (l *Limiter) Authenticate(r *http.Request, verifiers []Verifier, logger *zap.Logger, scopeKey, scopeValue string) (Identity, *Refusal) {
	who, down, refused := authorize(r.Context(), verifiers, bearerToken(r))
	key := clientKey(r)
	// A source the box could not consult is named here once per window
	// per source, whatever the budgets and whether or not another
	// source then accepted the token; a refusal is still charged below
	// like any other — see unavailable for why both.
	for _, u := range down {
		if l.outage(u.label, scopeKey) {
			fields := scopeField([]zap.Field{zap.String("source", u.label)}, scopeKey, scopeValue)
			logger.Warn("webhook auth could not consult a trust source",
				append(fields, zap.String("remote", key), zap.String("reason", boundRefusal(u.Error())))...)
		}
	}
	if refused != nil {
		// What a failure costs in the journal is the limiter's call
		// (see Limiter): the count of lines, and — the scope and the
		// refusal being request input, both logged bounded — the size
		// of each. The refusal is for this journal only: the response
		// stays the same flat 401 for every reason and every verdict, so
		// a caller learns neither which apps exist, nor what a source
		// pins, nor whether its failures are still being logged.
		v := l.fail(key)
		if v.log {
			logger.Warn("webhook auth failed",
				append(scopeField(nil, scopeKey, scopeValue), zap.String("remote", key), zap.String("refused", refused.Error()))...)
		}
		if v.trippedKey {
			logger.Warn("webhook auth failures from this address throttled: further ones are not logged",
				zap.String("remote", key), zap.Int("failures", FailBudget), zap.Duration("window", FailWindow))
		}
		if v.trippedGlobal {
			logger.Warn("webhook auth failures throttled process-wide: further ones are not logged",
				zap.Int("failures", FailGlobalBudget), zap.Duration("window", FailWindow))
		}
		return Identity{}, &Refusal{Status: http.StatusUnauthorized, Message: unauthorizedMessage}
	}
	l.clear(key)
	return who, nil
}
