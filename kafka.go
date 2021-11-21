package gomiddleware

import (
	"context"
	"log"
	"time"

	"github.com/segmentio/kafka-go"
)

func Produce(kafkaWriter *kafka.Writer, ctx context.Context, message string) {
	// intialize the writer with the broker addresses, and the topic
	msg := kafka.Message{
		// Key:   []byte(fmt.Sprintf("address-%s", req.RemoteAddr)),
		Value: []byte(message),
	}
	err := kafkaWriter.WriteMessages(ctx, msg)

	if err != nil {
		log.Fatalln(err)
	}

}
func GetKafkaWriter(kafkaURL, topic string, batchSize int, batchTimeout time.Duration) *kafka.Writer {
	return &kafka.Writer{
		Addr:         kafka.TCP(kafkaURL),
		Topic:        topic,
		BatchSize:    batchSize,
		BatchTimeout: batchTimeout,
	}
}
