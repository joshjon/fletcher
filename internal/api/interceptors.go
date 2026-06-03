package api

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"go.jetify.com/typeid"

	"github.com/joshjon/fletcher/internal/errs"
)

// requestIDPrefix is the typeid prefix for request correlation IDs.
const requestIDPrefix = "req"

// requestIDKey is the context key used to carry the per-request correlation ID.
type requestIDKey struct{}

// WithRequestID returns a new context carrying id. Mostly useful in tests;
// production code calls into RequestIDInterceptor.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the correlation ID attached to ctx, or "" if none.
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey{}).(string)
	return v
}

// RequestIDInterceptor stamps each inbound RPC with a fresh typeid and
// attaches it to the context. Downstream logs (via ContextLogHandler) pick
// it up automatically.
func RequestIDInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			id, err := typeid.WithPrefix(requestIDPrefix)
			if err == nil {
				ctx = WithRequestID(ctx, id.String())
			}
			return next(ctx, req)
		}
	}
}

// ErrorInterceptor maps domain errors to connect codes. Errors already of
// type *connect.Error pass through; errors satisfying errs.Categorized are
// mapped to the corresponding connect.Code; everything else maps to
// Internal with a sanitised wire message.
//
// Every non-nil error is logged with the procedure, category, and full
// chain — sanitisation only affects what the *client* sees.
func ErrorInterceptor(logger *slog.Logger) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			resp, err := next(ctx, req)
			if err == nil {
				return resp, nil
			}

			var connectErr *connect.Error
			if errors.As(err, &connectErr) {
				logger.ErrorContext(ctx, "rpc connect error",
					slog.String("procedure", req.Spec().Procedure),
					slog.String("code", connectErr.Code().String()),
					slog.String("err", err.Error()),
				)
				return nil, err
			}

			cat := errs.CategoryOf(err)
			logger.ErrorContext(ctx, "rpc error",
				slog.String("procedure", req.Spec().Procedure),
				slog.String("category", cat.String()),
				slog.String("err", err.Error()),
			)

			if cat == errs.CategoryUnknown {
				// Don't leak internals — client sees a generic message.
				return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}
			return nil, connect.NewError(categoryToCode(cat), err)
		}
	}
}

func categoryToCode(c errs.Category) connect.Code {
	switch c {
	case errs.CategoryConflict:
		return connect.CodeAlreadyExists
	case errs.CategoryDeadlineExceeded:
		return connect.CodeDeadlineExceeded
	case errs.CategoryFailedPrecondition:
		return connect.CodeFailedPrecondition
	case errs.CategoryInvalidArgument:
		return connect.CodeInvalidArgument
	case errs.CategoryNotFound:
		return connect.CodeNotFound
	case errs.CategoryPermissionDenied:
		return connect.CodePermissionDenied
	case errs.CategoryUnauthenticated:
		return connect.CodeUnauthenticated
	case errs.CategoryUnavailable:
		return connect.CodeUnavailable
	}
	return connect.CodeInternal
}
