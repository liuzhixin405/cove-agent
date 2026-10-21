package api

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// RetryableError is a failure worth retrying (5xx, 429, transport errors).
//
// Status and RetryAfter used to be dropped here: a 5xx that exhausted its
// retries reached the fallback classifiers as a bare "server error 503" they
// did not treat as temporary, and Retry-After was never honoured.
type RetryableError struct {
	Msg        string
	Status     int           // HTTP status, 0 for transport errors
	RetryAfter time.Duration // server-requested wait before the next attempt
}

func (e *RetryableError) Error() string {
	if e.Status != 0 {
		return (&StatusError{Status: e.Status, Msg: e.Msg}).Error()
	}
	return e.Msg
}

// StatusError carries the HTTP status of a failed provider call.
//
// Without it, fallback.go had to classify failures by substring-matching the
// error text for "429", "500", "401" and friends — which misfires on any
// message that merely contains those digits ("max_tokens must be under 1500",
// "model gpt-4o-mini-2024-07-18"), wrongly cooling down or permanently
// blacklisting a perfectly healthy provider. Providers return this type so the
// status is read, not guessed; the substring heuristics remain only as a
// fallback for transport-level errors that carry no status at all.
type StatusError struct {
	Status int
	Msg    string
}

// Error leads with a Chinese hint for the failures a user can act on. The UI
// shows only the first ~120 characters, and the raw JSON body that used to
// fill them rarely said what to do.
func (e *StatusError) Error() string {
	if hint := statusHint(e.Status, e.Msg); hint != "" {
		return fmt.Sprintf("API error %d（%s）: %s", e.Status, hint, e.Msg)
	}
	return fmt.Sprintf("API error %d: %s", e.Status, e.Msg)
}

func statusHint(status int, msg string) string {
	if status != http.StatusRequestEntityTooLarge && isContextLengthText(status, msg) {
		return "上下文超出模型上限，请用 /compact 压缩对话或新开会话"
	}
	if status == http.StatusBadRequest && strings.Contains(msg, "unsupported countries") {
		// Anthropic (first-party and via Bedrock) checks the egress IP after
		// authentication; the body reads like a request error.
		return "出口 IP 所在地区不受 Anthropic 支持，请确认代理/VPN 已生效（鉴权已通过，与密钥和模型配置无关）"
	}
	switch {
	case status == http.StatusUnauthorized:
		return "API Key 无效或已过期，请检查 api_key 配置"
	case status == http.StatusPaymentRequired:
		return "账户余额不足，请到服务商控制台充值"
	case status == http.StatusForbidden:
		return "无权限访问该模型或接口，请检查账号权限和模型名称"
	case status == http.StatusNotFound:
		return "接口地址或模型不存在，请检查 base_url 和模型名称"
	case status == http.StatusRequestEntityTooLarge:
		return "请求过大，请用 /compact 压缩对话或减少附件"
	case status == http.StatusTooManyRequests:
		return "请求被限流，请稍后重试"
	case status >= 500:
		return "服务商服务暂时不可用，请稍后重试"
	}
	return ""
}

// statusOf returns the HTTP status carried by err, or 0 when it carries none.
func statusOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	var re *RetryableError
	if errors.As(err, &re) {
		return re.Status
	}
	return 0
}

func isRetryable(err error) bool {
	var re *RetryableError
	return errors.As(err, &re)
}

// retryAfterOf returns the wait the server asked for, or 0.
func retryAfterOf(err error) time.Duration {
	var re *RetryableError
	if errors.As(err, &re) {
		return re.RetryAfter
	}
	return 0
}

// maxRetryAfter caps a server-requested wait so a bogus header cannot stall
// a turn for minutes; beyond this the fallback's cooldown takes over.
const maxRetryAfter = 60 * time.Second

// retryJitter returns a value in [0, 1) that spreads the backoff. Replaced in
// tests.
var retryJitter = rand.Float64

// retryDelay is the wait before retry number attempt+1: exponential backoff
// scaled by a random factor in [0.5, 1.5), or the server's Retry-After when
// that is longer. Without the jitter every client that hit the same 429 or
// 5xx retried in lockstep and collided again.
func retryDelay(cfg retryConfig, attempt int, retryAfter time.Duration) time.Duration {
	delay := time.Duration(float64(time.Duration(1<<attempt)*cfg.BaseDelay) * (0.5 + retryJitter()))
	if retryAfter > maxRetryAfter {
		retryAfter = maxRetryAfter
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	return delay
}

// IsContextLengthError reports whether err says the request did not fit the
// model's context window. The engine needs this to compact and retry instead
// of failing the turn. It looks through wrappers, including the fallback
// chain's combined error.
func IsContextLengthError(err error) bool {
	found := false
	walkErrors(err, func(e error) bool {
		// walkErrors visits every layer, so each StatusError and
		// RetryableError in the chain is the first match of its own visit.
		var se *StatusError
		var re *RetryableError
		switch {
		case errors.As(e, &se):
			found = isContextLengthText(se.Status, se.Msg)
		case errors.As(e, &re):
			found = isContextLengthText(re.Status, re.Msg)
		}
		return found
	})
	return found
}

// isContextLengthText matches the wordings DeepSeek/OpenAI ("maximum context
// length", context_length_exceeded), Anthropic ("prompt is too long",
// request_too_large), llama.cpp-based local servers such as llama-server and
// LM Studio ("exceeds the available context size", exceed_context_size_error,
// n_ctx) and other compatible servers use for an oversized prompt.
func isContextLengthText(status int, msg string) bool {
	if status == http.StatusRequestEntityTooLarge {
		return true
	}
	if status != 0 && status != http.StatusBadRequest {
		return false
	}
	s := strings.ToLower(msg)
	for _, p := range []string{
		"context_length_exceeded", "maximum context length", "context length",
		"context window", "prompt is too long", "request_too_large",
		"input is too long", "too many input tokens",
		"exceed_context_size", "context size", "n_ctx",
	} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// walkErrors visits err and everything it wraps, depth first, until visit
// returns true.
func walkErrors(err error, visit func(error) bool) bool {
	if err == nil {
		return false
	}
	if visit(err) {
		return true
	}
	switch u := any(err).(type) {
	case interface{ Unwrap() error }:
		return walkErrors(u.Unwrap(), visit)
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if walkErrors(e, visit) {
				return true
			}
		}
	}
	return false
}
