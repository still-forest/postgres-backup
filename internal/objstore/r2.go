package objstore

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// R2Options configures a Cloudflare R2 client.
type R2Options struct {
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	// HostBase is the account endpoint, https://<account>.r2.cloudflarestorage.com.
	HostBase string
}

// R2 is a Store backed by Cloudflare R2 over the S3 API.
type R2 struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
}

// NewR2 builds an R2-backed Store.
func NewR2(ctx context.Context, o R2Options) (*R2, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		// R2 has no regions; the SDK requires one for SigV4 signing and
		// "auto" is what Cloudflare specifies. The DEFAULT_REGION value the
		// previous container passed ("apac") is a jurisdiction hint used at
		// bucket creation, never a signing region.
		awsconfig.WithRegion("auto"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(o.AccessKeyID, o.SecretAccessKey, ""),
		),
		// R2 rejects requests carrying more than one non-default checksum, which
		// is what the SDK sends by default. This mirrors the same fix already
		// applied on the Rails side in api/config/storage.yml. Use the named
		// constants: these are an int enum, and a literal 0 would silently mean
		// "unset" and restore the broken default.
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(opt *s3.Options) {
		opt.BaseEndpoint = aws.String(o.HostBase)
		// R2's certificate covers *.r2.cloudflarestorage.com, so virtual-host
		// addressing would produce a two-label subdomain the wildcard misses.
		opt.UsePathStyle = true
	})

	// s3/manager is deprecated in favour of feature/s3/transfermanager, which
	// is still v0.x. For the component that stores backups, the stable v1 API
	// is the better trade; revisit once transfermanager reaches 1.0.
	up := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = 16 * 1024 * 1024
		u.Concurrency = 4
		// R2 bills storage for the parts of an abandoned multipart upload.
		u.LeavePartsOnError = false
	})

	return &R2{client: client, uploader: up, bucket: o.Bucket}, nil
}

// Upload streams r to key. Multipart is handled by the SDK's uploader.
func (r *R2) Upload(ctx context.Context, key string, body io.Reader, meta map[string]string) error {
	_, err := r.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:   aws.String(r.bucket),
		Key:      aws.String(key),
		Body:     body,
		Metadata: meta,
		// ChecksumAlgorithm is deliberately unset; combined with
		// RequestChecksumCalculationWhenRequired above, that is what keeps R2 happy.
	})
	if err != nil {
		return fmt.Errorf("uploading %s: %w", key, err)
	}
	return nil
}

// List enumerates every object under prefix.
//
// A paging error aborts the whole listing. Callers use the result to decide
// what to delete, so returning a partial list that looks complete would cause
// data loss.
func (r *R2) List(ctx context.Context, prefix string) ([]Object, error) {
	p := s3.NewListObjectsV2Paginator(r.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(r.bucket),
		Prefix: aws.String(prefix),
	})

	var out []Object
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", prefix, err)
		}
		for _, o := range page.Contents {
			out = append(out, Object{
				Key:          aws.ToString(o.Key),
				Size:         aws.ToInt64(o.Size),
				LastModified: aws.ToTime(o.LastModified),
			})
		}
	}
	return out, nil
}

// Download writes the object at key to w.
func (r *R2) Download(ctx context.Context, key string, w io.Writer) error {
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("downloading %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()

	if _, err := io.Copy(w, out.Body); err != nil {
		return fmt.Errorf("reading %s: %w", key, err)
	}
	return nil
}

// Head returns an object's size and user metadata.
func (r *R2) Head(ctx context.Context, key string) (Object, map[string]string, error) {
	out, err := r.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return Object{}, nil, fmt.Errorf("head %s: %w", key, err)
	}
	return Object{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		LastModified: aws.ToTime(out.LastModified),
	}, out.Metadata, nil
}

// Delete removes one object.
func (r *R2) Delete(ctx context.Context, key string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("deleting %s: %w", key, err)
	}
	return nil
}

// CheckAccess verifies the credentials can reach the bucket, without creating
// anything. The container this replaces ran `s3cmd ls || s3cmd mb`, which turned
// a bucket-scoped key into an attempted bucket creation and a fatal error.
func (r *R2) CheckAccess(ctx context.Context) error {
	_, err := r.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(r.bucket)})
	if err != nil {
		return fmt.Errorf("bucket %q is not reachable with the configured credentials: %w", r.bucket, err)
	}
	return nil
}
