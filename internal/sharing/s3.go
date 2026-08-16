package sharing

// Real ObjectStore over an S3-compatible API (AWS S3, Cloudflare R2, Backblaze
// B2, MinIO), for the bucket share transport. Pure Go (aws-sdk-go-v2), so the
// CGO=0 static-binary invariant holds. Credentials are read from the environment,
// never persisted in the ledger.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
)

// s3ReadCeiling bounds any single object read defensively (blob or manifest); the
// transport re-validates real sizes downstream. A max-size manifest
// (MaxManifestBytes) age-encrypted and base64-wrapped in the envelope JSON
// grows ~1.4x, so the ceiling keeps generous headroom over both that and a max
// blob — set too low, a legit max manifest would silently truncate to a parse error.
const s3ReadCeiling = MaxManifestBytes*2 + MaxMemoryBytes

type S3Store struct {
	client *s3.Client
	bucket string
}

// NewObjectStore builds the real store from a BucketConfig. Region defaults to
// "auto" (R2); a custom Endpoint + path-style addressing makes R2/B2/MinIO work.
func NewObjectStore(cfg BucketConfig) (ObjectStore, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("share bucket: no bucket configured")
	}
	ak, sk, err := BucketCredentials(cfg)
	if err != nil {
		return nil, err
	}
	region := cfg.Region
	if region == "" {
		region = "auto"
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(ak, sk, "")),
	)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = true
	})
	return &S3Store{client: client, bucket: cfg.Bucket}, nil
}

// BucketCredentials reads the access/secret key from the environment. SecretRef
// names the env prefix (default MORA_SHARE); standard AWS_* vars are a fallback.
func BucketCredentials(cfg BucketConfig) (string, string, error) {
	prefix := cfg.SecretRef
	if prefix == "" {
		prefix = "MORA_SHARE"
	}
	ak := os.Getenv(prefix + "_ACCESS_KEY_ID")
	sk := os.Getenv(prefix + "_SECRET_ACCESS_KEY")
	if ak == "" {
		ak = os.Getenv("AWS_ACCESS_KEY_ID")
	}
	if sk == "" {
		sk = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	if ak == "" || sk == "" {
		return "", "", fmt.Errorf("share bucket %q: missing credentials — set %[2]s_ACCESS_KEY_ID and %[2]s_SECRET_ACCESS_KEY (or AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY) in the environment", cfg.Bucket, prefix)
	}
	return ak, sk, nil
}

func (s *S3Store) PutObject(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	return err
}

func (s *S3Store) GetObject(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if IsNotFoundErr(err) {
			return nil, ErrObjectNotFound
		}
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(io.LimitReader(out.Body, s3ReadCeiling))
}

func (s *S3Store) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			if o.Key != nil {
				out = append(out, *o.Key)
			}
		}
	}
	return out, nil
}

func (s *S3Store) DeleteObject(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	return err
}

// IsNotFoundErr recognizes a missing-key error across AWS S3 (typed NoSuchKey /
// NotFound) and S3-compatible providers that only set an API error code.
func IsNotFoundErr(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	// S3-compatible providers that only set an HTTP 404 (no typed / coded error).
	var statusErr interface{ HTTPStatusCode() int }
	if errors.As(err, &statusErr) && statusErr.HTTPStatusCode() == 404 {
		return true
	}
	return false
}
