package handlers_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// migrateCall records one migrate_message_templates request seen by the mock.
type migrateCall struct {
	DestWABA   string
	SourceWABA string
	PageNumber string
	Body       map[string]any
}

// mockMigrationServer fakes the two Meta endpoints the migration handlers use:
// GET /{waba}/message_templates (per WABA) and POST /{waba}/migrate_message_templates.
type mockMigrationServer struct {
	*httptest.Server
	mu        sync.Mutex
	templates map[string][]map[string]any // waba id -> template list
	failList  map[string]bool             // waba id -> respond 500 to list calls
	listAuth  map[string][]string         // waba id -> Authorization headers seen on list calls
	// migrateFn builds the migrate response for a call; nil means "nothing
	// migrated, nothing failed".
	migrateFn func(call migrateCall) (int, map[string]any)
	calls     []migrateCall
}

func newMockMigrationServer(t *testing.T) *mockMigrationServer {
	t.Helper()
	m := &mockMigrationServer{
		templates: map[string][]map[string]any{},
		failList:  map[string]bool{},
		listAuth:  map[string][]string{},
	}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if !assert.GreaterOrEqual(t, len(parts), 3, "unexpected path %s", r.URL.Path) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		waba, resource := parts[1], parts[2]

		switch {
		case r.Method == http.MethodGet && resource == "message_templates":
			m.mu.Lock()
			list := m.templates[waba]
			fail := m.failList[waba]
			m.listAuth[waba] = append(m.listAuth[waba], r.Header.Get("Authorization"))
			m.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "boom", "code": 2}})
				return
			}
			if list == nil {
				list = []map[string]any{}
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": list})

		case r.Method == http.MethodPost && resource == "migrate_message_templates":
			call := migrateCall{
				DestWABA:   waba,
				SourceWABA: r.URL.Query().Get("source_waba_id"),
				PageNumber: r.URL.Query().Get("page_number"),
			}
			if r.ContentLength > 0 {
				_ = json.NewDecoder(r.Body).Decode(&call.Body)
			}
			m.mu.Lock()
			m.calls = append(m.calls, call)
			fn := m.migrateFn
			m.mu.Unlock()

			status, resp := http.StatusOK, map[string]any{"migrated_templates": []string{}, "failed_templates": map[string]string{}}
			if fn != nil {
				status, resp = fn(call)
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(resp)

		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "unexpected " + r.Method + " " + r.URL.Path}})
		}
	}))
	t.Cleanup(m.Server.Close)
	return m
}

func (m *mockMigrationServer) setTemplates(waba string, list []map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templates[waba] = list
}

func (m *mockMigrationServer) setListFails(waba string, fails bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failList[waba] = fails
}

func (m *mockMigrationServer) listAuthHeaders(waba string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.listAuth[waba]...)
}

func (m *mockMigrationServer) migrateCalls() []migrateCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]migrateCall, len(m.calls))
	copy(out, m.calls)
	return out
}

func metaTemplate(id, name, language, status, quality string) map[string]any {
	t := map[string]any{
		"id": id, "name": name, "language": language, "category": "UTILITY", "status": status,
		"components": []map[string]any{{"type": "BODY", "text": "Body of " + name}},
	}
	if quality != "" {
		t["quality_score"] = map[string]any{"score": quality}
	}
	return t
}

// migrationFixture wires an org, a user with the given template permissions,
// and two connected accounts (source + destination) against the mock server.
type migrationFixture struct {
	app    *handlers.App
	server *mockMigrationServer
	org    *models.Organization
	user   *models.User
	source *models.WhatsAppAccount
	dest   *models.WhatsAppAccount
}

func newMigrationFixture(t *testing.T, permissionKeys ...string) *migrationFixture {
	t.Helper()
	server := newMockMigrationServer(t)
	app := newTemplateTestApp(t, server.Server)

	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "Template Migrator "+uuid.New().String()[:8], permissionKeys)
	user := testutil.CreateTestUser(t, app.DB, org.ID,
		testutil.WithEmail(testutil.UniqueEmail("template-migration")),
		testutil.WithRoleID(&role.ID))
	source := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID,
		testutil.WithAccountName("source-"+uuid.New().String()[:8]),
		withBusinessID("1"+numericID()),
		withAccessToken("source-token"))
	dest := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID,
		testutil.WithAccountName("dest-"+uuid.New().String()[:8]),
		withBusinessID("2"+numericID()),
		withAccessToken("dest-token"))

	return &migrationFixture{app: app, server: server, org: org, user: user, source: source, dest: dest}
}

