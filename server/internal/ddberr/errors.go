// Package ddberr holds DynamoDB's error types: the __type names, default
// messages and HTTP statuses the AWS SDKs match on. Clients branch on the
// short name (err.name in the JS SDK), so these must match DynamoDB exactly;
// the conformance suite checks them against the real service.
package ddberr

import (
	"fmt"
	"net/http"
	"strings"
)

// Namespaces DynamoDB puts in front of the short error name.
const (
	nsDynamo   = "com.amazonaws.dynamodb.v20120810#"
	nsCoral    = "com.amazon.coral.service#"
	nsValidate = "com.amazon.coral.validate#"
)

// Error is a DynamoDB error response. Type is the full __type value; Extra
// holds additional top-level body fields (Item on ConditionalCheckFailed,
// CancellationReasons on TransactionCanceled).
type Error struct {
	Type    string
	Message string
	Status  int
	Extra   map[string]any
}

func (e *Error) Error() string { return e.Code() + ": " + e.Message }

// Code is the short name after the namespace, e.g. "ValidationException".
func (e *Error) Code() string {
	if i := strings.LastIndexByte(e.Type, '#'); i >= 0 {
		return e.Type[i+1:]
	}
	return e.Type
}

// Body is the JSON response body.
func (e *Error) Body() map[string]any {
	b := make(map[string]any, len(e.Extra)+2)
	for k, v := range e.Extra {
		b[k] = v
	}
	b["__type"] = e.Type
	if e.Message != "" {
		b["message"] = e.Message
	}
	return b
}

func newErr(ns, code string, status int, msg string) *Error {
	return &Error{Type: ns + code, Message: msg, Status: status}
}

// Validation is a ValidationException with a formatted message. Use DynamoDB's
// own wording; tests compare messages with the real service.
func Validation(format string, args ...any) *Error {
	return newErr(nsValidate, "ValidationException", http.StatusBadRequest, fmt.Sprintf(format, args...))
}

// ResourceNotFound uses DynamoDB's default message when msg is empty.
func ResourceNotFound(msg string) *Error {
	if msg == "" {
		msg = "Requested resource not found"
	}
	return newErr(nsDynamo, "ResourceNotFoundException", http.StatusBadRequest, msg)
}

func ResourceInUse(msg string) *Error {
	return newErr(nsDynamo, "ResourceInUseException", http.StatusBadRequest, msg)
}

// BackupNotFound reports an unknown backup ARN.
func BackupNotFound(msg string) *Error {
	return newErr(nsDynamo, "BackupNotFoundException", http.StatusBadRequest, msg)
}

// TableNotFound is a restore operation's missing-source-table error, distinct
// from ResourceNotFound.
func TableNotFound(msg string) *Error {
	return newErr(nsDynamo, "TableNotFoundException", http.StatusBadRequest, msg)
}

// TableAlreadyExists reports a restore target whose name is taken.
func TableAlreadyExists(msg string) *Error {
	return newErr(nsDynamo, "TableAlreadyExistsException", http.StatusBadRequest, msg)
}

// PointInTimeRecoveryUnavailable reports that the table has no restorable
// window: recovery is off, or the initial copy taken when it was enabled has
// not finished.
func PointInTimeRecoveryUnavailable(msg string) *Error {
	return newErr(nsDynamo, "PointInTimeRecoveryUnavailableException", http.StatusBadRequest, msg)
}

// InvalidRestoreTime reports a restore time outside the table's window.
func InvalidRestoreTime(msg string) *Error {
	return newErr(nsDynamo, "InvalidRestoreTimeException", http.StatusBadRequest, msg)
}

// GlobalTableNotFound is the 2017 global tables API's error for a table that is
// not a global table (missing, or present but with no replicas).
func GlobalTableNotFound(msg string) *Error {
	return newErr(nsDynamo, "GlobalTableNotFoundException", http.StatusBadRequest, msg)
}

func GlobalTableAlreadyExists(msg string) *Error {
	return newErr(nsDynamo, "GlobalTableAlreadyExistsException", http.StatusBadRequest, msg)
}

