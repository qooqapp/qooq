package controlclient

import "errors"

// asAPIError is errors.As spelled once, so wrapping stays free for callers.
func asAPIError(err error, target **APIError) bool { return errors.As(err, target) }
