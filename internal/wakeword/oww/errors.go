package oww

import "errors"

// ErrMissingWeights reports a malformed asset bundle.
var ErrMissingWeights = errors.New("oww: mel filterbank missing from the asset bundle")