func withBusinessID(id string) testutil.WhatsAppAccountOption {
	return func(a *models.WhatsAppAccount) { a.BusinessID = id }
}

func withAccessToken(token string) testutil.WhatsAppAccountOption {
	return func(a *models.WhatsAppAccount) { a.AccessToken = token }
}

// numericID returns a unique digits-only suffix, since WABA IDs are numeric.
func numericID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000_000)
}

func (f *migrationFixture) request(t *testing.T, body map[string]any) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, body)
	testutil.SetAuthContext(req, f.org.ID, f.user.ID)
	return req
}

// --- PreviewTemplateMigration ---

func TestApp_PreviewTemplateMigration_ConnectedSource(t *testing.T) {
	t.Parallel()

	f := newMigrationFixture(t, "templates:read")
	f.server.setTemplates(f.source.BusinessID, []map[string]any{
		metaTemplate("s1", "order_update", "en", "APPROVED", "GREEN"),
		metaTemplate("s2", "promo", "en", "APPROVED", "RED"),
		metaTemplate("s3", "otp", "en", "PENDING", "UNKNOWN"),
		metaTemplate("s4", "welcome", "hi", "APPROVED", ""),
	})
	f.server.setTemplates(f.dest.BusinessID, []map[string]any{
		metaTemplate("d1", "WELCOME", "hi", "APPROVED", "GREEN"),
	})

	req := f.request(t, map[string]any{
		"whatsapp_account":        f.dest.Name,
		"source_whatsapp_account": f.source.Name,
	})
	require.NoError(t, f.app.PreviewTemplateMigration(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data struct {
			SourceWABAID string                                  `json:"source_waba_id"`
			SourceLabel  string                                  `json:"source_label"`
			DestWABAID   string                                  `json:"destination_waba_id"`
			Total        int                                     `json:"total"`
			Eligible     int                                     `json:"eligible"`
			Templates    []handlers.TemplateMigrationPreviewItem `json:"templates"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	assert.Equal(t, f.source.BusinessID, resp.Data.SourceWABAID)
	assert.Equal(t, f.source.Name, resp.Data.SourceLabel)
	assert.Equal(t, f.dest.BusinessID, resp.Data.DestWABAID)
	assert.Equal(t, 4, resp.Data.Total)
	assert.Equal(t, 2, resp.Data.Eligible)

	byID := map[string]handlers.TemplateMigrationPreviewItem{}
	for _, item := range resp.Data.Templates {
		byID[item.ID] = item
	}
	assert.True(t, byID["s1"].Eligible)
	assert.Equal(t, "GREEN", byID["s1"].QualityRating)
	assert.False(t, byID["s1"].ExistsOnDestination)

	assert.False(t, byID["s2"].Eligible)
	assert.Contains(t, byID["s2"].Reason, "GREEN or UNKNOWN")

	assert.False(t, byID["s3"].Eligible)
	assert.Contains(t, byID["s3"].Reason, "APPROVED")

	assert.True(t, byID["s4"].Eligible, "missing quality counts as UNKNOWN")
	assert.True(t, byID["s4"].ExistsOnDestination, "name+language collision is case-insensitive")

	assert.Equal(t, []string{"Bearer source-token"}, f.server.listAuthHeaders(f.source.BusinessID), "a connected source is read with its own token")
	assert.Equal(t, []string{"Bearer dest-token"}, f.server.listAuthHeaders(f.dest.BusinessID))
	assert.Empty(t, f.server.migrateCalls(), "preview must not migrate")
}

func TestApp_PreviewTemplateMigration_RawWABAID(t *testing.T) {
	t.Parallel()

	f := newMigrationFixture(t, "templates:read")
	rawWABA := "9" + numericID()
	f.server.setTemplates(rawWABA, []map[string]any{
		metaTemplate("r1", "hello", "en", "APPROVED", "UNKNOWN"),
	})

	req := f.request(t, map[string]any{
		"whatsapp_account": f.dest.Name,
		"source_waba_id":   " " + rawWABA + " ",
	})
	require.NoError(t, f.app.PreviewTemplateMigration(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.Equal(t, []string{"Bearer dest-token"}, f.server.listAuthHeaders(rawWABA), "a raw WABA is read with the destination's token")

	var resp struct {
		Data struct {
			SourceWABAID string `json:"source_waba_id"`
			SourceLabel  string `json:"source_label"`
			Eligible     int    `json:"eligible"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	assert.Equal(t, rawWABA, resp.Data.SourceWABAID)
	assert.Equal(t, rawWABA, resp.Data.SourceLabel)
	assert.Equal(t, 1, resp.Data.Eligible)
}

func TestApp_PreviewTemplateMigration_Validation(t *testing.T) {
	t.Parallel()

	f := newMigrationFixture(t, "templates:read")

	cases := []struct {
		name    string
		body    map[string]any
		status  int
		message string
	}{
		{"missing destination", map[string]any{"source_waba_id": "1"}, fasthttp.StatusBadRequest, "whatsapp_account is required"},
		{"unknown destination", map[string]any{"whatsapp_account": "nope", "source_waba_id": "1"}, fasthttp.StatusNotFound, "WhatsApp account not found"},
		{"missing source", map[string]any{"whatsapp_account": f.dest.Name}, fasthttp.StatusBadRequest, "source_whatsapp_account or source_waba_id is required"},
		{"both sources", map[string]any{"whatsapp_account": f.dest.Name, "source_whatsapp_account": f.source.Name, "source_waba_id": "1"}, fasthttp.StatusBadRequest, "not both"},
		{"same account", map[string]any{"whatsapp_account": f.dest.Name, "source_whatsapp_account": f.dest.Name}, fasthttp.StatusBadRequest, "must be different"},
		{"same waba id", map[string]any{"whatsapp_account": f.dest.Name, "source_waba_id": f.dest.BusinessID}, fasthttp.StatusBadRequest, "different from the destination"},
		{"unknown source account", map[string]any{"whatsapp_account": f.dest.Name, "source_whatsapp_account": "ghost"}, fasthttp.StatusNotFound, "Source WhatsApp account not found"},
		{"non-numeric waba id", map[string]any{"whatsapp_account": f.dest.Name, "source_waba_id": "123/subscribed_apps?x="}, fasthttp.StatusBadRequest, "must be a numeric"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := f.request(t, tc.body)
			require.NoError(t, f.app.PreviewTemplateMigration(req))
			testutil.AssertErrorResponse(t, req, tc.status, tc.message)
		})
	}
	assert.Empty(t, f.server.migrateCalls())
}

func TestApp_PreviewTemplateMigration_SourceFetchFails(t *testing.T) {
	t.Parallel()

	f := newMigrationFixture(t, "templates:read")
	f.server.setListFails(f.source.BusinessID, true)

	req := f.request(t, map[string]any{
		"whatsapp_account":        f.dest.Name,
		"source_whatsapp_account": f.source.Name,
	})
	require.NoError(t, f.app.PreviewTemplateMigration(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusBadGateway, "Failed to fetch templates from the source WABA")
	assert.Contains(t, string(testutil.GetResponseBody(req)), "Common causes")
}

func TestApp_PreviewTemplateMigration_Forbidden(t *testing.T) {
	t.Parallel()

	f := newMigrationFixture(t, "contacts:read")
	req := f.request(t, map[string]any{"whatsapp_account": f.dest.Name, "source_whatsapp_account": f.source.Name})
	require.NoError(t, f.app.PreviewTemplateMigration(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

// --- MigrateTemplates ---

func TestApp_MigrateTemplates_ByIDs(t *testing.T) {
	t.Parallel()

	f := newMigrationFixture(t, "templates:read", "templates:write")
	f.server.migrateFn = func(call migrateCall) (int, map[string]any) {
		return http.StatusOK, map[string]any{
			"migrated_templates": []string{"n1", "n2"},
			"failed_templates":   map[string]string{"s3": "Incorrect category"},
		}
	}
	// After migration the destination lists the new templates; sync must pick them up.
	f.server.setTemplates(f.dest.BusinessID, []map[string]any{
		metaTemplate("n1", "order_update", "en", "APPROVED", "UNKNOWN"),
		metaTemplate("n2", "welcome", "en", "APPROVED", "UNKNOWN"),
	})

	req := f.request(t, map[string]any{
		"whatsapp_account":        f.dest.Name,
		"source_whatsapp_account": f.source.Name,
		"template_ids":            []string{"s1", " s2 ", "s1", "", "s3"},
	})
	require.NoError(t, f.app.MigrateTemplates(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	calls := f.server.migrateCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, f.dest.BusinessID, calls[0].DestWABA)
	assert.Equal(t, f.source.BusinessID, calls[0].SourceWABA)
	assert.Equal(t, "", calls[0].PageNumber, "page_number is not sent when migrating by id")
	assert.Equal(t, []any{"s1", "s2", "s3"}, calls[0].Body["template_ids"], "ids are trimmed and de-duplicated")
	assert.Nil(t, calls[0].Body["count"], "count is not sent when migrating by id")

	var resp struct {
		Data struct {
			Message           string            `json:"message"`
			MigratedCount     int               `json:"migrated_count"`
			FailedCount       int               `json:"failed_count"`
			MigratedTemplates []string          `json:"migrated_templates"`
			FailedTemplates   map[string]string `json:"failed_templates"`
			SyncedCount       int               `json:"synced_count"`
			SyncError         string            `json:"sync_error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	assert.Equal(t, 2, resp.Data.MigratedCount)
	assert.Equal(t, 1, resp.Data.FailedCount)
	assert.Equal(t, []string{"n1", "n2"}, resp.Data.MigratedTemplates)
	assert.Equal(t, "Incorrect category", resp.Data.FailedTemplates["s3"])
	assert.Equal(t, 2, resp.Data.SyncedCount)
	assert.Empty(t, resp.Data.SyncError)
	assert.Contains(t, resp.Data.Message, "Migrated 2")

	var local []models.Template
	require.NoError(t, f.app.DB.Where("organization_id = ? AND whats_app_account = ?", f.org.ID, f.dest.Name).Find(&local).Error)
	assert.Len(t, local, 2, "destination was synced after migration")

	var logs []models.AuditLog
	require.NoError(t, f.app.DB.Where("organization_id = ? AND resource_id = ?", f.org.ID, f.dest.ID).Find(&logs).Error)
	require.Len(t, logs, 1)
	assert.Equal(t, "account", logs[0].ResourceType)
}

func TestApp_MigrateTemplates_AllEligibleFromSourceList(t *testing.T) {
	t.Parallel()

	f := newMigrationFixture(t, "templates:read", "templates:write")
	rawWABA := "9" + numericID()
	f.server.setTemplates(rawWABA, []map[string]any{
		metaTemplate("a", "one", "en", "APPROVED", "GREEN"),
		metaTemplate("b", "two", "en", "APPROVED", "RED"),
		metaTemplate("c", "three", "en", "PENDING", "UNKNOWN"),
		metaTemplate("d", "four", "en", "APPROVED", ""),
	})
	f.server.migrateFn = func(call migrateCall) (int, map[string]any) {
		return http.StatusOK, map[string]any{"migrated_templates": []string{"n1", "n2"}, "failed_templates": map[string]string{}}
	}

	req := f.request(t, map[string]any{
		"whatsapp_account": f.dest.Name,
		"source_waba_id":   rawWABA,
	})
	require.NoError(t, f.app.MigrateTemplates(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	calls := f.server.migrateCalls()
	require.Len(t, calls, 1, "eligible ids are migrated in one chunk; no page walk")
	assert.Equal(t, rawWABA, calls[0].SourceWABA)
	assert.Equal(t, "", calls[0].PageNumber)
	assert.Equal(t, []any{"a", "d"}, calls[0].Body["template_ids"], "only APPROVED + GREEN/UNKNOWN templates are sent")

	var resp struct {
		Data struct {
			RequestedCount int `json:"requested_count"`
			SkippedCount   int `json:"skipped_count"`
			MigratedCount  int `json:"migrated_count"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	assert.Equal(t, 2, resp.Data.RequestedCount)
	assert.Equal(t, 2, resp.Data.SkippedCount)
	assert.Equal(t, 2, resp.Data.MigratedCount)
}

func TestApp_MigrateTemplates_ChunksIDs(t *testing.T) {
	t.Parallel()

	f := newMigrationFixture(t, "templates:read", "templates:write")
	f.server.migrateFn = func(call migrateCall) (int, map[string]any) {
		ids, _ := call.Body["template_ids"].([]any)
		migrated := make([]string, len(ids))
		for i := range ids {
			migrated[i] = fmt.Sprintf("new-%d", i)
		}
		return http.StatusOK, map[string]any{"migrated_templates": migrated, "failed_templates": map[string]string{}}
	}

	ids := make([]string, 250)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%03d", i)
	}
	req := f.request(t, map[string]any{
		"whatsapp_account":        f.dest.Name,
		"source_whatsapp_account": f.source.Name,
		"template_ids":            ids,
	})
	require.NoError(t, f.app.MigrateTemplates(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	calls := f.server.migrateCalls()
	require.Len(t, calls, 3, "250 ids -> chunks of 100, 100, 50")
	assert.Len(t, calls[0].Body["template_ids"], 100)
	assert.Len(t, calls[1].Body["template_ids"], 100)
	assert.Len(t, calls[2].Body["template_ids"], 50)
	assert.Equal(t, "id-100", calls[1].Body["template_ids"].([]any)[0], "chunks are contiguous slices")

	var resp struct {
		Data struct {
			MigratedCount int `json:"migrated_count"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	assert.Equal(t, 250, resp.Data.MigratedCount)
}

func TestApp_MigrateTemplates_BlankIDsRejected(t *testing.T) {
	t.Parallel()

	f := newMigrationFixture(t, "templates:read", "templates:write")
	req := f.request(t, map[string]any{
		"whatsapp_account":        f.dest.Name,
		"source_whatsapp_account": f.source.Name,
		"template_ids":            []string{"", "  "},
	})
	require.NoError(t, f.app.MigrateTemplates(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "template_ids contains no valid IDs")
	assert.Empty(t, f.server.migrateCalls(), "blank ids must not widen into a whole-WABA migration")
}

func TestApp_MigrateTemplates_SyncFailureIsReportedNotFatal(t *testing.T) {
	t.Parallel()

	f := newMigrationFixture(t, "templates:read", "templates:write")
	f.server.migrateFn = func(call migrateCall) (int, map[string]any) {
		return http.StatusOK, map[string]any{"migrated_templates": []string{"n1"}, "failed_templates": map[string]string{}}
	}
	f.server.setListFails(f.dest.BusinessID, true)

	req := f.request(t, map[string]any{
		"whatsapp_account":        f.dest.Name,
		"source_whatsapp_account": f.source.Name,
		"template_ids":            []string{"s1"},
	})
	require.NoError(t, f.app.MigrateTemplates(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), "migration succeeded even though the follow-up sync failed")

	var resp struct {
		Data struct {
			MigratedCount int    `json:"migrated_count"`
			SyncedCount   int    `json:"synced_count"`
			SyncError     string `json:"sync_error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	assert.Equal(t, 1, resp.Data.MigratedCount)
	assert.Equal(t, 0, resp.Data.SyncedCount)
	assert.Contains(t, resp.Data.SyncError, "could not be synced")
}

func TestApp_MigrateTemplates_MetaErrorReturnsPartialResult(t *testing.T) {
	t.Parallel()

	f := newMigrationFixture(t, "templates:read", "templates:write")
	var calls int
	f.server.migrateFn = func(call migrateCall) (int, map[string]any) {
		calls++
		if calls == 1 {
			return http.StatusOK, map[string]any{"migrated_templates": []string{"a"}, "failed_templates": map[string]string{}}
		}
		return http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "(#100) Source WABA not in same business", "code": 100}}
	}

	ids := make([]string, 150)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%03d", i)
	}
	req := f.request(t, map[string]any{
		"whatsapp_account":        f.dest.Name,
		"source_whatsapp_account": f.source.Name,
		"template_ids":            ids,
	})
	require.NoError(t, f.app.MigrateTemplates(req))
	assert.Equal(t, fasthttp.StatusBadGateway, testutil.GetResponseStatusCode(req))

	var resp struct {
		Message string `json:"message"`
		Data    struct {
			MigratedCount int `json:"migrated_count"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &resp))
	assert.Contains(t, resp.Message, "Migration stopped after 1 template(s)")
	assert.Contains(t, resp.Message, "same business")
	assert.Contains(t, resp.Message, "Common causes")
	assert.Equal(t, 1, resp.Data.MigratedCount, "partial progress is reported")
}

func TestApp_MigrateTemplates_Forbidden(t *testing.T) {
	t.Parallel()

	f := newMigrationFixture(t, "templates:read", "templates:sync")
	req := f.request(t, map[string]any{"whatsapp_account": f.dest.Name, "source_whatsapp_account": f.source.Name})
	require.NoError(t, f.app.MigrateTemplates(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
	assert.Empty(t, f.server.migrateCalls())
}

func TestApp_MigrateTemplates_Validation(t *testing.T) {
	t.Parallel()

	f := newMigrationFixture(t, "templates:read", "templates:write")
	req := f.request(t, map[string]any{"whatsapp_account": f.dest.Name})
	require.NoError(t, f.app.MigrateTemplates(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "source_whatsapp_account or source_waba_id is required")
	assert.Empty(t, f.server.migrateCalls())
}
