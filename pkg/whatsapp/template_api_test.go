package whatsapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, server *httptest.Server) *whatsapp.Client {
	t.Helper()
	log := testutil.NopLogger()
	client := whatsapp.NewWithTimeout(log, 5*time.Second)
	client.HTTPClient = &http.Client{
		Transport: &testServerTransport{serverURL: server.URL},
	}
	return client
}

// --- SubmitTemplate ---

func TestClient_SubmitTemplate_Success(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Contains(t, r.URL.Path, "/message_templates")
		assert.Equal(t, "Bearer test-access-token", r.Header.Get("Authorization"))

		var body map[string]any
		err := json.NewDecoder(r.Body).Decode(&body)
		require.NoError(t, err)
		assert.Equal(t, "hello_world", body["name"])
		assert.Equal(t, "en", body["language"])
		assert.Equal(t, "MARKETING", body["category"])

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "tmpl-123"})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)
	ctx := context.Background()

	tmpl := &whatsapp.TemplateSubmission{
		Name:        "hello_world",
		Language:    "en",
		Category:    "MARKETING",
		BodyContent: "Hello! Welcome to our service.",
	}

	id, err := client.SubmitTemplate(ctx, account, tmpl)
	require.NoError(t, err)
	assert.Equal(t, "tmpl-123", id)
}

func TestClient_SubmitTemplate_WithVariables(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		components := body["components"].([]any)
		// Should have BODY component
		var bodyComp map[string]any
		for _, c := range components {
			comp := c.(map[string]any)
			if comp["type"] == "BODY" {
				bodyComp = comp
			}
		}
		require.NotNil(t, bodyComp)
		assert.Equal(t, "Hello {{1}}! Your order {{2}} is ready.", bodyComp["text"])

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "tmpl-456"})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)
	ctx := context.Background()

	tmpl := &whatsapp.TemplateSubmission{
		Name:        "order_ready",
		Language:    "en",
		Category:    "UTILITY",
		BodyContent: "Hello {{1}}! Your order {{2}} is ready.",
		SampleValues: []any{
			map[string]any{"component": "body", "index": 1, "value": "John"},
			map[string]any{"component": "body", "index": 2, "value": "ORD-123"},
		},
	}

	id, err := client.SubmitTemplate(ctx, account, tmpl)
	require.NoError(t, err)
	assert.Equal(t, "tmpl-456", id)
}

func TestClient_SubmitTemplate_APIError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(whatsapp.MetaAPIError{
			Error: struct {
				Message      string `json:"message"`
				Type         string `json:"type"`
				Code         int    `json:"code"`
				ErrorSubcode int    `json:"error_subcode"`
				ErrorUserMsg string `json:"error_user_msg"`
				ErrorData    struct {
					Details string `json:"details"`
				} `json:"error_data"`
				FBTraceID string `json:"fbtrace_id"`
			}{
				Message: "Invalid template name",
				Code:    100,
			},
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)
	ctx := context.Background()

	tmpl := &whatsapp.TemplateSubmission{
		Name:        "",
		Language:    "en",
		Category:    "MARKETING",
		BodyContent: "Hello",
	}

	_, err := client.SubmitTemplate(ctx, account, tmpl)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Invalid template name")
}

func TestClient_SubmitTemplate_MissingVariableSamples(t *testing.T) {
	t.Parallel()

	// SubmitTemplate should fail if body has variables but no samples
	log := testutil.NopLogger()
	client := whatsapp.NewWithTimeout(log, 5*time.Second)

	account := &whatsapp.Account{
		PhoneID:     "123",
		BusinessID:  "456",
		APIVersion:  "v21.0",
		AccessToken: "token",
	}

	tmpl := &whatsapp.TemplateSubmission{
		Name:         "test",
		Language:     "en",
		Category:     "UTILITY",
		BodyContent:  "Hello {{1}}!",
		SampleValues: []any{},
	}

	_, err := client.SubmitTemplate(context.Background(), account, tmpl)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sample values are required")
}

