package s3api

import (
	"encoding/xml"
	"net/http"
)

// S3Error mirrors the on-wire XML error body that S3 clients parse.
type S3Error struct {
	XMLName    xml.Name `xml:"Error"`
	Code       string   `xml:"Code"`
	Message    string   `xml:"Message"`
	Resource   string   `xml:"Resource,omitempty"`
	RequestID  string   `xml:"RequestId,omitempty"`
	httpStatus int
}

func (e *S3Error) Error() string { return e.Code + ": " + e.Message }
func (e *S3Error) Status() int   { return e.httpStatus }

func newErr(code, msg string, status int) *S3Error {
	return &S3Error{Code: code, Message: msg, httpStatus: status}
}

// Catalogue of the S3 error codes Telang returns. Codes match the AWS S3
// catalogue so clients fall back gracefully.
var (
	ErrNoSuchBucket             = newErr("NoSuchBucket", "The specified bucket does not exist", http.StatusNotFound)
	ErrNoSuchKey                = newErr("NoSuchKey", "The specified key does not exist", http.StatusNotFound)
	ErrBucketAlreadyOwnedByYou  = newErr("BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it", http.StatusConflict)
	ErrBucketNotEmpty           = newErr("BucketNotEmpty", "The bucket you tried to delete is not empty", http.StatusConflict)
	ErrInvalidBucketName        = newErr("InvalidBucketName", "The specified bucket is not valid", http.StatusBadRequest)
	ErrInvalidArgument          = newErr("InvalidArgument", "Invalid Argument", http.StatusBadRequest)
	ErrEntityTooLarge           = newErr("EntityTooLarge", "Your proposed upload exceeds the maximum allowed size", http.StatusRequestEntityTooLarge)
	ErrMissingContentLength     = newErr("MissingContentLength", "You must provide the Content-Length HTTP header", http.StatusLengthRequired)
	ErrAccessDenied             = newErr("AccessDenied", "Access Denied", http.StatusForbidden)
	ErrSignatureDoesNotMatch    = newErr("SignatureDoesNotMatch", "The request signature does not match the calculated signature", http.StatusForbidden)
	ErrInvalidAccessKeyID       = newErr("InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records", http.StatusForbidden)
	ErrRequestTimeTooSkewed     = newErr("RequestTimeTooSkewed", "The difference between the request time and the server's time is too large", http.StatusForbidden)
	ErrInternalError            = newErr("InternalError", "We encountered an internal error. Please try again.", http.StatusInternalServerError)
	ErrNotImplemented           = newErr("NotImplemented", "A header you provided implies functionality that is not implemented", http.StatusNotImplemented)
	ErrSlowDown                 = newErr("SlowDown", "Reduce your request rate.", http.StatusServiceUnavailable)
	ErrServiceUnavailable       = newErr("ServiceUnavailable", "Reduce your request rate.", http.StatusServiceUnavailable)
	ErrInsufficientStorage      = newErr("InsufficientStorage", "Server has insufficient storage to complete the request.", http.StatusInsufficientStorage)
	ErrXAmzContentSHA256Missing = newErr("XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed", http.StatusBadRequest)
)

// writeErr serialises an *S3Error to the response in S3's XML format.
func writeErr(w http.ResponseWriter, r *http.Request, e *S3Error) {
	cp := *e
	cp.Resource = r.URL.Path
	cp.RequestID = requestIDFrom(r)
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", cp.RequestID)
	w.WriteHeader(cp.httpStatus)
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	_ = enc.Encode(&cp)
}
