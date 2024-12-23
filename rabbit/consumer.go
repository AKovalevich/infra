package infrarabbit

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	infralog "github.com/pushwoosh/infra/log"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
)

const (
	defaultPrefetchCount = 1
	defaultVHost         = "/"
	defaultUser          = "guest"
	defaultPassword      = "guest"

	connCloseChanSize             = 8096
	metricsIntervalCheckDefault   = time.Hour * 24 * 365
	heartbeatIntervalCheck        = time.Second
	heartbeatReconnectionInterval = 5 * time.Minute
)

var connectionsManager = newConnManager()

type Consumer struct {
	connCfg         *ConnectionConfig
	cfg             *ConsumerConfig
	ch              chan *Message
	mu              sync.Mutex
	closed          chan bool
	isClosed        bool
	itemsInProgress sync.WaitGroup
}

func (c *Consumer) start() {
	cfg := c.cfg
	host, _ := getHostPort(c.connCfg.Address)

	metricsInterval := metricsIntervalCheckDefault
	if cfg.Metrics != nil && cfg.Metrics.CheckInterval > 0 {
		metricsInterval = cfg.Metrics.CheckInterval
	}

	metricsTicker := time.NewTicker(metricsInterval)
	defer metricsTicker.Stop()

	heartbeatTicker := time.NewTicker(heartbeatIntervalCheck)
	defer heartbeatTicker.Stop()

	var channel *amqp.Channel
	var deliveries <-chan amqp.Delivery

reconnectLoop:
	for !c.isClosed {
		infralog.Error("connectionsManager.Get",
			zap.String("queue", cfg.Queue), zap.Error(errors.New("connectionsManager.Get")))
		conn, isNewConn, err := connectionsManager.Get(c.connCfg, cfg.Tag)
		if err != nil {
			infralog.Error("connectionsManager.Get",
				zap.String("queue", cfg.Queue),
				zap.Error(err))
			time.Sleep(time.Second) // time to wait to not make infinite "for" loop
			continue
		}

		infralog.Error("connectionsManager.CreateConsumerChannel",
			zap.String("queue", cfg.Queue), zap.Error(errors.New("connectionsManager.CreateConsumerChannel")))
		channel, deliveries, err = connectionsManager.CreateConsumerChannel(
			conn,
			cfg.Tag,
			cfg.Queue,
			cfg.QueuePriority,
			cfg.PrefetchCount)
		if err != nil {
			infralog.Error("CreateConsumerChannel",
				zap.String("queue", cfg.Queue),
				zap.Error(err))
			connectionsManager.CloseConnection(conn)
			time.Sleep(time.Second) // time to wait to not make infinite "for" loop
			continue
		}

		channelClose := channel.NotifyClose(make(chan *amqp.Error, connCloseChanSize))
		var connClose chan *amqp.Error
		if isNewConn {
			connClose = conn.NotifyClose(make(chan *amqp.Error, connCloseChanSize))
		}

		lastTimeConnectionUsed := time.Now()
		isNeedRecreateChannel := atomic.Bool{}

		var callback = func(err error) {
			if err != nil {
				infralog.Error("callback error",
					zap.String("queue", cfg.Queue),
					zap.Error(err))
				isNeedRecreateChannel.Store(true)
			}
			c.itemsInProgress.Done()
		}

		for !c.isClosed {
			select {
			case closeErr, isOpen := <-connClose:
				if closeErr != nil {
					infralog.Error("rabbit consumer connection error",
						zap.String("queue", cfg.Queue),
						zap.Error(closeErr))
				}

				if closeErr != nil || !isOpen {
					go readAllErrors(connClose)
					connectionsManager.CloseConnection(conn)
					continue reconnectLoop
				}
			case closeErr, isOpen := <-channelClose:
				if closeErr != nil {
					infralog.Error("rabbit consumer channel error",
						zap.String("queue", cfg.Queue),
						zap.Error(closeErr))
				}

				if closeErr != nil || !isOpen {
					go readAllErrors(channelClose)
					connectionsManager.CloseConsumerChannel(channel)
					continue reconnectLoop
				}
			case <-heartbeatTicker.C:
				if time.Since(lastTimeConnectionUsed) > heartbeatReconnectionInterval || isNeedRecreateChannel.Load() {
					if isNeedRecreateChannel.Load() {
						infralog.Error("heartbeatTicker.C",
							zap.Error(errors.New("heartbeatTicker.C")),
							zap.Duration("lastTimeConnectionUsed duration", time.Since(lastTimeConnectionUsed)),
							zap.Time("lastTimeConnectionUsed", lastTimeConnectionUsed),
							zap.String("queue", cfg.Queue),
							zap.Bool("isNeedRecreateChannel", isNeedRecreateChannel.Load()))
					}
					connectionsManager.CloseConsumerChannel(channel)
					continue reconnectLoop
				}
			case <-metricsTicker.C:
				go collectMetrics(cfg, channel, host, cfg.Queue)
			case msg, isOpen := <-deliveries:
				if !isOpen {
					connectionsManager.CloseConsumerChannel(channel)
					continue reconnectLoop
				}
				lastTimeConnectionUsed = time.Now()
				c.itemsInProgress.Add(1)
				c.ch <- &Message{
					msg:      &msg,
					host:     host,
					queue:    cfg.Queue,
					callback: callback,
				}
			}
		}
	}
	c.itemsInProgress.Wait()
	connectionsManager.CloseConsumerChannel(channel)
	close(c.ch)
	close(c.closed)
}

func (c *Consumer) Consume() chan *Message {
	return c.ch
}

func (c *Consumer) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.isClosed {
		return nil
	}

	c.isClosed = true
	<-c.closed
	return nil
}

func collectMetrics(
	cfg *ConsumerConfig,
	channel *amqp.Channel,
	host string,
	queue string,
) {
	defer func() {
		if e := recover(); e != nil {
			infralog.Error(
				"unable to collect rabbit metrics",
				zap.Error(errors.Errorf("%v", e)))
		}
	}()

	if channel != nil && !channel.IsClosed() && cfg.Metrics != nil {
		q, err := channel.QueueDeclarePassive(
			queue,
			false, // durable
			false, // delete when unused
			false, // exclusive
			false, // noWait
			nil,   // arguments
		)
		if err != nil {
			return
		}

		if cfg.Metrics.QueueLength != nil {
			cfg.Metrics.QueueLength(host, queue, int64(q.Messages))
		}

		if cfg.Metrics.QueueDelay == nil {
			return
		}

		if q.Messages == 0 {
			cfg.Metrics.QueueDelay(host, queue, 0)
			return
		}

		msg, ok, err := channel.Get(queue, false)
		if err == nil && ok {
			seconds := time.Since(msg.Timestamp).Seconds()
			_ = msg.Reject(true)
			if seconds >= 0 {
				cfg.Metrics.QueueDelay(host, queue, int64(seconds))
			}
		}
	}
}

func readAllErrors(ch chan *amqp.Error) {
	for range ch {
		// need to read all errors to avoid deadlocks
		// https://github.com/rabbitmq/amqp091-go/issues/18
	}
}