// --- FetchTemplates ---

func TestClient_FetchTemplates_Success(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Contains(t, r.URL.Path, "/message_templates")

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "1", "name": "hello", "language": "en", "category": "MARKETING", "status": "APPROVED", "quality_rating": "HIGH"},
				{"id": "2", "name": "goodbye", "language": "en", "category": "UTILITY", "status": "PENDING", "quality_rating": "UNKNOWN"},
			},
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)

	templates, err := client.FetchTemplates(context.Background(), account)
	require.NoError(t, err)
	require.Len(t, templates, 2)
	assert.Equal(t, "hello", templates[0].Name)
	assert.Equal(t, "APPROVED", templates[0].Status)
	assert.Equal(t, "HIGH", templates[0].QualityRating)
	assert.Equal(t, "goodbye", templates[1].Name)
	assert.Equal(t, "UNKNOWN", templates[1].QualityRating)
}

func TestClient_FetchTemplates_Empty(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)

	templates, err := client.FetchTemplates(context.Background(), account)
	require.NoError(t, err)
	assert.Empty(t, templates)
}

func TestClient_FetchTemplates_APIError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(whatsapp.MetaAPIError{
			Error: struct {
				Message      string `json:"message"`
				Type         string `json:"type"`
				Code         int    `json:"code"`
				ErrorSubcode int    `json:"error_subcode"`
				ErrorUserMsg string `json:"error_user_msg"`
				ErrorData    struct {
					Details string `json:"details"`
				} `json:"error_data"`
				FBTraceID string `json:"fbtrace_id"`
			}{
				Message: "Invalid access token",
				Code:    190,
			},
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)

	_, err := client.FetchTemplates(context.Background(), account)
	require.Error(t, err)
}

// --- DeleteTemplate ---

func TestClient_DeleteTemplate_Success(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Contains(t, r.URL.Path, "/message_templates")
		assert.Contains(t, r.URL.RawQuery, "name=hello_world")

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)

	err := client.DeleteTemplate(context.Background(), account, "hello_world")
	require.NoError(t, err)
}

func TestClient_DeleteTemplate_APIError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(whatsapp.MetaAPIError{
			Error: struct {
				Message      string `json:"message"`
				Type         string `json:"type"`
				Code         int    `json:"code"`
				ErrorSubcode int    `json:"error_subcode"`
				ErrorUserMsg string `json:"error_user_msg"`
				ErrorData    struct {
					Details string `json:"details"`
				} `json:"error_data"`
				FBTraceID string `json:"fbtrace_id"`
			}{
				Message: "Template not found",
				Code:    100,
			},
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)

	err := client.DeleteTemplate(context.Background(), account, "nonexistent")
	require.Error(t, err)
}

// --- FetchTemplates pagination ---

func TestClient_FetchTemplates_FollowsCursor(t *testing.T) {
	t.Parallel()

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		if r.URL.Query().Get("after") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "1", "name": "one", "language": "en", "status": "APPROVED"},
					{"id": "2", "name": "two", "language": "en", "status": "APPROVED"},
				},
				"paging": map[string]any{
					"cursors": map[string]string{"before": "b1", "after": "cursor-2"},
					"next":    "https://graph.facebook.com/next-page",
				},
			})
			return
		}
		assert.Equal(t, "cursor-2", r.URL.Query().Get("after"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "3", "name": "three", "language": "en", "status": "APPROVED"},
			},
			"paging": map[string]any{
				"cursors": map[string]string{"before": "b2", "after": "cursor-3"},
			},
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	templates, err := client.FetchTemplates(context.Background(), testAccount(server.URL))
	require.NoError(t, err)
	require.Len(t, templates, 3)
	assert.Equal(t, 2, calls)
	assert.Equal(t, "three", templates[2].Name)
}

// --- MigrateTemplates ---

