package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type S3Store struct {
	client *s3.Client
	bucket string
}

type S3Config struct {
	Region    string
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	PathStyle bool
}

func NewS3Store(ctx context.Context, cfg S3Config) (*S3Store, error) {
	if strings.TrimSpace(cfg.Region) == "" {
		cfg.Region = "ap-south-1"
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, fmt.Errorf("s3 bucket is required")
	}

	loadOptions := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
	}
	if cfg.AccessKey != "" || cfg.SecretKey != "" {
		loadOptions = append(loadOptions, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.PathStyle
	})
	return &S3Store{
		client: client,
		bucket: cfg.Bucket,
	}, nil
}

func (s *S3Store) EnsureBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	if err == nil {
		return nil
	}
	var notFound *types.NotFound
	if err != nil && !strings.Contains(err.Error(), "NotFound") && !strings.Contains(err.Error(), "404") && !strings.Contains(err.Error(), "NoSuchBucket") && !errorAs(err, &notFound) {
		return err
	}
	_, err = s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.bucket)})
	return err
}

func (s *S3Store) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	return err
}

func (s *S3Store) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isObjectNotFound(err) {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, key)
		}
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func errorAs(err error, target any) bool {
	type aser interface {
		As(any) bool
	}
	if v, ok := err.(aser); ok {
		return v.As(target)
	}
	return false
}

func isObjectNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errorAs(err, &noSuchKey) {
		return true
	}
	message := err.Error()
	return errors.Is(err, ErrNotFound) ||
		strings.Contains(message, "NoSuchKey") ||
		strings.Contains(message, "NotFound") ||
		strings.Contains(message, "404")
}
