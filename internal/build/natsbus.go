package build

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

const buildStreamName = "BUILD"

type NATSBus struct {
	conn *nats.Conn
	js   nats.JetStreamContext
}

func NewNATSBus(url string) (*NATSBus, error) {
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
	_, err := b.js.StreamInfo(buildStreamName)
	if err == nil {
		return nil
	}
	if err != nats.ErrStreamNotFound {
		return err
	}
	_, err = b.js.AddStream(&nats.StreamConfig{
		Name:      buildStreamName,
		Subjects:  []string{"build.*"},
		Storage:   nats.FileStorage,
		Retention: nats.WorkQueuePolicy,
		Replicas:  1,
		MaxAge:    24 * time.Hour,
	})
	return err
}

func (b *NATSBus) PublishBuild(ctx context.Context, region string, assignment BuildAssignment) error {
	data, err := json.Marshal(assignment)
	if err != nil {
		return err
	}
	_, err = b.js.PublishMsg(&nats.Msg{
		Subject: fmt.Sprintf("build.%s", region),
		Data:    data,
	}, nats.Context(ctx))
	return err
}

func (b *NATSBus) ConsumeBuild(ctx context.Context, region, consumer string, concurrency int, handler BuildHandler) error {
	if concurrency <= 0 {
		concurrency = 1
	}
	subject := fmt.Sprintf("build.%s", region)
	sub, err := b.js.PullSubscribe(subject, consumer, nats.BindStream(buildStreamName))
	if err != nil {
		return err
	}
	defer func() {
		_ = sub.Unsubscribe()
	}()
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		available := concurrency - len(sem)
		if available <= 0 {
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
				continue
			}
		}
		msgs, err := sub.Fetch(available, nats.MaxWait(2*time.Second))
		if err != nil {
			if err == nats.ErrTimeout || err == context.DeadlineExceeded {
				continue
			}
			return err
		}
		for _, msg := range msgs {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case sem <- struct{}{}:
			}
			wg.Add(1)
			go func(msg *nats.Msg) {
				defer wg.Done()
				defer func() { <-sem }()

				var assignment BuildAssignment
				if err := json.Unmarshal(msg.Data, &assignment); err != nil {
					_ = msg.Term()
					return
				}
				if err := handler(ctx, assignment); err != nil {
					_ = msg.Nak()
					return
				}
				_ = msg.Ack()
			}(msg)
		}
	}
}

func (b *NATSBus) Close() error {
	if b.conn != nil {
		b.conn.Close()
	}
	return nil
}
