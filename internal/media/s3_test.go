package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Example from the AWS documentation "Signature Calculations for the Authorization Header:
// Transferring Payload in a Single Chunk", GET Object.
func TestSignV4KnownVector(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	signV4(req, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", "s3",
		emptySHA256, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("signature mismatch\n got %s\nwant %s", got, want)
	}
}

func TestObjectURL(t *testing.T) {
	s := &S3{Endpoint: "https://s3.example.com", Bucket: "b", PathStyle: true}
	u, _ := s.objectURL("1/a b.png", nil)
	if u.String() != "https://s3.example.com/b/1/a%20b.png" {
		t.Fatal(u.String())
	}
	s.PathStyle = false
	u, _ = s.objectURL("1/x.png", nil)
	if u.String() != "https://b.s3.example.com/1/x.png" {
		t.Fatal(u.String())
	}
}

// TestS3Integration runs against a real bucket when MINO_TEST_S3_ENDPOINT etc. are set, e.g. MinIO:
//
//	docker run -d -p 9100:9000 -e MINIO_ROOT_USER=minio -e MINIO_ROOT_PASSWORD=minio123 minio/minio server /data
func TestS3Integration(t *testing.T) {
	endpoint := os.Getenv("MINO_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINO_TEST_S3_ENDPOINT not set")
	}
	s := &S3{Endpoint: endpoint, Region: "us-east-1", Bucket: os.Getenv("MINO_TEST_S3_BUCKET"),
		AccessKey: os.Getenv("MINO_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("MINO_TEST_S3_SECRET_KEY"),
		PublicURL: endpoint + "/" + os.Getenv("MINO_TEST_S3_BUCKET"), PathStyle: true}
	ctx := context.Background()
	body := []byte("hello mino")
	sum := sha256.Sum256(body)
	for _, key := range []string{"t1/a.txt", "t1/b.txt", "t2/c.txt"} {
		if err := s.Put(ctx, key, bytes.NewReader(body), int64(len(body)), "text/plain", hex.EncodeToString(sum[:])); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := s.do(ctx, http.MethodGet, "t1/a.txt", nil, nil, 0, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "hello mino" {
		t.Fatalf("read back %q", got)
	}
	if err := s.DeletePrefix(ctx, "t1/"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.do(ctx, http.MethodGet, "t1/a.txt", nil, nil, 0, "", nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("object survived DeletePrefix: %v", err)
	}
	if err := s.Delete(ctx, "t2/c.txt"); err != nil {
		t.Fatal(err)
	}
}
