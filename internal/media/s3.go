package media

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// S3 stores files in an S3-compatible bucket (AWS S3, Cloudflare R2, MinIO, Backblaze B2, ...).
// It implements the three operations the media library needs with AWS Signature V4, avoiding
// a large SDK dependency.
type S3 struct {
	Endpoint  string // scheme://host[:port]
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	PublicURL string // base URL for downloads
	PathStyle bool
	Client    *http.Client
	now       func() time.Time
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (s *S3) Name() string { return "s3" }

func (s *S3) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

func (s *S3) objectURL(key string, query url.Values) (*url.URL, error) {
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid S3 endpoint %q", s.Endpoint)
	}
	if s.PathStyle {
		u.Path = "/" + s.Bucket + "/" + key
	} else {
		u.Host = s.Bucket + "." + u.Host
		u.Path = "/" + key
	}
	u.RawPath = s3EscapePath(u.Path)
	u.RawQuery = canonicalQuery(query)
	return u, nil
}

func (s *S3) do(ctx context.Context, method, key string, query url.Values, body io.Reader, size int64, payloadHash string, header http.Header) (*http.Response, error) {
	u, err := s.objectURL(key, query)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	for k, v := range header {
		req.Header[k] = v
	}
	if payloadHash == "" {
		payloadHash = emptySHA256
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	signV4(req, s.AccessKey, s.SecretKey, s.Region, "s3", payloadHash, now())
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("s3 %s %s: %s: %s", method, key, resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

func (s *S3) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType, sha256Hex string) error {
	h := http.Header{}
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "public, max-age=31536000, immutable") // keys are random and never reused
	resp, err := s.do(ctx, http.MethodPut, key, nil, body, size, sha256Hex, h)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, key, nil, nil, 0, "", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

type listResult struct {
	Contents []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// DeletePrefix lists and deletes objects one by one. It only runs when a whole site is deleted.
func (s *S3) DeletePrefix(ctx context.Context, prefix string) error {
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, http.MethodGet, "", q, nil, 0, "", nil)
		if err != nil {
			return err
		}
		var res listResult
		err = xml.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("s3 list: %w", err)
		}
		for _, c := range res.Contents {
			if err := s.Delete(ctx, c.Key); err != nil {
				return err
			}
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			return nil
		}
		token = res.NextContinuationToken
	}
}

func (s *S3) URL(key string) string {
	return s.PublicURL + "/" + s3EscapePath(key)
}

// signV4 adds AWS Signature Version 4 headers. Every header already set on req is signed,
// plus Host, X-Amz-Date and X-Amz-Content-Sha256.
func signV4(req *http.Request, accessKey, secretKey, region, service, payloadHash string, t time.Time) {
	t = t.UTC()
	amzDate := t.Format("20060102T150405Z")
	date := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.TrimSpace(strings.Join(v, ","))
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{
		req.Method, path, canonicalQuery(req.URL.Query()), canonHeaders.String(), signed, payloadHash,
	}, "\n")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	key := hmacSHA256([]byte("AWS4"+secretKey), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+sig)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// awsEscape percent-encodes everything except unreserved characters (RFC 3986), as SigV4 requires.
func awsEscape(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' || (keepSlash && c == '/') {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func s3EscapePath(p string) string { return awsEscape(p, true) }

func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, awsEscape(k, false)+"="+awsEscape(v, false))
		}
	}
	return strings.Join(parts, "&")
}
