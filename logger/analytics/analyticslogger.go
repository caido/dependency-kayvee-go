package analytics

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/firehose"
	"github.com/aws/aws-sdk-go/service/firehose/firehoseiface"
	"github.com/eapache/go-resiliency/retrier"
	"gopkg.in/Clever/kayvee-go.v6/logger"
)

//go:generate mockgen -package $GOPACKAGE -destination mock_firehose.go github.com/aws/aws-sdk-go/service/firehose/firehoseiface FirehoseAPI

// Logger writes to Firehose instead of the logging pipeline
type Logger struct {
	logger.KayveeLogger
	fhStream        string
	fhAPI           firehoseiface.FirehoseAPI
	batch           []*firehose.Record
	batchBytes      int
	maxBatchRecords int
	maxBatchBytes   int
	mu              sync.Mutex
}

var _ logger.KayveeLogger = &Logger{}
var _ io.WriteCloser = &Logger{}

var ignoredFields = []string{"level", "source", "title", "deploy_env", "wf_id"}

const timeoutForSendingBatches = time.Minute

// firehosePutRecordBatchMaxRecords is an AWS limit.
// https://docs.aws.amazon.com/firehose/latest/APIReference/API_PutRecordBatch.html
const firehosePutRecordBatchMaxRecords = 500

// firehosePutRecordBatchMaxBytes is an AWS limit on total bytes in a PutRecordBatch request.
// https://docs.aws.amazon.com/firehose/latest/APIReference/API_PutRecordBatch.html
const firehosePutRecordBatchMaxBytes = 4000000

// Config configures things related to collecting analytics.
type Config struct {
	// DBName is the name of the ark db. Either specify this or StreamName.
	DBName string
	// Environment is the name of the environment to point to. Default is _DEPLOY_ENV.
	Environment string
	// StreamName is the name of the Firehose to send to. Either specify this or DBName.
	StreamName string
	// Region is the region where this is running. Defaults to _POD_REGION.
	Region string
	// FirehosePutRecordBatchMaxRecords overrides the default value (500) for the maximum number of records to send in a firehose batch.
	FirehosePutRecordBatchMaxRecords int
	// FirehosePutRecordBatchMaxBytes overrides the default value (4000000) for the maximum number of bytes to send in a firehose batch.
	FirehosePutRecordBatchMaxBytes int
	// FirehoseAPI defaults to an API object configured with Region, but can be overriden here.
	FirehoseAPI firehoseiface.FirehoseAPI
}

// New returns a logger that writes to an analytics ark db.
// It takes as input the db name and the ark db config file.
func New(c Config) (*Logger, error) {
	l := logger.New(c.DBName)
	al := &Logger{KayveeLogger: l}
	l.SetOutput(al)
	env, dbname, streamName := c.Environment, c.DBName, c.StreamName
	if dbname != "" && streamName != "" {
		return nil, errors.New("cannot specify both DBName and StreamName in logger config")
	}
	if dbname == "" && streamName == "" {
		return nil, errors.New("must specify either DBName or StreamName in logger config")
	}
	if env == "" {
		if env = os.Getenv("_DEPLOY_ENV"); env == "" {
			return nil, errors.New("env could not be set (either pass in explicit env, or set _DEPLOY_ENV)")
		}
	}
	if dbname != "" {
		al.fhStream = fmt.Sprintf("%s--%s", env, dbname)
	} else {
		al.fhStream = streamName
	}

	if v := c.FirehosePutRecordBatchMaxRecords; v != 0 {
		al.maxBatchRecords = min(v, firehosePutRecordBatchMaxRecords)
	} else {
		al.maxBatchRecords = firehosePutRecordBatchMaxRecords
	}
	if v := c.FirehosePutRecordBatchMaxBytes; v != 0 {
		al.maxBatchBytes = min(v, firehosePutRecordBatchMaxBytes)
	} else {
		al.maxBatchBytes = firehosePutRecordBatchMaxBytes
	}

	if c.FirehoseAPI != nil {
		al.fhAPI = c.FirehoseAPI
	} else if c.Region != "" {
		al.fhAPI = firehose.New(session.New(&aws.Config{
			Region: aws.String(c.Region),
		}))
	} else {
		return nil, errors.New("must provide FirehoseAPI or Region")
	}
	return al, nil
}

// Write a log.
func (al *Logger) Write(bs []byte) (int, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(bs, &m); err != nil {
		return 0, err
	}
	// delete kv-added fields we don't care about. We only want the logger.M values.
	for _, f := range ignoredFields {
		delete(m, f)
	}
	bs, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	bs = append(bs, '\n')
	al.mu.Lock()
	al.batchBytes += len(bs)
	al.batch = append(al.batch, &firehose.Record{Data: bs})
	shouldSendBatch := len(al.batch) == al.maxBatchRecords || al.batchBytes > int(0.9*float64(al.maxBatchBytes))
	if shouldSendBatch {
		batch := al.batch
		al.batch = []*firehose.Record{}
		al.batchBytes = 0
		// be careful not to send al.batch, since we will unlock before we finish sending the batch
		go sendBatch(batch, al.fhAPI, al.fhStream, time.Now().Add(timeoutForSendingBatches))
	}
	al.mu.Unlock()
	return len(bs), nil
}

// Close flushes all logs to Firehose.
func (al *Logger) Close() error {
	al.mu.Lock()
	if len(al.batch) > 0 {
		batch := al.batch
		al.batch = []*firehose.Record{}
		al.batchBytes = 0
		// be careful not to send al.batch, since we will unlock before we finish sending the batch
		go sendBatch(batch, al.fhAPI, al.fhStream, time.Now().Add(timeoutForSendingBatches))
	}
	al.mu.Unlock()
	return nil
}

func sendBatch(batch []*firehose.Record, fhAPI firehoseiface.FirehoseAPI, fhStream string, timeout time.Time) error {
	// call PutRecordBatch until all records in the batch have been sent successfully
	for time.Now().Before(timeout) {
		var result *firehose.PutRecordBatchOutput
		r := retrier.New(retrier.ExponentialBackoff(5, 100*time.Millisecond), RequestErrorClassifier{})
		if err := r.Run(func() error {
			out, err := fhAPI.PutRecordBatch(&firehose.PutRecordBatchInput{
				DeliveryStreamName: aws.String(fhStream),
				Records:            batch,
			})
			if err != nil {
				return err
			}
			result = out
			return nil
		}); err != nil {
			return fmt.Errorf("PutRecords: %v", err)
		}
		if aws.Int64Value(result.FailedPutCount) == 0 {
			break
		}
		// formulate a new batch consisting of the unprocessed items
		newbatch := []*firehose.Record{}
		for i, res := range result.RequestResponses {
			if aws.StringValue(res.ErrorCode) == "" {
				continue
			}
			newbatch = append(newbatch, batch[i])
		}
		batch = newbatch
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// RequestErrorClassifier corrects for AWS SDK's lack of automatic retry on
// "RequestError: connection reset by peer"
type RequestErrorClassifier struct{}

var _ retrier.Classifier = RequestErrorClassifier{}

// Classify the error.
func (RequestErrorClassifier) Classify(err error) retrier.Action {
	if err == nil {
		return retrier.Succeed
	}
	if aerr, ok := err.(awserr.Error); ok && aerr.Code() == "RequestError" {
		return retrier.Retry
	}
	return retrier.Fail
}
