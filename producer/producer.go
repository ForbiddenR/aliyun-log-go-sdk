package producer

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	sls "github.com/aliyun/aliyun-log-go-sdk"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
)

const (
	TimeoutExecption      = "TimeoutExecption"
	IllegalStateException = "IllegalStateException"
)

type Producer struct {
	producerConfig        *ProducerConfig
	logAccumulator        *LogAccumulator
	mover                 *Mover
	threadPool            *IoThreadPool
	moverWaitGroup        *sync.WaitGroup
	ioWorkerWaitGroup     *sync.WaitGroup
	ioThreadPoolWaitGroup *sync.WaitGroup
	buckets               int
	logger                log.Logger
	producerLogGroupSize  int64
	monitor               *ProducerMonitor
}

func NewProducer(producerConfig *ProducerConfig) (*Producer, error) {
	logger := getProducerLogger(producerConfig)
	finalProducerConfig := validateProducerConfig(producerConfig, logger)

	client, err := createClient(finalProducerConfig, false, logger)
	if err != nil {
		return nil, err
	}
	return createProducerInternal(client, finalProducerConfig, logger), nil
}

// Deprecated: use NewProducer instead.
func InitProducer(producerConfig *ProducerConfig) *Producer {
	logger := getProducerLogger(producerConfig)
	finalProducerConfig := validateProducerConfig(producerConfig, logger)

	client, _ := createClient(finalProducerConfig, true, logger)
	return createProducerInternal(client, finalProducerConfig, logger)
}

func createProducerInternal(client sls.ClientInterface, finalProducerConfig *ProducerConfig, logger log.Logger) *Producer {
	configureClient(client, finalProducerConfig)
	retryQueue := initRetryQueue()
	errorStatusMap := func() map[int]*string {
		errorCodeMap := map[int]*string{}
		for _, v := range finalProducerConfig.NoRetryStatusCodeList {
			errorCodeMap[int(v)] = nil
		}
		return errorCodeMap
	}()
	producer := &Producer{
		producerConfig: finalProducerConfig,
		buckets:        finalProducerConfig.Buckets,
	}
	ioWorker := initIoWorker(client, retryQueue, logger, finalProducerConfig.MaxIoWorkerCount, errorStatusMap, producer)
	threadPool := initIoThreadPool(ioWorker, logger)
	logAccumulator := initLogAccumulator(finalProducerConfig, ioWorker, logger, threadPool, producer)
	mover := initMover(logAccumulator, retryQueue, ioWorker, logger, threadPool)

	producer.logAccumulator = logAccumulator
	producer.mover = mover
	producer.threadPool = threadPool
	producer.moverWaitGroup = &sync.WaitGroup{}
	producer.ioWorkerWaitGroup = &sync.WaitGroup{}
	producer.ioThreadPoolWaitGroup = &sync.WaitGroup{}
	producer.logger = logger
	producer.monitor = newProducerMonitor()
	return producer
}

func configureClient(client sls.ClientInterface, producerConfig *ProducerConfig) {
	if producerConfig.Region != "" {
		client.SetRegion(producerConfig.Region)
	}
	if producerConfig.AuthVersion != "" {
		client.SetAuthVersion(producerConfig.AuthVersion)
	}
	if producerConfig.HTTPClient != nil {
		client.SetHTTPClient(producerConfig.HTTPClient)
	}
	if producerConfig.UserAgent != "" {
		client.SetUserAgent(producerConfig.UserAgent)
	}
}

func createClient(producerConfig *ProducerConfig, allowStsFallback bool, logger log.Logger) (sls.ClientInterface, error) {
	// use CredentialsProvider
	if producerConfig.CredentialsProvider != nil {
		return sls.CreateNormalInterfaceV2(producerConfig.Endpoint, producerConfig.CredentialsProvider), nil
	}
	// use UpdateStsTokenFunc
	if producerConfig.UpdateStsToken != nil && producerConfig.StsTokenShutDown != nil {
		client, err := sls.CreateTokenAutoUpdateClient(producerConfig.Endpoint, producerConfig.UpdateStsToken, producerConfig.StsTokenShutDown)
		if err == nil || !allowStsFallback {
			return client, err
		}
		// for backward compatibility
		level.Warn(logger).Log("msg", "Failed to create ststoken client, use default client without ststoken.", "error", err)
	}
	// fallback to default static long-lived AK
	staticProvider := sls.NewStaticCredentialsProvider(producerConfig.AccessKeyID, producerConfig.AccessKeySecret, "")
	return sls.CreateNormalInterfaceV2(producerConfig.Endpoint, staticProvider), nil
}

