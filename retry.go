/*
Simple library for retry mechanism

slightly inspired by [Try::Tiny::Retry](https://metacpan.org/pod/Try::Tiny::Retry)

# SYNOPSIS

http get with retry:

	url := "http://example.com"
	var body []byte

	err := retry.Do(
		func() error {
			resp, err := http.Get(url)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			body, err = ioutil.ReadAll(resp.Body)
			if err != nil {
				return err
			}

			return nil
		},
	)
	if err != nil {
		// handle error
	}

	fmt.Println(string(body))

http get with retry with data:

	url := "http://example.com"

	body, err := retry.DoWithData(
		func() ([]byte, error) {
			resp, err := http.Get(url)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return nil, err
			}

			return body, nil
		},
	)
	if err != nil {
		// handle error
	}

	fmt.Println(string(body))

[next examples](https://github.com/avast/retry-go/tree/master/examples)

# SEE ALSO

* [giantswarm/retry-go](https://github.com/giantswarm/retry-go) - slightly complicated interface.

* [sethgrid/pester](https://github.com/sethgrid/pester) - only http retry for http calls with retries and backoff

* [cenkalti/backoff](https://github.com/cenkalti/backoff) - Go port of the exponential backoff algorithm from Google's HTTP Client Library for Java. Really complicated interface.

* [rafaeljesus/retry-go](https://github.com/rafaeljesus/retry-go) - looks good, slightly similar as this package, don't have 'simple' `Retry` method

* [matryer/try](https://github.com/matryer/try) - very popular package, nonintuitive interface (for me)

# BREAKING CHANGES

* 4.0.0
  - infinity retry is possible by set `Attempts(0)` by PR [#49](https://github.com/avast/retry-go/pull/49)

* 3.0.0
  - `DelayTypeFunc` accepts a new parameter `err` - this breaking change affects only your custom Delay Functions. This change allow [make delay functions based on error](examples/delay_based_on_error_test.go).

* 1.0.2 -> 2.0.0
  - argument of `retry.Delay` is final delay (no multiplication by `retry.Units` anymore)
  - function `retry.Units` are removed
  - [more about this breaking change](https://github.com/avast/retry-go/issues/7)

* 0.3.0 -> 1.0.0
  - `retry.Retry` function are changed to `retry.Do` function
  - `retry.RetryCustom` (OnRetry) and `retry.RetryCustomWithOpts` functions are now implement via functions produces Options (aka `retry.OnRetry`)
*/

package retry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Function signature of retryable function
type RetryableFunc func() error

// Function signature of retryable function with data
type RetryableFuncWithData[T any] func() (T, error)

// Default timer is a wrapper around time.After
type timerImpl struct{}

func (t *timerImpl) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// Do executes the retryable function with the given options
func Do(retryableFunc RetryableFunc, opts ...Option) error {
	retryableFuncWithData := func() (any, error) {
		return nil, retryableFunc()
	}

	_, err := DoWithData(retryableFuncWithData, opts...)
	return err
}

// DoWithData executes the retryable function with data with the given options
func DoWithData[T any](retryableFunc RetryableFuncWithData[T], options ...Option) (T, error) {
	var attemptCount uint
	var emptyResult T

	// Default configuration
	config := newDefaultRetryConfig()

	// Apply options
	for _, opt := range options {
		opt(config)
	}

	// Check if context is already done
	if err := config.context.Err(); err != nil {
		return emptyResult, err
	}

	// Setting attempts to 0 means we'll retry until we succeed
	if config.attempts == 0 {
		for {
			result, err := retryableFunc()
			if err == nil {
				return result, nil
			}

			if !config.retryIf(err) || !IsRecoverable(err) {
				return emptyResult, err
			}

			attemptCount++
			config.onRetry(attemptCount, err)
			select {
			case <-config.timer.After(delay(config, attemptCount, err)):
			case <-config.context.Done():
				if config.wrapContextErrorWithLastError {
					return emptyResult, Error{config.context.Err(), err}
				}
				return emptyResult, config.context.Err()
			}
		}
	}

	errorLog := Error{}

	attemptsForError := make(map[error]uint, len(config.attemptsForError))
	for err, attempts := range config.attemptsForError {
		attemptsForError[err] = attempts
	}

	shouldRetry := true
	errorCounts := make(map[error]uint, len(attemptsForError))
	for err, attempts := range attemptsForError {
		errorCounts[err] = attempts
	}

	for shouldRetry {
		result, err := retryableFunc()
		if err == nil {
			return result, nil
		}

		errorLog = append(errorLog, unpackUnrecoverable(err))

		if !config.retryIf(err) {
			break
		}

		config.onRetry(attemptCount, err) // onRetry should be called before checking attempts

		if attempts, ok := errorCounts[err]; ok {
			attempts--
			errorCounts[err] = attempts
			shouldRetry = shouldRetry && attempts > 0
		}

		// If this is the last attempt, don't wait
		if attemptCount == config.attempts-1 {
			break
		}

		select {
		case <-config.timer.After(delay(config, attemptCount, err)):
		case <-config.context.Done():
			if config.lastErrorOnly {
				return emptyResult, config.context.Err()
			}

			return emptyResult, append(errorLog, config.context.Err())
		}

		attemptCount++
		shouldRetry = shouldRetry && attemptCount < config.attempts
	}

	if config.lastErrorOnly {
		return emptyResult, errorLog.Unwrap()
	}

	return emptyResult, errorLog
}

