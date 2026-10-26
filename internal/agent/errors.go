package agent

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"syscall"

	"github.com/tawanorg/claude-sync/internal/storage"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// ErrorKind sorts a sync failure by what the user can do about it. It is
// stored in status.json, so `stradl agent status` and the menu bar show the
// same short line.
type ErrorKind string

const (
	ErrorKeyMismatch ErrorKind = "key-mismatch" // passphrase differs from the other Macs'
	ErrorAuth        ErrorKind = "auth"         // storage rejected the credentials
	ErrorNoContainer ErrorKind = "no-container" // the container or bucket is gone
	ErrorOffline     ErrorKind = "offline"      // storage unreachable; retrying fixes it
	ErrorOther       ErrorKind = "other"
)

// Summary is the short status line for k.
func (k ErrorKind) Summary() string {
	switch k {
	case ErrorKeyMismatch:
		return "Passphrase doesn't match your other Macs"
	case ErrorAuth:
		return "Storage token expired or invalid"
	case ErrorNoContainer:
		return "Storage container not found"
	case ErrorOffline:
		return "Offline: will retry"
	}
	return "Sync error (see log)"
}

// urgent reports whether syncing stays broken until the user steps in, so
// the first failure is worth a notification.
func (k ErrorKind) urgent() bool { return k == ErrorKeyMismatch || k == ErrorAuth }

// rank orders kinds from the one most worth showing (the user must act) to
// the least.
func (k ErrorKind) rank() int {
	switch k {
	case ErrorKeyMismatch:
		return 0
	case ErrorAuth:
		return 1
	case ErrorNoContainer:
		return 2
	case ErrorOffline:
		return 3
	}
	return 4
}

// ClassifyError sorts the error a whole sync failed with. A sync fails as a
// whole on a missing object only when listing the container (or reading its
// key check) does, so storage.ErrNotFound here means the container is gone.
func ClassifyError(err error) ErrorKind {
	if err == nil {
		return ""
	}
	if !errors.Is(err, sync.ErrKeyMismatch) && errors.Is(err, storage.ErrNotFound) {
		return ErrorNoContainer
	}
	return classify(err)
}

// classifyFileErrors sorts the per-file errors of a sync that otherwise
// completed, by the most telling of them. A missing object there is that
// file's problem, not a missing container.
func classifyFileErrors(errs []error) ErrorKind {
	var kind ErrorKind
	for _, err := range errs {
		if k := classify(err); kind == "" || k.rank() < kind.rank() {
			kind = k
		}
	}
	return kind
}

func classify(err error) ErrorKind {
	if errors.Is(err, sync.ErrKeyMismatch) {
		return ErrorKeyMismatch
	}
	// Credential and container problems are recognized by message first:
	// they can arrive wrapped in transport errors that look like network
	// trouble.
	if k := classifyMessage(err.Error()); k != ErrorOffline && k != ErrorOther {
		return k
	}
	var netErr net.Error
	var urlErr *url.Error
	switch {
	case errors.As(err, &netErr), errors.As(err, &urlErr),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return ErrorOffline
	}
	return classifyMessage(err.Error())
}

// Message fragments by kind, lowercase. Azure's are the error codes and the
// hint internal/storage/azure adds; S3-style backends use the others.
var (
	keyMismatchText = []string{"encryption key doesn't match"}
	authText        = []string{"sas token may have expired", "authenticationfailed", "expiredtoken",
		"invalidaccesskeyid", "signaturedoesnotmatch"}
	noContainerText = []string{"containernotfound", "nosuchbucket"}
	offlineText     = []string{"no such host", "connection refused", "connection reset", "network is unreachable",
		"no route to host", "host is down", "timeout", "deadline exceeded"}
)

// classifyMessage sorts an error by its text alone, for errors that have
// lost their type (a status file written by an older agent).
func classifyMessage(msg string) ErrorKind {
	msg = strings.ToLower(msg)
	has := func(frags []string) bool {
		for _, f := range frags {
			if strings.Contains(msg, f) {
				return true
			}
		}
		return false
	}
	switch {
	case has(keyMismatchText):
		return ErrorKeyMismatch
	case has(authText):
		return ErrorAuth
	case has(noContainerText):
		return ErrorNoContainer
	case has(offlineText):
		return ErrorOffline
	}
	return ErrorOther
}
