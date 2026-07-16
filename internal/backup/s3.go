package backup

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Config describes an S3-compatible destination. Endpoint is optional for
// AWS and required for self-hosted endpoints such as MinIO.
type S3Config struct {
	Endpoint, Region, Bucket, Prefix, AccessKey, SecretKey string
}

func (c S3Config) Validate() error {
	if strings.TrimSpace(c.Bucket) == "" {
		return fmt.Errorf("backup S3 bucket is required")
	}
	if strings.TrimSpace(c.AccessKey) == "" || strings.TrimSpace(c.SecretKey) == "" {
		return fmt.Errorf("backup S3 credentials are required")
	}
	return nil
}

// UploadS3 sends content directly to object storage. The reader is consumed by
// the HTTP transport; no staging file is created on the MiniDock host.
func UploadS3(ctx context.Context, cfg S3Config, key string, body io.Reader) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	options := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	}
	if cfg.Endpoint != "" {
		options = append(options, awsconfig.WithBaseEndpoint(cfg.Endpoint))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return fmt.Errorf("load S3 configuration: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) { o.UsePathStyle = cfg.Endpoint != "" })
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:               aws.String(cfg.Bucket),
		Key:                  aws.String(strings.TrimPrefix(strings.TrimSuffix(cfg.Prefix, "/")+"/"+key, "/")),
		Body:                 body,
		ContentType:          aws.String("application/octet-stream"),
		ServerSideEncryption: "AES256",
	})
	if err != nil {
		return fmt.Errorf("upload backup to S3: %w", err)
	}
	return nil
}
