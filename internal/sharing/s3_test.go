package sharing

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type statusError struct{ code int }

func (e statusError) Error() string       { return "status" }
func (e statusError) HTTPStatusCode() int { return e.code }
func clearBucketEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"MORA_SHARE_ACCESS_KEY_ID", "MORA_SHARE_SECRET_ACCESS_KEY", "CUSTOM_ACCESS_KEY_ID", "CUSTOM_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		t.Setenv(key, "")
	}
}
func TestBucketCredentialsPrecedenceAndFallback(t *testing.T) {
	clearBucketEnv(t)
	cfg := BucketConfig{Bucket: "b", SecretRef: "CUSTOM"}
	t.Setenv("CUSTOM_ACCESS_KEY_ID", "custom-ak")
	t.Setenv("CUSTOM_SECRET_ACCESS_KEY", "custom-sk")
	t.Setenv("AWS_ACCESS_KEY_ID", "aws-ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-sk")
	ak, sk, err := BucketCredentials(cfg)
	if err != nil || ak != "custom-ak" || sk != "custom-sk" {
		t.Fatalf("custom=(%q,%q,%v)", ak, sk, err)
	}
	t.Setenv("CUSTOM_ACCESS_KEY_ID", "")
	t.Setenv("CUSTOM_SECRET_ACCESS_KEY", "")
	ak, sk, err = BucketCredentials(cfg)
	if err != nil || ak != "aws-ak" || sk != "aws-sk" {
		t.Fatalf("fallback=(%q,%q,%v)", ak, sk, err)
	}
}
func TestBucketCredentialsMissingAndStoreConfig(t *testing.T) {
	clearBucketEnv(t)
	if _, _, err := BucketCredentials(BucketConfig{Bucket: "b"}); err == nil || !strings.Contains(err.Error(), "MORA_SHARE_ACCESS_KEY_ID") {
		t.Fatalf("missing error=%v", err)
	}
	if _, err := NewObjectStore(BucketConfig{}); err == nil || !strings.Contains(err.Error(), "no bucket") {
		t.Fatalf("empty bucket error=%v", err)
	}
	if _, err := NewObjectStore(BucketConfig{Bucket: "b"}); err == nil || !strings.Contains(err.Error(), "missing credentials") {
		t.Fatalf("store credentials error=%v", err)
	}
}
func TestIsNotFoundErr(t *testing.T) {
	for _, err := range []error{&types.NoSuchKey{}, &types.NotFound{}, &smithy.GenericAPIError{Code: "NoSuchKey"}, &smithy.GenericAPIError{Code: "NotFound"}, statusError{404}} {
		if !IsNotFoundErr(err) {
			t.Errorf("did not recognize %T", err)
		}
	}
	for _, err := range []error{errors.New("other"), &smithy.GenericAPIError{Code: "AccessDenied"}, statusError{500}} {
		if IsNotFoundErr(err) {
			t.Errorf("false positive %T", err)
		}
	}
}

func TestS3StoreOperationsAgainstCompatibleEndpoint(t *testing.T) {
	clearBucketEnv(t)
	t.Setenv("MORA_SHARE_ACCESS_KEY_ID", "ak")
	t.Setenv("MORA_SHARE_SECRET_ACCESS_KEY", "sk")
	var puts, deletes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			puts++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete:
			deletes++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsTruncated>false</IsTruncated><Contents><Key>pre/a.age</Key></Contents><Contents><Key>pre/b.age</Key></Contents></ListBucketResult>`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/missing"):
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte("payload"))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	store, err := NewObjectStore(BucketConfig{Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.PutObject(ctx, "key", []byte("x")); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetObject(ctx, "key")
	if err != nil || string(got) != "payload" {
		t.Fatalf("get=%q err=%v", got, err)
	}
	if _, err := store.GetObject(ctx, "missing"); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("missing error=%v", err)
	}
	keys, err := store.ListKeys(ctx, "pre/")
	if err != nil || !reflect.DeepEqual(keys, []string{"pre/a.age", "pre/b.age"}) {
		t.Fatalf("keys=%v err=%v", keys, err)
	}
	if err := store.DeleteObject(ctx, "key"); err != nil {
		t.Fatal(err)
	}
	if puts != 1 || deletes != 1 {
		t.Fatalf("puts=%d deletes=%d", puts, deletes)
	}
}
