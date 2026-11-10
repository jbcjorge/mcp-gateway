package cabundle

import errors "github.com/jbcjorge/errors-library"

// ErrBundleMissing is returned when require_bundle is set and the bundle is
// missing or unreadable after the check/generate flow.
var ErrBundleMissing = errors.New("required CA bundle missing or unreadable")
