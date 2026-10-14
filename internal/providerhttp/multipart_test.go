package providerhttp

import (
	"bytes"
	"crypto/sha256"
	"mime/multipart"
	"testing"
)

func TestMultipartBoundaryIsStableAndValid(t *testing.T) {
	a := MultipartBoundary([]byte("model"), []byte("data"))
	if a != MultipartBoundary([]byte("model"), []byte("data")) {
		t.Fatal("same values gave different boundaries")
	}
	if a == MultipartBoundary([]byte("modeld"), []byte("ata")) {
		t.Fatal("value lengths do not affect the boundary")
	}
	if err := multipart.NewWriter(&bytes.Buffer{}).SetBoundary(a); err != nil {
		t.Fatalf("invalid boundary %q: %v", a, err)
	}
}

func TestMultipartBoundarySkipsCandidateFoundInValue(t *testing.T) {
	sum := sha256.Sum256([]byte("seed"))
	first := boundaryFrom(sum[:], nil)
	value := []byte("before " + first + " after")
	got := boundaryFrom(sum[:], [][]byte{value})
	if got == first || bytes.Contains(value, []byte(got)) {
		t.Fatalf("boundary %q occurs in its value", got)
	}
}