func TestClient_MigrateTemplates_Success(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v21.0/987654321/migrate_message_templates", r.URL.Path)
		assert.Equal(t, "111222333", r.URL.Query().Get("source_waba_id"))
		assert.False(t, r.URL.Query().Has("page_number"), "page_number must not be sent with template_ids")
		assert.Equal(t, "Bearer test-access-token", r.Header.Get("Authorization"))

		var body map[string]any
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, float64(50), body["count"])
		assert.Equal(t, []any{"t1", "t2"}, body["template_ids"])

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"migrated_templates": []string{"9001", "9002"},
			"failed_templates":   map[string]string{"t3": "Incorrect category"},
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	res, err := client.MigrateTemplates(context.Background(), testAccount(server.URL), whatsapp.MigrateTemplatesOptions{
		SourceWABAID: "111222333",
		PageNumber:   2,
		Count:        50,
		TemplateIDs:  []string{"t1", "t2"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"9001", "9002"}, res.MigratedTemplates)
	assert.Equal(t, map[string]string{"t3": "Incorrect category"}, res.FailedTemplates)
}

func TestClient_MigrateTemplates_PageModeSendsPageNumberAndNoBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "3", r.URL.Query().Get("page_number"))
		assert.Equal(t, int64(0), r.ContentLength)

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"migrated_templates": []string{}})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	res, err := client.MigrateTemplates(context.Background(), testAccount(server.URL), whatsapp.MigrateTemplatesOptions{
		SourceWABAID: "111222333",
		PageNumber:   3,
	})
	require.NoError(t, err)
	assert.Empty(t, res.MigratedTemplates)
	assert.NotNil(t, res.FailedTemplates)
	assert.Empty(t, res.FailedTemplates)
}

func TestClient_MigrateTemplates_UsesOwnTimeout(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"migrated_templates": []string{"1"}})
	}))
	defer server.Close()

	// A client whose shared timeout would fail any request must still complete
	// a migrate call, proving the call does not inherit the shared timeout.
	client := newTestClient(t, server)
	client.HTTPClient.Timeout = time.Nanosecond
	res, err := client.MigrateTemplates(context.Background(), testAccount(server.URL), whatsapp.MigrateTemplatesOptions{
		SourceWABAID: "111222333",
		TemplateIDs:  []string{"t1"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"1"}, res.MigratedTemplates)
	assert.Equal(t, time.Nanosecond, client.HTTPClient.Timeout, "shared client must be left untouched")
}

func TestClient_MigrateTemplates_MetaError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "(#100) Source WABA is not owned by the same business",
				"code":    100,
			},
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.MigrateTemplates(context.Background(), testAccount(server.URL), whatsapp.MigrateTemplatesOptions{
		SourceWABAID: "111222333",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "same business")
}

func TestClient_MigrateTemplates_Validation(t *testing.T) {
	t.Parallel()

	client := whatsapp.NewWithTimeout(testutil.NopLogger(), time.Second)
	account := &whatsapp.Account{BusinessID: "1", APIVersion: "v21.0", AccessToken: "x"}
	ctx := context.Background()

	_, err := client.MigrateTemplates(ctx, account, whatsapp.MigrateTemplatesOptions{})
	assert.ErrorContains(t, err, "source WABA ID is required")

	tooMany := make([]string, whatsapp.MaxMigrateTemplatesPerRequest+1)
	_, err = client.MigrateTemplates(ctx, account, whatsapp.MigrateTemplatesOptions{SourceWABAID: "2", TemplateIDs: tooMany})
	assert.ErrorContains(t, err, "at most 500")

	_, err = client.MigrateTemplates(ctx, account, whatsapp.MigrateTemplatesOptions{SourceWABAID: "2", Count: 501})
	assert.ErrorContains(t, err, "count must be")

	_, err = client.MigrateTemplates(ctx, account, whatsapp.MigrateTemplatesOptions{SourceWABAID: "2", PageNumber: -1})
	assert.ErrorContains(t, err, "page number")
}
