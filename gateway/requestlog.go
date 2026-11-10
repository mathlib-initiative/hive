package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"
)

// requestLog uploads one record per forwarded request to a bucket. Uploads
// start once the response is complete, so the bucket never delays clients.
type requestLog struct {
	bucket  *bucket
	gzip    bool
	uploads sync.WaitGroup
}

// uploadDelays are the waits before the second and the third attempt.
var uploadDelays = []time.Duration{time.Second, 5 * time.Second}

// record is a request and its response: the request header as the client
// sent it, the request body as far as the upstream read it, and the response
// as the client received it.
type record struct {
	ID         string    `json:"id"`
	Time       time.Time `json:"time"`
	Client     string    `json:"client,omitempty"`
	Method     string    `json:"method"`
	URL        string    `json:"url"`
	Status     int       `json:"status"`
	DurationMS int64     `json:"duration_ms"`
	Note       string    `json:"note,omitempty"`
	Request    message   `json:"request"`
	Response   message   `json:"response"`
}

// message is the header and body of a request or a response. A body that is
// not valid UTF-8, such as a compressed one, is kept in BodyBase64 instead.
type message struct {
	Header     http.Header `json:"header"`
	Body       string      `json:"body,omitempty"`
	BodyBase64 []byte      `json:"body_base64,omitempty"`
}

func newMessage(header http.Header, body []byte) message {
	if utf8.Valid(body) {
		return message{Header: header, Body: string(body)}
	}
	return message{Header: header, BodyBase64: body}
}

// capture collects the request body as the proxy reads it, and the response
// body as the proxy writes it.
type capture struct {
	header http.Header // of the request, without client credentials
	// The transport reads the request body in a goroutine of its own, which
	// may still run after the response is complete.
	mu       sync.Mutex
	request  bytes.Buffer
	response bytes.Buffer
}

// newCapture returns a capture and a copy of r whose body is captured.
func newCapture(r *http.Request) (*capture, *http.Request) {
	c := &capture{header: r.Header.Clone()}
	for _, h := range clientCredentials {
		c.header.Del(h)
	}
	c.header.Del("Proxy-Authorization")
	r = r.Clone(r.Context())
	r.Body = teeBody{r.Body, c}
	return c, r
}

type teeBody struct {
	io.ReadCloser
	c *capture
}

func (t teeBody) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	t.c.mu.Lock()
	t.c.request.Write(p[:n])
	t.c.mu.Unlock()
	return n, err
}

// add uploads the record of a finished request.
func (l *requestLog) add(start time.Time, r *http.Request, w *loggingWriter, note string) {
	c := w.capture
	c.mu.Lock()
	requestBody := bytes.Clone(c.request.Bytes())
	c.mu.Unlock()
	id := make([]byte, 8)
	rand.Read(id)
	rec := &record{
		ID:         hex.EncodeToString(id),
		Time:       start.UTC().Truncate(time.Millisecond),
		Client:     w.client,
		Method:     r.Method,
		URL:        r.URL.RequestURI(),
		Status:     w.status,
		DurationMS: time.Since(start).Milliseconds(),
		Note:       note,
		Request:    newMessage(c.header, requestBody),
		Response:   newMessage(w.Header().Clone(), c.response.Bytes()),
	}
	l.uploads.Go(func() {
		key, data, contentType := l.encode(rec)
		if err := l.put(key, data, contentType); err != nil {
			log.Printf("request log: %s: upload failed %d times, dropped; last error: %v", key, len(uploadDelays)+1, err)
		}
	})
}

// encode returns a record's key, below the bucket URL, and its content:
// [CLIENT/]YYYY-MM-DD/HHMMSS.mmm-ID.json, or .json.gz when compressed.
func (l *requestLog) encode(rec *record) (key string, data []byte, contentType string) {
	key = rec.Time.Format("2006-01-02/150405.000") + "-" + rec.ID + ".json"
	if rec.Client != "" {
		key = rec.Client + "/" + key
	}
	// Prompts and code are full of <, > and &, so leave them unescaped.
	var b bytes.Buffer
	var w io.Writer = &b
	var zw *gzip.Writer
	if l.gzip {
		zw = gzip.NewWriter(&b)
		w = zw
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(rec)
	if zw == nil {
		return key, b.Bytes(), "application/json"
	}
	zw.Close()
	return key + ".gz", b.Bytes(), "application/gzip"
}

func (l *requestLog) put(key string, data []byte, contentType string) error {
	for i := 0; ; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		err := l.bucket.put(ctx, key, data, contentType)
		cancel()
		if err == nil || i == len(uploadDelays) {
			return err
		}
		time.Sleep(uploadDelays[i])
	}
}

// wait waits until every upload has finished, or ctx is done.
func (l *requestLog) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		l.uploads.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
