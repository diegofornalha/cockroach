// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package cloudimpl

import (
	"context"
	"io"
	"net/url"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/storage/cloud"
	"github.com/cockroachdb/cockroach/pkg/util/contextutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
)

type s3Storage struct {
	bucket   *string
	conf     *roachpb.ExternalStorage_S3
	ioConf   base.ExternalIODirConfig
	prefix   string
	opts     session.Options
	settings *cluster.Settings
}

var _ cloud.ExternalStorage = &s3Storage{}

type serverSideEncMode string

const (
	kmsEnc    serverSideEncMode = "aws:kms"
	aes256Enc serverSideEncMode = "AES256"
)

// S3URI returns the string URI for a given bucket and path.
func S3URI(bucket, path string, conf *roachpb.ExternalStorage_S3) string {
	q := make(url.Values)
	setIf := func(key, value string) {
		if value != "" {
			q.Set(key, value)
		}
	}
	setIf(AWSAccessKeyParam, conf.AccessKey)
	setIf(AWSSecretParam, conf.Secret)
	setIf(AWSTempTokenParam, conf.TempToken)
	setIf(AWSEndpointParam, conf.Endpoint)
	setIf(S3RegionParam, conf.Region)
	setIf(AuthParam, conf.Auth)
	setIf(AWSServerSideEncryptionMode, conf.ServerEncMode)
	setIf(AWSServerSideEncryptionKMSID, conf.ServerKMSID)

	s3URL := url.URL{
		Scheme:   "s3",
		Host:     bucket,
		Path:     path,
		RawQuery: q.Encode(),
	}

	return s3URL.String()
}

// MakeS3Storage returns an instance of S3 ExternalStorage.
func MakeS3Storage(
	ctx context.Context,
	ioConf base.ExternalIODirConfig,
	conf *roachpb.ExternalStorage_S3,
	settings *cluster.Settings,
) (cloud.ExternalStorage, error) {
	if conf == nil {
		return nil, errors.Errorf("s3 upload requested but info missing")
	}
	config := conf.Keys()
	if conf.Endpoint != "" {
		if ioConf.DisableHTTP {
			return nil, errors.New(
				"custom endpoints disallowed for s3 due to --external-io-disable-http flag")
		}
		config.Endpoint = &conf.Endpoint
		if conf.Region == "" {
			conf.Region = "default-region"
		}
		client, err := makeHTTPClient(settings)
		if err != nil {
			return nil, err
		}
		config.HTTPClient = client
	}

	// "specified": use credentials provided in URI params; error if not present.
	// "implicit": enable SharedConfig, which loads in credentials from environment.
	//             Detailed in https://docs.aws.amazon.com/sdk-for-go/api/aws/session/
	// "": default to `specified`.
	opts := session.Options{}
	switch conf.Auth {
	case "", AuthParamSpecified:
		if conf.AccessKey == "" {
			return nil, errors.Errorf(
				"%s is set to '%s', but %s is not set",
				AuthParam,
				AuthParamSpecified,
				AWSAccessKeyParam,
			)
		}
		if conf.Secret == "" {
			return nil, errors.Errorf(
				"%s is set to '%s', but %s is not set",
				AuthParam,
				AuthParamSpecified,
				AWSSecretParam,
			)
		}
		opts.Config.MergeIn(config)
	case AuthParamImplicit:
		if ioConf.DisableImplicitCredentials {
			return nil, errors.New(
				"implicit credentials disallowed for s3 due to --external-io-implicit-credentials flag")
		}
		opts.SharedConfigState = session.SharedConfigEnable
	default:
		return nil, errors.Errorf("unsupported value %s for %s", conf.Auth, AuthParam)
	}

	// TODO(yevgeniy): Revisit retry logic.  Retrying 10 times seems arbitrary.
	maxRetries := 10
	opts.Config.MaxRetries = &maxRetries

	if conf.Endpoint != "" {
		opts.Config.S3ForcePathStyle = aws.Bool(true)
	}
	if log.V(2) {
		opts.Config.LogLevel = aws.LogLevel(aws.LogDebugWithRequestRetries | aws.LogDebugWithRequestErrors)
		opts.Config.CredentialsChainVerboseErrors = aws.Bool(true)
	}

	// Ensure that a KMS ID is specified if server side encryption is set to use
	// KMS.
	if conf.ServerEncMode != "" {
		switch conf.ServerEncMode {
		case string(aes256Enc):
		case string(kmsEnc):
			if conf.ServerKMSID == "" {
				return nil, errors.New("AWS_SERVER_KMS_ID param must be set" +
					" when using aws:kms server side encryption mode.")
			}
		default:
			return nil, errors.Newf("unsupported server encryption mode %s. "+
				"Supported values are `aws:kms` and `AES256`.", conf.ServerEncMode)
		}
	}

	return &s3Storage{
		bucket:   aws.String(conf.Bucket),
		conf:     conf,
		ioConf:   ioConf,
		prefix:   conf.Prefix,
		opts:     opts,
		settings: settings,
	}, nil
}

func (s *s3Storage) newS3Client(ctx context.Context) (*s3.S3, error) {
	sess, err := session.NewSessionWithOptions(s.opts)
	if err != nil {
		return nil, errors.Wrap(err, "new aws session")
	}
	if s.conf.Region == "" {
		if err := delayedRetry(ctx, func() error {
			var err error
			s.conf.Region, err = s3manager.GetBucketRegion(ctx, sess, s.conf.Bucket, "us-east-1")
			return err
		}); err != nil {
			return nil, errors.Wrap(err, "could not find s3 bucket's region")
		}
	}
	sess.Config.Region = aws.String(s.conf.Region)
	return s3.New(sess), nil
}

