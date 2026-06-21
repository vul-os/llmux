package bedrock

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AWS Signature Version 4 signing, implemented with stdlib only. This is a
// focused signer for POST requests to a single service ("bedrock"), but the
// canonicalization follows the general SigV4 algorithm so it also validates
// against AWS's published GET test vectors (see sigv4_test.go).

// credentials holds the values read from the AWS environment.
type credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // optional (temporary credentials)
}

const (
	sigV4Algorithm = "AWS4-HMAC-SHA256"
	awsRequest     = "aws4_request"
)

// signRequest signs an *http.Request in place by adding the Authorization header
// (and X-Amz-Date, plus X-Amz-Security-Token when a session token is present).
//
// body is the exact request body bytes (used for the payload hash). signTime is
// passed in explicitly rather than read from the clock so signing is
// deterministic and unit-testable against known vectors.
func signRequest(req *http.Request, body []byte, creds credentials, region, service string, signTime time.Time) {
	signTime = signTime.UTC()
	amzDate := signTime.Format("20060102T150405Z")
	dateStamp := signTime.Format("20060102")

	// Required headers that participate in the signature.
	req.Header.Set("X-Amz-Date", amzDate)
	if req.Header.Get("Host") == "" && req.Host == "" {
		req.Host = req.URL.Host
	}
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	payloadHash := hexSHA256(body)

	canonicalHeaders, signedHeaders := canonicalizeHeaders(req)

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := strings.Join([]string{dateStamp, region, service, awsRequest}, "/")

	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		credentialScope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signingKey := signingKeyCacheGet(creds.SecretAccessKey, dateStamp, region, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authorization := sigV4Algorithm +
		" Credential=" + creds.AccessKeyID + "/" + credentialScope +
		", SignedHeaders=" + signedHeaders +
		", Signature=" + signature
	req.Header.Set("Authorization", authorization)
}

// canonicalURI returns the URI-encoded, normalized path. AWS expects each path
// segment to be RFC 3986 encoded (but the path separators preserved). For an
// empty path, "/" is used.
func canonicalURI(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

// canonicalQuery returns the canonical query string: params sorted by key (then
// value) and URI-encoded per RFC 3986.
func canonicalQuery(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	values := u.Query()
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		vs := values[k]
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, awsURIEncode(k, true)+"="+awsURIEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// canonicalizeHeaders builds the canonical-headers block and the signed-headers
// list. All header names are lowercased; values are trimmed and internal runs of
// whitespace collapsed. The Host header is sourced from the request URL/Host.
func canonicalizeHeaders(req *http.Request) (canonical, signed string) {
	type hv struct {
		name   string
		values []string
	}
	collected := map[string][]string{}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	collected["host"] = []string{host}

	for name, vals := range req.Header {
		lower := strings.ToLower(name)
		// Skip headers that must not be signed for this signer's scope.
		if lower == "authorization" {
			continue
		}
		for _, v := range vals {
			collected[lower] = append(collected[lower], v)
		}
	}

	headers := make([]hv, 0, len(collected))
	for name, vals := range collected {
		headers = append(headers, hv{name: name, values: vals})
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].name < headers[j].name })

	var cb strings.Builder
	names := make([]string, 0, len(headers))
	for _, h := range headers {
		trimmed := make([]string, len(h.values))
		for i, v := range h.values {
			trimmed[i] = collapseWhitespace(v)
		}
		cb.WriteString(h.name)
		cb.WriteByte(':')
		cb.WriteString(strings.Join(trimmed, ","))
		cb.WriteByte('\n')
		names = append(names, h.name)
	}
	return cb.String(), strings.Join(names, ";")
}

// collapseWhitespace trims and collapses internal whitespace runs to single
// spaces, per the SigV4 header-value canonicalization rules.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// signingKeyCache memoizes derived SigV4 signing keys, which are stable per
// (dateStamp, region, service, secret) but otherwise recomputed (4 HMACs) on
// every request. Keys are derived once per day per scope. The cache key hashes
// the secret rather than storing it in cleartext.
var signingKeyCache sync.Map // map[string][]byte

// signingKeyCacheMiss counts cache misses (compute+store). Test-only hook; reads
// MUST NOT affect signing behavior.
var signingKeyCacheMiss int64

// signingKeyCacheGet returns the cached signing key for the scope, computing and
// storing it on a miss. The returned slice MUST NOT be mutated by callers.
func signingKeyCacheGet(secret, dateStamp, region, service string) []byte {
	secretHash := sha256.Sum256([]byte(secret))
	cacheKey := dateStamp + "|" + region + "|" + service + "|" + hex.EncodeToString(secretHash[:])
	if v, ok := signingKeyCache.Load(cacheKey); ok {
		return v.([]byte)
	}
	key := deriveSigningKey(secret, dateStamp, region, service)
	actual, loaded := signingKeyCache.LoadOrStore(cacheKey, key)
	if !loaded {
		atomic.AddInt64(&signingKeyCacheMiss, 1)
	}
	return actual.([]byte)
}

// deriveSigningKey computes the SigV4 signing key via the nested-HMAC chain.
func deriveSigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte(awsRequest))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// awsURIEncode implements AWS's RFC 3986 percent-encoding. Unreserved
// characters (A-Z a-z 0-9 - _ . ~) pass through; everything else is encoded.
// When encodeSlash is false, "/" is left unescaped (used for path segments).
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			const hexUpper = "0123456789ABCDEF"
			b.WriteByte(hexUpper[c>>4])
			b.WriteByte(hexUpper[c&0xf])
		}
	}
	return b.String()
}
