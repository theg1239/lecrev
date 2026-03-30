package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/ishaan/eeeverc/internal/domain"
)

const executionStreamName = "EXECUTION"

type NATSBus struct {
	conn *nats.Conn
	js   nats.JetStreamContext
}

func NewNATS(url string) (*NATSBus, error) {
	conn, err := nats.Connect(url)
	if err != nil {
		return nil, err
	}
	js, err := conn.JetStream()
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &NATSBus{conn: conn, js: js}, nil
}

func (b *NATSBus) EnsureStream() error {
	_, err := b.js.StreamInfo(executionStreamName)
	if err == nil {
		return nil
	}
	if err != nats.ErrStreamNotFound {
		return err
	}
	_, err = b.js.AddStream(&nats.StreamConfig{
		Name:      executionStreamName,
		Subjects:  []string{"execution.*"},
		Storage:   nats.FileStorage,
		Retention: nats.WorkQueuePolicy,
		Replicas:  1,
		MaxAge:    24 * time.Hour,
	})
	return err
}

func (b *NATSBus) PublishExecution(ctx context.Context, region string, assignment domain.Assignment) error {
	data, err := json.Marshal(assignment)
	if err != nil {
		return err
	}
	subject := fmt.Sprintf("execution.%s", region)
	_, err = b.js.PublishMsg(&nats.Msg{
		Subject: subject,
		Data:    data,
	}, nats.Context(ctx))
	return err
}

func (b *NATSBus) ConsumeExecution(ctx context.Context, region, consumer string, handler Handler) error {
	subject := fmt.Sprintf("execution.%s", region)
	sub, err := b.js.PullSubscribe(subject, consumer, nats.BindStream(executionStreamName))
	if err != nil {
		return err
	}
	defer func() {
		_ = sub.Unsubscribe()
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		msgs, err := sub.Fetch(1, nats.MaxWait(2*time.Second))
		if err != nil {
			if err == nats.ErrTimeout || err == context.DeadlineExceeded {
				continue
			}
			return err
		}
		for _, msg := range msgs {
			var assignment domain.Assignment
			if err := json.Unmarshal(msg.Data, &assignment); err != nil {
				_ = msg.Term()
				continue
			}
			if err := handler(ctx, assignment); err != nil {
				_ = msg.Nak()
				continue
			}
			_ = msg.Ack()
		}
	}
}

func (b *NATSBus) Close() error {
	if b.conn != nil {
		b.conn.Close()
	}
	return nil
}
