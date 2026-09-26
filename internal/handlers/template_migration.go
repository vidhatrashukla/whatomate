package handlers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// Template migration copies approved templates from one WhatsApp Business
// Account (WABA) into another using Meta's migrate_message_templates API.
// Meta recreates the templates on the destination; the source WABA is left
// untouched. Both WABAs must belong to the same Meta Business, and only
// templates with status APPROVED and a GREEN or UNKNOWN quality score are
// eligible. Migrated templates start with an UNKNOWN quality rating on the
// destination.

// migrateTemplatesChunkSize is how many template IDs go into one migrate
// call. Meta allows up to 500 but advises reducing the batch when a call runs
// past its 30 second budget, so we stay well under.
const migrateTemplatesChunkSize = 100

// wabaIDPattern is the shape of a Meta WhatsApp Business Account ID. A raw
// source ID is interpolated into a Graph API path, so anything else is
// rejected before it reaches the client.
var wabaIDPattern = regexp.MustCompile(`^[0-9]+$`)

// TemplateMigrationRequest identifies the destination account and the source
// WABA. Exactly one of SourceWhatsAppAccount (a connected account in this
// organization) or SourceWABAID (a raw WABA ID under the same Meta Business)
// must be given.
type TemplateMigrationRequest struct {
	WhatsAppAccount       string `json:"whatsapp_account"`
	SourceWhatsAppAccount string `json:"source_whatsapp_account"`
	SourceWABAID          string `json:"source_waba_id"`
}

// MigrateTemplatesRequest is the body for POST /api/templates/migrate.
type MigrateTemplatesRequest struct {
	TemplateMigrationRequest
	// TemplateIDs restricts the migration to these source template IDs. When
	// omitted, every eligible template on the source WABA is migrated.
	TemplateIDs []string `json:"template_ids"`
}

// TemplateMigrationPreviewItem describes one source template and whether it
// can be migrated.
type TemplateMigrationPreviewItem struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Language      string `json:"language"`
	Category      string `json:"category"`
	Status        string `json:"status"`
	QualityRating string `json:"quality_rating"`
	// Eligible is true when Meta's documented preconditions are met.
	Eligible bool `json:"eligible"`
	// Reason explains why the template is not eligible.
	Reason string `json:"reason,omitempty"`
	// ExistsOnDestination flags a name + language that already exists on the
	// destination WABA. Meta's docs do not say how it treats duplicates, so
	// the template stays selectable; clients should default it to unchecked.
	ExistsOnDestination bool `json:"exists_on_destination"`
}

// migrationSource is the resolved source of a migration: the WABA ID Meta
// needs, plus an account the templates can be listed with.
type migrationSource struct {
	wabaID string
	label  string
	reader *whatsapp.Account
}

// resolveMigrationSource turns the request into a source WABA ID and a
// credential for reading its templates. A connected account is read with its
// own token, which certainly has access. A raw WABA ID is read with the
// destination's token, which works when both WABAs sit under the same Meta
// Business and that token's system user has the source WABA assigned.
// On failure the error envelope has already been sent.
func (a *App) resolveMigrationSource(r *fastglue.Request, orgID uuid.UUID, req TemplateMigrationRequest, dest *models.WhatsAppAccount) (*migrationSource, error) {
	sourceName := strings.TrimSpace(req.SourceWhatsAppAccount)
	sourceWABA := strings.TrimSpace(req.SourceWABAID)

	switch {
	case sourceName != "" && sourceWABA != "":
		_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Provide either source_whatsapp_account or source_waba_id, not both", nil, "")
		return nil, errEnvelopeSent
	case sourceName != "":
		if sourceName == dest.Name {
			_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Source and destination accounts must be different", nil, "")
			return nil, errEnvelopeSent
		}
		source, err := a.resolveWhatsAppAccount(orgID, sourceName)
		if err != nil {
			_ = r.SendErrorEnvelope(fasthttp.StatusNotFound, "Source WhatsApp account not found", nil, "")
			return nil, errEnvelopeSent
		}
		if source.BusinessID == dest.BusinessID {
			_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Source and destination accounts share the same WABA", nil, "")
			return nil, errEnvelopeSent
		}
		return &migrationSource{wabaID: source.BusinessID, label: source.Name, reader: a.toWhatsAppAccount(source)}, nil
	case sourceWABA != "":
		if !wabaIDPattern.MatchString(sourceWABA) {
			_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "source_waba_id must be a numeric WhatsApp Business Account ID", nil, "")
			return nil, errEnvelopeSent
		}
		if sourceWABA == dest.BusinessID {
			_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Source WABA must be different from the destination WABA", nil, "")
			return nil, errEnvelopeSent
		}
		reader := a.toWhatsAppAccount(dest)
		reader.BusinessID = sourceWABA
		return &migrationSource{wabaID: sourceWABA, label: sourceWABA, reader: reader}, nil
	default:
		_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "source_whatsapp_account or source_waba_id is required", nil, "")
		return nil, errEnvelopeSent
	}
}

