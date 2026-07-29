package oauthproxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxy "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestCCLCooldownPolicy(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		cooldown time.Duration
		quota    bool
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, cooldown: 10 * time.Second},
		{name: "rate limit", status: http.StatusTooManyRequests, cooldown: 10 * time.Second, quota: true},
		{name: "request timeout", status: http.StatusRequestTimeout, cooldown: 2 * time.Second},
		{name: "internal error", status: http.StatusInternalServerError, cooldown: 2 * time.Second},
		{name: "bad gateway", status: http.StatusBadGateway, cooldown: 2 * time.Second},
		{name: "unavailable", status: http.StatusServiceUnavailable, cooldown: 2 * time.Second},
		{name: "gateway timeout", status: http.StatusGatewayTimeout, cooldown: 2 * time.Second},
		{name: "payment required", status: http.StatusPaymentRequired, cooldown: 30 * time.Minute},
		{name: "forbidden", status: http.StatusForbidden, cooldown: 30 * time.Minute},
		{name: "not found", status: http.StatusNotFound, cooldown: 12 * time.Hour},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			hook := &cclCooldownHook{now: func() time.Time { return now }}
			manager := coreauth.NewManager(nil, nil, hook)
			hook.manager = manager
			authID := "codex-" + tc.name
			model := "gpt-test"
			if _, err := manager.Register(context.Background(), &coreauth.Auth{
				ID:       authID,
				Provider: ProviderCodex,
			}); err != nil {
				t.Fatalf("Register() error = %v", err)
			}

			manager.MarkResult(context.Background(), coreauth.Result{
				AuthID:   authID,
				Provider: ProviderCodex,
				Model:    model,
				Success:  false,
				Error: &coreauth.Error{
					HTTPStatus: tc.status,
					Message:    http.StatusText(tc.status),
				},
			})

			updated, ok := manager.GetByID(authID)
			if !ok || updated == nil {
				t.Fatal("updated auth not found")
			}
			state := updated.ModelStates[model]
			if state == nil {
				t.Fatalf("model state %q not found: %#v", model, updated.ModelStates)
			}
			wantDeadline := now.Add(tc.cooldown)
			if !state.NextRetryAfter.Equal(wantDeadline) {
				t.Fatalf("model retry deadline = %v, want %v", state.NextRetryAfter, wantDeadline)
			}
			if !updated.NextRetryAfter.Equal(wantDeadline) {
				t.Fatalf("auth retry deadline = %v, want %v", updated.NextRetryAfter, wantDeadline)
			}
			if state.Quota.Exceeded != tc.quota || updated.Quota.Exceeded != tc.quota {
				t.Fatalf("quota state = model:%t auth:%t, want %t",
					state.Quota.Exceeded, updated.Quota.Exceeded, tc.quota)
			}
			if tc.quota {
				if !state.Quota.NextRecoverAt.Equal(wantDeadline) ||
					!updated.Quota.NextRecoverAt.Equal(wantDeadline) {
					t.Fatalf("quota recovery = model:%v auth:%v, want %v",
						state.Quota.NextRecoverAt, updated.Quota.NextRecoverAt, wantDeadline)
				}
				if state.Quota.BackoffLevel != 0 || updated.Quota.BackoffLevel != 0 {
					t.Fatalf("429 backoff level = model:%d auth:%d, want 0",
						state.Quota.BackoffLevel, updated.Quota.BackoffLevel)
				}
			}
		})
	}
}

func TestCCLCooldownPolicyLeavesRequestFailuresUnchanged(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		if cooldown, ok := cclCooldownForError(&coreauth.Error{HTTPStatus: status}); ok {
			t.Fatalf("status %d unexpectedly overridden with %s", status, cooldown)
		}
	}
}