func (s *s3Storage) Conf() roachpb.ExternalStorage {
	return roachpb.ExternalStorage{
		Provider: roachpb.ExternalStorageProvider_S3,
		S3Config: s.conf,
	}
}

func (s *s3Storage) ExternalIOConf() base.ExternalIODirConfig {
	return s.ioConf
}

func (s *s3Storage) Settings() *cluster.Settings {
	return s.settings
}

func (s *s3Storage) WriteFile(ctx context.Context, basename string, content io.ReadSeeker) error {
	client, err := s.newS3Client(ctx)
	if err != nil {
		return err
	}
	err = contextutil.RunWithTimeout(ctx, "put s3 object",
		timeoutSetting.Get(&s.settings.SV),
		func(ctx context.Context) error {
			putObjectInput := s3.PutObjectInput{
				Bucket: s.bucket,
				Key:    aws.String(path.Join(s.prefix, basename)),
				Body:   content,
			}

			// If a server side encryption mode is provided in the URI, we must set
			// the header values to enable SSE before writing the file to the s3
			// bucket.
			if s.conf.ServerEncMode != "" {
				switch s.conf.ServerEncMode {
				case string(aes256Enc):
					putObjectInput.SetServerSideEncryption(s.conf.ServerEncMode)
				case string(kmsEnc):
					putObjectInput.SetServerSideEncryption(s.conf.ServerEncMode)
					putObjectInput.SetSSEKMSKeyId(s.conf.ServerKMSID)
				default:
					return errors.Newf("unsupported server encryption mode %s. "+
						"Supported values are `aws:kms` and `AES256`.", s.conf.ServerEncMode)
				}
			}
			_, err := client.PutObjectWithContext(ctx, &putObjectInput)
			return err
		})
	return errors.Wrap(err, "failed to put s3 object")
}

func (s *s3Storage) ReadFile(ctx context.Context, basename string) (io.ReadCloser, error) {
	// https://github.com/cockroachdb/cockroach/issues/23859
	client, err := s.newS3Client(ctx)
	if err != nil {
		return nil, err
	}

	out, err := client.GetObjectWithContext(ctx, &s3.GetObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(path.Join(s.prefix, basename)),
	})
	if err != nil {
		if aerr := (awserr.Error)(nil); errors.As(err, &aerr) {
			switch aerr.Code() {
			// Relevant 404 errors reported by AWS.
			case s3.ErrCodeNoSuchBucket, s3.ErrCodeNoSuchKey:
				return nil, errors.Wrapf(ErrFileDoesNotExist, "s3 object does not exist: %s", err.Error())
			}
		}
		return nil, errors.Wrap(err, "failed to get s3 object")
	}
	return out.Body, nil
}

func (s *s3Storage) ListFiles(ctx context.Context, patternSuffix string) ([]string, error) {
	var fileList []string

	pattern := s.prefix
	if patternSuffix != "" {
		if containsGlob(s.prefix) {
			return nil, errors.New("prefix cannot contain globs pattern when passing an explicit pattern")
		}
		pattern = path.Join(pattern, patternSuffix)
	}
	client, err := s.newS3Client(ctx)
	if err != nil {
		return nil, err
	}

	var matchErr error
	err = client.ListObjectsPagesWithContext(
		ctx,
		&s3.ListObjectsInput{
			Bucket: s.bucket,
			Prefix: aws.String(getPrefixBeforeWildcard(s.prefix)),
		},
		func(page *s3.ListObjectsOutput, lastPage bool) bool {
			for _, fileObject := range page.Contents {
				matches, err := path.Match(pattern, *fileObject.Key)
				if err != nil {
					matchErr = err
					return false
				}
				if matches {
					if patternSuffix != "" {
						if !strings.HasPrefix(*fileObject.Key, s.prefix) {
							// TODO(dt): return a nice rel-path instead of erroring out.
							matchErr = errors.New("pattern matched file outside of path")
							return false
						}
						fileList = append(fileList, strings.TrimPrefix(strings.TrimPrefix(*fileObject.Key, s.prefix), "/"))
					} else {

						fileList = append(fileList, S3URI(*s.bucket, *fileObject.Key, s.conf))
					}
				}
			}
			return !lastPage
		},
	)
	if err != nil {
		return nil, errors.Wrap(err, `failed to list s3 bucket`)
	}
	if matchErr != nil {
		return nil, errors.Wrap(matchErr, `failed to list s3 bucket`)
	}

	return fileList, nil
}

func (s *s3Storage) Delete(ctx context.Context, basename string) error {
	client, err := s.newS3Client(ctx)
	if err != nil {
		return err
	}
	return contextutil.RunWithTimeout(ctx, "delete s3 object",
		timeoutSetting.Get(&s.settings.SV),
		func(ctx context.Context) error {
			_, err := client.DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{
				Bucket: s.bucket,
				Key:    aws.String(path.Join(s.prefix, basename)),
			})
			return err
		})
}

func (s *s3Storage) Size(ctx context.Context, basename string) (int64, error) {
	client, err := s.newS3Client(ctx)
	if err != nil {
		return 0, err
	}
	var out *s3.HeadObjectOutput
	err = contextutil.RunWithTimeout(ctx, "get s3 object header",
		timeoutSetting.Get(&s.settings.SV),
		func(ctx context.Context) error {
			var err error
			out, err = client.HeadObjectWithContext(ctx, &s3.HeadObjectInput{
				Bucket: s.bucket,
				Key:    aws.String(path.Join(s.prefix, basename)),
			})
			return err
		})
	if err != nil {
		return 0, errors.Wrap(err, "failed to get s3 object headers")
	}
	return *out.ContentLength, nil
}

func (s *s3Storage) Close() error {
	return nil
}
