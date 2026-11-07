package ddberr

import "net/http"

// IdempotentParameterMismatch is the error TransactWriteItems returns when a
// ClientRequestToken is reused with a different request body. DynamoDB keeps
// the token's result for ten minutes, so a differing body within that window
// is rejected rather than executed.
func IdempotentParameterMismatch() *Error {
	return newErr(nsDynamo, "IdempotentParameterMismatchException", http.StatusBadRequest,
		"The request uses the same client token as a previous, but non-identical request.")
}