// newDefaultRetryConfig returns the default configuration for retry
func newDefaultRetryConfig() *Config {
	return &Config{
		attempts:         uint(10),
		attemptsForError: make(map[error]uint),
		delay:            100 * time.Millisecond,
		maxJitter:        100 * time.Millisecond,
		onRetry:          func(n uint, err error) {},
		retryIf:          IsRecoverable,
		delayType:        CombineDelay(BackOffDelay, RandomDelay),
		lastErrorOnly:    false,
		context:          context.Background(),
		timer:            &timerImpl{},
	}
}

// Error type represents list of errors in retry
type Error []error

// Error method return string representation of Error
// It is an implementation of error interface
func (e Error) Error() string {
	logsWithNumbers := make([]string, len(e))
	for i, err := range e {
		if err != nil {
			logsWithNumbers[i] = fmt.Sprintf("#%d: %s", i+1, err.Error())
		}
	}

	return fmt.Sprintf("All attempts fail:\n%s", strings.Join(logsWithNumbers, "\n"))
}

// Is checks if the error list contains the target error
func (e Error) Is(target error) bool {
	for _, v := range e {
		if errors.Is(v, target) {
			return true
		}
	}
	return false
}

// As checks if the error list contains an error assignable to the target type
func (e Error) As(target interface{}) bool {
	for _, v := range e {
		if errors.As(v, target) {
			return true
		}
	}
	return false
}

/*
Unwrap the last error for compatibility with `errors.Unwrap()`.
When you need to unwrap all errors, you should use `WrappedErrors()` instead.

	err := Do(
		func() error {
			return errors.New("original error")
		},
		Attempts(1),
	)

	fmt.Println(errors.Unwrap(err)) # "original error" is printed

Added in version 4.2.0.
*/
func (e Error) Unwrap() error {
	return e[len(e)-1]
}

// WrappedErrors returns the list of errors that this Error is wrapping.
// It is an implementation of the `errwrap.Wrapper` interface
// in package [errwrap](https://github.com/hashicorp/errwrap) so that
// `retry.Error` can be used with that library.
func (e Error) WrappedErrors() []error {
	return e
}

type unrecoverableError struct {
	error
}

func (e unrecoverableError) Error() string {
	if e.error == nil {
		return "unrecoverable error"
	}
	return e.error.Error()
}

func (e unrecoverableError) Unwrap() error {
	return e.error
}

// Unrecoverable wraps an error in `unrecoverableError` struct
func Unrecoverable(err error) error {
	return unrecoverableError{err}
}

// IsRecoverable checks if error is an instance of `unrecoverableError`
func IsRecoverable(err error) bool {
	return !errors.Is(err, unrecoverableError{})
}

// Is checks if the error is an instance of `unrecoverableError`
func (unrecoverableError) Is(err error) bool {
	_, isUnrecoverable := err.(unrecoverableError)
	return isUnrecoverable
}

func unpackUnrecoverable(err error) error {
	if unrecoverable, isUnrecoverable := err.(unrecoverableError); isUnrecoverable {
		return unrecoverable.error
	}

	return err
}

func delay(config *Config, n uint, err error) time.Duration {
	delayTime := config.delayType(n, err, config)
	if config.maxDelay > 0 && delayTime > config.maxDelay {
		delayTime = config.maxDelay
	}

	return delayTime
}
