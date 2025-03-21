package resources

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	"github.com/gruntwork-io/cloud-nuke/config"
	"github.com/gruntwork-io/cloud-nuke/logging"
	"github.com/gruntwork-io/cloud-nuke/report"
	"github.com/gruntwork-io/go-commons/errors"
	"github.com/hashicorp/go-multierror"
)

func (ks *KinesisStreams) getAll(c context.Context, configObj config.Config) ([]*string, error) {
	var allStreams []*string

	paginator := kinesis.NewListStreamsPaginator(ks.Client, &kinesis.ListStreamsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(c)
		if err != nil {
			return nil, errors.WithStackTrace(err)
		}

		for _, stream := range page.StreamNames {
			if configObj.KinesisStream.ShouldInclude(config.ResourceValue{
				Name: aws.String(stream),
			}) {
				allStreams = append(allStreams, aws.String(stream))
			}
		}
	}

	return allStreams, nil
}

func (ks *KinesisStreams) nukeAll(identifiers []*string) error {
	if len(identifiers) == 0 {
		logging.Debugf("No Kinesis Streams to nuke in region: %s", ks.Region)
	}

	// NOTE: we don't need to do pagination here, because the pagination is handled by the caller to this function,
	// based on KinesisStream.MaxBatchSize, however we add a guard here to warn users when the batching fails and
	// has a chance of throttling AWS. Since we concurrently make one call for each identifier, we pick 100 for the
	// limit here because many APIs in AWS have a limit of 100 requests per second.
	if len(identifiers) > 100 {
		logging.Errorf("Nuking too many Kinesis Streams at once (100): halting to avoid hitting AWS API rate limiting")
		return TooManyStreamsErr{}
	}

	// There is no bulk delete Kinesis Stream API, so we delete the batch of Kinesis Streams concurrently
	// using go routines.
	logging.Debugf("Deleting Kinesis Streams in region: %s", ks.Region)
	wg := new(sync.WaitGroup)
	wg.Add(len(identifiers))
	errChans := make([]chan error, len(identifiers))
	for i, streamName := range identifiers {
		errChans[i] = make(chan error, 1)
		go ks.deleteAsync(wg, errChans[i], streamName)
	}
	wg.Wait()

	// Collect all the errors from the async delete calls into a single error struct.
	// NOTE: We ignore OperationAbortedException which is thrown when there is an eventual consistency issue, where
	// cloud-nuke picks up a Stream that is already requested to be deleted.
	var allErrs *multierror.Error
	for _, errChan := range errChans {
		if err := <-errChan; err != nil {
			allErrs = multierror.Append(allErrs, err)
		}
	}
	finalErr := allErrs.ErrorOrNil()
	if finalErr != nil {
		return errors.WithStackTrace(finalErr)
	}
	return nil
}

func (ks *KinesisStreams) deleteAsync(
	wg *sync.WaitGroup,
	errChan chan error,
	streamName *string,
) {
	defer wg.Done()
	input := &kinesis.DeleteStreamInput{StreamName: streamName}
	_, err := ks.Client.DeleteStream(ks.Context, input)

	// Record status of this resource
	e := report.Entry{
		Identifier:   aws.ToString(streamName),
		ResourceType: "Kinesis Stream",
		Error:        err,
	}
	report.Record(e)

	errChan <- err

	streamNameStr := aws.ToString(streamName)
	if err == nil {
		logging.Debugf("[OK] Kinesis Stream %s delete in %s", streamNameStr, ks.Region)
	} else {
		logging.Debugf("[Failed] Error deleting Kinesis Stream %s in %s: %s", streamNameStr, ks.Region, err)
	}
}

// Custom errors

type TooManyStreamsErr struct{}

func (err TooManyStreamsErr) Error() string {
	return "Too many Streams requested at once."
}