func TestCCLCooldownPolicyAuthLevel(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		status   int
		cooldown time.Duration
		quota    bool
	}{
		{status: http.StatusUnauthorized, cooldown: 10 * time.Second},
		{status: http.StatusTooManyRequests, cooldown: 10 * time.Second, quota: true},
		{status: http.StatusInternalServerError, cooldown: 2 * time.Second},
	} {
		hook := &cclCooldownHook{now: func() time.Time { return now }}
		manager := coreauth.NewManager(nil, nil, hook)
		hook.manager = manager
		authID := http.StatusText(tc.status)
		if _, err := manager.Register(context.Background(), &coreauth.Auth{
			ID:       authID,
			Provider: ProviderCodex,
		}); err != nil {
			t.Fatalf("Register(%d) error = %v", tc.status, err)
		}
		manager.MarkResult(context.Background(), coreauth.Result{
			AuthID:   authID,
			Provider: ProviderCodex,
			Success:  false,
			Error: &coreauth.Error{
				HTTPStatus: tc.status,
				Message:    http.StatusText(tc.status),
			},
		})

		updated, ok := manager.GetByID(authID)
		if !ok || updated == nil {
			t.Fatalf("updated auth for status %d not found", tc.status)
		}
		wantDeadline := now.Add(tc.cooldown)
		if !updated.NextRetryAfter.Equal(wantDeadline) {
			t.Errorf("status %d auth retry deadline = %v, want %v",
				tc.status, updated.NextRetryAfter, wantDeadline)
		}
		if updated.Quota.Exceeded != tc.quota {
			t.Errorf("status %d quota = %t, want %t", tc.status, updated.Quota.Exceeded, tc.quota)
		}
		if tc.quota && !updated.Quota.NextRecoverAt.Equal(wantDeadline) {
			t.Errorf("status %d quota recovery = %v, want %v",
				tc.status, updated.Quota.NextRecoverAt, wantDeadline)
		}
	}
}

func TestRuntimeCoreAuthManagerInstallsPolicyOnlyWhenRequested(t *testing.T) {
	withPolicy := newRuntimeCoreAuthManager(nil, true)
	withoutPolicy := newRuntimeCoreAuthManager(nil, false)
	withPolicy.SetConfig(&sdkconfig.Config{})
	withoutPolicy.SetConfig(&sdkconfig.Config{})

	retryDelay := func(manager *coreauth.Manager, authID string) time.Duration {
		t.Helper()
		if _, err := manager.Register(context.Background(), &coreauth.Auth{
			ID:       authID,
			Provider: ProviderCodex,
		}); err != nil {
			t.Fatalf("Register(%q) error = %v", authID, err)
		}
		started := time.Now()
		manager.MarkResult(context.Background(), coreauth.Result{
			AuthID:   authID,
			Provider: ProviderCodex,
			Model:    "gpt-test",
			Error: &coreauth.Error{
				HTTPStatus: http.StatusInternalServerError,
				Message:    "server error",
			},
		})
		updated, ok := manager.GetByID(authID)
		if !ok || updated == nil || updated.ModelStates["gpt-test"] == nil {
			t.Fatalf("updated auth %q not found", authID)
		}
		return updated.ModelStates["gpt-test"].NextRetryAfter.Sub(started)
	}

	if got := retryDelay(withPolicy, "with-ccl-policy"); got < time.Second || got > 3*time.Second {
		t.Errorf("CCL policy 500 cooldown = %s, want about 2s", got)
	}
	if got := retryDelay(withoutPolicy, "without-ccl-policy"); got < 59*time.Second || got > 61*time.Second {
		t.Errorf("upstream default 500 cooldown = %s, want about 60s", got)
	}
}

func TestUsesCodexOAuthCooldownPolicy(t *testing.T) {
	for _, provider := range []string{ProviderCodex, ProviderChatGPT, ProviderChatGPTLegacy, " GPT "} {
		if !usesCodexOAuthCooldownPolicy(provider) {
			t.Errorf("usesCodexOAuthCooldownPolicy(%q) = false, want true", provider)
		}
	}
	for _, provider := range []string{ProviderCopilot, ProviderKiro, ProviderGemini, ""} {
		if usesCodexOAuthCooldownPolicy(provider) {
			t.Errorf("usesCodexOAuthCooldownPolicy(%q) = true, want false", provider)
		}
	}
}

func TestCodexOAuthPolicyKeeps401ModelRoutable(t *testing.T) {
	authID := "ccl-codex-cooldown-auth"
	model := "ccl-codex-cooldown-model"
	registry := cliproxy.GlobalModelRegistry()
	registry.RegisterClient(authID, ProviderCodex, []*cliproxy.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.UnregisterClient(authID) })

	hook := &cclCooldownHook{now: time.Now}
	manager := coreauth.NewManager(nil, nil, hook)
	hook.manager = manager
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: ProviderCodex,
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	manager.MarkResult(context.Background(), coreauth.Result{
		AuthID:   authID,
		Provider: ProviderCodex,
		Model:    model,
		Success:  false,
		Error: &coreauth.Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "unauthorized",
		},
	})

	found := false
	for _, info := range registry.GetAvailableModelsByProvider(ProviderCodex) {
		if info != nil && info.ID == model {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("model %q disappeared from Codex routing during its 10s cooldown", model)
	}
}
