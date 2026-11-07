package seerr

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	const secret = "s3cret"
	body := `{"notification_type":"ISSUE_CREATED","issue_id":"7","subject":"Film (2020)","reported_by":"ann"}`

	tests := []struct {
		name       string
		auth       string
		body       string
		receiveErr error
		wantStatus int
		wantCalled bool
	}{
		{"good", secret, body, nil, http.StatusNoContent, true},
		{"missing secret", "", body, nil, http.StatusUnauthorized, false},
		{"wrong secret", "nope", body, nil, http.StatusUnauthorized, false},
		{"secret prefix", secret[:3], body, nil, http.StatusUnauthorized, false},
		{"bad json", secret, `{"notification_type":`, nil, http.StatusBadRequest, false},
		{"bad payload", secret, body, ErrBadPayload, http.StatusBadRequest, true},
		{"receive fails", secret, body, errors.New("discord down"), http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *Payload
			h := Handler(secret, func(_ context.Context, p Payload) error {
				got = &p
				return tt.receiveErr
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))

			req := httptest.NewRequest(http.MethodPost, "/seerr", strings.NewReader(tt.body))
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if (got != nil) != tt.wantCalled {
				t.Fatalf("receive called = %v, want %v", got != nil, tt.wantCalled)
			}
			if got != nil && (got.NotificationType != NotifyIssueCreated || got.IssueID != "7" || got.ReportedBy != "ann") {
				t.Errorf("payload = %+v", *got)
			}
		})
	}
}

func TestPayloadIssue(t *testing.T) {
	tests := []struct {
		in   string
		want int
		ok   bool
	}{
		{"12", 12, true},
		{" 3 ", 3, true},
		{"", 0, false},
		{"0", 0, false},
		{"abc", 0, false},
	}
	for _, tt := range tests {
		got, err := Payload{IssueID: tt.in}.Issue()
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("Issue(%q) = %d, %v; want %d, ok %v", tt.in, got, err, tt.want, tt.ok)
		}
	}
}

func TestClient(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "key" {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(b))
		switch r.URL.Path {
		case "/api/v1/issue/5":
			io.WriteString(w, `{"id":5,"issueType":1,"status":1,"problemSeason":2,"problemEpisode":0,
				"media":{"mediaType":"tv","tmdbId":66857,"tvdbId":311900},
				"createdBy":{"displayName":"ann"},"comments":[{"id":1,"message":"no sound","user":{"displayName":"ann"}}]}`)
		case "/api/v1/issue/5/comment", "/api/v1/issue/5/resolved":
			io.WriteString(w, `{"id":5}`)
		default:
			http.Error(w, `{"message":"Issue not found."}`, http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "key")
	ctx := context.Background()

	is, err := c.Issue(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if is.ProblemSeason != 2 || is.Media.TVDBID != 311900 || is.Media.MediaType != MediaTV || is.Comments[0].Message != "no sound" {
		t.Errorf("issue = %+v", is)
	}
	if err := c.Comment(ctx, 5, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := c.Resolve(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Issue(ctx, 6); err == nil {
		t.Error("missing issue: want error")
	}
	want := []string{
		"GET /api/v1/issue/5 ",
		`POST /api/v1/issue/5/comment {"message":"hello"}`,
		"POST /api/v1/issue/5/resolved ",
		"GET /api/v1/issue/6 ",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}
