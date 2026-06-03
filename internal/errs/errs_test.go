package errs_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/errs"
)

func TestCategoryOfFindsCategoryThroughWrappers(t *testing.T) {
	root := errs.New(errs.CategoryNotFound, "job not found")
	wrapped := fmt.Errorf("get job: %w", root)
	require.Equal(t, errs.CategoryNotFound, errs.CategoryOf(wrapped))
}

func TestCategoryOfReturnsUnknownForPlainErrors(t *testing.T) {
	require.Equal(t, errs.CategoryUnknown, errs.CategoryOf(errors.New("nope")))
}

func TestWrapPreservesUnderlyingErrorForIs(t *testing.T) {
	sentinel := errors.New("base")
	got := errs.Wrap(sentinel, errs.CategoryConflict)
	require.ErrorIs(t, got, sentinel)
	require.Equal(t, errs.CategoryConflict, errs.CategoryOf(got))
}

func TestWrapNilReturnsNil(t *testing.T) {
	require.Nil(t, errs.Wrap(nil, errs.CategoryInvalidArgument))
}

func TestCategoryStringIsStable(t *testing.T) {
	require.Equal(t, "not_found", errs.CategoryNotFound.String())
	require.Equal(t, "invalid_argument", errs.CategoryInvalidArgument.String())
	require.Equal(t, "unknown", errs.CategoryUnknown.String())
}
