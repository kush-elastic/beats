// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package awss3

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/aws/smithy-go/middleware"

	"github.com/elastic/beats/v7/libbeat/beat"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/elastic/elastic-agent-libs/logp"
)

// Run 'go generate' to create mocks that are used in tests.
//go:generate go run go.uber.org/mock/mockgen -source=interfaces.go -destination=mock_interfaces_test.go -package awss3 -mock_names=sqsAPI=MockSQSAPI,sqsProcessor=MockSQSProcessor,s3API=MockS3API,s3Pager=MockS3Pager,s3ObjectHandlerFactory=MockS3ObjectHandlerFactory,s3ObjectHandler=MockS3ObjectHandler
//go:generate go run go.uber.org/mock/mockgen -destination=mock_publisher_test.go -package=awss3 -mock_names=Client=MockBeatClient,Pipeline=MockBeatPipeline github.com/elastic/beats/v7/libbeat/beat Client,Pipeline
//go:generate go run github.com/elastic/go-licenser -license Elastic .
//go:generate go run golang.org/x/tools/cmd/goimports -w -local github.com/elastic .

// ------
// SQS interfaces
// ------

const s3RequestURLMetadataKey = `x-beat-s3-request-url`

type sqsAPI interface {
	ReceiveMessage(ctx context.Context, maxMessages int) ([]types.Message, error)
	DeleteMessage(ctx context.Context, msg *types.Message) error
	ChangeMessageVisibility(ctx context.Context, msg *types.Message, timeout time.Duration) error
	GetQueueAttributes(ctx context.Context, attr []types.QueueAttributeName) (map[string]string, error)
}

type sqsProcessor interface {
	// ProcessSQS processes and SQS message. It takes fully ownership of the
	// given message and is responsible for updating the message's visibility
	// timeout while it is being processed and for deleting it when processing
	// completes successfully.
	ProcessSQS(ctx context.Context, msg *types.Message, eventCallback func(e beat.Event)) sqsProcessingResult
}

// ------
// S3 interfaces
// ------

type s3API interface {
	s3Getter
	s3Mover
	s3Lister
}

type s3Getter interface {
	GetObject(ctx context.Context, region, bucket, key string) (*s3.GetObjectOutput, error)
}

type s3Mover interface {
	CopyObject(ctx context.Context, region, from_bucket, to_bucket, from_key, to_key string) (*s3.CopyObjectOutput, error)
	DeleteObject(ctx context.Context, region, bucket, key string) (*s3.DeleteObjectOutput, error)
}

type s3Lister interface {
	ListObjectsPaginator(bucket, prefix string) s3Pager
}

type s3Pager interface {
	HasMorePages() bool // NextPage retrieves the next ListObjectsV2 page.
	NextPage(ctx context.Context, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

type s3ObjectHandlerFactory interface {
	// Create returns a new s3ObjectHandler that can be used to process the
	// specified S3 object. If the handler is not configured to process the
	// given S3 object (based on key name) then it will return nil.
	Create(ctx context.Context, obj s3EventV2) s3ObjectHandler
}

type s3ObjectHandler interface {
	// ProcessS3Object downloads the S3 object, parses it, creates events, and
	// passes to the given callback. It returns when processing finishes or
	// when it encounters an unrecoverable error.
	ProcessS3Object(log *logp.Logger, eventCallback func(e beat.Event)) error

	// FinalizeS3Object finalizes processing of an S3 object after the current
	// batch is finished.
	FinalizeS3Object() error
}

// ------
// AWS SQS implementation
// ------

type awsSQSAPI struct {
	client            *sqs.Client
	queueURL          string
	apiTimeout        time.Duration
	visibilityTimeout time.Duration
	longPollWaitTime  time.Duration
}

func (a *awsSQSAPI) ReceiveMessage(ctx context.Context, maxMessages int) ([]types.Message, error) {
	const sqsMaxNumberOfMessagesLimit = 10
	ctx, cancel := context.WithTimeout(ctx, a.apiTimeout)
	defer cancel()

	receiveMessageOutput, err := a.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            awssdk.String(a.queueURL),
		MaxNumberOfMessages: int32(min(maxMessages, sqsMaxNumberOfMessagesLimit)),
		VisibilityTimeout:   int32(a.visibilityTimeout.Seconds()),
		WaitTimeSeconds:     int32(a.longPollWaitTime.Seconds()),
		AttributeNames:      []types.QueueAttributeName{sqsApproximateReceiveCountAttribute, sqsSentTimestampAttribute},
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("api_timeout exceeded: %w", err)
		}
		return nil, fmt.Errorf("sqs ReceiveMessage failed: %w", err)
	}

	return receiveMessageOutput.Messages, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (a *awsSQSAPI) DeleteMessage(ctx context.Context, msg *types.Message) error {
	ctx, cancel := context.WithTimeout(ctx, a.apiTimeout)
	defer cancel()
	_, err := a.client.DeleteMessage(ctx,
		&sqs.DeleteMessageInput{
			QueueUrl:      awssdk.String(a.queueURL),
			ReceiptHandle: msg.ReceiptHandle,
		})

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("api_timeout exceeded: %w", err)
		}
		return fmt.Errorf("sqs DeleteMessage failed: %w", err)
	}

	return nil
}

