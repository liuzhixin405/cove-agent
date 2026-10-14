package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// retryBodyHint matches the wait a 429 body asks for: Google's RetryInfo
// ("retryDelay": "43s") or its message ("Please retry in 43.33s").
var retryBodyHint = regexp.MustCompile(`(?i)(?:"retryDelay"\s*:\s*"|retry in )([0-9]+(?:\.[0-9]+)?)s`)

// retryAfterFromBody is the wait a rate-limit response body names, 0 when
// it names none. Without it the retries went straight back into the limit
// and the turn failed although the server had said when to come back.
func retryAfterFromBody(body string) time.Duration {
	m := retryBodyHint.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	secs, err := strconv.ParseFloat(m[1], 64)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

// retryNotify, when set, is told about a retry that waits long enough for
// the user to wonder (the engine prints it); nil = silent.
var retryNotify atomic.Pointer[func(string)]

// SetRetryNotifier installs the notice sink for long retry waits (nil
// removes it). A 429 wait of 40 s used to show only "已 30s 无进展（可能卡住）".
func SetRetryNotifier(f func(string)) {
	if f == nil {
		retryNotify.Store(nil)
		return
	}
	retryNotify.Store(&f)
}

// noticeRetryThreshold: shorter waits are not announced.
const noticeRetryThreshold = 5 * time.Second

func announceRetry(status int, wait time.Duration, attempt, maxAttempts int) {
	f := retryNotify.Load()
	if f == nil || wait < noticeRetryThreshold {
		return
	}
	why := "请求失败"
	switch {
	case status == http.StatusTooManyRequests:
		why = "请求被限流（429）"
	case status >= 500:
		why = fmt.Sprintf("服务端错误（%d）", status)
	}
	(*f)(fmt.Sprintf("%s，%d 秒后自动重试（第 %d/%d 次）", why, int(wait.Round(time.Second)/time.Second), attempt+1, maxAttempts))
}

func retryWithBackoff[T any](ctx context.Context, cfg retryConfig, operation func() (T, error)) (T, error) {
	var zero T
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		result, err := operation()
		if err == nil {
			return result, nil
		}
		if attempt == cfg.MaxRetries || !isRetryable(err) {
			return zero, err
		}
		wait := retryDelay(cfg, attempt, retryAfterOf(err))
		status := 0
		var re *RetryableError
		if errors.As(err, &re) {
			status = re.Status
		}
		announceRetry(status, wait, attempt, cfg.MaxRetries)
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(wait):
		}
	}
	return zero, fmt.Errorf("max retries exceeded")
}

// isResponseHeaderTimeout reports whether err is the transport giving up on
// the response headers (ResponseHeaderTimeout). net/http reports it only as
// a timeout *url.Error with this text (HTTP/1 and HTTP/2 alike); a dial or
// TLS handshake timeout is a different, cheap failure and is not matched.
func isResponseHeaderTimeout(err error) bool {
	return isClientTimeout(err) && strings.Contains(err.Error(), "timeout awaiting response headers")
}

func retryConnectHTTP(
	ctx context.Context,
	cfg retryConfig,
	connect func(context.Context) (*http.Response, error),
	shouldRetryStatus func(statusCode int) bool,
) (*http.Response, error) {
	headerTimeouts := 0
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		resp, err := connect(ctx)
		if err != nil {
			// A request that could not be built (auth/signing) will not
			// build on the next attempt either.
			if errors.Is(err, errCreateRequest) {
				return nil, err
			}
			// A server that accepted the request and never answered costs a
			// whole ResponseHeaderTimeout (180s cloud, 15 min local) per
			// attempt. It used to be retried like a refused connection, so
			// the turn failed only after ~12 minutes, an hour locally. One
			// retry covers a request lost in a restart; refused and reset
			// connections keep the full schedule.
			if isResponseHeaderTimeout(err) {
				headerTimeouts++
				if headerTimeouts > 1 {
					return nil, err
				}
			}
			if attempt == cfg.MaxRetries {
				return nil, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelay(cfg, attempt, 0)):
			}
			continue
		}

		if shouldRetryStatus != nil && shouldRetryStatus(resp.StatusCode) {
			if attempt == cfg.MaxRetries {
				return resp, nil
			}
			retryAfter := RetryAfterFor(resp.StatusCode, resp.Header)
			if retryAfter == 0 && resp.StatusCode == http.StatusTooManyRequests {
				// Gemini's OpenAI-compatible endpoint sends no Retry-After
				// header; the wait is in the body (retryAfterFromBody).
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
				retryAfter = retryAfterFromBody(string(body))
			}
			_ = resp.Body.Close()
			wait := retryDelay(cfg, attempt, retryAfter)
			announceRetry(resp.StatusCode, wait, attempt, cfg.MaxRetries)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			continue
		}

		return resp, nil
	}

	return nil, fmt.Errorf("max retries exceeded")
}
