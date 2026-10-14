package api

import (
	"context"
	"errors"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
)

// ValidationInterceptor enforces protobuf validation rules before unary service dispatch.
func ValidationInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			msg, ok := req.Any().(proto.Message)
			if !ok {
				return nil, connect.NewError(connect.CodeInternal, errors.New("invalid request message"))
			}
			if err := protovalidate.Validate(msg); err != nil {
				if _, ok := errors.AsType[*protovalidate.ValidationError](err); ok {
					return nil, connect.NewError(connect.CodeInvalidArgument, err)
				}
				return nil, connect.NewError(connect.CodeInternal, errors.New("request validation unavailable"))
			}
			return next(ctx, req)
		}
	}
}
