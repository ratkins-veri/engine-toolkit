package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/pkg/errors"
)

// Engine consumes messages and calls webhooks to
// fulfil the requests.
type Engine struct {
	producer Producer
	consumer Consumer

	client   *http.Client
	logDebug func(args ...interface{})

	// Config holds the Engine configuration.
	Config Config
}

// NewEngine makes a new Engine with the specified Consumer and Producer.
func NewEngine() *Engine {
	return &Engine{
		logDebug: func(args ...interface{}) {
			log.Println(args...)
		},
		Config: NewConfig(),
		client: http.DefaultClient,
	}
}

// isTrainingTask gets whether the task is a training task or not.
func isTrainingTask() (bool, error) {
	payload, err := EnvPayload()
	if err == ErrNoPayload {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return payload.Mode == "library-train", nil
}

// Run runs the Engine.
// Context errors may be returned.
func (e *Engine) Run(ctx context.Context) error {
	isTraining, err := isTrainingTask()
	if err != nil {
		return errors.Wrap(err, "isTrainingTask")
	}
	if isTraining {
		e.logDebug("running subprocess...")
		return e.runSubprocessOnly(ctx)
	}
	e.logDebug("running inference...")
	return e.runInference(ctx)
}

// runSubprocessOnly starts the subprocess and doesn't do anything else.
// This is used for training tasks.
func (e *Engine) runSubprocessOnly(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, e.Config.Subprocess.Arguments[0], e.Config.Subprocess.Arguments[1:]...)
	cmd.Stdout = e.Config.Stdout
	cmd.Stderr = e.Config.Stderr
	cmd.Stderr = e.Config.Stderr // TODO: deal with stderr
	if err := cmd.Run(); err != nil {
		return errors.Wrap(err, e.Config.Subprocess.Arguments[0])
	}
	return nil
}

