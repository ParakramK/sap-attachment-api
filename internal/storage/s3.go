package storage

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/parakram/sap-attachment-api/internal/config"
)

// S3 stores objects in an S3 bucket (AWS or any S3-compatible endpoint, e.g.
// MinIO). PutObject/CompleteMultipartUpload make Save atomic.
type S3 struct {
	client *s3.Client
	bucket string
}

func NewS3(cfg *config.Config) (*S3, error) {
	awscfg, err := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithRegion(cfg.S3Region),
	)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	// Explicit keys win; otherwise fall back to the AWS default credential
	// chain (env, shared config, IAM role).
	if cfg.S3AccessKey != "" {
		awscfg.Credentials = credentials.NewStaticCredentialsProvider(
			cfg.S3AccessKey, cfg.S3SecretKey, "")
	}

	client := s3.NewFromConfig(awscfg, func(o *s3.Options) {
		if cfg.S3Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.S3Endpoint)
		}
		o.UsePathStyle = cfg.S3PathStyle
	})

	return &S3{client: client, bucket: cfg.S3Bucket}, nil
}

// Save performs a single atomic PutObject from the staged file.
// shortcut: single PUT caps at 5 GB per object; switch to
// feature/s3/transfermanager if MAX_UPLOAD_MB ever approaches that.
func (s *S3) Save(ctx context.Context, staged *StagedFile, filename, contentType string) (string, error) {
	rel := ObjectPath(staged.SHA256, filename)

	f, err := os.Open(staged.Path)
	if err != nil {
		return "", fmt.Errorf("open staged file: %w", err)
	}
	defer func() { _ = f.Close() }()

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(rel),
		Body:          f,
		ContentLength: &staged.Size,
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("upload object: %w", err)
	}
	return rel, nil
}

func (s *S3) Has(ctx context.Context, rel string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(rel),
	})
	if err != nil {
		if notFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("head object: %w", err)
	}
	return true, nil
}

func (s *S3) Delete(ctx context.Context, rel string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(rel),
	})
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}

func (s *S3) Reference(rel string) string {
	return fmt.Sprintf("s3://%s/%s", s.bucket, rel)
}

// notFound recognizes 404s across the SDK error types and S3-compatible
// implementations that omit the canonical NotFound error body.
func notFound(err error) bool {
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) && re.HTTPStatusCode() == 404 {
		return true
	}
	return false
}
