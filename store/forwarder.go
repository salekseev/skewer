package store

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inconshreveable/log15"
	"github.com/stephane-martin/skewer/conf"
	"github.com/stephane-martin/skewer/javascript"
	"github.com/stephane-martin/skewer/metrics"
	"github.com/stephane-martin/skewer/model"
	sarama "gopkg.in/Shopify/sarama.v1"
)

func NewForwarder(test bool, m *metrics.Metrics, logger log15.Logger) (fwder Forwarder) {
	f := kafkaForwarder{test: test, logger: logger.New("class", "kafkaForwarder"), metrics: m}
	f.errorChan = make(chan struct{})
	f.wg = &sync.WaitGroup{}
	return &f
}

type kafkaForwarder struct {
	logger     log15.Logger
	errorChan  chan struct{}
	wg         *sync.WaitGroup
	forwarding int32
	metrics    *metrics.Metrics
	test       bool
}

func (fwder *kafkaForwarder) ErrorChan() chan struct{} {
	return fwder.errorChan
}

func (fwder *kafkaForwarder) WaitFinished() {
	fwder.wg.Wait()
}

type dummyKafkaForwarder struct {
	logger     log15.Logger
	errorChan  chan struct{}
	wg         *sync.WaitGroup
	forwarding int32
	metrics    *metrics.Metrics
	test       bool
}

func (fwder *kafkaForwarder) Forward(ctx context.Context, from Store, to conf.KafkaConfig) bool {
	// ensure Forward is only executing once
	if !atomic.CompareAndSwapInt32(&fwder.forwarding, 0, 1) {
		return false
	}
	fwder.errorChan = make(chan struct{})
	fwder.wg.Add(1)
	go fwder.doForward(ctx, from, to)
	go func() {
		fwder.wg.Wait()
		atomic.StoreInt32(&fwder.forwarding, 0)
	}()
	return true
}

func (fwder *kafkaForwarder) doForward(ctx context.Context, from Store, to conf.KafkaConfig) {
	defer fwder.wg.Done()
	var succChan <-chan *sarama.ProducerMessage
	var failChan <-chan *sarama.ProducerError
	var producer sarama.AsyncProducer

	if !fwder.test {
		producer = fwder.getProducer(ctx, &to)
		if producer == nil {
			return
		}
		succChan = producer.Successes()
		failChan = producer.Errors()
		// listen for kafka responses
		fwder.wg.Add(1)
		go fwder.listenKafkaResponses(from, succChan, failChan)
	}

	fwder.wg.Add(1)
	go fwder.getAndSendMessages(ctx, from, producer)

}