// runInference starts the subprocess and routes work to webhooks.
// This is used to process files.
func (e *Engine) runInference(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var cmd *exec.Cmd
	if len(e.Config.Subprocess.Arguments) > 0 {
		cmd = exec.CommandContext(ctx, e.Config.Subprocess.Arguments[0], e.Config.Subprocess.Arguments[1:]...)
		cmd.Stdout = e.Config.Stdout
		cmd.Stderr = e.Config.Stderr
		if err := cmd.Start(); err != nil {
			return errors.Wrap(err, e.Config.Subprocess.Arguments[0])
		}
		// TODO: we need to know if the subprocess crashes, and maybe we cancel the context
		// and report a failure.
		readyCtx, cancel := context.WithTimeout(ctx, e.Config.Subprocess.ReadyTimeout)
		defer cancel()
		e.logDebug("waiting for ready... will expire after", e.Config.Subprocess.ReadyTimeout)
		if err := e.ready(readyCtx); err != nil {
			return err
		}
	}
	e.logDebug("waiting for messages...")
	go func() {
		defer func() {
			e.logDebug("shutting down...")
			cancel()
		}()
		for {
			select {
			case msg, ok := <-e.consumer.Messages():
				if !ok {
					return
				}
				if err := e.processMessage(ctx, msg); err != nil {
					e.logDebug(fmt.Sprintf("processing error: %v", err))
				}
				e.consumer.MarkOffset(msg, "")
			case <-time.After(e.Config.EndIfIdleDuration):
				e.logDebug(fmt.Sprintf("idle for %s", e.Config.EndIfIdleDuration))
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	if cmd != nil {
		// wait for the command
		if err := cmd.Wait(); err != nil {
			if err := ctx.Err(); err != nil {
				// if the context has an error, we'll assume this command
				// errored because we terminated it (via context).
				return ctx.Err()
			}
			// otherwise, the subprocess has crashed
			return errors.Wrap(err, e.Config.Subprocess.Arguments[0])
		}
		return nil
	}
	<-ctx.Done()
	return nil
}

func (e *Engine) processMessage(ctx context.Context, msg *sarama.ConsumerMessage) error {
	var typeCheck struct {
		Type messageType
	}
	if err := json.Unmarshal(msg.Value, &typeCheck); err != nil {
		return errors.Wrap(err, "unmarshal message value JSON")
	}
	switch typeCheck.Type {
	case messageTypeMediaChunk:
		if err := e.processMessageMediaChunk(ctx, msg); err != nil {
			return errors.Wrap(err, "process media chunk")
		}
	default:
		e.logDebug(fmt.Sprintf("ignoring message of type %q: %+v", typeCheck.Type, msg))
	}
	return nil
}

// periodicallySendProgressMessage sends a processing update immediately, and then at regular
// intervals specified by Config.Tasks.ProcessingUpdateInterval.
// Use the context to cancel this operation.
// The returned channel will be closed once the operation has completed.
func (e *Engine) periodicallySendProgressMessage(ctx context.Context, messageKey []byte, taskID, chunkUUID string) chan struct{} {
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-time.After(e.Config.Tasks.ProcessingUpdateInterval):
				updateMessage := chunkProcessedStatus{
					Type:         messageTypeChunkProcessedStatus,
					TimestampUTC: time.Now().Unix(),
					TaskID:       taskID,
					ChunkUUID:    chunkUUID,
					Status:       chunkStatusProcessing,
				}
				_, _, err := e.producer.SendMessage(&sarama.ProducerMessage{
					Topic: e.Config.Kafka.ChunkTopic,
					Key:   sarama.ByteEncoder(messageKey),
					Value: newJSONEncoder(updateMessage),
				})
				if err != nil {
					e.logDebug("WARN", "failed to send chunk processing update:", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return finished
}

// processMessageMediaChunk processes a single media chunk as described by the sarama.ConsumerMessage.
func (e *Engine) processMessageMediaChunk(ctx context.Context, msg *sarama.ConsumerMessage) error {
	var mediaChunk mediaChunkMessage
	if err := json.Unmarshal(msg.Value, &mediaChunk); err != nil {
		return errors.Wrap(err, "unmarshal message value JSON")
	}
	finalUpdateMessage := chunkProcessedStatus{
		Type:      messageTypeChunkProcessedStatus,
		TaskID:    mediaChunk.TaskID,
		ChunkUUID: mediaChunk.ChunkUUID,
		Status:    chunkStatusSuccess, // optimistic
	}
	defer func() {
		// send the final message
		finalUpdateMessage.TimestampUTC = time.Now().Unix()
		_, _, err := e.producer.SendMessage(&sarama.ProducerMessage{
			Topic: e.Config.Kafka.ChunkTopic,
			Key:   sarama.ByteEncoder(msg.Key),
			Value: newJSONEncoder(finalUpdateMessage),
		})
		if err != nil {
			e.logDebug("WARN", "failed to send final chunk update:", err)
		}
	}()
	// send chunk 'processing' update
	updateMessage := chunkProcessedStatus{
		Type:         messageTypeChunkProcessedStatus,
		TimestampUTC: time.Now().Unix(),
		TaskID:       mediaChunk.TaskID,
		ChunkUUID:    mediaChunk.ChunkUUID,
		Status:       chunkStatusProcessing,
	}
	_, _, err := e.producer.SendMessage(&sarama.ProducerMessage{
		Topic: e.Config.Kafka.ChunkTopic,
		Key:   sarama.ByteEncoder(msg.Key),
		Value: newJSONEncoder(updateMessage),
	})
	if err != nil {
		err = errors.Wrapf(err, "SendMessage: %q %s %s", e.Config.Kafka.ChunkTopic, messageTypeChunkProcessedStatus, chunkStatusProcessing)
		finalUpdateMessage.Status = chunkStatusError
		finalUpdateMessage.ErrorMsg = err.Error()
		return err
	}

	// start sending chunk processing updates
	ctxProcessingUpdate, cancelProcessingUpdate := context.WithCancel(ctx)
	processingUpdateFinished := e.periodicallySendProgressMessage(ctxProcessingUpdate, msg.Key, mediaChunk.TaskID, mediaChunk.ChunkUUID)

	ignoreChunk := false
	retry := newDoubleTimeBackoff(
		e.Config.Webhooks.Backoff.InitialBackoffDuration,
		e.Config.Webhooks.Backoff.MaxBackoffDuration,
		e.Config.Webhooks.Backoff.MaxRetries,
	)
	var engineOutputContent engineOutput
	err = retry.Do(func() error {
		req, err := newRequestFromMediaChunk(e.client, e.Config.Webhooks.Process.URL, mediaChunk)
		if err != nil {
			return errors.Wrap(err, "new request")
		}
		req = req.WithContext(ctx)
		resp, err := e.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, resp.Body); err != nil {
			return errors.Wrap(err, "read body")
		}
		if resp.StatusCode == http.StatusNoContent {
			ignoreChunk = true
			return nil
		}
		if resp.StatusCode != http.StatusOK {
			return errors.Errorf("%d: %s", resp.StatusCode, buf.String())
		}
		if buf.Len() > 0 {
			if err := json.NewDecoder(&buf).Decode(&engineOutputContent); err != nil {
				return errors.Wrap(err, "decode response")
			}
		}
		return nil
	}) // NOTE: this err is handled below

	// stop sending processing updates
	cancelProcessingUpdate()
	<-processingUpdateFinished

	if err != nil {
		// send error message
		err = errors.Wrapf(err, "SendMessage: %q %s %s", e.Config.Kafka.ChunkTopic, messageTypeChunkProcessedStatus, chunkStatusSuccess)
		finalUpdateMessage.Status = chunkStatusError
		finalUpdateMessage.ErrorMsg = err.Error()
		return err
	}
	if ignoreChunk {
		finalUpdateMessage.Status = chunkStatusIgnored
		return nil
	}
	content, err := json.Marshal(engineOutputContent)
	if err != nil {
		err = errors.Wrap(err, "json: marshal engine output content")
		finalUpdateMessage.Status = chunkStatusError
		finalUpdateMessage.ErrorMsg = err.Error()
		return err
	}
	// send output message
	outputMessage := mediaChunkMessage{
		Type:          messageTypeEngineOutput,
		TaskID:        mediaChunk.TaskID,
		JobID:         mediaChunk.JobID,
		ChunkUUID:     mediaChunk.ChunkUUID,
		StartOffsetMS: mediaChunk.StartOffsetMS,
		EndOffsetMS:   mediaChunk.EndOffsetMS,
		Content:       string(content),
	}
	_, _, err = e.producer.SendMessage(&sarama.ProducerMessage{
		Topic: e.Config.Kafka.ChunkTopic,
		Key:   sarama.ByteEncoder(msg.Key),
		Value: newJSONEncoder(outputMessage),
	})
	if err != nil {
		err = errors.Wrapf(err, "SendMessage: %q %s %s", e.Config.Kafka.ChunkTopic, messageTypeEngineOutput, err)
		finalUpdateMessage.Status = chunkStatusError
		finalUpdateMessage.ErrorMsg = err.Error()
		return err
	}
	return nil
}

// ready returns a channel that is closed when the engine is
// ready.
// The channel may receive an error if something goes wrong while waiting
// for the engine to become ready.
func (e *Engine) ready(ctx context.Context) error {
	start := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().Sub(start) >= e.Config.Webhooks.Ready.MaximumPollDuration {
			e.logDebug("ready: exceeded", e.Config.Webhooks.Ready.MaximumPollDuration)
			return errReadyTimeout
		}
		resp, err := http.Get(e.Config.Webhooks.Ready.URL)
		if err != nil {
			e.logDebug("ready: err:", err)
			time.Sleep(e.Config.Webhooks.Ready.PollDuration)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			e.logDebug("ready: status:", resp.Status)
			time.Sleep(e.Config.Webhooks.Ready.PollDuration)
			continue
		}
		e.logDebug("ready: yes")
		return nil
	}
}

func nolog(args ...interface{}) {}

// errReadyTimeout is sent down the Ready channel if the
// Webhooks.Ready.MaximumPollDuration is exceeded.
var errReadyTimeout = errors.New("ready: maximum duration exceeded")

// jsonEncoder encodes JSON.
type jsonEncoder struct {
	v    interface{}
	once sync.Once
	b    []byte
	err  error
}

func newJSONEncoder(v interface{}) sarama.Encoder {
	return &jsonEncoder{v: v}
}

func (j *jsonEncoder) encode() {
	j.once.Do(func() {
		j.b, j.err = json.Marshal(j.v)
	})
}

func (j *jsonEncoder) Encode() ([]byte, error) {
	j.encode()
	return j.b, j.err
}

func (j *jsonEncoder) Length() int {
	j.encode()
	return len(j.b)
}
