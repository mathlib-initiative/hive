package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSignV4 checks the example "PUT Object" from the Amazon S3 API
// reference, Signature Version 4 with the Authorization header.
func TestSignV4(t *testing.T) {
	req, _ := http.NewRequest("PUT", "https://examplebucket.s3.amazonaws.com/test%24file.text", strings.NewReader("Welcome to Amazon S3."))
	req.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
	req.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
	signV4(req, "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072", "us-east-1",
		"AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))

	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
		"SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class," +
		"Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization\n got %s\nwant %s", got, want)
	}
}

func TestS3Escape(t *testing.T) {
	for in, want := range map[string]string{
		"/bucket/a-b_c.d~e/f.json": "/bucket/a-b_c.d~e/f.json",
		"/test$file.text":          "/test%24file.text",
		"/a b/ü+=:@":               "/a%20b/%C3%BC%2B%3D%3A%40",
	} {
		if got := s3Escape(in); got != want {
			t.Errorf("s3Escape(%q) = %q, want %q", in, got, want)
		}
	}
}