// resolveMigrationDestination loads and validates the destination account.
// On failure the error envelope has already been sent.
func (a *App) resolveMigrationDestination(r *fastglue.Request, orgID uuid.UUID, accountName string) (*models.WhatsAppAccount, error) {
	accountName = strings.TrimSpace(accountName)
	if accountName == "" {
		_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "whatsapp_account is required", nil, "")
		return nil, errEnvelopeSent
	}
	dest, err := a.resolveWhatsAppAccount(orgID, accountName)
	if err != nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusNotFound, "WhatsApp account not found", nil, "")
		return nil, errEnvelopeSent
	}
	return dest, nil
}

// templateQuality returns the quality value Meta reported, preferring the
// newer quality_score.score over the legacy quality_rating.
func templateQuality(t whatsapp.MetaTemplate) string {
	if t.QualityScore != nil && t.QualityScore.Score != "" {
		return t.QualityScore.Score
	}
	return t.QualityRating
}

// migrationEligibility applies Meta's documented preconditions: status
// APPROVED and a GREEN or UNKNOWN quality score. The legacy quality_rating
// field uses HIGH/MEDIUM/LOW; HIGH is treated as GREEN. An absent rating is
// treated as UNKNOWN, matching how sync stores it. Meta remains the judge:
// anything it still refuses comes back in failed_templates.
func migrationEligibility(t whatsapp.MetaTemplate) (bool, string) {
	if !strings.EqualFold(t.Status, "APPROVED") {
		return false, fmt.Sprintf("Only APPROVED templates can be migrated (status is %s)", t.Status)
	}
	switch strings.ToUpper(templateQuality(t)) {
	case "", "GREEN", "UNKNOWN", "HIGH":
		return true, ""
	default:
		return false, fmt.Sprintf("Quality rating must be GREEN or UNKNOWN (rating is %s)", templateQuality(t))
	}
}

// metaMigrationErrorMessage turns a Meta failure into a message that names
// the likely cause. A timeout gets its own text because Meta may still have
// completed the work.
func metaMigrationErrorMessage(prefix string, err error) string {
	if isTimeout(err) {
		return prefix + ": the request to Meta timed out. Templates may still have been migrated; sync the destination and preview again before retrying."
	}
	return prefix + ": " + err.Error() +
		". Common causes: the two WABAs are not under the same Meta Business, or the access token's system user does not have the source WABA assigned."
}

// isTimeout reports whether err came from a client or context deadline.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// PreviewTemplateMigration lists the templates on a source WABA and marks
// which ones Meta will accept for migration into the destination account.
// It reads Meta only; nothing is written.
func (a *App) PreviewTemplateMigration(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceTemplates, models.ActionRead)
	if err != nil {
		return nil
	}

	var req TemplateMigrationRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	dest, err := a.resolveMigrationDestination(r, orgID, req.WhatsAppAccount)
	if err != nil {
		return nil
	}
	source, err := a.resolveMigrationSource(r, orgID, req, dest)
	if err != nil {
		return nil
	}

	ctx := context.Background()
	sourceTemplates, err := a.WhatsApp.FetchTemplates(ctx, source.reader)
	if err != nil {
		a.Log.Error("Failed to fetch source WABA templates", "error", err, "source_waba_id", source.wabaID)
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, metaMigrationErrorMessage("Failed to fetch templates from the source WABA", err), nil, "")
	}
	destTemplates, err := a.WhatsApp.FetchTemplates(ctx, a.toWhatsAppAccount(dest))
	if err != nil {
		a.Log.Error("Failed to fetch destination WABA templates", "error", err, "destination_waba_id", dest.BusinessID)
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Failed to fetch templates from the destination WABA: "+err.Error(), nil, "")
	}

	existing := make(map[string]struct{}, len(destTemplates))
	for _, t := range destTemplates {
		existing[templateKey(t)] = struct{}{}
	}

	items := make([]TemplateMigrationPreviewItem, 0, len(sourceTemplates))
	eligibleCount := 0
	for _, t := range sourceTemplates {
		eligible, reason := migrationEligibility(t)
		if eligible {
			eligibleCount++
		}
		_, exists := existing[templateKey(t)]
		items = append(items, TemplateMigrationPreviewItem{
			ID:                  t.ID,
			Name:                t.Name,
			Language:            t.Language,
			Category:            t.Category,
			Status:              t.Status,
			QualityRating:       templateQuality(t),
			Eligible:            eligible,
			Reason:              reason,
			ExistsOnDestination: exists,
		})
	}

	return r.SendEnvelope(map[string]any{
		"source_waba_id":      source.wabaID,
		"source_label":        source.label,
		"destination_waba_id": dest.BusinessID,
		"templates":           items,
		"total":               len(items),
		"eligible":            eligibleCount,
	})
}

