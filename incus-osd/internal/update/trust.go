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
	"strconv"

	"github.com/lxc/incus-os/incus-osd/internal/providers"
	"github.com/lxc/incus-os/incus-osd/internal/state"
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
	case errors.As(err, &urlErr), errors.As(err, &netErr), errors.Is(err, context.DeadlineExceeded), errors.Is(err, providers.ErrServerResponse):
		return ErrorKindNetwork
	case errors.As(err, &syntaxErr), errors.As(err, &typeErr), errors.Is(err, gzip.ErrHeader), errors.Is(err, gzip.ErrChecksum),
		errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, util.ErrSMIMEUnreadable):
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
	st.LastError = err.Error()

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

	slog.ErrorContext(ctx, TrustEscalation(st.TrustFailures))

	return true
}

// TrustEscalation is the owner's warning after repeated trust failures.
func TrustEscalation(failures int) string {
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
