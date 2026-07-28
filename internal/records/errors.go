package records

import (
	"errors"
	"fmt"
)

// Kind classifies an error for exit-code mapping. See docs/v1-spec.md §22.
type Kind int

const (
	KindGeneral Kind = iota
	KindValidation
	KindNotFound
	KindConflict
	KindPermission
	KindOffline
	KindPartial
	KindGit
	KindDatabase
)

// ExitCode maps an error kind to the CLI exit code contract.
func (k Kind) ExitCode() int {
	switch k {
	case KindValidation:
		return 2
	case KindNotFound:
		return 3
	case KindConflict:
		return 4
	case KindPermission:
		return 5
	case KindOffline:
		return 6
	case KindPartial:
		return 7
	default: // general, git, database
		return 1
	}
}

// Error is Ark's typed error.
type Error struct {
	Kind    Kind
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// ExitCode returns the exit code for any error, defaulting to 1.
func ExitCode(err error) int {
	var ae *Error
	if errors.As(err, &ae) {
		return ae.Kind.ExitCode()
	}
	return 1
}

func NotFoundf(format string, args ...any) error {
	return &Error{Kind: KindNotFound, Message: fmt.Sprintf(format, args...)}
}

func Conflictf(format string, args ...any) error {
	return &Error{Kind: KindConflict, Message: fmt.Sprintf(format, args...)}
}

func Offlinef(format string, args ...any) error {
	return &Error{Kind: KindOffline, Message: fmt.Sprintf(format, args...)}
}

func Permissionf(format string, args ...any) error {
	return &Error{Kind: KindPermission, Message: fmt.Sprintf(format, args...)}
}

func Partialf(format string, args ...any) error {
	return &Error{Kind: KindPartial, Message: fmt.Sprintf(format, args...)}
}

func GitErr(msg string, err error) error {
	return &Error{Kind: KindGit, Message: msg, Err: err}
}

func DBErr(msg string, err error) error {
	return &Error{Kind: KindDatabase, Message: msg, Err: err}
}
