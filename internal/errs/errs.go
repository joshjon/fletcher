// Package errs declares the daemon's error-category model. Domain packages
// attach a Category to their errors (typed or wrapped); the Connect
// interceptor at the API edge translates Category → connect.Code without
// the domain code ever importing connect-go.
//
// Why categories instead of mapping sentinels: it keeps domain packages
// transport-agnostic, which matches the "no jack-of-all-trades package"
// rule (STANDARDS.md). Each component owns its sentinels; only this small
// package knows the cross-cutting category enum.
package errs

import (
	"errors"
	"fmt"
)

// Category is the high-level error classification used by the Connect
// interceptor to map domain errors to wire codes. The names mirror the
// connect-go codes we actually use; ordering is alphabetised within
// stable position 0 (Unknown).
type Category int

// Category values.
const (
	CategoryUnknown Category = iota
	CategoryConflict
	CategoryDeadlineExceeded
	CategoryFailedPrecondition
	CategoryInvalidArgument
	CategoryNotFound
	CategoryPermissionDenied
	CategoryUnauthenticated
	CategoryUnavailable
)

// String returns a snake_case rendering of the category, useful for logs.
func (c Category) String() string {
	switch c {
	case CategoryConflict:
		return "conflict"
	case CategoryDeadlineExceeded:
		return "deadline_exceeded"
	case CategoryFailedPrecondition:
		return "failed_precondition"
	case CategoryInvalidArgument:
		return "invalid_argument"
	case CategoryNotFound:
		return "not_found"
	case CategoryPermissionDenied:
		return "permission_denied"
	case CategoryUnauthenticated:
		return "unauthenticated"
	case CategoryUnavailable:
		return "unavailable"
	}
	return "unknown"
}

// Categorized is implemented by errors that know their Category. The
// interceptor uses errors.As to discover it.
type Categorized interface {
	error
	Category() Category
}

// Wrap attaches a Category to err. Wrapping nil returns nil. The result
// unwraps to err, so errors.Is/As keep working through the wrap.
func Wrap(err error, cat Category) error {
	if err == nil {
		return nil
	}
	return &categorized{err: err, cat: cat}
}

// New creates a Categorized error with the given message.
func New(cat Category, msg string) error {
	return &categorized{err: errors.New(msg), cat: cat}
}

// Newf creates a Categorized error formatted from format.
func Newf(cat Category, format string, args ...any) error {
	return &categorized{err: fmt.Errorf(format, args...), cat: cat}
}

// CategoryOf walks the err chain and returns the first Category it finds.
// Returns CategoryUnknown when no Categorized error is present.
func CategoryOf(err error) Category {
	var c Categorized
	if errors.As(err, &c) {
		return c.Category()
	}
	return CategoryUnknown
}

type categorized struct {
	err error
	cat Category
}

func (c *categorized) Error() string      { return c.err.Error() }
func (c *categorized) Unwrap() error      { return c.err }
func (c *categorized) Category() Category { return c.cat }
