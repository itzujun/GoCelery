package gocelery

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/itzujun/gocelery/retry"
	"github.com/itzujun/gocelery/tasks"
	"github.com/itzujun/gocelery/tracing"
	"github.com/opentracing/opentracing-go"
)

type Worker struct {
	server          *Server
	ConsumerTag     string
	Concurrency     int
	Queue           string
	errorHandler    func(err error)
	preTaskHandler  func(*tasks.Signature)
	postTaskHandler func(*tasks.Signature)
}

func (worker *Worker) Launch() error {
	errorsChan := make(chan error)

	worker.LaunchAsync(errorsChan)

	return <-errorsChan
}

func (worker *Worker) LaunchAsync(errorsChan chan<- error) {
	cnf := worker.server.GetConfig()
	broker := worker.server.GetBroker()

	fmt.Println("Launching a worker with the following settings:")
	fmt.Printf("- Broker: %s \n", cnf.Broker)
	if worker.Queue == "" {
		fmt.Printf("- DefaultQueue: %s \n", cnf.DefaultQueue)
	} else {
		fmt.Printf("- CustomQueue: %s \n", worker.Queue)
	}
	fmt.Printf("- ResultBackend: %s \n", cnf.ResultBackend)
	if cnf.AMQP != nil {
		fmt.Printf("- AMQP: %s", cnf.AMQP.Exchange)
		fmt.Printf("  - Exchange: %s \n", cnf.AMQP.Exchange)
		fmt.Printf("  - ExchangeType: %s \n", cnf.AMQP.ExchangeType)
		fmt.Printf("  - BindingKey: %s \n", cnf.AMQP.BindingKey)
		fmt.Printf("  - PrefetchCount: %d \n", cnf.AMQP.PrefetchCount)
	}

	go func() {
		for {
			retry, err := broker.StartConsuming(worker.ConsumerTag, worker.Concurrency, worker)

			if retry {
				if worker.errorHandler != nil {
					worker.errorHandler(err)
				} else {
					fmt.Printf("broker failed with error: %s", err)
				}
			} else {
				errorsChan <- err // stop the goroutine
				return
			}
		}
	}()
	if !cnf.NoUnixSignals {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		var signalsReceived uint
		go func() {
			for {
				select {
				case s := <-sig:
					fmt.Printf("signal received: %v", s)
					signalsReceived++
					if signalsReceived < 2 {
						go func() {
							worker.Quit()
							errorsChan <- errors.New("worker quit gracefully")
						}()
					} else {
						errorsChan <- errors.New("worker quit abruptly")
					}
				}
			}
		}()
	}
}

func (worker *Worker) CustomQueue() string {
	return worker.Queue
}

func (worker *Worker) Quit() {
	worker.server.GetBroker().StopConsuming()
}

func (worker *Worker) Process(signature *tasks.Signature) error {
	if !worker.server.IsTaskRegistered(signature.Name) {
		return nil
	}
	taskFunc, err := worker.server.GetRegisteredTask(signature.Name)
	if err != nil {
		return nil
	}
	if err = worker.server.GetBackend().SetStateReceived(signature); err != nil {
		return fmt.Errorf("set state to 'received' for task %s returned error: %s", signature.UUID, err)
	}
	task, err := tasks.New(taskFunc, signature.Args)
	if err != nil {
		worker.taskFailed(signature, err)
		return err
	}
	taskSpan := tracing.StartSpanFromHeaders(signature.Headers, signature.Name)
	tracing.AnnotateSpanWithSignatureInfo(taskSpan, signature)
	task.Context = opentracing.ContextWithSpan(task.Context, taskSpan)
	if err = worker.server.GetBackend().SetStateStarted(signature); err != nil {
		return fmt.Errorf("set state to 'started' for task %s returned error: %s", signature.UUID, err)
	}
	if worker.preTaskHandler != nil {
		worker.preTaskHandler(signature)
	}
	if worker.postTaskHandler != nil {
		defer worker.postTaskHandler(signature)
	}
	results, err := task.Call()
	if err != nil {
		retriableErr, ok := interface{}(err).(tasks.ErrRetryTaskLater)
		if ok {
			return worker.retryTaskIn(signature, retriableErr.RetryIn())
		}
		if signature.RetryCount > 0 {
			return worker.taskRetry(signature)
		}
		return worker.taskFailed(signature, err)
	}
	return worker.taskSucceeded(signature, results)
}