func (fwder *kafkaForwarder) getAndSendMessages(ctx context.Context, from Store, producer sarama.AsyncProducer) {
	defer func() {
		if producer != nil {
			producer.AsyncClose()
		}
		fwder.wg.Done()
	}()

	jsenvs := map[string]javascript.FilterEnvironment{}

ForOutputs:
	for {
		select {
		case <-ctx.Done():
			return
		case message, more := <-from.Outputs():
			if !more {
				return
			}
			env, ok := jsenvs[message.ConfId]
			if !ok {
				config, err := from.GetSyslogConfig(message.ConfId)
				if err != nil {
					fwder.logger.Warn("Could not find the stored configuration for a message", "confId", message.ConfId, "msgId", message.Uid)
					from.PermError(message.Uid)
					continue ForOutputs
				}
				jsenvs[message.ConfId] = javascript.NewFilterEnvironment(
					config.FilterFunc,
					config.TopicFunc,
					config.TopicTmpl,
					config.PartitionFunc,
					config.PartitionTmpl,
					fwder.logger,
				)
				env = jsenvs[message.ConfId]
			}

			topic, errs := env.Topic(message.Parsed.Fields)
			for _, err := range errs {
				fwder.logger.Info("Error calculating topic", "error", err, "uid", message.Uid)
			}
			partitionKey, errs := env.PartitionKey(message.Parsed.Fields)
			for _, err := range errs {
				fwder.logger.Info("Error calculating the partition key", "error", err, "uid", message.Uid)
			}

			if len(topic) == 0 || len(partitionKey) == 0 {
				fwder.logger.Warn("Topic or PartitionKey could not be calculated", "uid", message.Uid)
				from.PermError(message.Uid)
				continue ForOutputs
			}

			tmsg, filterResult, err := env.FilterMessage(message.Parsed.Fields)

			switch filterResult {
			case javascript.DROPPED:
				from.ACK(message.Uid)
				fwder.metrics.MessageFilteringCounter.WithLabelValues("dropped", message.Parsed.Client).Inc()
				continue ForOutputs
			case javascript.REJECTED:
				fwder.metrics.MessageFilteringCounter.WithLabelValues("rejected", message.Parsed.Client).Inc()
				from.NACK(message.Uid)
				continue ForOutputs
			case javascript.PASS:
				fwder.metrics.MessageFilteringCounter.WithLabelValues("passing", message.Parsed.Client).Inc()
				if tmsg == nil {
					from.ACK(message.Uid)
					continue ForOutputs
				}
			default:
				from.PermError(message.Uid)
				fwder.logger.Warn("Error happened processing message", "uid", message.Uid, "error", err)
				fwder.metrics.MessageFilteringCounter.WithLabelValues("unknown", message.Parsed.Client).Inc()
				continue ForOutputs
			}

			nmsg := model.ParsedMessage{
				Fields:         tmsg,
				Client:         message.Parsed.Client,
				LocalPort:      message.Parsed.LocalPort,
				UnixSocketPath: message.Parsed.UnixSocketPath,
			}

			kafkaMsg, err := nmsg.ToKafkaMessage(partitionKey, topic)
			if err != nil {
				fwder.logger.Warn("Error generating Kafka message", "error", err, "uid", message.Uid)
				from.PermError(message.Uid)
				continue ForOutputs
			}

			kafkaMsg.Metadata = message.Uid
			if producer == nil {
				v, _ := kafkaMsg.Value.Encode()
				pkey, _ := kafkaMsg.Key.Encode()
				fwder.logger.Info("Message", "partitionkey", string(pkey), "topic", kafkaMsg.Topic, "msgid", message.Uid)
				fmt.Println(string(v))
				fmt.Println()
				from.ACK(message.Uid)
			} else {
				producer.Input() <- kafkaMsg
			}
		}
	}
}

func (fwder *kafkaForwarder) getProducer(ctx context.Context, to *conf.KafkaConfig) sarama.AsyncProducer {
	var producer sarama.AsyncProducer
	var err error
	for {
		producer, err = to.GetAsyncProducer()
		if err == nil {
			fwder.logger.Debug("Got a Kafka producer")
			return producer
		} else {
			fwder.metrics.KafkaConnectionErrorCounter.Inc()
			fwder.logger.Warn("Error getting a Kafka client", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (fwder *kafkaForwarder) listenKafkaResponses(from Store, succChan <-chan *sarama.ProducerMessage, failChan <-chan *sarama.ProducerError) {
	defer fwder.wg.Done()

	once := &sync.Once{}

	for {
		if succChan == nil && failChan == nil {
			return
		}
		select {
		case succ, more := <-succChan:
			if more {
				from.ACK(succ.Metadata.(string))
				fwder.metrics.KafkaAckNackCounter.WithLabelValues("ack", succ.Topic).Inc()
			} else {
				succChan = nil
			}

		case fail, more := <-failChan:
			if more {
				from.NACK(fail.Msg.Metadata.(string))
				fwder.logger.Info("Kafka producer error", "error", fail.Error())
				if model.IsFatalKafkaError(fail.Err) {
					once.Do(func() { close(fwder.errorChan) })
				}
				fwder.metrics.KafkaAckNackCounter.WithLabelValues("nack", fail.Msg.Topic).Inc()
			} else {
				failChan = nil
			}
		}
	}

}
