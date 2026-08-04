package oauthproxy

import (
	"context"
	"net/http"
	"strings"
	"time"

	cliproxy "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	cclTransientErrorCooldown = 2 * time.Second
	cclAuthErrorCooldown      = 10 * time.Second
	cclRateLimitCooldown      = 10 * time.Second
	cclPaymentErrorCooldown   = 30 * time.Minute
	cclNotFoundCooldown       = 12 * time.Hour
)

// cclCooldownHook replaces CLIProxyAPI's cooldown scheduling for CCL-managed
// GPT subscriptions and ordinary API-key runtimes. These runtimes commonly have
// a single credential, so the defaults turn one upstream failure into a long
// stream of auth_unavailable responses. Kiro uses its own direct adapter and
// never installs this hook.
type cclCooldownHook struct {
	coreauth.NoopHook
	manager *coreauth.Manager
	now     func() time.Time
}

func newRuntimeCoreAuthManager(store coreauth.Store, shortCooldownPolicy bool) *coreauth.Manager {
	if !shortCooldownPolicy {
		return coreauth.NewManager(store, nil, nil)
	}
	hook := &cclCooldownHook{now: time.Now}
	manager := coreauth.NewManager(store, nil, hook)
	hook.manager = manager
	return manager
}

func usesCodexOAuthCooldownPolicy(providerName string) bool {
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case ProviderCodex, ProviderChatGPT, ProviderChatGPTLegacy:
		return true
	default:
		return false
	}
}

func (h *cclCooldownHook) OnResult(ctx context.Context, result coreauth.Result) {
	if h == nil || h.manager == nil || result.Success || result.Error == nil {
		return
	}
	cooldown, ok := cclCooldownForError(result.Error)
	if !ok {
		return
	}
	auth, ok := h.manager.GetByID(result.AuthID)
	if !ok || auth == nil {
		return
	}
	now := time.Now()
	if h.now != nil {
		now = h.now()
	}
	deadline := now.Add(cooldown)

	if result.Model == "" {
		applyCCLAuthCooldown(auth, result.Error.HTTPStatus, deadline, now)
	} else {
		state := auth.ModelStates[result.Model]
		if state == nil {
			// MarkResult normally creates the exact model state before invoking
			// this hook. Fall back to auth-level state for defensive compatibility
			// with executor results that omit or rewrite the model key.
			applyCCLAuthCooldown(auth, result.Error.HTTPStatus, deadline, now)
		} else {
			applyCCLModelCooldown(state, result.Error.HTTPStatus, deadline, now)
			recomputeCCLAuthAvailability(auth, now)
		}
	}

	updateCtx := context.Background()
	if ctx != nil {
		updateCtx = context.WithoutCancel(ctx)
	}
	_, _ = h.manager.Update(updateCtx, auth)
	clearOverriddenRegistryCooldown(result)
	LogWarnf("ccl upstream cooldown provider=%q status=%d model=%q duration=%s",
		result.Provider, result.Error.HTTPStatus, result.Model, cooldown)
}

// CLIProxyAPI mirrors 401 and 429 cooldowns into its global model registry.
// Those registry entries otherwise outlive the shorter CCL deadline and can
// keep the model hidden after the auth manager is ready to retry it.
func clearOverriddenRegistryCooldown(result coreauth.Result) {
	if result.Model == "" || result.Error == nil {
		return
	}
	status := result.Error.HTTPStatus
	if status != http.StatusUnauthorized && status != http.StatusTooManyRequests {
		return
	}

	registry := cliproxy.GlobalModelRegistry()
	if status == http.StatusTooManyRequests {
		registry.ClearModelQuotaExceeded(result.AuthID, result.Model)
	}
	// ResumeClientModel is implemented by the concrete registry but omitted
	// from the public SDK interface in CLIProxyAPI v7.2.95. A structural type
	// assertion keeps this compatibility shim local and upgrade-safe.
	if resumable, ok := registry.(interface {
		ResumeClientModel(clientID, modelID string)
	}); ok {
		resumable.ResumeClientModel(result.AuthID, result.Model)
	}
}