func (worker *Worker) taskRetry(signature *tasks.Signature) error {
	if err := worker.server.GetBackend().SetStateRetry(signature); err != nil {
		return fmt.Errorf("set state to 'retry' for task %s returned error: %s", signature.UUID, err)
	}
	signature.RetryCount--
	signature.RetryTimeout = retry.FibonacciNext(signature.RetryTimeout)
	eta := time.Now().UTC().Add(time.Second * time.Duration(signature.RetryTimeout))
	signature.ETA = &eta
	fmt.Println("task %s failed. Going to retry in %d seconds.", signature.UUID, signature.RetryTimeout)
	fmt.Println("task %s failed. Going to retry in %d seconds.", signature.UUID, signature.RetryTimeout)
	_, err := worker.server.SendTask(signature)
	return err
}

func (worker *Worker) retryTaskIn(signature *tasks.Signature, retryIn time.Duration) error {
	if err := worker.server.GetBackend().SetStateRetry(signature); err != nil {
		return fmt.Errorf("set state to 'retry' for task %s returned error: %s", signature.UUID, err)
	}
	eta := time.Now().UTC().Add(retryIn)
	signature.ETA = &eta
	fmt.Printf("task %s failed. Going to retry in %.0f seconds.", signature.UUID, retryIn.Seconds())
	_, err := worker.server.SendTask(signature)
	return err
}

func (worker *Worker) taskSucceeded(signature *tasks.Signature, taskResults []*tasks.TaskResult) error {
	if err := worker.server.GetBackend().SetStateSuccess(signature, taskResults); err != nil {
		return fmt.Errorf("set state to 'success' for task %s returned error: %s", signature.UUID, err)
	}
	var debugResults = "[]"
	results, err := tasks.ReflectTaskResults(taskResults)
	if err != nil {
		fmt.Printf("reflect task result error %s", err.Error())
		return nil
	} else {
		debugResults = tasks.HumanReadableResults(results)
	}
	fmt.Printf("Processed task %s. Results = %s", signature.UUID, debugResults)
	for _, successTask := range signature.OnSuccess {
		if signature.Immutable == false {
			for _, taskResult := range taskResults {
				successTask.Args = append(successTask.Args, tasks.Arg{
					Type:  taskResult.Type,
					Value: taskResult.Value,
				})
			}
		}
		worker.server.SendTask(successTask)
	}
	if signature.GroupUUID == "" {
		return nil
	}
	groupCompleted, err := worker.server.GetBackend().GroupCompleted(
		signature.GroupUUID,
		signature.GroupTaskCount,
	)
	if err != nil {
		return fmt.Errorf("completed check for group %s returned error: %s", signature.GroupUUID, err)
	}
	if !groupCompleted {
		return nil
	}
	if worker.hasAMQPBackend() {
		defer worker.server.GetBackend().PurgeGroupMeta(signature.GroupUUID)
	}
	if signature.ChordCallback == nil {
		return nil
	}
	shouldTrigger, err := worker.server.GetBackend().TriggerChord(signature.GroupUUID)
	if err != nil {
		return fmt.Errorf("triggering chord for group %s returned error: %s", signature.GroupUUID, err)
	}
	if !shouldTrigger {
		return nil
	}
	taskStates, err := worker.server.GetBackend().GroupTaskStates(
		signature.GroupUUID,
		signature.GroupTaskCount,
	)
	if err != nil {
		return nil
	}
	for _, taskState := range taskStates {
		if !taskState.IsSuccess() {
			return nil
		}
		if signature.ChordCallback.Immutable == false {
			for _, taskResult := range taskState.Results {
				signature.ChordCallback.Args = append(signature.ChordCallback.Args, tasks.Arg{
					Type:  taskResult.Type,
					Value: taskResult.Value,
				})
			}
		}
	}

	_, err = worker.server.SendTask(signature.ChordCallback)
	if err != nil {
		return err
	}
	return nil
}

func (worker *Worker) taskFailed(signature *tasks.Signature, taskErr error) error {
	if err := worker.server.GetBackend().SetStateFailure(signature, taskErr.Error()); err != nil {
		return fmt.Errorf("set state to 'failure' for task %s returned error: %s", signature.UUID, err)
	}
	if worker.errorHandler != nil {
		worker.errorHandler(taskErr)
	} else {
		fmt.Printf("failed processing task %s. Error = %v", signature.UUID, taskErr)
	}
	for _, errorTask := range signature.OnError {
		args := append([]tasks.Arg{{
			Type:  "string",
			Value: taskErr.Error(),
		}}, errorTask.Args...)
		errorTask.Args = args
		worker.server.SendTask(errorTask)
	}
	return nil
}

func (worker *Worker) hasAMQPBackend() bool {
	return false
}

func (worker *Worker) SetErrorHandler(handler func(err error)) {
	worker.errorHandler = handler
}

func (worker *Worker) SetPreTaskHandler(handler func(*tasks.Signature)) {
	worker.preTaskHandler = handler
}

func (worker *Worker) SetPostTaskHandler(handler func(*tasks.Signature)) {
	worker.postTaskHandler = handler
}

func (worker *Worker) GetServer() *Server {
	return worker.server
}