func (a *awsSQSAPI) ChangeMessageVisibility(ctx context.Context, msg *types.Message, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, a.apiTimeout)
	defer cancel()

	_, err := a.client.ChangeMessageVisibility(ctx,
		&sqs.ChangeMessageVisibilityInput{
			QueueUrl:          awssdk.String(a.queueURL),
			ReceiptHandle:     msg.ReceiptHandle,
			VisibilityTimeout: int32(timeout.Seconds()),
		})

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("api_timeout exceeded: %w", err)
		}
		return fmt.Errorf("sqs ChangeMessageVisibility failed: %w", err)
	}

	return nil
}

func (a *awsSQSAPI) GetQueueAttributes(ctx context.Context, attr []types.QueueAttributeName) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, a.apiTimeout)
	defer cancel()

	attributeOutput, err := a.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		AttributeNames: attr,
		QueueUrl:       awssdk.String(a.queueURL),
	})

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("api_timeout exceeded: %w", err)
		}
		return nil, fmt.Errorf("sqs GetQueueAttributes failed: %w", err)
	}

	return attributeOutput.Attributes, nil
}

// ------
// AWS S3 implementation
// ------

type awsS3API struct {
	client *s3.Client

	// others is the set of other clients referred
	// to by notifications seen by the API connection.
	// The number of cached elements is limited to
	// awsS3APIcacheMax.
	mu     sync.RWMutex
	others map[string]*s3.Client
}

const awsS3APIcacheMax = 100

func newAWSs3API(cli *s3.Client) *awsS3API {
	return &awsS3API{client: cli, others: make(map[string]*s3.Client)}
}

func (a *awsS3API) GetObject(ctx context.Context, region, bucket, key string) (*s3.GetObjectOutput, error) {
	getObjectOutput, err := a.clientFor(region).GetObject(ctx, &s3.GetObjectInput{
		Bucket: awssdk.String(bucket),
		Key:    awssdk.String(key),
	}, s3.WithAPIOptions(
		func(stack *middleware.Stack) error {
			// adds AFTER operation finalize middleware
			return stack.Finalize.Add(middleware.FinalizeMiddlewareFunc("add s3 request url to metadata",
				func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (
					out middleware.FinalizeOutput, metadata middleware.Metadata, err error,
				) {
					out, metadata, err = next.HandleFinalize(ctx, in)
					requestURL, parseErr := url.Parse(in.Request.(*smithyhttp.Request).URL.String())
					if parseErr != nil {
						return out, metadata, err
					}

					requestURL.RawQuery = ""

					metadata.Set(s3RequestURLMetadataKey, requestURL.String())

					return out, metadata, err
				},
			), middleware.After)
		}))

	if err != nil {
		return nil, fmt.Errorf("s3 GetObject failed: %w", err)
	}

	return getObjectOutput, nil
}

func (a *awsS3API) CopyObject(ctx context.Context, region, from_bucket, to_bucket, from_key, to_key string) (*s3.CopyObjectOutput, error) {
	copyObjectOutput, err := a.clientFor(region).CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     awssdk.String(to_bucket),
		CopySource: awssdk.String(fmt.Sprintf("%s/%s", from_bucket, from_key)),
		Key:        awssdk.String(to_key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 CopyObject failed: %w", err)
	}
	return copyObjectOutput, nil
}

func (a *awsS3API) DeleteObject(ctx context.Context, region, bucket, key string) (*s3.DeleteObjectOutput, error) {
	deleteObjectOutput, err := a.clientFor(region).DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: awssdk.String(bucket),
		Key:    awssdk.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 DeleteObject failed: %w", err)
	}
	return deleteObjectOutput, nil
}

func (a *awsS3API) clientFor(region string) *s3.Client {
	// Conditionally replace the client if the region of
	// the request does not match the pre-prepared client.
	opts := a.client.Options()
	if region == "" || opts.Region == region {
		return a.client
	}
	// Use a cached client if we have already seen this region.
	a.mu.RLock()
	cli, ok := a.others[region]
	a.mu.RUnlock()
	if ok {
		return cli
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Check that another writer did not beat us here.
	cli, ok = a.others[region]
	if ok {
		// ... they did.
		return cli
	}

	// Otherwise create a new client and cache it.
	opts.Region = region
	cli = s3.New(opts)
	// We should never be in the situation that the cache
	// grows unbounded, but ensure this is the case.
	if len(a.others) >= awsS3APIcacheMax {
		// Do a single iteration delete to perform a
		// random cache eviction.
		for r := range a.others {
			delete(a.others, r)
			break
		}
	}
	a.others[region] = cli

	return cli
}

func (a *awsS3API) ListObjectsPaginator(bucket, prefix string) s3Pager {
	pager := s3.NewListObjectsV2Paginator(a.client, &s3.ListObjectsV2Input{
		Bucket: awssdk.String(bucket),
		Prefix: awssdk.String(prefix),
	})

	return pager
}