func validateProducerConfig(producerConfig *ProducerConfig, logger log.Logger) *ProducerConfig {
	if producerConfig.MaxReservedAttempts <= 0 {
		level.Warn(logger).Log("msg", "This MaxReservedAttempts parameter must be greater than zero,program auto correction to default value")
		producerConfig.MaxReservedAttempts = 11
	}
	if producerConfig.MaxBatchCount > 40960 || producerConfig.MaxBatchCount <= 0 {
		level.Warn(logger).Log("msg", "The parameter MaxBatchCount exceeds the set maximum and has been reset to the set maximum of 40960.")
		producerConfig.MaxBatchCount = 40960
	}
	if producerConfig.MaxBatchSize > 1024*1024*5 || producerConfig.MaxBatchSize <= 0 {
		level.Warn(logger).Log("msg", "The parameter MaxBatchSize exceeds the settable maximum and has reset a single logGroup memory size of up to 5M.")
		producerConfig.MaxBatchSize = 1024 * 1024 * 5
	}
	if producerConfig.MaxIoWorkerCount <= 0 {
		level.Warn(logger).Log("msg", "The MaxIoWorkerCount parameter cannot be less than zero and has been reset to the default value of 50")
		producerConfig.MaxIoWorkerCount = 50
	}
	if producerConfig.BaseRetryBackoffMs <= 0 {
		level.Warn(logger).Log("msg", "The BaseRetryBackoffMs parameter cannot be less than zero and has been reset to the default value of 100 milliseconds")
		producerConfig.BaseRetryBackoffMs = 100
	}
	if producerConfig.TotalSizeLnBytes <= 0 {
		level.Warn(logger).Log("msg", "The TotalSizeLnBytes parameter cannot be less than zero and has been reset to the default value of 100M")
		producerConfig.TotalSizeLnBytes = 100 * 1024 * 1024
	}
	if producerConfig.LingerMs < 100 {
		level.Warn(logger).Log("msg", "The LingerMs parameter cannot be less than 100 milliseconds and has been reset to the default value of 2000 milliseconds")
		producerConfig.LingerMs = 2000
	}
	return producerConfig
}

func (producer *Producer) HashSendLogWithCallBack(project, logstore, shardHash, topic, source string, log *sls.Log, callback CallBack) error {
	err := producer.waitTime()
	if err != nil {
		return err
	}
	if producer.producerConfig.AdjustShargHash {
		shardHash, err = AdjustHash(shardHash, producer.buckets)
		if err != nil {
			return err
		}
	}
	return producer.logAccumulator.addLogToProducerBatch(project, logstore, shardHash, topic, source, log, callback)
}

func (producer *Producer) HashSendLogListWithCallBack(project, logstore, shardHash, topic, source string, logList []*sls.Log, callback CallBack) (err error) {

	err = producer.waitTime()
	if err != nil {
		return err
	}
	if producer.producerConfig.AdjustShargHash {
		shardHash, err = AdjustHash(shardHash, producer.buckets)
		if err != nil {
			return err
		}
	}
	return producer.logAccumulator.addLogToProducerBatch(project, logstore, shardHash, topic, source, logList, callback)
}

func (producer *Producer) SendLog(project, logstore, topic, source string, log *sls.Log) error {
	err := producer.waitTime()
	if err != nil {
		return err
	}
	return producer.logAccumulator.addLogToProducerBatch(project, logstore, "", topic, source, log, nil)
}

func (producer *Producer) SendLogList(project, logstore, topic, source string, logList []*sls.Log) (err error) {
	err = producer.waitTime()
	if err != nil {
		return err
	}

	return producer.logAccumulator.addLogToProducerBatch(project, logstore, "", topic, source, logList, nil)

}

func (producer *Producer) HashSendLog(project, logstore, shardHash, topic, source string, log *sls.Log) error {
	err := producer.waitTime()
	if err != nil {
		return err
	}
	if producer.producerConfig.AdjustShargHash {
		shardHash, err = AdjustHash(shardHash, producer.buckets)
		if err != nil {
			return err
		}
	}
	return producer.logAccumulator.addLogToProducerBatch(project, logstore, shardHash, topic, source, log, nil)
}

func (producer *Producer) HashSendLogList(project, logstore, shardHash, topic, source string, logList []*sls.Log) (err error) {
	err = producer.waitTime()
	if err != nil {
		return err
	}
	if producer.producerConfig.AdjustShargHash {
		shardHash, err = AdjustHash(shardHash, producer.buckets)
		if err != nil {
			return err
		}
	}
	return producer.logAccumulator.addLogToProducerBatch(project, logstore, shardHash, topic, source, logList, nil)

}

