// Package sigv4 verifies AWS Signature Version 4 request signatures
// (header-based signing only) against a store of access keys.
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

const (
	algorithm  = "AWS4-HMAC-SHA256"
	timeFormat = "20060102T150405Z"
	service    = "dynamodb"
)

// Verifier authenticates requests; it implements api.Authenticator.
type Verifier struct {
	Keys    Keys
	Region  string           // "" accepts any region in the credential scope
	Now     func() time.Time // nil means time.Now
	MaxSkew time.Duration    // 0 means 5 minutes
}

func (v *Verifier) Authenticate(r *http.Request, body []byte) error {
	auth := r.Header.Get("Authorization")
	if auth == "" || !strings.HasPrefix(auth, algorithm+" ") {
		return ddberr.MissingAuthenticationToken()
	}
	badSig := ddberr.InvalidSignature()

	// Credential=..., SignedHeaders=..., Signature=...
	var cred, signed, sig string
	for _, part := range strings.Split(strings.TrimPrefix(auth, algorithm+" "), ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return badSig
		}
		switch k {
		case "Credential":
			cred = val
		case "SignedHeaders":
			signed = val
		case "Signature":
			sig = val
		}
	}
	if cred == "" || signed == "" || sig == "" {
		return badSig
	}
	scope := strings.Split(cred, "/")
	if len(scope) != 5 || scope[0] == "" || scope[4] != "aws4_request" {
		return badSig
	}
	keyID, date, region, svc := scope[0], scope[1], scope[2], scope[3]

	secret, ok := v.Keys.Secret(keyID)
	if !ok {
		return ddberr.UnrecognizedClient()
	}
	if svc != service || (v.Region != "" && region != v.Region) {
		return badSig
	}

	// Time check.
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	now = now.UTC()
	skew := v.MaxSkew
	if skew == 0 {
		skew = 5 * time.Minute
	}
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		return badSig
	}
	t, err := time.Parse(timeFormat, amzDate)
	if err != nil {
		return badSig
	}
	mins := int(skew / time.Minute)
	if t.Before(now.Add(-skew)) {
		return &ddberr.Error{Type: badSig.Type, Status: 400, Message: fmt.Sprintf(
			"Signature expired: %s is now earlier than %s (%s - %d min.)",
			amzDate, now.Add(-skew).Format(timeFormat), now.Format(timeFormat), mins)}
	}
	if t.After(now.Add(skew)) {
		return &ddberr.Error{Type: badSig.Type, Status: 400, Message: fmt.Sprintf(
			"Signature not yet current: %s is still later than %s (%s + %d min.)",
			amzDate, now.Add(skew).Format(timeFormat), now.Format(timeFormat), mins)}
	}
	if date != t.Format("20060102") {
		return badSig
	}

	names := strings.Split(signed, ";")
	hasHost, hasDate := false, false
	var ch strings.Builder
	for _, n := range names {
		if n == "" || n != strings.ToLower(n) {
			return badSig
		}
		var val string
		if n == "host" {
			hasHost = true
			if r.Host == "" {
				return badSig
			}
			val = r.Host
		} else {
			vals, ok := r.Header[http.CanonicalHeaderKey(n)]
			if !ok {
				return badSig
			}
			val = strings.Join(vals, ",")
		}
		if n == "x-amz-date" {
			hasDate = true
		}
		ch.WriteString(n + ":" + collapse(val) + "\n")
	}
	if !hasHost || !hasDate {
		return badSig
	}

	sum := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(sum[:])
	payload := bodyHash
	if h := r.Header.Get("X-Amz-Content-Sha256"); h != "" {
		if h != bodyHash {
			return badSig
		}
		payload = h
	}

	uri := "/"
	if r.URL != nil {
		if p := r.URL.EscapedPath(); p != "" {
			uri = p
		}
	}
	query := ""
	if r.URL != nil {
		query = canonicalQuery(r.URL.Query())
	}
	canon := strings.Join([]string{r.Method, uri, query, ch.String(), signed, payload}, "\n")
	// ch.String() ends in "\n", so the join yields the blank line the spec requires.

	csum := sha256.Sum256([]byte(canon))
	sts := algorithm + "\n" + amzDate + "\n" + strings.Join(scope[1:], "/") + "\n" + hex.EncodeToString(csum[:])

	k := hmacSHA([]byte("AWS4"+secret), date)
	k = hmacSHA(k, region)
	k = hmacSHA(k, svc)
	k = hmacSHA(k, "aws4_request")
	want := hex.EncodeToString(hmacSHA(k, sts))
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(sig))) {
		return badSig
	}
	return nil
}

func hmacSHA(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func canonicalQuery(q url.Values) string {
	var parts []string
	for k, vs := range q {
		for _, v := range vs {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func awsEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(url.QueryEscape(s), "+", "%20"), "%7E", "~")
}
