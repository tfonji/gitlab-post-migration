package gitlabclient

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// maxLoggedBody caps how much of an error response body is logged, so a
// large HTML error page can't flood the job log.
const maxLoggedBody = 2048

// loggingTransport logs every HTTP round trip to the GitLab API. Failed
// responses (>= 400) are always logged at WARN with GitLab's response body,
// since that body is usually the only place the real reason lives (e.g.
// "Branch already exists"); successful ones only at DEBUG. Request headers
// are never logged -- they carry the token.
type loggingTransport struct {
	next http.RoundTripper
}

func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.next.RoundTrip(req)
	elapsed := time.Since(start).Round(time.Millisecond)

	attrs := []any{
		"method", req.Method,
		"path", req.URL.Path,
		"duration", elapsed,
	}
	if req.URL.RawQuery != "" {
		attrs = append(attrs, "query", req.URL.RawQuery)
	}

	if err != nil {
		slog.Warn("gitlab api request failed", append(attrs, "error", err)...)
		return resp, err
	}
	attrs = append(attrs, "status", resp.StatusCode)
	if id := resp.Header.Get("X-Request-Id"); id != "" {
		// Lets GitLab admins find the exact request in the instance logs.
		attrs = append(attrs, "request_id", id)
	}

	// A GET that 404s is how tasks probe "does this exist yet?" (branch,
	// protection, membership), so it's expected, not an error.
	if resp.StatusCode < 400 || (req.Method == http.MethodGet && resp.StatusCode == http.StatusNotFound) {
		slog.Debug("gitlab api", attrs...)
		return resp, nil
	}

	// Read the body for the log, then hand the caller an identical copy so
	// go-gitlab can still build its own error from it.
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if readErr == nil {
		b := strings.TrimSpace(string(body))
		if len(b) > maxLoggedBody {
			b = b[:maxLoggedBody] + "...(truncated)"
		}
		attrs = append(attrs, "response", b)
	}
	slog.Warn("gitlab api error", attrs...)
	return resp, nil
}

// LogIdentity logs who the token authenticates as and what it's allowed to
// do, so a job log answers "which user/token ran this, and is it admin?"
// without anyone re-running curl by hand. It never fails the run: a token
// type that can't introspect itself (e.g. CI_JOB_TOKEN) just logs a warning.
func (c *Client) LogIdentity(ctx context.Context) {
	if c.token == "" {
		slog.Error("no GitLab token set (GITLAB_TOKEN is empty) -- every API call will be unauthenticated")
		return
	}

	user, _, err := c.REST.Users.CurrentUser(gitlab.WithContext(ctx))
	if err != nil {
		slog.Warn("could not look up token user", "error", err)
	} else {
		slog.Info("authenticated",
			"gitlab_url", c.baseURL,
			"user", user.Username,
			"user_id", user.ID,
			"is_admin", user.IsAdmin,
			"bot", user.Bot,
		)
	}

	tok, _, err := c.REST.PersonalAccessTokens.GetSinglePersonalAccessToken(gitlab.WithContext(ctx))
	if err != nil {
		slog.Warn("could not look up token details", "error", err)
		return
	}
	attrs := []any{"token_name", tok.Name, "scopes", strings.Join(tok.Scopes, ",")}
	if tok.ExpiresAt != nil {
		attrs = append(attrs, "expires_at", tok.ExpiresAt.String())
	}
	slog.Info("token", attrs...)
	if user != nil && user.IsAdmin && !contains(tok.Scopes, "admin_mode") {
		slog.Warn("token user is an admin but the token lacks the admin_mode scope -- if Admin Mode is enabled, admin rights won't apply to API calls")
	}
}

// LogCIContext logs which pipeline/job/commit this run is and who
// triggered it, when running under GitLab CI.
func LogCIContext() {
	if os.Getenv("GITLAB_CI") == "" {
		return
	}
	slog.Info("ci context",
		"pipeline_id", os.Getenv("CI_PIPELINE_ID"),
		"job_id", os.Getenv("CI_JOB_ID"),
		"job_name", os.Getenv("CI_JOB_NAME"),
		"commit", os.Getenv("CI_COMMIT_SHORT_SHA"),
		"ref", os.Getenv("CI_COMMIT_REF_NAME"),
		"triggered_by", os.Getenv("GITLAB_USER_LOGIN"),
	)
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
