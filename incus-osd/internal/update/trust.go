package update

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/lxc/incus/v7/shared/subprocess"

	"github.com/lxc/incus-os/incus-osd/internal/providers"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
	"github.com/lxc/incus-os/incus-osd/internal/util"
)

// TrustStatus is the update status while the update server's key isn't trusted.
const TrustStatus = "Update server key is not trusted by this system"

// Kinds of update check failure, as reported in last_error_kind.
const (
	ErrorKindNetwork   = "network"
	ErrorKindParse     = "parse"
	ErrorKindSignature = "signature"
	ErrorKindTrust     = "trust"
	ErrorKindOther     = "other"
)

// trustEscalateAfter is how many consecutive trust failures warn the owner.
const trustEscalateAfter = 3

// errorKind classifies an update check failure.
func errorKind(err error) string {
	var (
		trustErr  *util.TrustError
		sigErr    *util.SignatureError
		urlErr    *url.Error
		netErr    net.Error
		syntaxErr *json.SyntaxError
		typeErr   *json.UnmarshalTypeError
	)

	switch {
	case err == nil:
		return ""
	case errors.As(err, &trustErr):
		return ErrorKindTrust
	case errors.As(err, &sigErr):
		return ErrorKindSignature
	case systemd.IsUKIRejected(err) && errors.Is(err, util.ErrAuthenticodeUnreadable):
		return ErrorKindParse
	case systemd.IsUKIRejected(err):
		return ErrorKindSignature
	case errors.As(err, &urlErr), errors.As(err, &netErr), errors.Is(err, context.DeadlineExceeded), errors.Is(err, providers.ErrServerResponse):
		return ErrorKindNetwork
	case errors.As(err, &syntaxErr), errors.As(err, &typeErr), errors.Is(err, gzip.ErrHeader), errors.Is(err, gzip.ErrChecksum),
		errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, util.ErrSMIMEUnreadable), errors.Is(err, util.ErrSMIMEDamaged):
		return ErrorKindParse
	default:
		return ErrorKindOther
	}
}

// failedStatus is the status for a failed check: the trust phrase, or otherwise fallback.
func failedStatus(err error, fallback string) string {
	if errorKind(err) == ErrorKindTrust {
		return TrustStatus
	}

	return fallback
}

// checkCycle is one update check run, which may ask the update server several times.
type checkCycle struct {
	trustCounted bool
}

type checkCycleKey struct{}

// withCheckCycle starts a check run, unless ctx is already in one.
func withCheckCycle(ctx context.Context) context.Context {
	_, ok := ctx.Value(checkCycleKey{}).(*checkCycle)
	if ok {
		return ctx
	}

	return context.WithValue(ctx, checkCycleKey{}, &checkCycle{})
}

// recordCheck records the outcome of asking the update server, and reports whether it was the
// trust failure that warns the owner. A run counts at most one trust failure.
func recordCheck(ctx context.Context, s *state.State, err error) bool {
	st := &s.System.Update.State

	if err == nil {
		st.LastErrorKind = ""
		st.LastError = ""
		st.EmbeddedCAFingerprint = ""
		st.PresentedCAFingerprint = ""
		st.TrustFailures = 0
		st.TrustEscalated = false

		return false
	}

	st.LastErrorKind = errorKind(err)
	st.LastError = shortError(err)

	trustErr, ok := errors.AsType[*util.TrustError](err)
	if !ok {
		return false
	}

	st.EmbeddedCAFingerprint = trustErr.ExpectedFingerprint
	st.PresentedCAFingerprint = trustErr.PresentedFingerprint

	cycle, inCycle := ctx.Value(checkCycleKey{}).(*checkCycle)
	if inCycle && cycle.trustCounted {
		return false
	}

	if inCycle {
		cycle.trustCounted = true
	}

	st.TrustFailures++

	slog.WarnContext(ctx, TrustStatus, "embedded_ca", trustErr.ExpectedFingerprint, "presented_ca", trustErr.PresentedFingerprint,
		"presented_subject", trustErr.PresentedSubject, "root_ca", trustErr.RootFingerprint, "failures", st.TrustFailures)

	if st.TrustFailures < trustEscalateAfter || st.TrustEscalated {
		return false
	}

	st.TrustEscalated = true

	slog.ErrorContext(ctx, trustEscalation(st.TrustFailures))

	return true
}

// lastErrorMax is the longest last_error kept in the update state.
const lastErrorMax = 200

// urlQuery matches a URL's query string, which may carry an authentication token.
var urlQuery = regexp.MustCompile(`(https?://[^\s?"']*)\?[^\s"']*`)

// shortError is err as one short line, without URL query strings or command lines.
func shortError(err error) string {
	msg := err.Error()

	runErr, ok := errors.AsType[subprocess.RunError](err)
	if ok {
		msg = strings.Replace(msg, runErr.Error(), "command failed: "+runErr.Unwrap().Error(), 1)
	}

	msg, _, _ = strings.Cut(msg, "\n")
	msg = urlQuery.ReplaceAllString(msg, "$1")

	if len(msg) > lastErrorMax {
		msg = strings.ToValidUTF8(msg[:lastErrorMax-len("…")], "") + "…"
	}

	return msg
}

// trustEscalation is the owner's warning after repeated trust failures.
func trustEscalation(failures int) string {
	return "No updates for " + strconv.Itoa(failures) + " checks: the update server's signing key is not trusted; " +
		"compare the CA fingerprints under Updates and see the security reference"
}

// modalErrorText is the update modal's text for a failed check.
func modalErrorText(msg string, err error, providerType string) string {
	trustErr, ok := errors.AsType[*util.TrustError](err)
	if ok {
		return "[red]" + TrustStatus + "[white] (embedded CA " + orUnknown(trustErr.ExpectedFingerprint) +
			", presented CA " + orUnknown(trustErr.PresentedFingerprint) + "; provider: " + providerType + ")"
	}

	return "[red]Error[white] " + msg + ": " + err.Error() + " (provider: " + providerType + ")"
}

func orUnknown(v string) string {
	if v == "" {
		return "unknown"
	}

	return v
}
