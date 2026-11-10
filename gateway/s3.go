package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// bucket puts objects into an S3-compatible bucket, signing each request
// with AWS Signature Version 4.
type bucket struct {
	url             *url.URL // the bucket, plus an optional key prefix
	region          string
	accessKeyID     string
	secretAccessKey string
}

func newBucket(l *RequestLog) *bucket {
	u, _ := url.Parse(l.URL) // checked by Config.validate
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""
	return &bucket{url: u, region: l.Region, accessKeyID: l.AccessKeyID, secretAccessKey: l.SecretAccessKey}
}

func (b *bucket) put(ctx context.Context, key string, body []byte, contentType string) error {
	u := *b.url
	u.Path += "/" + key
	u.RawPath = s3Escape(u.Path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	sum := sha256.Sum256(body)
	signV4(req, hex.EncodeToString(sum[:]), b.region, b.accessKeyID, b.secretAccessKey, time.Now())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// As for upstreams, errors name the host but not the path.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e struct{ Code string }
		xml.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		if e.Code != "" {
			return fmt.Errorf("%s (%s)", resp.Status, e.Code)
		}
		return errors.New(resp.Status)
	}
	return nil
}

// signV4 adds the Authorization header of AWS Signature Version 4 for S3,
// signing the host and every header set on req. payloadHash is the hex
// SHA-256 of the body. The request must have no query, and its path must be
// escaped as s3Escape does.
func signV4(req *http.Request, payloadHash, region, accessKeyID, secretAccessKey string, t time.Time) {
	amzDate := t.UTC().Format("20060102T150405Z")
	scope := amzDate[:8] + "/" + region + "/s3/aws4_request"
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	names := []string{"host"}
	for k := range req.Header {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)
	var headers strings.Builder
	for _, name := range names {
		v := req.Host
		if name != "host" {
			v = strings.Join(req.Header.Values(name), ",")
		}
		headers.WriteString(name + ":" + strings.Join(strings.Fields(v), " ") + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{req.Method, req.URL.EscapedPath(), "", headers.String(), signed, payloadHash}, "\n")
	hash := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(hash[:])

	key := []byte("AWS4" + secretAccessKey)
	for _, part := range []string{amzDate[:8], region, "s3", "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKeyID+"/"+scope+
		",SignedHeaders="+signed+",Signature="+hex.EncodeToString(hmacSHA256(key, toSign)))
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// s3Escape percent-encodes every byte of path except the unreserved
// characters and '/', as Signature Version 4 requires for S3.
func s3Escape(path string) string {
	var b strings.Builder
	for _, c := range []byte(path) {
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || strings.IndexByte("-._~/", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
