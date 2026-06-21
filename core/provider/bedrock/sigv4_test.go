package bedrock

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSigV4Vector validates the signer against AWS's well-known SigV4 test
// vector (the "get-vanilla" example from the AWS aws-sig-v4-test-suite):
//
//	access key:  AKIDEXAMPLE
//	secret key:  wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY
//	region:      us-east-1
//	service:     service
//	host:        example.amazonaws.com
//	time:        20150830T123600Z
//	method:      GET, path "/", no query, signed headers host;x-amz-date
//
// The signature published in the official AWS test suite for these exact inputs
// is:
//
//	5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31
//
// Matching it proves the canonical-request / string-to-sign / key-derivation /
// signature chain is correct independent of Bedrock.
//
// NOTE: the task brief quoted the constant
// 5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7 for these
// inputs. That value does not reproduce from the documented get-vanilla
// parameters under any standard canonicalization variant I could construct
// (host-only, host+x-amz-date, POST, content-type, query-bearing, etc.), so it
// appears to be a transcription error in the brief. The signer here is verified
// against the genuine, independently reproducible AWS get-vanilla signature
// above (also computable with botocore / aws-sig-v4-test-suite fixtures).
func TestSigV4Vector(t *testing.T) {
	creds := credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}
	signTime := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

	req, err := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.amazonaws.com"

	signRequest(req, nil, creds, "us-east-1", "service", signTime)

	auth := req.Header.Get("Authorization")
	const wantSig = "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if !strings.Contains(auth, "Signature="+wantSig) {
		t.Fatalf("signature mismatch\n got auth: %s\n want signature: %s", auth, wantSig)
	}

	// Sanity-check the rest of the Authorization header structure.
	wantCred := "Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request"
	if !strings.Contains(auth, wantCred) {
		t.Errorf("credential scope mismatch: %s", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=host;x-amz-date") {
		t.Errorf("signed headers mismatch: %s", auth)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
		t.Errorf("X-Amz-Date = %q, want 20150830T123600Z", got)
	}
}

// TestSigV4SessionToken verifies temporary credentials add the security-token
// header and that it participates in SignedHeaders.
func TestSigV4SessionToken(t *testing.T) {
	creds := credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		SessionToken:    "FAKE-SESSION-TOKEN",
	}
	req, err := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/invoke", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	signRequest(req, []byte("{}"), creds, "us-east-1", "bedrock", time.Now())

	if got := req.Header.Get("X-Amz-Security-Token"); got != "FAKE-SESSION-TOKEN" {
		t.Fatalf("missing session token header: %q", got)
	}
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "x-amz-security-token") {
		t.Errorf("session token not in signed headers: %s", auth)
	}
}

// TestSigV4SigningKeyCache verifies that two signings on the same (date, region,
// service, secret) reuse the cached derived key (one miss), while a different
// date forces a recompute (another miss). The cached key MUST NOT change the
// resulting signature (covered by TestSigV4Vector).
func TestSigV4SigningKeyCache(t *testing.T) {
	signingKeyCache = sync.Map{}
	atomic.StoreInt64(&signingKeyCacheMiss, 0)

	creds := credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}
	mkReq := func() *http.Request {
		req, err := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "example.amazonaws.com"
		return req
	}

	day1 := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	signRequest(mkReq(), nil, creds, "us-east-1", "service", day1)
	signRequest(mkReq(), nil, creds, "us-east-1", "service", day1.Add(2*time.Hour))

	if got := atomic.LoadInt64(&signingKeyCacheMiss); got != 1 {
		t.Fatalf("same-date signings should reuse cached key: misses = %d, want 1", got)
	}

	// A different date must recompute (a second miss).
	day2 := day1.AddDate(0, 0, 1)
	signRequest(mkReq(), nil, creds, "us-east-1", "service", day2)
	if got := atomic.LoadInt64(&signingKeyCacheMiss); got != 2 {
		t.Fatalf("different date should recompute: misses = %d, want 2", got)
	}
}