func ReplicaAlreadyExists(msg string) *Error {
	return newErr(nsDynamo, "ReplicaAlreadyExistsException", http.StatusBadRequest, msg)
}

func ReplicaNotFound(msg string) *Error {
	return newErr(nsDynamo, "ReplicaNotFoundException", http.StatusBadRequest, msg)
}

// ConditionalCheckFailed carries the item (DynamoDB JSON) when the request
// asked for ReturnValuesOnConditionCheckFailure ALL_OLD; pass nil otherwise.
func ConditionalCheckFailed(item map[string]any) *Error {
	e := newErr(nsDynamo, "ConditionalCheckFailedException", http.StatusBadRequest, "The conditional request failed")
	if item != nil {
		e.Extra = map[string]any{"Item": item}
	}
	return e
}

// TransactionCanceled lists one reason per transaction item, in request order.
// Each reason is {"Code": "None"|"ConditionalCheckFailed"|..., "Message": ..., "Item": ...}.
func TransactionCanceled(reasons []map[string]any) *Error {
	codes := make([]string, len(reasons))
	for i, r := range reasons {
		codes[i], _ = r["Code"].(string)
	}
	msg := "Transaction cancelled, please refer cancellation reasons for specific reasons [" + strings.Join(codes, ", ") + "]"
	e := newErr(nsDynamo, "TransactionCanceledException", http.StatusBadRequest, msg)
	e.Extra = map[string]any{"CancellationReasons": reasons}
	return e
}

// DuplicateItem is what ExecuteStatement's INSERT (and ExecuteTransaction)
// report when the primary key already exists, matching DynamoDB's PartiQL.
func DuplicateItem() *Error {
	return newErr(nsDynamo, "DuplicateItemException", http.StatusBadRequest, "Duplicate primary key exists in table")
}

func ItemCollectionSizeLimitExceeded(msg string) *Error {
	return newErr(nsDynamo, "ItemCollectionSizeLimitExceededException", http.StatusBadRequest, msg)
}

func LimitExceeded(msg string) *Error {
	return newErr(nsDynamo, "LimitExceededException", http.StatusBadRequest, msg)
}

func ProvisionedThroughputExceeded() *Error {
	return newErr(nsDynamo, "ProvisionedThroughputExceededException", http.StatusBadRequest,
		"The level of configured provisioned throughput for the table was exceeded. Consider increasing your provisioning level with the UpdateTable API.")
}

func Internal() *Error {
	return newErr(nsDynamo, "InternalServerError", http.StatusInternalServerError, "Internal server error")
}

// ExpiredIterator is the DynamoDB Streams error for a shard iterator older than
// 15 minutes.
func ExpiredIterator(msg string) *Error {
	return newErr(nsDynamo, "ExpiredIteratorException", http.StatusBadRequest, msg)
}

// TrimmedDataAccess is the DynamoDB Streams error for a position older than the
// oldest kept stream record.
func TrimmedDataAccess(msg string) *Error {
	return newErr(nsDynamo, "TrimmedDataAccessException", http.StatusBadRequest, msg)
}

// Protocol and authentication errors use the coral service namespace.

func Serialization(msg string) *Error {
	return newErr(nsCoral, "SerializationException", http.StatusBadRequest, msg)
}

func UnknownOperation() *Error {
	return newErr(nsCoral, "UnknownOperationException", http.StatusBadRequest, "")
}

func MissingAuthenticationToken() *Error {
	return newErr(nsCoral, "MissingAuthenticationTokenException", http.StatusBadRequest, "Request is missing Authentication Token")
}

func UnrecognizedClient() *Error {
	return newErr(nsCoral, "UnrecognizedClientException", http.StatusBadRequest, "The security token included in the request is invalid.")
}

func InvalidSignature() *Error {
	return newErr(nsCoral, "InvalidSignatureException", http.StatusBadRequest,
		"The request signature we calculated does not match the signature you provided. Check your AWS Secret Access Key and signing method. Consult the service documentation for details.")
}
