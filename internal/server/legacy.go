package server

// The legacy service token's mode: ARK_LEGACY_TOKEN, RFC-0003 Stage 3
// ("narrow, on an announced date") and Stage 4's first half
// (elk-work/ark#54).
//
// Retiring one string the whole fleet holds is not a deploy, it is a
// migration, and the thing that makes a migration survivable is a dial the
// operator turns rather than a release they have to roll back. This file is
// that dial, and it has exactly three positions:
//
//   - **full** (and unset) — today's behaviour, byte for byte. The legacy
//     bearer authenticates and carries implicit `admin` on every repository.
//     Deploying this change without setting the variable changes nothing.
//   - **readonly** — the bearer still authenticates and may pull and read;
//     every write is refused with the `permission` code (spec §22, client
//     exit 5) and a message naming the cutover. This is the position that
//     catches a forgotten CI job as a failed push rather than as silent
//     success, and it is reversible by unsetting one variable.
//   - **off** — the legacy branch is not registered at all. An ARK_API_TOKEN
//     bearer is then an unknown credential and takes the same path as any bad
//     token: 401, "invalid or missing token".
//
// Anything else fails startup. A typo must not quietly mean `full`: the whole
// point of the dial is that its position is knowable, and "ARK_LEGACY_TOKEN
// was set to `read-only` and the service kept accepting writes" is the exact
// silent failure the staging was designed to avoid.
//
// **ARK_API_TOKEN stays required, including under `off`.** Making it optional
// is Stage 4, and it has a second job here — it is still the default HMAC
// signing key for local-mode blob URLs (see Server.signingKey). `off` retires
// the token as a *bearer*, which is the half this file owns.

import (
	"fmt"
	"net/http"
	"strings"
)

// The three positions of ARK_LEGACY_TOKEN.
const (
	LegacyModeFull     = "full"
	LegacyModeReadonly = "readonly"
	LegacyModeOff      = "off"
)

// LegacyModeValues is what ARK_LEGACY_TOKEN accepts, for the caller that
// validates it at startup rather than discovering a typo as an outage —
// the same shape, and the same reason, as DefaultGrantValues.
var LegacyModeValues = []string{LegacyModeFull, LegacyModeReadonly, LegacyModeOff}

// ParseLegacyMode resolves the value of ARK_LEGACY_TOKEN, or says why it
// cannot. Empty is `full`, which is what every deployment configured before
// this variable existed is running.
//
// It is exported and lives here rather than in cmd/ark-server so the rule and
// its error message are testable without standing a service up, and so the
// modes have one definition that both the parser and the server read.
func ParseLegacyMode(v string) (string, error) {
	switch v {
	case "":
		return LegacyModeFull, nil
	case LegacyModeFull, LegacyModeReadonly, LegacyModeOff:
		return v, nil
	}
	return "", fmt.Errorf("ARK_LEGACY_TOKEN is %q; it takes %s (unset means %s, which is the behaviour before this setting existed)",
		v, strings.Join(LegacyModeValues, ", "), LegacyModeFull)
}

// legacyMode is the mode this service is running in, with empty read as
// `full`. Every decision below goes through it, so a Server built as a struct
// literal — which is how every test and cmd/ark-server builds one — behaves
// exactly as it did before this field existed.
func (s *Server) legacyMode() string {
	if s.LegacyMode == "" {
		return LegacyModeFull
	}
	return s.LegacyMode
}

// legacyAccepted reports whether the service token is a bearer at all. False
// only under `off`, where the legacy comparison is simply not made.
func (s *Server) legacyAccepted() bool {
	return s.legacyMode() != LegacyModeOff
}

// legacyReadonly reports whether this request's principal is the legacy
// service token on a service running `readonly` — the one combination that
// authenticates and may not write.
//
// It takes the principal rather than reading it from the request because the
// two callers have it in different forms, and because a nil principal must
// answer false: a route registered without s.auth is a bug, and the half of
// that bug which does not hand out access is refusing, not exempting.
func (s *Server) legacyReadonly(who *authenticated) bool {
	return who != nil && who.Legacy && s.legacyMode() == LegacyModeReadonly
}

// legacyReadonlyMessage is what a refused legacy write says. It names the
// cutover rather than the missing grant, because "principal legacy holds read
// on repository X" would send the reader to `ark repo grant legacy --write`,
// which is not a thing and would not help: the answer is a credential of their
// own, not a bigger grant for the string everybody shares.
const legacyReadonlyMessage = "legacy token is read-only; use a principal credential — see RFC-0003. " +
	"This service runs with ARK_LEGACY_TOKEN=readonly: the shared ARK_API_TOKEN may pull and read, and " +
	"every write is refused. `ark login` obtains a credential of your own, and an admin of this " +
	"repository grants it with `ark repo grant <email> --write`"

// legacyOp names the operation a refusal was about, for the log. The route
// pattern rather than the path: `POST /v1/repositories/{repo}/tasks` is the
// thing an operator greps for, while the path carries a ULID that makes every
// line unique and nothing countable.
func legacyOp(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return r.Method + " " + r.URL.Path
}

// logLegacyRefusal records one refused write. This is the observability half
// of Stage 3 and it is the point of the stage: `readonly` is run for two weeks
// before the cutover so that the log answers "who has not moved yet?" —
// grep `mode=readonly` and every line names a caller still holding the shared
// token and the operation it tried.
func (s *Server) logLegacyRefusal(r *http.Request) {
	if s.Log == nil {
		return
	}
	s.Log.Warn("legacy token refused", "principal", legacyPrincipalID,
		"mode", LegacyModeReadonly, "op", legacyOp(r),
		"method", r.Method, "path", r.URL.Path)
}

// refuseLegacyReadonly logs the refusal and returns it. `permission` and 403,
// the same pair every authorization refusal uses, so no client had to learn
// anything for this to land (spec §22 → exit 5).
func (s *Server) refuseLegacyReadonly(r *http.Request) *writeFault {
	s.logLegacyRefusal(r)
	return faultPermission(legacyReadonlyMessage)
}

// legacyReadonlyCreate is the refusal a legacy bearer gets for *creating* a
// repository under `readonly`.
//
// It travels out of the registration transaction as an error so the
// transaction rolls back — registration is the one write s.authorize cannot
// refuse on its own, because it asks for `read` (the idempotent
// re-registration every sync makes needs no more than that) and only finds out
// it is creating something once it is inside the transaction. Comparing
// against this exact value afterwards is what lets the refusal be logged once,
// outside a closure that reruns on a lost compare-and-swap.
var legacyReadonlyCreate = faultPermission(legacyReadonlyMessage)
