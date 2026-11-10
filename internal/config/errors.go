package config

import errors "github.com/jbcjorge/errors-library"

// Sentinel errors for configuration loading and topology resolution.
// Callers can match them with errors.Is.
var (
	ErrConfigLoad          = errors.New("failed to load config file %s")
	ErrConfigParse         = errors.New("failed to parse config file %s")
	ErrBackendsLoad        = errors.New("failed to load backends file %s")
	ErrBackendsParse       = errors.New("failed to parse backends file %s")
	ErrRouteUnknown        = errors.New("backends route references unknown server or composition: %s")
	ErrMemberServerUnknown = errors.New("composition member references unknown server: %s")
)