// templateKey is the case-insensitive name + language identity Meta uses for
// templates within a WABA.
func templateKey(t whatsapp.MetaTemplate) string {
	return strings.ToLower(t.Name) + "|" + strings.ToLower(t.Language)
}

// MigrateTemplates copies templates from a source WABA into the destination
// account's WABA, then syncs the destination so the new templates appear
// locally. With template_ids it migrates exactly those. Without, it lists the
// source, keeps the templates that meet Meta's preconditions, and migrates
// those by ID. Either way Meta is called in chunks of migrateTemplatesChunkSize.
func (a *App) MigrateTemplates(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceTemplates, models.ActionWrite)
	if err != nil {
		return nil
	}

	var req MigrateTemplatesRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	dest, err := a.resolveMigrationDestination(r, orgID, req.WhatsAppAccount)
	if err != nil {
		return nil
	}
	source, err := a.resolveMigrationSource(r, orgID, req.TemplateMigrationRequest, dest)
	if err != nil {
		return nil
	}

	ctx := context.Background()

	templateIDs := dedupeStrings(req.TemplateIDs)
	if len(req.TemplateIDs) > 0 && len(templateIDs) == 0 {
		// The caller tried to restrict the migration but every id was blank;
		// silently widening to "everything" would be the opposite of the ask.
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "template_ids contains no valid IDs", nil, "")
	}
	skipped := 0
	if len(templateIDs) == 0 {
		sourceTemplates, err := a.WhatsApp.FetchTemplates(ctx, source.reader)
		if err != nil {
			a.Log.Error("Failed to fetch source WABA templates", "error", err, "source_waba_id", source.wabaID)
			return r.SendErrorEnvelope(fasthttp.StatusBadGateway, metaMigrationErrorMessage("Failed to fetch templates from the source WABA", err), nil, "")
		}
		for _, t := range sourceTemplates {
			if eligible, _ := migrationEligibility(t); eligible {
				templateIDs = append(templateIDs, t.ID)
			} else {
				skipped++
			}
		}
	}

	destAccount := a.toWhatsAppAccount(dest)
	migrated := []string{}
	failed := map[string]string{}
	var migrateErr error

	for start := 0; start < len(templateIDs); start += migrateTemplatesChunkSize {
		end := min(start+migrateTemplatesChunkSize, len(templateIDs))
		res, err := a.WhatsApp.MigrateTemplates(ctx, destAccount, whatsapp.MigrateTemplatesOptions{
			SourceWABAID: source.wabaID,
			TemplateIDs:  templateIDs[start:end],
		})
		if err != nil {
			migrateErr = err
			break
		}
		migrated = append(migrated, res.MigratedTemplates...)
		for id, reason := range res.FailedTemplates {
			failed[id] = reason
		}
	}

	result := map[string]any{
		"source_waba_id":      source.wabaID,
		"destination_waba_id": dest.BusinessID,
		"requested_count":     len(templateIDs),
		"skipped_count":       skipped,
		"migrated_count":      len(migrated),
		"failed_count":        len(failed),
		"migrated_templates":  migrated,
		"failed_templates":    failed,
		"synced_count":        0,
	}

	// Whatever Meta did is already done; pull the destination so local
	// templates reflect it even when the migration stopped early. A call
	// that never reached Meta successfully has nothing to sync.
	if len(migrated) > 0 || len(failed) > 0 || isTimeout(migrateErr) {
		synced, syncErr := a.syncTemplatesFromMeta(orgID, dest)
		result["synced_count"] = synced
		if syncErr != nil {
			a.Log.Error("Failed to sync templates after migration", "error", syncErr, "account", dest.Name)
			result["sync_error"] = "Migration finished but the destination could not be synced: " + syncErr.Error()
		}
	}

	if len(migrated) > 0 || len(failed) > 0 {
		a.logAudit(orgID, userID,
			"account", dest.ID, models.AuditActionUpdated, nil, nil,
			map[string]any{
				"field":          "templates_migrated",
				"source_waba_id": source.wabaID,
				"migrated":       len(migrated),
				"failed":         len(failed),
			},
		)
	}

	if migrateErr != nil {
		a.Log.Error("Template migration stopped", "error", migrateErr, "source_waba_id", source.wabaID, "destination_waba_id", dest.BusinessID, "migrated", len(migrated))
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway,
			metaMigrationErrorMessage(fmt.Sprintf("Migration stopped after %d template(s)", len(migrated)), migrateErr),
			result, "")
	}

	result["message"] = fmt.Sprintf("Migrated %d template(s), %d failed", len(migrated), len(failed))
	return r.SendEnvelope(result)
}

// dedupeStrings trims, drops empties and removes duplicates while keeping the
// first occurrence's position.
func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