func cclCooldownForError(resultErr *coreauth.Error) (time.Duration, bool) {
	if resultErr == nil {
		return 0, false
	}
	code := strings.ToLower(strings.TrimSpace(resultErr.Code))
	message := strings.ToLower(resultErr.Message)
	switch {
	case strings.Contains(code, "invalid_grant") || strings.Contains(message, "invalid_grant"):
		return cclPaymentErrorCooldown, true
	case strings.Contains(code, "model_not_supported") || strings.Contains(message, "model not supported"):
		return cclNotFoundCooldown, true
	}
	switch resultErr.HTTPStatus {
	case http.StatusUnauthorized:
		return cclAuthErrorCooldown, true
	case http.StatusTooManyRequests:
		return cclRateLimitCooldown, true
	case http.StatusRequestTimeout,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return cclTransientErrorCooldown, true
	case http.StatusPaymentRequired, http.StatusForbidden:
		return cclPaymentErrorCooldown, true
	case http.StatusNotFound:
		return cclNotFoundCooldown, true
	default:
		return 0, false
	}
}

func applyCCLAuthCooldown(auth *coreauth.Auth, status int, deadline, now time.Time) {
	auth.Unavailable = true
	auth.NextRetryAfter = deadline
	auth.UpdatedAt = now
	if status == http.StatusTooManyRequests {
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		auth.Quota.NextRecoverAt = deadline
		// CCL uses a fixed ten-second 429 window instead of CPA's exponential
		// ladder, so no previous backoff level should leak into the next failure.
		auth.Quota.BackoffLevel = 0
	}
}

func applyCCLModelCooldown(state *coreauth.ModelState, status int, deadline, now time.Time) {
	state.Unavailable = true
	state.NextRetryAfter = deadline
	state.UpdatedAt = now
	if status == http.StatusTooManyRequests {
		state.Quota.Exceeded = true
		state.Quota.Reason = "quota"
		state.Quota.NextRecoverAt = deadline
		state.Quota.BackoffLevel = 0
	}
}

// recomputeCCLAuthAvailability mirrors the scheduler-relevant aggregate
// fields after a model-specific deadline is shortened.
func recomputeCCLAuthAvailability(auth *coreauth.Auth, now time.Time) {
	if auth == nil || len(auth.ModelStates) == 0 {
		return
	}
	allUnavailable := true
	earliestRetry := time.Time{}
	quotaExceeded := false
	quotaRecover := time.Time{}
	maxBackoffLevel := 0
	hasState := false

	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		hasState = true
		stateUnavailable := state.Status == coreauth.StatusDisabled
		if state.Status != coreauth.StatusDisabled && state.Unavailable &&
			!state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now) {
			stateUnavailable = true
			if earliestRetry.IsZero() || state.NextRetryAfter.Before(earliestRetry) {
				earliestRetry = state.NextRetryAfter
			}
		}
		if !stateUnavailable {
			allUnavailable = false
		}
		if state.Quota.Exceeded {
			quotaExceeded = true
			if quotaRecover.IsZero() ||
				(!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
				quotaRecover = state.Quota.NextRecoverAt
			}
			if state.Quota.BackoffLevel > maxBackoffLevel {
				maxBackoffLevel = state.Quota.BackoffLevel
			}
		}
	}
	if !hasState {
		return
	}

	auth.Unavailable = allUnavailable
	if allUnavailable {
		auth.NextRetryAfter = earliestRetry
	} else {
		auth.NextRetryAfter = time.Time{}
	}
	auth.Quota.Exceeded = quotaExceeded
	if quotaExceeded {
		auth.Quota.Reason = "quota"
		auth.Quota.NextRecoverAt = quotaRecover
		auth.Quota.BackoffLevel = maxBackoffLevel
	} else {
		auth.Quota = coreauth.QuotaState{}
	}
	auth.UpdatedAt = now
}