func (producer *Producer) SendLogWithCallBack(project, logstore, topic, source string, log *sls.Log, callback CallBack) error {
	err := producer.waitTime()
	if err != nil {
		return err
	}
	return producer.logAccumulator.addLogToProducerBatch(project, logstore, "", topic, source, log, callback)
}

func (producer *Producer) SendLogListWithCallBack(project, logstore, topic, source string, logList []*sls.Log, callback CallBack) (err error) {
	err = producer.waitTime()
	if err != nil {
		return err
	}
	return producer.logAccumulator.addLogToProducerBatch(project, logstore, "", topic, source, logList, callback)

}

// todo: refactor this
func (producer *Producer) waitTime() error {
	if atomic.LoadInt64(&producer.producerLogGroupSize) <= producer.producerConfig.TotalSizeLnBytes {
		return nil
	}

	// no wait
	if producer.producerConfig.MaxBlockSec == 0 {
		if atomic.LoadInt64(&producer.producerLogGroupSize) > producer.producerConfig.TotalSizeLnBytes {
			level.Error(producer.logger).Log("msg", "Over producer set maximum blocking time")
			return errors.New(TimeoutExecption)
		}
		return nil
	}

	defer producer.monitor.recordWaitMemory(time.Now())

	// infinite wait
	if producer.producerConfig.MaxBlockSec < 0 {
		for atomic.LoadInt64(&producer.producerLogGroupSize) > producer.producerConfig.TotalSizeLnBytes {
			time.Sleep(waitTimeUnit)
		}
		return nil
	}

	// todo: refine this, limited wait
	for i := 0; i < producer.producerConfig.MaxBlockSec*waitUnitPerSec; i++ {
		if atomic.LoadInt64(&producer.producerLogGroupSize) > producer.producerConfig.TotalSizeLnBytes {
			time.Sleep(waitTimeUnit)
		} else {
			return nil
		}
	}

	producer.monitor.incWaitMemoryFail()
	level.Error(producer.logger).Log("msg", "Over producer set maximum blocking time")
	return errors.New(TimeoutExecption)
}

const waitTimeUnit = time.Millisecond * 10
const waitUnitPerSec = int(time.Second / waitTimeUnit)

func (producer *Producer) Start() {
	producer.moverWaitGroup.Add(1)
	level.Info(producer.logger).Log("msg", "producer mover start")
	go producer.mover.run(producer.moverWaitGroup, producer.producerConfig)
	producer.ioThreadPoolWaitGroup.Add(1)
	go producer.threadPool.start(producer.ioWorkerWaitGroup, producer.ioThreadPoolWaitGroup)
	if !producer.producerConfig.DisableRuntimeMetrics {
		go producer.monitor.reportThread(time.Minute, producer.logger)
	}
}

// Limited closing transfer parameter nil, safe closing transfer timeout time, timeout Ms parameter in milliseconds
func (producer *Producer) Close(timeoutMs int64) error {
	startCloseTime := time.Now()
	producer.sendCloseProdcerSignal()
	producer.moverWaitGroup.Wait()
	producer.threadPool.ShutDown()
	for !producer.threadPool.Stopped() {
		if time.Since(startCloseTime) > time.Duration(timeoutMs)*time.Millisecond {
			level.Warn(producer.logger).Log("msg", "The producer timeout closes, and some of the cached data may not be sent properly")
			return errors.New(TimeoutExecption)
		}
		time.Sleep(100 * time.Millisecond)
	}
	level.Info(producer.logger).Log("msg", "All groutines of producer have been shutdown")
	return nil
}

func (producer *Producer) SafeClose() {
	producer.sendCloseProdcerSignal()
	producer.moverWaitGroup.Wait()
	level.Info(producer.logger).Log("msg", "Mover close finish")
	producer.threadPool.ShutDown()
	producer.ioThreadPoolWaitGroup.Wait()
	level.Info(producer.logger).Log("msg", "IoThreadPool close finish")
	producer.ioWorkerWaitGroup.Wait()
	level.Info(producer.logger).Log("msg", "Producer close finish")
}

func (producer *Producer) sendCloseProdcerSignal() {
	level.Info(producer.logger).Log("msg", "producer start closing")
	producer.closeStstokenChannel()
	producer.mover.moverShutDownFlag.Store(true)
	producer.logAccumulator.shutDownFlag.Store(true)
	producer.mover.ioWorker.retryQueueShutDownFlag.Store(true)
}

func (producer *Producer) closeStstokenChannel() {
	if producer.producerConfig.StsTokenShutDown != nil {
		close(producer.producerConfig.StsTokenShutDown)
		level.Info(producer.logger).Log("msg", "producer closed ststoken")
	}
}
