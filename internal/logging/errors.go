package logging

import errors "github.com/jbcjorge/errors-library"

// ErrLogFileOpen is returned when the configured log file cannot be opened.
var ErrLogFileOpen = errors.New("failed to open log file %s")
