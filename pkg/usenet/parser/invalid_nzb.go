package parser

// InvalidNZBError identifies malformed input while preserving the original
// message and cause. Diagnostics can classify it without inspecting body text.
type InvalidNZBError struct {
	cause error
}

func NewInvalidNZBError(cause error) error {
	if cause == nil {
		return nil
	}
	return &InvalidNZBError{cause: cause}
}

func (e *InvalidNZBError) Error() string { return e.cause.Error() }
func (e *InvalidNZBError) Unwrap() error { return e.cause }
